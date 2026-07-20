package wa

import (
	"testing"
	"time"

	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// TestHandleMessageRevoke cubre SOLO el camino revoke de HandleMessage: un
// ProtocolMessage REVOKE debe marcar el mensaje original como eliminado y retornar
// temprano, ANTES de tocar el cliente whatsmeow (por eso funciona con client nil:
// si el early-return se rompiera, GetChatName dereferenciaría nil y el test panicaría).
func TestHandleMessageRevoke(t *testing.T) {
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	chat := types.JID{User: "5215551234567", Server: types.DefaultUserServer}
	sender := types.JID{User: "5215551234567", Server: types.DefaultUserServer}

	newRevokeEvent := func(targetID string) *events.Message {
		pm := &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}
		if targetID != "" {
			pm.Key = &waCommon.MessageKey{ID: proto.String(targetID)}
		}
		return &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{Chat: chat, Sender: sender},
				ID:            "revoke-evt-1",
				Timestamp:     ts.Add(time.Minute),
			},
			Message: &waE2E.Message{ProtocolMessage: pm},
		}
	}

	t.Run("persiste el mensaje como eliminado sin tocar el timestamp", func(t *testing.T) {
		s := newTestService(t)
		mustStoreMsg(t, s, "orig", chat.String(), sender.User, "texto original", ts, false, "", "")

		s.HandleMessage(newRevokeEvent("orig"))

		msgs, err := s.Store.GetMessages(chat.String(), 10)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(msgs) != 1 {
			// El evento de revoke NO debe insertarse como mensaje propio (early return).
			t.Fatalf("esperaba 1 mensaje (el original marcado), got %d", len(msgs))
		}
		if got := msgs[0].Content; got != "🗑️ Mensaje eliminado" {
			t.Errorf("content: got %q, want %q", got, "🗑️ Mensaje eliminado")
		}
		// Invariante: el revoke se refleja in-place SIN reordenar el historial.
		if !msgs[0].Time.Equal(ts) {
			t.Errorf("timestamp: got %v, want %v (no debe cambiar)", msgs[0].Time, ts)
		}
	})

	t.Run("revoke sin key es un no-op", func(t *testing.T) {
		// Un REVOKE sin target (Key nil -> GetID() == "") no debe tocar nada ni panicar.
		s := newTestService(t)
		mustStoreMsg(t, s, "orig", chat.String(), sender.User, "texto original", ts, false, "", "")

		s.HandleMessage(newRevokeEvent(""))

		msgs, err := s.Store.GetMessages(chat.String(), 10)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(msgs) != 1 || msgs[0].Content != "texto original" {
			t.Errorf("sin target el original debe quedar intacto, got %+v", msgs)
		}
	})

	t.Run("revoke de un mensaje que no tenemos no inserta nada", func(t *testing.T) {
		// MarkMessageRevoked devuelve 0 filas sin error; el handler retorna temprano
		// igual y el chat queda vacío (no aparece un mensaje fantasma).
		s := newTestService(t)
		if err := s.Store.TouchChat(chat.String(), ts); err != nil {
			t.Fatalf("TouchChat: %v", err)
		}

		s.HandleMessage(newRevokeEvent("desconocido"))

		msgs, err := s.Store.GetMessages(chat.String(), 10)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("no debería haber mensajes, got %+v", msgs)
		}
	})
}
