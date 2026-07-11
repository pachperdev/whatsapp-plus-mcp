package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateMediaPath(t *testing.T) {
	dir := t.TempDir()

	regular := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(regular, []byte("data"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := ValidateMediaPath(regular); err != nil {
		t.Errorf("archivo regular debería aceptarse, got: %v", err)
	}

	hidden := filepath.Join(dir, ".secret")
	if err := os.WriteFile(hidden, []byte("data"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := ValidateMediaPath(hidden); err == nil {
		t.Error("componente oculto (.secret) debería rechazarse")
	}

	if err := ValidateMediaPath(dir); err == nil {
		t.Error("un directorio debería rechazarse (no es archivo regular)")
	}

	if err := ValidateMediaPath(filepath.Join(dir, "no-existe.jpg")); err == nil {
		t.Error("archivo inexistente debería rechazarse")
	}
}

// TestValidateMediaPathRejectsStore verifica que no se pueda leer/exfiltrar la
// sesión de WhatsApp (whatsapp.db), el historial (messages.db) ni el token
// aunque no empiecen con punto: viven bajo store/ y ninguna media legítima ahí.
func TestValidateMediaPathRejectsStore(t *testing.T) {
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

	if err := ValidateMediaPath("store/whatsapp.db"); err == nil {
		t.Error("un archivo dentro de store/ debería rechazarse (exfiltración de sesión)")
	}
	if err := ValidateMediaPath(sessionDB); err == nil {
		t.Error("store/whatsapp.db por ruta absoluta también debería rechazarse")
	}
}
