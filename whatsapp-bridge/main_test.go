package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestValidateMediaPath(t *testing.T) {
	dir := t.TempDir()

	regular := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(regular, []byte("data"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := validateMediaPath(regular); err != nil {
		t.Errorf("archivo regular debería aceptarse, got: %v", err)
	}

	hidden := filepath.Join(dir, ".secret")
	if err := os.WriteFile(hidden, []byte("data"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := validateMediaPath(hidden); err == nil {
		t.Error("componente oculto (.secret) debería rechazarse")
	}

	if err := validateMediaPath(dir); err == nil {
		t.Error("un directorio debería rechazarse (no es archivo regular)")
	}

	if err := validateMediaPath(filepath.Join(dir, "no-existe.jpg")); err == nil {
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

	if err := validateMediaPath("store/whatsapp.db"); err == nil {
		t.Error("un archivo dentro de store/ debería rechazarse (exfiltración de sesión)")
	}
	if err := validateMediaPath(sessionDB); err == nil {
		t.Error("store/whatsapp.db por ruta absoluta también debería rechazarse")
	}
}

// TestAnalyzeOggOpusTruncatedPage es la regresión del bounds-check: una página
// Ogg que declara un tamaño mayor que los bytes disponibles no debe paniquear.
func TestAnalyzeOggOpusTruncatedPage(t *testing.T) {
	// Página con numSegments=1 y segLen=255 → pageSize=283, pero solo damos 40 bytes.
	buf := make([]byte, 40)
	copy(buf[0:4], "OggS")
	binary.LittleEndian.PutUint32(buf[18:22], 0) // pageSeqNum=0 (entra al bloque OpusHead)
	buf[26] = 1                                   // numSegments=1
	buf[27] = 0xFF                                // segLen=255 → pageSize se dispara

	// No debe paniquear ni devolver error (la firma OggS es válida).
	dur, wf, err := analyzeOggOpus(buf)
	if err != nil {
		t.Fatalf("no debería fallar con OggS válido pero truncado: %v", err)
	}
	if dur < 1 {
		t.Errorf("duración debería ser al menos 1, got %d", dur)
	}
	if len(wf) != 64 {
		t.Errorf("waveform debería ser de 64 bytes, got %d", len(wf))
	}
}

func TestAnalyzeOggOpusNotOgg(t *testing.T) {
	if _, _, err := analyzeOggOpus([]byte("no soy ogg")); err == nil {
		t.Error("datos sin firma OggS deberían devolver error")
	}
	if _, _, err := analyzeOggOpus(nil); err == nil {
		t.Error("nil debería devolver error")
	}
}

func TestExtractTextContent(t *testing.T) {
	tests := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{"nil", nil, ""},
		{"conversation", &waE2E.Message{Conversation: proto.String("hola")}, "hola"},
		{
			"extended",
			&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("mundo")}},
			"mundo",
		},
		{
			"image caption",
			&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("una foto")}},
			"una foto",
		},
		{
			"ephemeral unwrap",
			&waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{Conversation: proto.String("efímero")},
			}},
			"efímero",
		},
		{"empty", &waE2E.Message{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractTextContent(tc.msg); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVcardPhone(t *testing.T) {
	tests := []struct {
		name  string
		vcard string
		want  string
	}{
		{"vacío", "", ""},
		{"tel simple", "BEGIN:VCARD\nTEL:+5491122334455\nEND:VCARD", "+5491122334455"},
		{"tel con params", "TEL;type=CELL;waid=549112233:+54 9 11 2233", "+54 9 11 2233"},
		{"sin tel", "BEGIN:VCARD\nFN:Juan\nEND:VCARD", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := vcardPhone(tc.vcard); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveMentions(t *testing.T) {
	t.Run("número explícito a JID", func(t *testing.T) {
		got := resolveMentions("", []string{"5491122334455"})
		want := []string{"5491122334455@s.whatsapp.net"}
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("auto-detección en texto", func(t *testing.T) {
		got := resolveMentions("hola @5491122334455 qué tal", nil)
		if len(got) != 1 || got[0] != "5491122334455@s.whatsapp.net" {
			t.Errorf("got %v", got)
		}
	})
	t.Run("dedup conservando orden", func(t *testing.T) {
		got := resolveMentions("@5491122334455", []string{"5491122334455"})
		if len(got) != 1 {
			t.Errorf("debería deduplicar, got %v", got)
		}
	})
	t.Run("JID explícito pasa tal cual", func(t *testing.T) {
		got := resolveMentions("", []string{"123-456@g.us"})
		if len(got) != 1 || got[0] != "123-456@g.us" {
			t.Errorf("got %v", got)
		}
	})
}

func TestParseParticipantJIDs(t *testing.T) {
	t.Run("número a JID", func(t *testing.T) {
		jids, err := parseParticipantJIDs([]string{"5491122334455"})
		if err != nil {
			t.Fatalf("no debería fallar: %v", err)
		}
		if len(jids) != 1 || jids[0].String() != "5491122334455@s.whatsapp.net" {
			t.Errorf("got %v", jids)
		}
	})
	t.Run("vacíos se saltan", func(t *testing.T) {
		jids, err := parseParticipantJIDs([]string{"", "  ", "5491122334455"})
		if err != nil {
			t.Fatalf("no debería fallar: %v", err)
		}
		if len(jids) != 1 {
			t.Errorf("los vacíos deberían saltarse, got %v", jids)
		}
	})
}
