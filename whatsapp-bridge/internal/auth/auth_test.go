package auth

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// tokenRe: el token es hex de 64 caracteres (32 bytes aleatorios codificados).
var tokenRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// tokenMode devuelve los permisos actuales del archivo de token.
func tokenMode(t *testing.T, dir string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, ".bridge_token"))
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	return fi.Mode().Perm()
}

// TestGetOrCreateBridgeToken cubre el ciclo de vida del token compartido bridge<->MCP:
// es la credencial que protege la API local, así que el formato (64 hex) y el modo
// 0600 del archivo son parte del contrato de seguridad.
func TestGetOrCreateBridgeToken(t *testing.T) {
	t.Run("crea un token 64-hex con modo 0600", func(t *testing.T) {
		dir := t.TempDir()
		tok, err := GetOrCreateBridgeToken(dir)
		if err != nil {
			t.Fatalf("GetOrCreateBridgeToken: %v", err)
		}
		if !tokenRe.MatchString(tok) {
			t.Errorf("formato: got %q, want 64 caracteres hex", tok)
		}
		if mode := tokenMode(t, dir); mode != 0o600 {
			t.Errorf("modo del archivo: got %o, want %o", mode, 0o600)
		}
		data, err := os.ReadFile(filepath.Join(dir, ".bridge_token"))
		if err != nil {
			t.Fatalf("leer token: %v", err)
		}
		if string(data) != tok {
			t.Errorf("el archivo debe contener el mismo token: got %q, want %q", data, tok)
		}
	})

	t.Run("reusa el token existente", func(t *testing.T) {
		// El server Python lee el mismo archivo: si cada arranque regenerara el
		// token, cualquier reinicio del bridge rompería la auth del MCP.
		dir := t.TempDir()
		first, err := GetOrCreateBridgeToken(dir)
		if err != nil {
			t.Fatalf("primera llamada: %v", err)
		}
		second, err := GetOrCreateBridgeToken(dir)
		if err != nil {
			t.Fatalf("segunda llamada: %v", err)
		}
		if second != first {
			t.Errorf("got %q, want %q (mismo token)", second, first)
		}
	})

	t.Run("re-aplica 0600 si el archivo quedó laxo", func(t *testing.T) {
		// Regresión del "best-effort chmod": un fallo previo pudo dejar el token
		// legible por otros usuarios; al reusarlo se debe volver a 0600.
		dir := t.TempDir()
		first, err := GetOrCreateBridgeToken(dir)
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		path := filepath.Join(dir, ".bridge_token")
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("chmod setup: %v", err)
		}
		tok, err := GetOrCreateBridgeToken(dir)
		if err != nil {
			t.Fatalf("GetOrCreateBridgeToken: %v", err)
		}
		if tok != first {
			t.Errorf("debería reusar el token: got %q, want %q", tok, first)
		}
		if mode := tokenMode(t, dir); mode != 0o600 {
			t.Errorf("modo tras reuso: got %o, want %o", mode, 0o600)
		}
	})

	t.Run("archivo vacío regenera", func(t *testing.T) {
		// Un archivo vacío (p. ej. de un crash a mitad de escritura) no es un token
		// válido: la auth fail-closed rechazaría todo. Debe regenerarse.
		dir := t.TempDir()
		path := filepath.Join(dir, ".bridge_token")
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		tok, err := GetOrCreateBridgeToken(dir)
		if err != nil {
			t.Fatalf("GetOrCreateBridgeToken: %v", err)
		}
		if !tokenRe.MatchString(tok) {
			t.Errorf("token regenerado inválido: got %q", tok)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("leer token: %v", err)
		}
		if string(data) != tok {
			t.Errorf("el token regenerado debe persistirse: got %q, want %q", data, tok)
		}
	})

	t.Run("solo whitespace regenera", func(t *testing.T) {
		// El TrimSpace del reuso trata "solo espacios" como vacío: regenerar.
		dir := t.TempDir()
		path := filepath.Join(dir, ".bridge_token")
		if err := os.WriteFile(path, []byte("  \n\t\n"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		tok, err := GetOrCreateBridgeToken(dir)
		if err != nil {
			t.Fatalf("GetOrCreateBridgeToken: %v", err)
		}
		if !tokenRe.MatchString(tok) {
			t.Errorf("token regenerado inválido: got %q", tok)
		}
	})

	t.Run("directorio inexistente falla", func(t *testing.T) {
		// Sin storeDir no hay dónde persistir: mejor error explícito que un token
		// efímero que el server Python nunca podría leer.
		dir := filepath.Join(t.TempDir(), "no-existe")
		if _, err := GetOrCreateBridgeToken(dir); err == nil {
			t.Error("esperaba error al no poder persistir el token")
		}
	})
}

func TestValidateMediaPath(t *testing.T) {
	dir := t.TempDir()
	v := NewValidator(filepath.Join(dir, "store"), nil)

	regular := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(regular, []byte("data"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	resolved, err := v.Validate(regular)
	if err != nil {
		t.Errorf("archivo regular debería aceptarse, got: %v", err)
	}
	if resolved == "" {
		t.Error("un archivo aceptado debería devolver la ruta canónica, no vacío")
	}

	hidden := filepath.Join(dir, ".secret")
	if err := os.WriteFile(hidden, []byte("data"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := v.Validate(hidden); err == nil {
		t.Error("componente oculto (.secret) debería rechazarse")
	}

	if _, err := v.Validate(dir); err == nil {
		t.Error("un directorio debería rechazarse (no es archivo regular)")
	}

	if _, err := v.Validate(filepath.Join(dir, "no-existe.jpg")); err == nil {
		t.Error("archivo inexistente debería rechazarse")
	}
}

// newStoreValidator crea un store/ con whatsapp.db bajo un dir temporal y devuelve
// un Validator apuntando a ese store, más el path del secreto.
func newStoreValidator(t *testing.T) (*Validator, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	storeDir := filepath.Join(dir, "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	sessionDB := filepath.Join(storeDir, "whatsapp.db")
	if err := os.WriteFile(sessionDB, []byte("secret-session-keys"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return NewValidator(storeDir, nil), sessionDB
}

// TestValidateAllowsStoreSubdirMedia: la media descargada vive en store/<chat>/ y
// DEBE poder reenviarse (send_file). Solo la raíz de store/ está protegida.
func TestValidateAllowsStoreSubdirMedia(t *testing.T) {
	v, sessionDB := newStoreValidator(t)
	storeDir := filepath.Dir(sessionDB)

	chatDir := filepath.Join(storeDir, "123@s.whatsapp.net")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	media := filepath.Join(chatDir, "photo.jpg")
	if err := os.WriteFile(media, []byte("jpeg-bytes"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := v.Validate(media); err != nil {
		t.Errorf("media en store/<chat>/ debería aceptarse (reenvío de descargas), got: %v", err)
	}
}

// TestValidateHardlinkInSubdir: un hardlink a whatsapp.db colocado en un subdir de
// media debe rechazarse igual (comparte inode, aunque no esté en la raíz).
func TestValidateHardlinkInSubdir(t *testing.T) {
	v, sessionDB := newStoreValidator(t)
	storeDir := filepath.Dir(sessionDB)
	chatDir := filepath.Join(storeDir, "123@s.whatsapp.net")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	link := filepath.Join(chatDir, "innocent.jpg")
	if err := os.Link(sessionDB, link); err != nil {
		t.Skipf("hardlink no soportado: %v", err)
	}
	if _, err := v.Validate(link); err == nil {
		t.Error("hardlink a whatsapp.db en un subdir debería rechazarse")
	}
}

// TestValidateAllowlist: con allowlist configurada, solo se acepta media que viva
// dentro de alguno de los directorios permitidos; el resto se rechaza.
func TestValidateAllowlist(t *testing.T) {
	dir := t.TempDir()
	allowed := filepath.Join(dir, "outbox")
	other := filepath.Join(dir, "other")
	for _, d := range []string{allowed, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	v := NewValidator(filepath.Join(dir, "store"), []string{allowed})

	inside := filepath.Join(allowed, "ok.jpg")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := v.Validate(inside); err != nil {
		t.Errorf("archivo dentro de la allowlist debería aceptarse, got: %v", err)
	}

	outside := filepath.Join(other, "no.jpg")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := v.Validate(outside); err == nil {
		t.Error("archivo fuera de la allowlist debería rechazarse")
	}
}

// TestValidateMediaPathRejectsStore verifica que no se pueda leer/exfiltrar la
// sesión de WhatsApp (whatsapp.db), el historial (messages.db) ni el token
// aunque no empiecen con punto: viven bajo store/ y ninguna media legítima ahí.
func TestValidateMediaPathRejectsStore(t *testing.T) {
	v, sessionDB := newStoreValidator(t)

	if _, err := v.Validate("store/whatsapp.db"); err == nil {
		t.Error("un archivo dentro de store/ debería rechazarse (exfiltración de sesión)")
	}
	if _, err := v.Validate(sessionDB); err == nil {
		t.Error("store/whatsapp.db por ruta absoluta también debería rechazarse")
	}
}

// TestValidateMediaPathRejectsStoreCaseVariant es la regresión del bypass en
// filesystems case-insensitive (APFS/NTFS): "STORE/whatsapp.db" apunta al mismo
// archivo que "store/whatsapp.db" pero el guard por prefijo de string (case-
// sensitive) no lo detectaba y permitía exfiltrar la sesión. La comparación por
// inode (os.SameFile) lo cierra. En filesystems case-sensitive el archivo con
// casing distinto no existe, así que se rechaza por "cannot resolve/stat" —
// también correcto: en ninguna plataforma debe filtrar el secreto.
func TestValidateMediaPathRejectsStoreCaseVariant(t *testing.T) {
	v, _ := newStoreValidator(t)

	for _, variant := range []string{"STORE/whatsapp.db", "Store/whatsapp.db"} {
		resolved, err := v.Validate(variant)
		if err == nil {
			if data, rerr := os.ReadFile(resolved); rerr == nil && string(data) == "secret-session-keys" {
				t.Errorf("%s ACEPTADO y filtra la sesión (bypass case-insensitive)", variant)
			}
		}
	}
}

// TestValidateMediaPathRejectsHardlink es la regresión del bypass por hardlink:
// un archivo fuera de store/ que comparte inode con whatsapp.db debe rechazarse.
func TestValidateMediaPathRejectsHardlink(t *testing.T) {
	v, sessionDB := newStoreValidator(t)

	link := filepath.Join(filepath.Dir(filepath.Dir(sessionDB)), "innocent.jpg")
	if err := os.Link(sessionDB, link); err != nil {
		t.Skipf("hardlink no soportado en este FS: %v", err)
	}
	if _, err := v.Validate(link); err == nil {
		t.Error("un hardlink a whatsapp.db debería rechazarse (mismo inode, otro path)")
	}
}
