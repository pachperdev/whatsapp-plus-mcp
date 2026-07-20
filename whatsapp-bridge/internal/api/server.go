// Package api: server.go conserva los helpers HTTP compartidos por todas las rutas
// (auth, decode, contratos de respuesta, guard anti-ban, app state con recuperación)
// y NewServer, que solo orquesta el registro por dominio. Los handlers viven en
// routes_{messages,chats,groups,contacts,session}.go.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"whatsapp-client/internal/store"
	"whatsapp-client/internal/wa"
)

// writeJSON serializa v como JSON en el ResponseWriter (el Content-Type y el status deben
// fijarse antes de llamar). Si la codificación falla —normalmente porque el cliente cerró la
// conexión— solo se registra: la respuesta ya está parcialmente enviada y no hay recuperación
// posible. Centraliza el manejo del error de Encode en los handlers HTTP.
func writeJSON(w http.ResponseWriter, v interface{}) {
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		fmt.Println("Error al codificar respuesta JSON:", err)
	}
}

// withAuth exige el token compartido (X-Auth-Token) en cada request antes de delegar en next.
// Fail-closed: si token=="" o el header no coincide (comparación en tiempo constante) responde 401.
func withAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Auth-Token")
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// parseInviteCode extrae el código de invitación de un link chat.whatsapp.com (o devuelve
// el string tal cual si ya es solo el código). Corta query string y fragment porque los
// links reales vienen como https://chat.whatsapp.com/<code>?mode=gi_t; sin esto el server
// rechaza el código con 400 bad-request.
func parseInviteCode(raw string) string {
	code := strings.TrimSpace(raw)
	if idx := strings.IndexAny(code, "?#"); idx >= 0 {
		code = code[:idx]
	}
	code = strings.TrimSuffix(code, "/")
	if idx := strings.LastIndex(code, "/"); idx >= 0 {
		code = code[idx+1:]
	}
	return code
}

// isAppStateConflict detecta el rechazo 409/LTHash del servidor al subir un patch de
// app state: ocurre cuando el estado local quedó desincronizado (típico tras un login
// por QR fresco, donde el servidor va varias versiones por delante del estado vacío local).
func isAppStateConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "mismatching LTHash") || strings.Contains(msg, `code="409"`)
}

// appStateSender es la porción mínima de *whatsmeow.Client que sendAppState usa.
// Es el seam de testeo de la lógica de recuperación: provocar un conflicto LTHash
// real exige un servidor desincronizado, así que los tests inyectan un fake que
// simula las respuestas. El call-site sigue pasando el client real (satisface la
// interfaz implícitamente).
type appStateSender interface {
	SendAppState(ctx context.Context, patch appstate.PatchInfo) error
	FetchAppState(ctx context.Context, name appstate.WAPatchName, fullSync, onlyIfNotSynced bool) error
	SendPeerMessage(ctx context.Context, message *waE2E.Message) (whatsmeow.SendResponse, error)
}

// appStateRecoveryPollInterval espacia los reintentos mientras se espera la copia
// limpia del teléfono primario. Es variable (no constante) solo para que los tests
// del poll no duerman segundos reales; en producción nunca se modifica.
var appStateRecoveryPollInterval = 3 * time.Second

