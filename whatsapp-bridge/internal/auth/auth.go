// Package auth agrupa las utilidades de seguridad del bridge: el token de
// autenticación compartido con el server MCP y la validación de rutas de media
// para prevenir exfiltración de archivos. El directorio del store se inyecta desde
// la config (ya absoluto), en vez de resolverse contra el CWD del proceso.
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
// Se persiste en <storeDir>/.bridge_token (0600); el server Python lo lee del mismo
// archivo. Asi la auth es automatica (sin config manual) y protege ante otros
// procesos locales. Al reusar un token existente re-aplica 0600 (best-effort) por si
// un fallo previo dejo el archivo con permisos laxos.
func GetOrCreateBridgeToken(storeDir string) (string, error) {
	path := filepath.Join(storeDir, ".bridge_token")
	if data, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			_ = os.Chmod(path, 0600)
			return tok, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := crand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate token: %v", err)
	}
	tok := hex.EncodeToString(buf)
	// os.WriteFile solo aplica el modo al CREAR el archivo: si regeneramos sobre un
	// .bridge_token preexistente (vacío/whitespace) con permisos laxos (p. ej. 0644),
	// el token vivo quedaría legible por otros usuarios. Eliminarlo primero garantiza
	// que 0600 se aplique en la creación, sin depender de que chmod funcione en el
	// filesystem (FUSE/overlay pueden rechazarlo y dejarían la API bloqueada en 401).
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte(tok), 0600); err != nil {
		return "", fmt.Errorf("failed to persist token: %v", err)
	}
	// Fail-closed solo ante un riesgo REAL: si el archivo aun quedo laxo (Remove
	// fallo y WriteFile reuso el preexistente), intentar chmod y verificar el modo
	// efectivo; con el token vivo legible por otros usuarios es mejor no arrancar.
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0600 {
		_ = os.Chmod(path, 0600)
		if fi, err = os.Stat(path); err == nil && fi.Mode().Perm() != 0600 {
			return "", fmt.Errorf("token file %s has insecure mode %o and could not be restricted to 0600", path, fi.Mode().Perm())
		}
	}
	return tok, nil
}

// Validator valida rutas de media contra exfiltracion. Conoce el directorio del
// store (para proteger sesion/historial/token) y, opcionalmente, una allowlist de
// directorios desde los que se permite enviar (confinamiento opt-in).
type Validator struct {
	storeDir    string
	allowedDirs []string
}

// NewValidator crea un Validator. storeDir debe ser absoluto. allowedDirs vacio
// desactiva el confinamiento por ubicacion (se preserva el comportamiento
// historico: cualquier archivo regular no-oculto fuera del store es enviable).
func NewValidator(storeDir string, allowedDirs []string) *Validator {
	return &Validator{storeDir: storeDir, allowedDirs: allowedDirs}
}

// Validate protege contra exfiltracion de archivos: resuelve symlinks, rechaza
// componentes ocultos (donde viven secretos: ~/.ssh, ~/.aws, ~/.gnupg...), rechaza
// los archivos del store del bridge (sesion whatsapp.db, historial messages.db,
// token) y exige un archivo regular existente. Devuelve la ruta canonica (sin
// symlinks) para que el caller lea de ESA ruta y no del string original, cerrando
// la ventana TOCTOU entre la validacion y la lectura.
func (v *Validator) Validate(p string) (string, error) {
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
	if err := v.rejectStorePaths(resolved, fi); err != nil {
		return "", err
	}
	if err := v.enforceAllowlist(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// rejectStorePaths bloquea los archivos SENSIBLES del store del bridge: la sesion de
// WhatsApp (whatsapp.db, con las claves), el historial (messages.db), el token y sus
// sidecars WAL/SHM —todos viven en la RAIZ de store/—. La media descargada vive en
// subdirectorios (store/<chat>/...) y NO se bloquea, para poder reenviarla. Compara
// por INODE (os.SameFile), no por prefijo de string: en un filesystem
// case-insensitive (APFS/NTFS) "STORE/whatsapp.db" apunta al MISMO archivo que
// "store/whatsapp.db" pero un prefijo case-sensitive no lo detectaba (bypass de
// exfiltracion); y un hardlink comparte inode con el original desde otro path.
func (v *Validator) rejectStorePaths(resolved string, fi os.FileInfo) error {
	storeDir := v.storeDir
	if real, err := filepath.EvalSymlinks(storeDir); err == nil {
		storeDir = real
	}
	di, err := os.Stat(storeDir)
	if err != nil {
		return nil // sin store resoluble no hay nada que proteger
	}
	// (a) rechaza si el padre INMEDIATO de resolved ES la raiz de store/: cubre
	// whatsapp.db/messages.db/.bridge_token y cualquier sidecar -wal/-shm/-journal,
	// sin bloquear la media en store/<chat>/. Inode-safe (ignora el casing).
	if pi, err := os.Stat(filepath.Dir(resolved)); err == nil && os.SameFile(pi, di) {
		return fmt.Errorf("path inside store root not allowed")
	}
	// (b) rechaza si resolved es un hardlink a un archivo sensible del store
	// (mismo inode, otro path —incluso dentro de un subdir de media—).
	for _, name := range []string{"whatsapp.db", "messages.db", ".bridge_token"} {
		if si, err := os.Stat(filepath.Join(storeDir, name)); err == nil && os.SameFile(fi, si) {
			return fmt.Errorf("path aliases a protected store file")
		}
	}
	return nil
}

// enforceAllowlist, si hay allowlist configurada, exige que resolved viva dentro de
// alguno de los directorios permitidos. Vacia = sin restriccion de ubicacion. La
// contencion se evalua por inode (os.SameFile en la cadena de ancestros), robusta a
// filesystems case-insensitive y symlinks.
func (v *Validator) enforceAllowlist(resolved string) error {
	if len(v.allowedDirs) == 0 {
		return nil
	}
	for _, dir := range v.allowedDirs {
		if underDir(resolved, dir) {
			return nil
		}
	}
	return fmt.Errorf("path outside the allowed media directories")
}

// underDir reporta si resolved vive dentro de dir (a cualquier profundidad),
// comparando por inode (os.SameFile) en la cadena de directorios ancestros.
func underDir(resolved, dir string) bool {
	di, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for cur := filepath.Dir(resolved); ; {
		if ci, err := os.Stat(cur); err == nil && os.SameFile(ci, di) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}
