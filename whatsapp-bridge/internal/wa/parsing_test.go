package wa

import (
	"testing"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

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
			if got := ExtractTextContent(tc.msg); got != tc.want {
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
		got := ResolveMentions("", []string{"5491122334455"})
		want := []string{"5491122334455@s.whatsapp.net"}
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("auto-detección en texto", func(t *testing.T) {
		got := ResolveMentions("hola @5491122334455 qué tal", nil)
		if len(got) != 1 || got[0] != "5491122334455@s.whatsapp.net" {
			t.Errorf("got %v", got)
		}
	})
	t.Run("dedup conservando orden", func(t *testing.T) {
		got := ResolveMentions("@5491122334455", []string{"5491122334455"})
		if len(got) != 1 {
			t.Errorf("debería deduplicar, got %v", got)
		}
	})
	t.Run("JID explícito pasa tal cual", func(t *testing.T) {
		got := ResolveMentions("", []string{"123-456@g.us"})
		if len(got) != 1 || got[0] != "123-456@g.us" {
			t.Errorf("got %v", got)
		}
	})
}

func TestParseParticipantJIDs(t *testing.T) {
	t.Run("número a JID", func(t *testing.T) {
		jids, err := ParseParticipantJIDs([]string{"5491122334455"})
		if err != nil {
			t.Fatalf("no debería fallar: %v", err)
		}
		if len(jids) != 1 || jids[0].String() != "5491122334455@s.whatsapp.net" {
			t.Errorf("got %v", jids)
		}
	})
	t.Run("vacíos se saltan", func(t *testing.T) {
		jids, err := ParseParticipantJIDs([]string{"", "  ", "5491122334455"})
		if err != nil {
			t.Fatalf("no debería fallar: %v", err)
		}
		if len(jids) != 1 {
			t.Errorf("los vacíos deberían saltarse, got %v", jids)
		}
	})
}

func TestExtractDirectPathFromURL(t *testing.T) {
	url := "https://mmg.whatsapp.net/v/t62.7118-24/13812002_n.enc?ccb=11-4&oh=abc"
	got := ExtractDirectPathFromURL(url)
	if got != "/v/t62.7118-24/13812002_n.enc" {
		t.Errorf("got %q", got)
	}
	// Sin ".net/": devuelve la URL original.
	if got := ExtractDirectPathFromURL("no-url"); got != "no-url" {
		t.Errorf("fallback got %q", got)
	}
}