// sendAppState sube un patch de app state con auto-recuperación en dos niveles ante un
// conflicto (isAppStateConflict):
//  1. resync completo de la colección (FetchAppState fullSync) y reintento;
//  2. si el propio resync viene corrupto del servidor (LTHash inválido también en el
//     snapshot), pide al teléfono primario una copia limpia de la colección
//     (BuildAppStateRecoveryRequest vía SendPeerMessage). whatsmeow instala la respuesta
//     automáticamente, así que se reintenta en un poll corto acotado por el timeout de
//     lectura del cliente Python (30s).
//
// Cualquier otro error se devuelve tal cual.
func sendAppState(client appStateSender, patch appstate.PatchInfo) error {
	ctx := context.Background()
	err := client.SendAppState(ctx, patch)
	if !isAppStateConflict(err) {
		return err
	}
	ferr := client.FetchAppState(ctx, patch.Type, true, false)
	if ferr == nil {
		return client.SendAppState(ctx, patch)
	}
	// Resync corrupto: recovery fatal vía teléfono primario (respuesta asíncrona).
	if _, perr := client.SendPeerMessage(ctx, whatsmeow.BuildAppStateRecoveryRequest(patch.Type)); perr != nil {
		return fmt.Errorf("resync de app state %s fallido (%v) y no se pudo pedir recovery al teléfono primario: %v", patch.Type, ferr, perr)
	}
	for i := 0; i < 6; i++ {
		time.Sleep(appStateRecoveryPollInterval)
		if client.FetchAppState(ctx, patch.Type, false, false) != nil {
			continue // la copia limpia del teléfono aún no llegó
		}
		if err = client.SendAppState(ctx, patch); err == nil || !isAppStateConflict(err) {
			return err
		}
	}
	return fmt.Errorf("app state %s en recuperación: se pidió una copia limpia al teléfono primario (debe estar online); reintentá la operación en unos segundos (error original: %v)", patch.Type, err)
}

// banBlocked responde 503 y devuelve true si hay un ban temporal vigente. Los
// envios salientes que NO pasan por svc.SendMessage (react/edit/revoke/poll/
// poll_vote usan client.SendMessage directo con Build*) deben chequearlo igual:
// son stanzas salientes que pueden empeorar un ban en curso. Preserva el
// invariante anti-ban del proyecto en todos los caminos de envio.
func banBlocked(w http.ResponseWriter, svc *wa.Service) bool {
	if banned, reason := svc.IsTempBanned(); banned {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("envio bloqueado: cuenta con ban temporal (%s). Espera a que expire; ver /api/status", reason),
		})
		return true
	}
	return false
}

// decodeJSON limita el body a 1 MiB (defensa DoS) y decodifica el JSON en dst.
// Si el body es inválido responde 400 con el contrato estándar
// {"success":false,"message":"invalid request"} y devuelve false; el handler
// debe cortar. El Content-Type ya lo fija el handler antes de llamar.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		respondErr(w, http.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

// parseJID valida raw con types.ParseJID. Si falla responde 400
// {"success":false,"message":"invalid "+field} y devuelve ok=false; el mensaje
// reproduce el de cada handler ("invalid chat_jid", "invalid group_jid", etc.),
// por eso field debe ser el correcto en cada call.
func parseJID(w http.ResponseWriter, raw, field string) (types.JID, bool) {
	jid, err := types.ParseJID(raw)
	if err != nil {
		respondErr(w, http.StatusBadRequest, "invalid "+field)
		return jid, false
	}
	return jid, true
}

// respondErr escribe el status y el cuerpo de error estándar
// {"success":false,"message":msg}. El Content-Type debe fijarse antes.
func respondErr(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]interface{}{"success": false, "message": msg})
}

// respondOK escribe una respuesta de éxito {"success":true, ...extra} con status
// 200 implícito. json ordena las claves del mapa, así que el resultado es
// byte-idéntico a construir el literal con "success":true ya incluido.
func respondOK(w http.ResponseWriter, extra map[string]interface{}) {
	m := map[string]interface{}{"success": true}
	for k, v := range extra {
		m[k] = v
	}
	writeJSON(w, m)
}

// NewServer registra todas las rutas REST del bridge sobre un mux propio (no el
// DefaultServeMux), cada una envuelta en withAuth con el token compartido, y devuelve el
// handler resultante. Solo orquesta: el registro concreto vive en las funciones
// register*Routes por dominio, que reciben las mismas dependencias que capturaban
// los closures cuando todo vivía aquí.
func NewServer(svc *wa.Service, client *whatsmeow.Client, st *store.MessageStore, token string, shutdownFn func()) http.Handler {
	mux := http.NewServeMux()
	registerSessionRoutes(mux, svc, client, token, shutdownFn)
	registerMessageRoutes(mux, svc, client, st, token)
	registerChatRoutes(mux, svc, client, st, token)
	registerGroupRoutes(mux, svc, client, token)
	registerContactRoutes(mux, svc, client, token)
	return mux
}
