package wa

import (
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"whatsapp-client/internal/store"
)

// newTestService arma un Service con un MessageStore real sobre una DB temporal y
// client whatsmeow NIL: alcanza para los métodos que solo consultan la DB
// (LoadGroupInvite, buildQuotedContext, LastMsgKey, HandleMessage/revoke). Los caminos
// que dereferencian s.Client quedan fuera a propósito (ver comentarios por test).
func newTestService(t *testing.T) *Service {
	t.Helper()
	st, err := store.NewMessageStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewService(nil, st, waLog.Noop, "", nil)
}

// mustStoreMsg inserta chat (FK) + mensaje, fallando el test si algo sale mal.
func mustStoreMsg(t *testing.T, s *Service, id, chatJID, sender, content string, ts time.Time,
	isFromMe bool, mediaType, filename string) {
	t.Helper()
	if err := s.Store.TouchChat(chatJID, ts); err != nil {
		t.Fatalf("TouchChat: %v", err)
	}
	if err := s.Store.StoreMessage(id, chatJID, sender, content, ts, isFromMe,
		mediaType, filename, "", "", nil, nil, nil, 0); err != nil {
		t.Fatalf("StoreMessage: %v", err)
	}
}

// TestLoadGroupInvite cubre la reconstrucción de una invitación capturada. Tiene
// historia de bug: reconstruir el inviter desde el sender (LID sin sufijo) producía un
// @s.whatsapp.net falso y el server respondía 410; por eso el inviter debe ser el
// chat_jid donde llegó el mensaje privado.
func TestLoadGroupInvite(t *testing.T) {
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		chatJID  string
		msgID    string
		filename string // JSON persistido con la invitación; "" = no insertar fila
		wantErr  string // fragmento esperado del error; "" = camino feliz
	}{
		{
			name:    "fila inexistente",
			chatJID: "5215551234567@s.whatsapp.net",
			msgID:   "no-existe",
			wantErr: "no encontrada",
		},
		{
			name:     "JSON corrupto en filename",
			chatJID:  "5215551234567@s.whatsapp.net",
			msgID:    "inv-corrupta",
			filename: "{esto no es json",
			wantErr:  "corruptos",
		},
		{
			// "bad..user" tiene dos puntos en el user -> types.ParseJID falla.
			name:     "group_jid invalido",
			chatJID:  "5215551234567@s.whatsapp.net",
			msgID:    "inv-groupjid",
			filename: `{"group_jid":"bad..user@g.us","code":"X","expiration":1}`,
			wantErr:  "group_jid invalido",
		},
		{
			// El chat_jid es el inviter: si no parsea, la invitación no sirve.
			name:     "chat_jid invalido",
			chatJID:  "bad..chat@s.whatsapp.net",
			msgID:    "inv-chatjid",
			filename: `{"group_jid":"123456789@g.us","code":"X","expiration":1}`,
			wantErr:  "chat_jid (inviter) invalido",
		},
		{
			name:     "camino feliz",
			chatJID:  "5215551234567@s.whatsapp.net",
			msgID:    "inv-ok",
			filename: `{"group_jid":"123456789@g.us","code":"ABCDefgh1234","expiration":1751371200}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestService(t)
			if tc.filename != "" {
				mustStoreMsg(t, s, tc.msgID, tc.chatJID, "111222333", "", ts, false,
					"group_invite", tc.filename)
			}

			groupJID, inviter, code, expiration, err := s.LoadGroupInvite(tc.chatJID, tc.msgID)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("esperaba error conteniendo %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error: got %q, want que contenga %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("no debería fallar: %v", err)
			}
			if groupJID.String() != "123456789@g.us" {
				t.Errorf("groupJID: got %q, want %q", groupJID.String(), "123456789@g.us")
			}
			// El inviter ES el chat donde llegó la invitación (mensaje privado), no el sender.
			if inviter.String() != tc.chatJID {
				t.Errorf("inviter: got %q, want %q", inviter.String(), tc.chatJID)
			}
			if code != "ABCDefgh1234" {
				t.Errorf("code: got %q, want %q", code, "ABCDefgh1234")
			}
			if expiration != 1751371200 {
				t.Errorf("expiration: got %d, want %d", expiration, 1751371200)
			}
		})
	}
}

// TestBuildQuotedContext cubre el armado del ContextInfo para replies. La rama
// isFromMe NO se testea: dereferencia s.Client.Store y aquí el client es nil
// (cubrirla requeriría un cliente whatsmeow real o un refactor de la firma).
func TestBuildQuotedContext(t *testing.T) {
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	t.Run("quoted inexistente devuelve nil", func(t *testing.T) {
		s := newTestService(t)
		if got := s.buildQuotedContext("no-existe"); got != nil {
			t.Errorf("sin fila en la DB debería devolver nil, got %+v", got)
		}
	})

	t.Run("grupo con sender sin @ agrega sufijo @lid", func(t *testing.T) {
		// En multidevice los participantes de grupo se guardan como LID pelado
		// (solo dígitos); el ContextInfo necesita el JID completo con @lid.
		s := newTestService(t)
		mustStoreMsg(t, s, "q1", "123456789@g.us", "8888777766", "hola grupo", ts, false, "", "")

		ctx := s.buildQuotedContext("q1")
		if ctx == nil {
			t.Fatal("esperaba ContextInfo, got nil")
		}
		if got := ctx.GetParticipant(); got != "8888777766@lid" {
			t.Errorf("participant: got %q, want %q", got, "8888777766@lid")
		}
		if got := ctx.GetStanzaID(); got != "q1" {
			t.Errorf("stanzaID: got %q, want %q", got, "q1")
		}
		if got := ctx.GetQuotedMessage().GetConversation(); got != "hola grupo" {
			t.Errorf("quoted content: got %q, want %q", got, "hola grupo")
		}
	})

	t.Run("grupo con sender con @ pasa tal cual", func(t *testing.T) {
		s := newTestService(t)
		mustStoreMsg(t, s, "q2", "123456789@g.us", "8888777766@lid", "otro", ts, false, "", "")

		ctx := s.buildQuotedContext("q2")
		if ctx == nil {
			t.Fatal("esperaba ContextInfo, got nil")
		}
		if got := ctx.GetParticipant(); got != "8888777766@lid" {
			t.Errorf("participant: got %q, want %q", got, "8888777766@lid")
		}
	})

	t.Run("chat directo usa el chat como participant", func(t *testing.T) {
		// En un chat 1:1 el autor de un mensaje recibido ES el contacto (el chat).
		s := newTestService(t)
		mustStoreMsg(t, s, "q3", "5215551234567@s.whatsapp.net", "5215551234567", "directo", ts, false, "", "")

		ctx := s.buildQuotedContext("q3")
		if ctx == nil {
			t.Fatal("esperaba ContextInfo, got nil")
		}
		if got := ctx.GetParticipant(); got != "5215551234567@s.whatsapp.net" {
			t.Errorf("participant: got %q, want %q", got, "5215551234567@s.whatsapp.net")
		}
	})

	t.Run("content vacío no arma QuotedMessage", func(t *testing.T) {
		// Un mensaje solo-media (content "") existe en la DB gracias al media_type;
		// citar sin texto no debe fabricar un QuotedMessage vacío.
		s := newTestService(t)
		mustStoreMsg(t, s, "q4", "5215551234567@s.whatsapp.net", "5215551234567", "", ts, false, "image", "foto.jpg")

		ctx := s.buildQuotedContext("q4")
		if ctx == nil {
			t.Fatal("esperaba ContextInfo, got nil")
		}
		if ctx.QuotedMessage != nil {
			t.Errorf("sin content no debería haber QuotedMessage, got %+v", ctx.QuotedMessage)
		}
	})
}

// TestLastMsgKey cubre el armado del MessageKey del último mensaje de un chat
// (lo usan BuildArchive y BuildMarkChatAsRead). No toca s.Client, así que todos
// los caminos —incluido isFromMe— son testeables con client nil.
func TestLastMsgKey(t *testing.T) {
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	groupJID := types.JID{User: "123456789", Server: types.GroupServer}
	directJID := types.JID{User: "5215551234567", Server: types.DefaultUserServer}

	t.Run("chat sin mensajes devuelve key nil", func(t *testing.T) {
		s := newTestService(t)
		key, _ := s.LastMsgKey(groupJID)
		if key != nil {
			t.Errorf("sin mensajes debería devolver nil, got %+v", key)
		}
	})

	t.Run("grupo ajeno con sender sin @ agrega @lid", func(t *testing.T) {
		s := newTestService(t)
		mustStoreMsg(t, s, "m1", groupJID.String(), "8888777766", "hola", ts, false, "", "")

		key, gotTS := s.LastMsgKey(groupJID)
		if key == nil {
			t.Fatal("esperaba MessageKey, got nil")
		}
		if got := key.GetParticipant(); got != "8888777766@lid" {
			t.Errorf("participant: got %q, want %q", got, "8888777766@lid")
		}
		if got := key.GetRemoteJID(); got != groupJID.String() {
			t.Errorf("remoteJID: got %q, want %q", got, groupJID.String())
		}
		if got := key.GetID(); got != "m1" {
			t.Errorf("id: got %q, want %q", got, "m1")
		}
		if key.GetFromMe() {
			t.Error("fromMe debería ser false para un mensaje ajeno")
		}
		if !gotTS.Equal(ts) {
			t.Errorf("timestamp: got %v, want %v", gotTS, ts)
		}
	})

	t.Run("grupo ajeno con sender con @ pasa tal cual", func(t *testing.T) {
		s := newTestService(t)
		mustStoreMsg(t, s, "m2", groupJID.String(), "8888777766@lid", "hola", ts, false, "", "")

		key, _ := s.LastMsgKey(groupJID)
		if key == nil {
			t.Fatal("esperaba MessageKey, got nil")
		}
		if got := key.GetParticipant(); got != "8888777766@lid" {
			t.Errorf("participant: got %q, want %q", got, "8888777766@lid")
		}
	})

	t.Run("grupo con mensaje propio no lleva participant", func(t *testing.T) {
		// El participant solo aplica a mensajes ajenos en grupos: para los propios
		// el server lo infiere del FromMe.
		s := newTestService(t)
		mustStoreMsg(t, s, "m3", groupJID.String(), "5215559999999", "mío", ts, true, "", "")

		key, _ := s.LastMsgKey(groupJID)
		if key == nil {
			t.Fatal("esperaba MessageKey, got nil")
		}
		if key.Participant != nil {
			t.Errorf("mensaje propio no debería llevar participant, got %q", key.GetParticipant())
		}
		if !key.GetFromMe() {
			t.Error("fromMe debería ser true")
		}
	})

	t.Run("chat directo no lleva participant", func(t *testing.T) {
		s := newTestService(t)
		mustStoreMsg(t, s, "m4", directJID.String(), "5215551234567", "hola", ts, false, "", "")

		key, _ := s.LastMsgKey(directJID)
		if key == nil {
			t.Fatal("esperaba MessageKey, got nil")
		}
		if key.Participant != nil {
			t.Errorf("chat directo no debería llevar participant, got %q", key.GetParticipant())
		}
	})

	t.Run("elige el mensaje más reciente", func(t *testing.T) {
		s := newTestService(t)
		mustStoreMsg(t, s, "viejo", directJID.String(), "5215551234567", "primero", ts, false, "", "")
		mustStoreMsg(t, s, "nuevo", directJID.String(), "5215551234567", "segundo", ts.Add(time.Hour), false, "", "")

		key, gotTS := s.LastMsgKey(directJID)
		if key == nil {
			t.Fatal("esperaba MessageKey, got nil")
		}
		if got := key.GetID(); got != "nuevo" {
			t.Errorf("debería elegir el más reciente: got %q, want %q", got, "nuevo")
		}
		if !gotTS.Equal(ts.Add(time.Hour)) {
			t.Errorf("timestamp: got %v, want %v", gotTS, ts.Add(time.Hour))
		}
	})
}
