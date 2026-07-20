// Package api: rutas del dominio SESIÓN/CUENTA (estado del bridge, login por QR,
// logout, apagado ordenado y preferencias de cuenta). Extraídas de server.go por
// dominio; movimiento mecánico sin cambios de lógica ni de contrato HTTP.
package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"

	"whatsapp-client/internal/wa"
)

// registerSessionRoutes registra las rutas de sesión/cuenta sobre el mux. Los handlers
// capturan svc/client/token/shutdownFn por closure, igual que en NewServer.
func registerSessionRoutes(mux *http.ServeMux, svc *wa.Service, client *whatsmeow.Client, token string, shutdownFn func()) {
	// Handler: estado del login por QR. Publica el código vigente (crudo + PNG base64)
	// para que el supervisor MCP lo muestre en la conversación o lo abra como imagen,
	// sin depender del stdout del proceso. El server HTTP arranca ANTES del pairing
	// precisamente para que esta ruta exista durante el modo QR.
	mux.HandleFunc("/api/qr", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			respondErr(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		if client != nil && client.Store != nil && client.Store.ID != nil && client.IsLoggedIn() {
			respondOK(w, map[string]interface{}{"qr_status": "logged_in"})
			return
		}
		status, code, expiresAt := svc.QRInfo()
		resp := map[string]interface{}{"qr_status": status}
		if status == "active" {
			png, err := qrcode.Encode(code, qrcode.Medium, 512)
			if err != nil {
				respondErr(w, http.StatusInternalServerError, fmt.Sprintf("failed to render QR: %v", err))
				return
			}
			resp["code"] = code
			resp["png_base64"] = base64.StdEncoding.EncodeToString(png)
			resp["expires_at"] = expiresAt.Format(time.RFC3339)
		}
		respondOK(w, resp)
	}))

	// Handler: apagado ordenado a pedido del supervisor. Permite reciclar el proceso
	// (p.ej. sesión zombie que necesita re-login por QR) sin señales de SO ni matar
	// procesos ajenos: responde y dispara el mismo camino que SIGTERM.
	mux.HandleFunc("/api/shutdown", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			respondErr(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		respondOK(w, map[string]interface{}{"message": "shutting down"})
		if shutdownFn != nil {
			go shutdownFn()
		}
	}))

	// Handler: estado de conexion/sesion/ban del cliente
	mux.HandleFunc("/api/status", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		m := svc.StatusSnapshot()
		m["success"] = true
		writeJSON(w, m)
	}))

	// --- Lote A1: perfil & cuenta (parte de sesión/cuenta) ---

	// Handler: set status message ("about" propio)
	mux.HandleFunc("/api/set_status", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetStatusRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if err := client.SetStatusMessage(context.Background(), req.Message); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "status updated"})
	}))

	// Handler: set default disappearing timer (aplica a chats NUEVOS)
	mux.HandleFunc("/api/default_disappearing", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req DefaultDisappearingRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		timer, ok := whatsmeow.ParseDisappearingTimerString(req.Duration)
		if !ok {
			respondErr(w, http.StatusBadRequest, "duration must be one of: off, 24h, 7d, 90d")
			return
		}
		if err := client.SetDefaultDisappearingTimer(context.Background(), timer); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "default disappearing timer set", "duration": req.Duration})
	}))

	// --- Logout: desvincula la sesión (requiere re-escanear QR para volver) ---
	mux.HandleFunc("/api/logout", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := client.Logout(context.Background()); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		svc.OnLoggedOut("logout solicitado por el usuario")
		respondOK(w, map[string]interface{}{"message": "logged out; reiniciar el bridge y re-escanear el QR para volver a vincular"})
	}))
}
