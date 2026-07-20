package store

import (
	"bytes"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// newTestStore crea un MessageStore sobre una DB temporal (el storeDir se inyecta,
// así que apuntamos a un directorio temporal aislado por test).
func newTestStore(t *testing.T) *MessageStore {
	t.Helper()
	s, err := NewMessageStore(t.TempDir())
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

// seedMessage inserta chat (FK) + mensaje con contenido de texto simple.
func seedMessage(t *testing.T, s *MessageStore, id, chatJID, sender, content string, ts time.Time,
	isFromMe bool, mediaType, filename string) {
	t.Helper()
	if err := s.StoreChat(chatJID, "", ts); err != nil {
		t.Fatalf("StoreChat: %v", err)
	}
	if err := s.StoreMessage(id, chatJID, sender, content, ts, isFromMe,
		mediaType, filename, "", "", nil, nil, nil, 0); err != nil {
		t.Fatalf("StoreMessage: %v", err)
	}
}

// TestMarkMessageRevoked verifica el invariante del revoke: el contenido cambia
// in-place pero el timestamp NO se toca (el historial no se reordena).
func TestMarkMessageRevoked(t *testing.T) {
	s := newTestStore(t)
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	seedMessage(t, s, "m1", "111@s.whatsapp.net", "111", "texto original", ts, false, "", "")

	n, err := s.MarkMessageRevoked("m1", "111@s.whatsapp.net")
	if err != nil {
		t.Fatalf("MarkMessageRevoked: %v", err)
	}
	if n != 1 {
		t.Errorf("filas afectadas: got %d, want %d", n, 1)
	}

	msgs, err := s.GetMessages("111@s.whatsapp.net", 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("esperaba 1 mensaje, got %d", len(msgs))
	}
	if got := msgs[0].Content; got != "🗑️ Mensaje eliminado" {
		t.Errorf("content: got %q, want %q", got, "🗑️ Mensaje eliminado")
	}
	if !msgs[0].Time.Equal(ts) {
		t.Errorf("el timestamp no debe cambiar: got %v, want %v", msgs[0].Time, ts)
	}
}

// TestMarkMessageRevokedMissing: revocar un id inexistente no es error, solo 0 filas
// (el caller decide qué hacer con un revoke de un mensaje que nunca capturamos).
func TestMarkMessageRevokedMissing(t *testing.T) {
	s := newTestStore(t)
	n, err := s.MarkMessageRevoked("no-existe", "111@s.whatsapp.net")
	if err != nil {
		t.Fatalf("no debería fallar: %v", err)
	}
	if n != 0 {
		t.Errorf("filas afectadas: got %d, want %d", n, 0)
	}
}

// TestApplyMessageEdit verifica el invariante del edit: contenido nuevo + sufijo
// " (editado)" in-place, SIN tocar el timestamp.
func TestApplyMessageEdit(t *testing.T) {
	s := newTestStore(t)
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	seedMessage(t, s, "m1", "111@s.whatsapp.net", "111", "texto original", ts, false, "", "")

	n, err := s.ApplyMessageEdit("m1", "111@s.whatsapp.net", "texto nuevo")
	if err != nil {
		t.Fatalf("ApplyMessageEdit: %v", err)
	}
	if n != 1 {
		t.Errorf("filas afectadas: got %d, want %d", n, 1)
	}

	msgs, err := s.GetMessages("111@s.whatsapp.net", 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("esperaba 1 mensaje, got %d", len(msgs))
	}
	if got := msgs[0].Content; got != "texto nuevo (editado)" {
		t.Errorf("content: got %q, want %q", got, "texto nuevo (editado)")
	}
	if !msgs[0].Time.Equal(ts) {
		t.Errorf("el timestamp no debe cambiar: got %v, want %v", msgs[0].Time, ts)
	}
}

// TestApplyMessageEditMissing: editar un id inexistente devuelve 0 filas sin error
// (el handler usa ese 0 para caer al flujo de inserción del texto editado).
func TestApplyMessageEditMissing(t *testing.T) {
	s := newTestStore(t)
	n, err := s.ApplyMessageEdit("no-existe", "111@s.whatsapp.net", "da igual")
	if err != nil {
		t.Fatalf("no debería fallar: %v", err)
	}
	if n != 0 {
		t.Errorf("filas afectadas: got %d, want %d", n, 0)
	}
}

// TestMediaInfoRoundTrip verifica que StoreMessage + GetMediaInfo preservan la metadata
// de descarga (el direct_path es el dato crítico: sin él la descarga mms3 da 403) y que
// StoreMediaInfo actualiza url/claves sin pisar el direct_path.
func TestMediaInfoRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	chat := "111@s.whatsapp.net"
	if err := s.StoreChat(chat, "", ts); err != nil {
		t.Fatalf("StoreChat: %v", err)
	}
	if err := s.StoreMessage("m1", chat, "111", "una foto", ts, false,
		"image", "foto.jpg", "https://mmg.whatsapp.net/a", "/v/t62/a.enc",
		[]byte{1, 2}, []byte{3, 4}, []byte{5, 6}, 1234); err != nil {
		t.Fatalf("StoreMessage: %v", err)
	}

	mediaType, filename, url, directPath, mediaKey, sha, encSHA, length, err := s.GetMediaInfo("m1", chat)
	if err != nil {
		t.Fatalf("GetMediaInfo: %v", err)
	}
	if mediaType != "image" || filename != "foto.jpg" {
		t.Errorf("tipo/filename: got %q/%q, want %q/%q", mediaType, filename, "image", "foto.jpg")
	}
	if url != "https://mmg.whatsapp.net/a" {
		t.Errorf("url: got %q, want %q", url, "https://mmg.whatsapp.net/a")
	}
	if directPath != "/v/t62/a.enc" {
		t.Errorf("directPath: got %q, want %q", directPath, "/v/t62/a.enc")
	}
	if !bytes.Equal(mediaKey, []byte{1, 2}) || !bytes.Equal(sha, []byte{3, 4}) || !bytes.Equal(encSHA, []byte{5, 6}) {
		t.Errorf("claves/hashes no preservados: key=%v sha=%v encSha=%v", mediaKey, sha, encSHA)
	}
	if length != 1234 {
		t.Errorf("fileLength: got %d, want %d", length, 1234)
	}

	// StoreMediaInfo actualiza url + claves (p. ej. tras re-subir la media) sin
	// tocar media_type/filename/direct_path.
	if err := s.StoreMediaInfo("m1", chat, "https://mmg.whatsapp.net/b",
		[]byte{9}, []byte{8}, []byte{7}, 4321); err != nil {
		t.Fatalf("StoreMediaInfo: %v", err)
	}
	_, _, url2, directPath2, mediaKey2, _, _, length2, err := s.GetMediaInfo("m1", chat)
	if err != nil {
		t.Fatalf("GetMediaInfo tras update: %v", err)
	}
	if url2 != "https://mmg.whatsapp.net/b" || length2 != 4321 || !bytes.Equal(mediaKey2, []byte{9}) {
		t.Errorf("update no aplicado: url=%q len=%d key=%v", url2, length2, mediaKey2)
	}
	if directPath2 != "/v/t62/a.enc" {
		t.Errorf("StoreMediaInfo no debe pisar direct_path: got %q", directPath2)
	}
}

// TestGetMediaInfoNullDirectPath: las filas creadas ANTES de la migración de
// direct_path tienen NULL en esa columna; el COALESCE del SELECT debe devolver ""
// en vez de fallar el Scan (string no acepta NULL).
func TestGetMediaInfoNullDirectPath(t *testing.T) {
	s := newTestStore(t)
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	chat := "111@s.whatsapp.net"
	seedMessage(t, s, "m1", chat, "111", "foto vieja", ts, false, "image", "foto.jpg")

	// Simular una fila pre-migración: direct_path en NULL explícito.
	if _, err := s.DB().Exec("UPDATE messages SET direct_path = NULL WHERE id = 'm1'"); err != nil {
		t.Fatalf("setup NULL: %v", err)
	}

	_, _, _, directPath, _, _, _, _, err := s.GetMediaInfo("m1", chat)
	if err != nil {
		t.Fatalf("GetMediaInfo con direct_path NULL no debería fallar: %v", err)
	}
	if directPath != "" {
		t.Errorf("directPath: got %q, want %q", directPath, "")
	}
}

// TestGetMediaInfoMissing: sin fila, GetMediaInfo propaga sql.ErrNoRows (el caller
// usa ese error para caer al lookup básico de media_type/filename).
func TestGetMediaInfoMissing(t *testing.T) {
	s := newTestStore(t)
	_, _, _, _, _, _, _, _, err := s.GetMediaInfo("no-existe", "111@s.whatsapp.net")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("got %v, want sql.ErrNoRows", err)
	}
}

