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
// rechaza componentes ocultos (donde viven secretos: ~/.ssh, ~/.aws, ~/.gnupg...)
// y exige que sea un archivo regular existente.
func ValidateMediaPath(p string) error {
	abs, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("invalid path: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("cannot resolve path: %v", err)
	}
	for _, part := range strings.Split(resolved, string(os.PathSeparator)) {
		if len(part) > 1 && strings.HasPrefix(part, ".") {
			return fmt.Errorf("hidden path component %q not allowed", part)
		}
	}
	// Rechaza cualquier archivo dentro del directorio store/ del bridge: ahi viven
	// la sesion de WhatsApp (whatsapp.db, con las claves), el historial completo
	// (messages.db) y el token de auth. No empiezan con "." (no los cubre el check
	// de arriba) y ninguna media legitima vive ahi.
	if storeDir, err := filepath.Abs("store"); err == nil {
		if real, err2 := filepath.EvalSymlinks(storeDir); err2 == nil {
			storeDir = real
		}
		if resolved == storeDir || strings.HasPrefix(resolved, storeDir+string(os.PathSeparator)) {
			return fmt.Errorf("path inside store directory not allowed")
		}
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("cannot stat file: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	return nil
}
