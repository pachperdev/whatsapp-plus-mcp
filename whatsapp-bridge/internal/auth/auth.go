// Package auth agrupa las utilidades de seguridad del bridge: el token de
// autenticación compartido con el server MCP y la validación de rutas de media
// para prevenir exfiltración de archivos. Ambas resuelven paths relativos al
// directorio store/ del proceso (igual que el resto del bridge).
package auth

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GetOrCreateBridgeToken devuelve un token compartido entre el bridge y el MCP server.
// Se persiste en store/.bridge_token (0600); el server Python lo lee del mismo archivo.
// Asi la auth es automatica (sin config manual) y protege ante otros procesos locales.
func GetOrCreateBridgeToken() (string, error) {
	path := filepath.Join("store", ".bridge_token")
	if data, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := crand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate token: %v", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok), 0600); err != nil {
		return "", fmt.Errorf("failed to persist token: %v", err)
	}
	return tok, nil
}

// ValidateMediaPath protege contra exfiltracion de archivos: resuelve symlinks,
// rechaza componentes ocultos (donde viven secretos: ~/.ssh, ~/.aws, ~/.gnupg...),
// rechaza los archivos del store del bridge (sesion whatsapp.db, historial
// messages.db, token) y exige que sea un archivo regular existente. Devuelve la
// ruta canonica (sin symlinks) para que el caller lea de ESA ruta y no del string
// original, cerrando la ventana TOCTOU entre la validacion y la lectura.
func ValidateMediaPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("invalid path: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path: %v", err)
	}
	for _, part := range strings.Split(resolved, string(os.PathSeparator)) {
		if len(part) > 1 && strings.HasPrefix(part, ".") {
			return "", fmt.Errorf("hidden path component %q not allowed", part)
		}
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("cannot stat file: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}
	if err := rejectStorePaths(resolved, fi); err != nil {
		return "", err
	}
	return resolved, nil
}

// rejectStorePaths bloquea cualquier archivo que sea (o viva dentro de) el store
// del bridge, donde viven la sesion de WhatsApp (whatsapp.db, con las claves), el
// historial (messages.db) y el token de auth. Compara por INODE (os.SameFile), no
// por prefijo de string: en un filesystem case-insensitive (APFS/NTFS) el path
// "STORE/whatsapp.db" apunta al MISMO archivo que "store/whatsapp.db" pero un
// prefijo case-sensitive no lo detectaba (bypass de exfiltracion); y un hardlink
// fuera de store/ comparte inode con el original. Ambos vectores se cierran aca.
func rejectStorePaths(resolved string, fi os.FileInfo) error {
	storeDir, err := filepath.Abs("store")
	if err != nil {
		return nil // sin store resoluble no hay nada que proteger
	}
	if real, err2 := filepath.EvalSymlinks(storeDir); err2 == nil {
		storeDir = real
	}
	// (a) rechaza si algun ancestro de resolved ES el directorio store/
	// (case-insensitive-safe: comparar inodes ignora el casing del path).
	if di, err2 := os.Stat(storeDir); err2 == nil {
		for cur := filepath.Dir(resolved); ; {
			if ci, err3 := os.Stat(cur); err3 == nil && os.SameFile(ci, di) {
				return fmt.Errorf("path inside store directory not allowed")
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
		}
	}
	// (b) rechaza si resolved es un hardlink a un archivo sensible del store
	// (mismo inode, otro path fuera de store/).
	for _, name := range []string{"whatsapp.db", "messages.db", ".bridge_token"} {
		if si, err2 := os.Stat(filepath.Join(storeDir, name)); err2 == nil && os.SameFile(fi, si) {
			return fmt.Errorf("path aliases a protected store file")
		}
	}
	return nil
}