// TestMessageSender: /api/star reconstruye el target del app-state con el sender
// crudo + is_from_me; sin fila debe devolver sql.ErrNoRows (best-effort del caller).
func TestMessageSender(t *testing.T) {
	s := newTestStore(t)
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	seedMessage(t, s, "m1", "111@s.whatsapp.net", "999888777", "hola", ts, true, "", "")

	sender, fromMe, err := s.MessageSender("m1")
	if err != nil {
		t.Fatalf("MessageSender: %v", err)
	}
	if sender != "999888777" {
		t.Errorf("sender: got %q, want %q", sender, "999888777")
	}
	if !fromMe {
		t.Error("fromMe debería ser true")
	}

	if _, _, err := s.MessageSender("no-existe"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("sin fila: got %v, want sql.ErrNoRows", err)
	}
}

// TestPollSender: el filtro media_type='poll' es la clave — un mensaje NORMAL con el
// mismo id no debe matchear (votar contra un no-poll rompería el MessageInfo del voto).
func TestPollSender(t *testing.T) {
	s := newTestStore(t)
	ts := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	// Mensaje normal con id "p1" en el chat A: NO es un poll.
	seedMessage(t, s, "p1", "aaa@s.whatsapp.net", "111", "texto normal", ts, false, "", "")
	if _, _, err := s.PollSender("p1", "aaa@s.whatsapp.net"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("un mensaje normal con el mismo id no debe matchear: got %v, want sql.ErrNoRows", err)
	}

	// Poll real con el MISMO id en otro chat: sí matchea.
	seedMessage(t, s, "p1", "bbb@s.whatsapp.net", "222333444", "", ts, false, "poll", `["Sí","No"]`)
	sender, fromMe, err := s.PollSender("p1", "bbb@s.whatsapp.net")
	if err != nil {
		t.Fatalf("PollSender: %v", err)
	}
	if sender != "222333444" {
		t.Errorf("sender: got %q, want %q", sender, "222333444")
	}
	if fromMe {
		t.Error("fromMe debería ser false")
	}

	// Chat correcto pero id inexistente: tampoco matchea.
	if _, _, err := s.PollSender("otro-id", "bbb@s.whatsapp.net"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("id inexistente: got %v, want sql.ErrNoRows", err)
	}
}
