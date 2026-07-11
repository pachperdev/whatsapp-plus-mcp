package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testToken = "s3cr3t-token"

// newNextSpy devuelve un HandlerFunc que responde 200 y registra si fue invocado.
func newNextSpy(called *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	}
}

func TestWithAuth_NoHeader_Unauthorized(t *testing.T) {
	called := false
	h := withAuth(testToken, newNextSpy(&called))

	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("sin header X-Auth-Token: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if called {
		t.Fatal("sin header X-Auth-Token: next NO debe ejecutarse")
	}
}

func TestWithAuth_ValidToken_CallsNext(t *testing.T) {
	called := false
	h := withAuth(testToken, newNextSpy(&called))

	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
	req.Header.Set("X-Auth-Token", testToken)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("token correcto: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !called {
		t.Fatal("token correcto: next SÍ debe ejecutarse")
	}
}

func TestWithAuth_WrongToken_Unauthorized(t *testing.T) {
	called := false
	h := withAuth(testToken, newNextSpy(&called))

	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
	req.Header.Set("X-Auth-Token", "wrong-token")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("token incorrecto: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if called {
		t.Fatal("token incorrecto: next NO debe ejecutarse")
	}
}

func TestWithAuth_EmptyToken_FailClosed(t *testing.T) {
	called := false
	h := withAuth("", newNextSpy(&called))

	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
	req.Header.Set("X-Auth-Token", "")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("token vacío (fail-closed): status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if called {
		t.Fatal("token vacío (fail-closed): next NO debe ejecutarse")
	}
}
