package wa

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// TestHandleMessageRevoke cubre SOLO el camino revoke de HandleMessage: un
// ProtocolMessage REVOKE debe marcar el mensaje original como eliminado y retornar
// temprano, ANTES de tocar el cliente whatsmeow (por eso funciona con client nil:
// si el early-return se rompiera, GetChatName dereferenciaría nil y el test panicaría).
func TestHandleMessageRevoke(t *testing.T) {
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	chat := types.JID{User: "5215551234567", Server: types.DefaultUserServer}
	sender := types.JID{User: "5215551234567", Server: types.DefaultUserServer}

	// revokeEvent arma el REVOKE compartido fijando chat/sender/timestamp de este test;
	// solo varía el targetID por subtest (el shape vive en newRevokeEvent, ver helpers_test.go).
	revokeEvent := func(targetID string) *events.Message {
		return newRevokeEvent(chat, sender, targetID, ts.Add(time.Minute))
	}

	t.Run("persiste el mensaje como eliminado sin tocar el timestamp", func(t *testing.T) {
		s := newTestService(t)
		mustStoreMsg(t, s, "orig", chat.String(), sender.User, "texto original", ts, false, "", "")

		s.HandleMessage(revokeEvent("orig"))

		// El evento de revoke NO debe insertarse como mensaje propio (early return):
		// el chat queda con 1 solo mensaje, el original marcado como eliminado.
		msg := assertRevokeTombstone(t, s, chat)
		// Invariante: el revoke se refleja in-place SIN reordenar el historial.
		if !msg.Time.Equal(ts) {
			t.Errorf("timestamp: got %v, want %v (no debe cambiar)", msg.Time, ts)
		}
	})

	t.Run("revoke sin key es un no-op", func(t *testing.T) {
		// Un REVOKE sin target (Key nil -> GetID() == "") no debe tocar nada ni panicar.
		s := newTestService(t)
		mustStoreMsg(t, s, "orig", chat.String(), sender.User, "texto original", ts, false, "", "")

		s.HandleMessage(revokeEvent(""))

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

		s.HandleMessage(revokeEvent("desconocido"))

		msgs, err := s.Store.GetMessages(chat.String(), 10)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("no debería haber mensajes, got %+v", msgs)
		}
	})
}
