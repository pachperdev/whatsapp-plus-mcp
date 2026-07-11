package store

import (
	"testing"
	"time"
)

// newTestStore crea un MessageStore sobre una DB temporal (NewMessageStore usa
// "store/messages.db" relativo al cwd, así que aislamos con t.Chdir).
func newTestStore(t *testing.T) *MessageStore {
	t.Helper()
	t.Chdir(t.TempDir())
	s, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreAndGetMessages(t *testing.T) {
	s := newTestStore(t)
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	if err := s.StoreChat("111@s.whatsapp.net", "Ana", ts); err != nil {
		t.Fatalf("StoreChat: %v", err)
	}
	if err := s.StoreMessage("m1", "111@s.whatsapp.net", "111", "hola", ts, false,
		"", "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatalf("StoreMessage: %v", err)
	}

	msgs, err := s.GetMessages("111@s.whatsapp.net", 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("esperaba 1 mensaje, got %d", len(msgs))
	}
	if msgs[0].Content != "hola" {
		t.Errorf("content: got %q", msgs[0].Content)
	}
}

func TestStoreMessageSkipsEmpty(t *testing.T) {
	s := newTestStore(t)
	ts := time.Now().UTC()
	if err := s.StoreChat("c@s.whatsapp.net", "", ts); err != nil {
		t.Fatalf("StoreChat: %v", err)
	}
	// Sin contenido ni media: no debe insertar (regla de storeMessageExec).
	if err := s.StoreMessage("m0", "c@s.whatsapp.net", "c", "", ts, false,
		"", "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatalf("StoreMessage: %v", err)
	}
	msgs, _ := s.GetMessages("c@s.whatsapp.net", 10)
	if len(msgs) != 0 {
		t.Errorf("un mensaje vacío no debería guardarse, got %d", len(msgs))
	}
}

func TestUnreadTracking(t *testing.T) {
	s := newTestStore(t)
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	if err := s.AddUnread("c1@s.whatsapp.net", "m1", ts); err != nil {
		t.Fatalf("AddUnread: %v", err)
	}
	if err := s.AddUnread("c1@s.whatsapp.net", "m2", ts); err != nil {
		t.Fatalf("AddUnread: %v", err)
	}
	// Idempotente: reinsertar el mismo id no duplica.
	if err := s.AddUnread("c1@s.whatsapp.net", "m1", ts); err != nil {
		t.Fatalf("AddUnread dup: %v", err)
	}

	chats, err := s.GetUnreadChats()
	if err != nil {
		t.Fatalf("GetUnreadChats: %v", err)
	}
	if len(chats) != 1 || chats[0].UnreadCount != 2 {
		t.Fatalf("esperaba 1 chat con 2 no-leídos, got %+v", chats)
	}

	n, err := s.ClearChatUnread("c1@s.whatsapp.net")
	if err != nil || n != 2 {
		t.Fatalf("ClearChatUnread: n=%d err=%v", n, err)
	}
	if chats, _ := s.GetUnreadChats(); len(chats) != 0 {
		t.Errorf("tras limpiar no debería quedar nada, got %+v", chats)
	}
}

func TestGetUnreadChatsExcludesBroadcast(t *testing.T) {
	s := newTestStore(t)
	ts := time.Now().UTC()
	_ = s.AddUnread("status@broadcast", "m1", ts)
	_ = s.AddUnread("123@newsletter", "m2", ts)
	_ = s.AddUnread("real@s.whatsapp.net", "m3", ts)

	chats, err := s.GetUnreadChats()
	if err != nil {
		t.Fatalf("GetUnreadChats: %v", err)
	}
	if len(chats) != 1 || chats[0].ChatJID != "real@s.whatsapp.net" {
		t.Errorf("status/newsletter deberían excluirse, got %+v", chats)
	}
}
