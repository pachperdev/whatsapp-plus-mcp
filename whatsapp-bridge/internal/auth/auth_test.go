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
	resolved, err := ValidateMediaPath(regular)
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
	if _, err := ValidateMediaPath(hidden); err == nil {
		t.Error("componente oculto (.secret) debería rechazarse")
	}

	if _, err := ValidateMediaPath(dir); err == nil {
		t.Error("un directorio debería rechazarse (no es archivo regular)")
	}

	if _, err := ValidateMediaPath(filepath.Join(dir, "no-existe.jpg")); err == nil {
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

	if _, err := ValidateMediaPath("store/whatsapp.db"); err == nil {
		t.Error("un archivo dentro de store/ debería rechazarse (exfiltración de sesión)")
	}
	if _, err := ValidateMediaPath(sessionDB); err == nil {
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
	dir := t.TempDir()
	t.Chdir(dir)

	storeDir := filepath.Join(dir, "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	secret := []byte("secret-session-keys")
	if err := os.WriteFile(filepath.Join(storeDir, "whatsapp.db"), secret, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	for _, variant := range []string{"STORE/whatsapp.db", "Store/whatsapp.db"} {
		resolved, err := ValidateMediaPath(variant)
		if err == nil {
			// Si fue aceptado, verificamos que al menos no filtre el secreto real.
			if data, rerr := os.ReadFile(resolved); rerr == nil && string(data) == string(secret) {
				t.Errorf("%s ACEPTADO y filtra la sesión (bypass case-insensitive)", variant)
			}
		}
	}
}

// TestValidateMediaPathRejectsHardlink es la regresión del bypass por hardlink:
// un archivo fuera de store/ que comparte inode con whatsapp.db debe rechazarse.
func TestValidateMediaPathRejectsHardlink(t *testing.T) {
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

	link := filepath.Join(dir, "innocent.jpg")
	if err := os.Link(sessionDB, link); err != nil {
		t.Skipf("hardlink no soportado en este FS: %v", err)
	}
	if _, err := ValidateMediaPath(link); err == nil {
		t.Error("un hardlink a whatsapp.db debería rechazarse (mismo inode, otro path)")
	}
}
