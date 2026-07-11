// Package config resuelve la configuracion del bridge una sola vez al arranque, a
// partir de variables de entorno con defaults iguales al layout historico. Todos
// los paths quedan absolutos: el bridge deja de depender del directorio de trabajo
// del proceso (antes, correr el binario desde otra carpeta creaba un store/ nuevo
// vacio en silencio).
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// Config es la configuracion resuelta del bridge.
type Config struct {
	// Addr es la direccion loopback donde escucha la REST API.
	Addr string
	// StoreDir es el directorio absoluto del store: DBs de sesion (whatsapp.db) e
	// historial (messages.db) y el token de auth (.bridge_token).
	StoreDir string
	// MediaAllowedDirs es una allowlist OPT-IN de directorios desde los que se
	// permite enviar media (send_file / set_group_photo). Vacia = sin restriccion
	// de ubicacion (se preserva el comportamiento historico); configurarla confina
	// los envios a esos arboles, mitigando exfiltracion por prompt-injection.
	MediaAllowedDirs []string
}

// Load arma la Config desde el entorno. Falla si Addr no es una direccion de
// loopback: el bridge da control total de la cuenta de WhatsApp y NUNCA debe
// exponerse en la red (invariante de seguridad).
func Load() (Config, error) {
	addr := getenv("WHATSAPP_BRIDGE_ADDR", "127.0.0.1:8080")
	if err := validateLoopback(addr); err != nil {
		return Config{}, err
	}

	storeDir, err := filepath.Abs(getenv("WHATSAPP_STORE_DIR", "store"))
	if err != nil {
		return Config{}, fmt.Errorf("WHATSAPP_STORE_DIR invalido: %w", err)
	}

	var allowed []string
	if raw := os.Getenv("WHATSAPP_MEDIA_ALLOWED_DIRS"); raw != "" {
		for _, d := range filepath.SplitList(raw) {
			if d == "" {
				continue
			}
			abs, err := filepath.Abs(d)
			if err != nil {
				return Config{}, fmt.Errorf("WHATSAPP_MEDIA_ALLOWED_DIRS: entrada invalida %q: %w", d, err)
			}
			// Resolver symlinks para comparar contra rutas ya resueltas del validador.
			if real, rerr := filepath.EvalSymlinks(abs); rerr == nil {
				abs = real
			}
			allowed = append(allowed, abs)
		}
	}

	return Config{Addr: addr, StoreDir: storeDir, MediaAllowedDirs: allowed}, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// validateLoopback exige que el host de addr sea loopback (127.0.0.0/8, ::1 o
// localhost). Rechaza 0.0.0.0, hosts vacios (":8080" bindearia todas las
// interfaces) y cualquier IP enrutable.
func validateLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("WHATSAPP_BRIDGE_ADDR invalido %q (se espera host:puerto): %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("WHATSAPP_BRIDGE_ADDR %q debe ser loopback (127.0.0.1 / ::1 / localhost); el bridge no debe exponerse en la red", addr)
	}
	return nil
}
