package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"whatsapp-client/internal/wa"
)

// Red de seguridad del refactor de deduplicación de handlers. Ejercita NewServer
// sobre httptest con client=nil/st=nil: solo cubrimos rutas cuyos caminos
// probados (auth, decode, parseJID, ban) retornan ANTES de tocar client/st, así
// que un nil no llega a desreferenciarse. Fija byte a byte los contratos de error.

// JIDs que types.ParseJID SÍ rechaza. Ojo: ParseJID es permisivo (un string sin
// "@" no da error); "1.2.3@..." falla con "unexpected number of dots".
const (
	badChatBody  = `{"chat_jid":"1.2.3@s.whatsapp.net"}`
	badGroupBody = `{"group_jid":"1.2.3@g.us"}`
	invalidJSON  = `{` // decode falla
)

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	svc := wa.NewService(nil, nil, nil, "", nil)
	return NewServer(svc, nil, nil, testToken, nil)
}

func newBannedTestServer(t *testing.T) http.Handler {
	t.Helper()
	svc := wa.NewService(nil, nil, nil, "", nil)
	svc.OnTempBan(104, "spam", time.Hour)
	return NewServer(svc, nil, nil, testToken, nil)
}

// doReq lanza un POST contra el handler. Con token=="" no manda X-Auth-Token.
func doReq(t *testing.T, h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// wantErrJSON fija el contrato de error: status esperado + cuerpo JSON
// {"success":false,"message":msg}.
func wantErrJSON(t *testing.T, rec *httptest.ResponseRecorder, status int, msg string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, status, rec.Body.String())
	}
	var got struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body no es JSON válido: %v (body=%q)", err, rec.Body.String())
	}
	if got.Success {
		t.Fatalf("success = true, want false (body=%q)", rec.Body.String())
	}
	if got.Message != msg {
		t.Fatalf("message = %q, want %q", got.Message, msg)
	}
}

// (a) 401 sin token: withAuth corta antes del handler (fail-closed).
func TestServer_Unauthorized(t *testing.T) {
	h := newTestServer(t)
	for _, path := range []string{
		"/api/send", "/api/react", "/api/star",
		"/api/group_participants", "/api/set_group_name",
		"/api/edit", "/api/mark_read", "/api/delete_chat",
	} {
		rec := doReq(t, h, path, "", `{}`)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s sin token: status=%d, want 401", path, rec.Code)
		}
	}
}

// (b) 400 + {"success":false,"message":"invalid request"} con body inválido.
func TestServer_InvalidBody(t *testing.T) {
	h := newTestServer(t)
	for _, path := range []string{
		"/api/react", "/api/star", "/api/mark_read",
		"/api/group_participants", "/api/set_group_name",
		"/api/delete_chat",
	} {
		rec := doReq(t, h, path, testToken, invalidJSON)
		wantErrJSON(t, rec, http.StatusBadRequest, "invalid request")
	}
}

// (c) 400 + mensaje de JID correcto ("invalid chat_jid" / "invalid group_jid").
func TestServer_InvalidJID(t *testing.T) {
	h := newTestServer(t)
	for _, path := range []string{"/api/react", "/api/star", "/api/mark_read", "/api/delete_chat"} {
		rec := doReq(t, h, path, testToken, badChatBody)
		wantErrJSON(t, rec, http.StatusBadRequest, "invalid chat_jid")
	}
	for _, path := range []string{"/api/group_participants", "/api/set_group_name"} {
		rec := doReq(t, h, path, testToken, badGroupBody)
		wantErrJSON(t, rec, http.StatusBadRequest, "invalid group_jid")
	}
}

// (d) react/edit/revoke/poll/poll_vote: con ban temporal vigente devuelven 503.
func TestServer_BanBlocks503(t *testing.T) {
	h := newBannedTestServer(t)
	for _, path := range []string{
		"/api/react", "/api/edit", "/api/revoke", "/api/poll", "/api/poll_vote",
	} {
		rec := doReq(t, h, path, testToken, `{}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s con ban: status=%d, want 503 (body=%q)", path, rec.Code, rec.Body.String())
		}
	}
}

// (e) CAMBIO INTENCIONAL: /api/send responde errores en JSON (no texto plano).
// Esta aserción refleja el estado DESEADO: falla contra el código viejo
// (http.Error → text/plain) y pasa tras unificar el contrato.
func TestSend_ErrorRespondsJSON(t *testing.T) {
	h := newTestServer(t)
	rec := doReq(t, h, "/api/send", testToken, invalidJSON)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("/api/send error: Content-Type=%q, want application/json (cambio intencional)", ct)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("/api/send error: body no es JSON: %v (body=%q)", err, rec.Body.String())
	}
	if got["success"] != false {
		t.Fatalf("/api/send error: success=%v, want false", got["success"])
	}
}

// --- /api/qr y /api/shutdown (autogestión del login para el supervisor MCP) ---

// doGet lanza un GET autenticado contra el handler.
func doGet(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestQR_SinCodigoActivo(t *testing.T) {
	h := newTestServer(t)
	rec := doGet(t, h, "/api/qr", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var got struct {
		Success  bool   `json:"success"`
		QRStatus string `json:"qr_status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body no es JSON: %v", err)
	}
	if !got.Success || got.QRStatus != "none" {
		t.Fatalf("got %+v, want success=true qr_status=none", got)
	}
}

func TestQR_CodigoActivoDevuelvePNG(t *testing.T) {
	svc := wa.NewService(nil, nil, nil, "", nil)
	svc.SetQRCode("test-code-123", 60*time.Second)
	h := NewServer(svc, nil, nil, testToken, nil)
	rec := doGet(t, h, "/api/qr", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var got struct {
		Success   bool   `json:"success"`
		QRStatus  string `json:"qr_status"`
		Code      string `json:"code"`
		PNGBase64 string `json:"png_base64"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body no es JSON: %v", err)
	}
	if got.QRStatus != "active" || got.Code != "test-code-123" {
		t.Fatalf("got %+v, want qr_status=active code=test-code-123", got)
	}
	png, err := base64.StdEncoding.DecodeString(got.PNGBase64)
	if err != nil {
		t.Fatalf("png_base64 no decodifica: %v", err)
	}
	if len(png) < 8 || string(png[1:4]) != "PNG" {
		t.Fatalf("payload no tiene magic PNG (len=%d)", len(png))
	}
}

func TestQR_SoloGET(t *testing.T) {
	h := newTestServer(t)
	rec := doReq(t, h, "/api/qr", testToken, "{}")
	wantErrJSON(t, rec, http.StatusMethodNotAllowed, "Method not allowed")
}

func TestShutdown_DisparaCallback(t *testing.T) {
	svc := wa.NewService(nil, nil, nil, "", nil)
	done := make(chan struct{})
	h := NewServer(svc, nil, nil, testToken, func() { close(done) })
	rec := doReq(t, h, "/api/shutdown", testToken, "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdownFn no fue invocado")
	}
}

func TestShutdown_SoloPOST(t *testing.T) {
	h := newTestServer(t)
	rec := doGet(t, h, "/api/shutdown", testToken)
	wantErrJSON(t, rec, http.StatusMethodNotAllowed, "Method not allowed")
}

func TestShutdown_SinCallbackNoRevienta(t *testing.T) {
	h := newTestServer(t) // shutdownFn nil
	rec := doReq(t, h, "/api/shutdown", testToken, "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
}
