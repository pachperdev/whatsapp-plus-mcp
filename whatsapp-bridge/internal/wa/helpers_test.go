package wa

import (
	"testing"
	"time"

	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatsapp-client/internal/store"
)

// Helpers compartidos por los tests de revoke. El test del dispatcher (HandleEvent)
// y el de HandleMessage construían a mano el MISMO *events.Message REVOKE y asertaban
// el MISMO tombstone; se centraliza aquí para no duplicar (misma cobertura, un solo
// lugar donde vive el shape del evento y el texto esperado).

// revokeTombstone es el texto con el que MarkMessageRevoked reemplaza el contenido de
// un mensaje revocado. Debe coincidir con el de internal/store (MarkMessageRevoked).
const revokeTombstone = "🗑️ Mensaje eliminado"

// newRevokeEvent construye un *events.Message que representa un REVOKE entrante
// apuntando a targetID (el id del mensaje original a marcar como eliminado). Un
// targetID vacío deja la Key en nil, simulando un REVOKE sin target (caso no-op).
func newRevokeEvent(chat, sender types.JID, targetID string, ts time.Time) *events.Message {
	pm := &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}
	if targetID != "" {
		pm.Key = &waCommon.MessageKey{ID: proto.String(targetID)}
	}
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender},
			ID:            "revoke-evt-1",
			Timestamp:     ts,
		},
		Message: &waE2E.Message{ProtocolMessage: pm},
	}
}

// assertRevokeTombstone verifica que el chat tenga EXACTAMENTE un mensaje y que su
// contenido sea el tombstone de revoke. Devuelve ese mensaje para que el caller haga
// aserciones extra (p. ej. que el timestamp NO se reordenó).
func assertRevokeTombstone(t *testing.T, s *Service, chat types.JID) store.Message {
	t.Helper()
	msgs, err := s.Store.GetMessages(chat.String(), 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("esperaba 1 mensaje (el original marcado), got %d", len(msgs))
	}
	if got := msgs[0].Content; got != revokeTombstone {
		t.Errorf("content: got %q, want %q", got, revokeTombstone)
	}
	return msgs[0]
}
