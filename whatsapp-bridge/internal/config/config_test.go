package config

import (
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("WHATSAPP_BRIDGE_ADDR", "")
	t.Setenv("WHATSAPP_STORE_DIR", "")
	t.Setenv("WHATSAPP_MEDIA_ALLOWED_DIRS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load con defaults no debería fallar: %v", err)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr default = %q, want 127.0.0.1:8080", cfg.Addr)
	}
	if !filepath.IsAbs(cfg.StoreDir) {
		t.Errorf("StoreDir debería ser absoluto, got %q", cfg.StoreDir)
	}
	if filepath.Base(cfg.StoreDir) != "store" {
		t.Errorf("StoreDir default debería terminar en 'store', got %q", cfg.StoreDir)
	}
	if len(cfg.MediaAllowedDirs) != 0 {
		t.Errorf("MediaAllowedDirs default debería ser vacío, got %v", cfg.MediaAllowedDirs)
	}
}

func TestLoadRejectsNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", "192.168.1.10:8080", ":8080", "8.8.8.8:80"} {
		t.Setenv("WHATSAPP_BRIDGE_ADDR", addr)
		if _, err := Load(); err == nil {
			t.Errorf("addr no-loopback %q debería rechazarse", addr)
		}
	}
}

func TestLoadAcceptsLoopbackVariants(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "127.0.0.5:9000", "[::1]:8080", "localhost:8080"} {
		t.Setenv("WHATSAPP_BRIDGE_ADDR", addr)
		if _, err := Load(); err != nil {
			t.Errorf("addr loopback %q debería aceptarse, got: %v", addr, err)
		}
	}
}

func TestLoadResolvesMediaAllowedDirs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WHATSAPP_BRIDGE_ADDR", "127.0.0.1:8080")
	t.Setenv("WHATSAPP_MEDIA_ALLOWED_DIRS", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load no debería fallar: %v", err)
	}
	if len(cfg.MediaAllowedDirs) != 1 {
		t.Fatalf("esperaba 1 dir permitido, got %v", cfg.MediaAllowedDirs)
	}
	if !filepath.IsAbs(cfg.MediaAllowedDirs[0]) {
		t.Errorf("el dir permitido debería quedar absoluto, got %q", cfg.MediaAllowedDirs[0])
	}
}
