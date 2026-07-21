package wa

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// TestHandleEventReceiptReadSelf cubre el caso Receipt del dispatcher: un read-receipt
// PROPIO (leí el chat desde el teléfono u otro device) debe limpiar los no-leídos de
// ESE chat en la DB. Client nil a propósito: el caso solo toca Store y Log, así que
// si alguien introdujera una dereferencia al cliente, este test panicaría.
func TestHandleEventReceiptReadSelf(t *testing.T) {
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	chat := types.JID{User: "5215551234567", Server: types.DefaultUserServer}
	otherChat := types.JID{User: "5215559999999", Server: types.DefaultUserServer}

	newReceipt := func(c types.JID, typ types.ReceiptType) *events.Receipt {
		return &events.Receipt{
			MessageSource: types.MessageSource{Chat: c},
			Type:          typ,
			Timestamp:     ts.Add(time.Minute),
		}
	}

	// addUnread siembra un no-leído (con su chat para el FK), fallando el test si algo sale mal.
	addUnread := func(t *testing.T, s *Service, c types.JID, msgID string) {
		t.Helper()
		if err := s.Store.TouchChat(c.String(), ts); err != nil {
			t.Fatalf("TouchChat: %v", err)
		}
		if err := s.Store.AddUnread(c.String(), msgID, ts); err != nil {
			t.Fatalf("AddUnread: %v", err)
		}
	}

	// unreadCount devuelve cuántos no-leídos hay para un chat según la DB.
	unreadCount := func(t *testing.T, s *Service, c types.JID) int {
		t.Helper()
		chats, err := s.Store.GetUnreadChats()
		if err != nil {
			t.Fatalf("GetUnreadChats: %v", err)
		}
		for _, u := range chats {
			if u.ChatJID == c.String() {
				return u.UnreadCount
			}
		}
		return 0
	}

	t.Run("read-self limpia los no-leídos solo del chat leído", func(t *testing.T) {
		s := newTestService(t)
		addUnread(t, s, chat, "u1")
		addUnread(t, s, otherChat, "u2")

		s.HandleEvent(newReceipt(chat, types.ReceiptTypeReadSelf))

		if got := unreadCount(t, s, chat); got != 0 {
			t.Errorf("no-leídos del chat leído: got %d, want 0", got)
		}
		// Los demás chats no deben verse afectados: el receipt es por chat.
		if got := unreadCount(t, s, otherChat); got != 1 {
			t.Errorf("no-leídos de otro chat: got %d, want 1 (no debe tocarse)", got)
		}
	})

	t.Run("read ajeno NO limpia", func(t *testing.T) {
		// ReceiptTypeRead = otros leyeron MIS mensajes; no dice nada de lo que yo leí.
		// Limpiar aquí vaciaría el tracking de no-leídos con cada tilde azul recibido.
		s := newTestService(t)
		addUnread(t, s, chat, "u1")

		s.HandleEvent(newReceipt(chat, types.ReceiptTypeRead))

		if got := unreadCount(t, s, chat); got != 1 {
			t.Errorf("un read ajeno no debe limpiar: got %d, want 1", got)
		}
	})
}

// TestHandleEventUnknownIsNoop: el dispatcher debe ignorar (sin panic ni efectos)
// cualquier evento que no contemple; whatsmeow emite muchos tipos que no manejamos.
func TestHandleEventUnknownIsNoop(t *testing.T) {
	s := newTestService(t)

	type unknownEvent struct{ X int }
	s.HandleEvent(&unknownEvent{X: 1})
	s.HandleEvent("un string no es un evento")
	s.HandleEvent(nil)

	chats, err := s.Store.GetUnreadChats()
	if err != nil {
		t.Fatalf("GetUnreadChats: %v", err)
	}
	if len(chats) != 0 {
		t.Errorf("un evento desconocido no debe tener efectos, got %+v", chats)
	}
}

// TestHandleEventMessageRevoke cubre el camino *events.Message del dispatcher usando
// el revoke (mismo patrón que TestHandleMessageRevoke, pero entrando por HandleEvent):
// verifica que el dispatch delega en HandleMessage. Client nil funciona porque el
// revoke retorna temprano, antes de tocar el cliente.
func TestHandleEventMessageRevoke(t *testing.T) {
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	chat := types.JID{User: "5215551234567", Server: types.DefaultUserServer}
	sender := types.JID{User: "5215551234567", Server: types.DefaultUserServer}

	s := newTestService(t)
	mustStoreMsg(t, s, "orig", chat.String(), sender.User, "texto original", ts, false, "", "")

	// Entra por HandleEvent (no directo a HandleMessage): verifica que el dispatch
	// delega en HandleMessage. El shape del evento y el tombstone son compartidos.
	s.HandleEvent(newRevokeEvent(chat, sender, "orig", ts.Add(time.Minute)))

	assertRevokeTombstone(t, s, chat)
}
