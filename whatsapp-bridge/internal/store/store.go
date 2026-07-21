// Package store es la capa de persistencia SQLite del bridge: guarda el historial
// de chats/mensajes y el tracking de no-leídos en messages.db. Encapsula el manejo
// del formato de timestamp (dbTime) que el resto del sistema —incluido el server
// Python— espera.
//
// Nota de migración: algunas consultas ad-hoc del bridge todavía acceden al handle
// vía DB(). Se irán migrando a métodos de MessageStore de forma incremental.
package store

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Registra el driver "sqlite" (modernc, sin CGO) para sql.Open.
	_ "modernc.org/sqlite"
)

// Message is a stored chat message row (subset used by the bridge).
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// MessageStore is the SQLite-backed handler for storing message history.
type MessageStore struct {
	db *sql.DB
	// storeDir es el directorio raíz del store (donde vive messages.db y, bajo
	// subdirectorios <safeChat>/, la media descargada). Se guarda para poder podar
	// esa media al borrar un chat (DeleteChat), replicando la ruta que arma DownloadMedia.
	storeDir string
}

// DB expone el handle SQLite para consultas ad-hoc que aún no tienen un método
// propio (batches transaccionales del history-sync, lookups puntuales). Es un
// punto de migración: idealmente cada consulta termina como método del store.
func (store *MessageStore) DB() *sql.DB {
	return store.db
}

// NewMessageStore initializes the message store, opening/creating the SQLite DB.
// NewMessageStore abre (creando si hace falta) la DB de mensajes en storeDir. El
// directorio se inyecta desde la config (ya absoluto) para no depender del CWD.
func NewMessageStore(storeDir string) (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll(storeDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages.
	// WAL: permite lecturas concurrentes (el server Python lee mientras el bridge escribe)
	//      sin bloquear, eliminando "database is locked" entre ambos procesos.
	// busy_timeout: una escritura reintenta hasta 5s antes de fallar por lock.
	// synchronous=NORMAL: seguro bajo WAL y mucho mas rapido que FULL.
	// ToSlash: el DSN file: usa '/' en todas las plataformas (Windows incluido).
	dbPath := filepath.ToSlash(filepath.Join(storeDir, "messages.db"))
	db, err := sql.Open("sqlite",
		"file:"+dbPath+"?_pragma=foreign_keys(on)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}
	// SQLite escribe en serie: una sola conexion serializa las escrituras y evita
	// la contencion interna entre el history-sync y los mensajes en vivo.
	db.SetMaxOpenConns(1)

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			direct_path TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);

		CREATE INDEX IF NOT EXISTS idx_messages_chat_time ON messages(chat_jid, timestamp DESC);
		CREATE INDEX IF NOT EXISTS idx_chats_lastmsg ON chats(last_message_time DESC);

		CREATE TABLE IF NOT EXISTS unread_messages (
			chat_jid TEXT,
			message_id TEXT,
			timestamp TIMESTAMP,
			PRIMARY KEY (chat_jid, message_id)
		);
	`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	// Migracion idempotente: agrega direct_path a DBs creadas antes de este campo.
	// Si la columna ya existe, el error ("duplicate column name") se ignora a proposito.
	// El directPath nativo del protobuf es necesario para descargar media: whatsmeow.Download
	// usa solo GetDirectPath, y reconstruirlo de la URL falla con el formato nuevo (mms3) -> 403.
	_, _ = db.Exec("ALTER TABLE messages ADD COLUMN direct_path TEXT")

	return &MessageStore{db: db, storeDir: storeDir}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// tsLayout es el formato de timestamp persistido en messages.db. Debe coincidir con el
// que escribia mattn/go-sqlite3 y con lo que datetime.fromisoformat() del server Python
// espera. modernc.org/sqlite por defecto serializa time.Time con time.Time.String()
// ("2026-06-23 16:30:45 -0500 COT"), lo que romperia el ORDER BY (columna TEXT) y el
// parseo en Python. dbTime fuerza el formato canonico, independiente del driver.
const tsLayout = "2006-01-02 15:04:05-07:00"

type dbTime time.Time

func (t dbTime) Value() (driver.Value, error) {
	return time.Time(t).Local().Format(tsLayout), nil
}

// Execer abstrae *sql.DB y *sql.Tx para reusar la logica de INSERT tanto en escrituras
// sueltas (store.DB()) como dentro de una transaccion (batch de history sync), sin duplicar SQL.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// StoreChatExec inserta/reemplaza un chat usando el Execer dado (DB o Tx).
func StoreChatExec(e Execer, jid, name string, lastMessageTime time.Time) error {
	_, err := e.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, dbTime(lastMessageTime),
	)
	return err
}

func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	return StoreChatExec(store.db, jid, name, lastMessageTime)
}

// TouchChat creates the chat if missing (with empty name) or just bumps its
// last_message_time, preserving the existing name. Used for outgoing messages
// where we don't resolve a display name.
func (store *MessageStore) TouchChat(jid string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		`INSERT INTO chats (jid, name, last_message_time) VALUES (?, '', ?)
		 ON CONFLICT(jid) DO UPDATE SET last_message_time=excluded.last_message_time`,
		jid, dbTime(lastMessageTime),
	)
	return err
}

// StoreMessageExec inserta/reemplaza un mensaje usando el Execer dado (DB o Tx).
func StoreMessageExec(e Execer, id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url, directPath string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := e.Exec(
		`INSERT OR REPLACE INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, direct_path, media_key, file_sha256, file_enc_sha256, file_length)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, content, dbTime(timestamp), isFromMe, mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url, directPath string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	return StoreMessageExec(store.db, id, chatJID, sender, content, timestamp, isFromMe,
		mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength)
}

// MarkMessageRevoked refleja un revoke entrante: marca el mensaje como eliminado
// in-place SIN tocar el timestamp (no reordena el historial). Devuelve filas afectadas.
func (store *MessageStore) MarkMessageRevoked(id, chatJID string) (int64, error) {
	res, err := store.db.Exec(
		"UPDATE messages SET content = ? WHERE id = ? AND chat_jid = ?",
		"🗑️ Mensaje eliminado", id, chatJID,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteChat poda localmente un chat tras un borrado app-state exitoso: en UNA sola
// transacción elimina sus mensajes, sus no-leídos y la fila del chat, de modo que
// list_chats/list_messages (que leen messages.db directamente) dejen de mostrarlo.
// Devuelve cuántos MENSAJES se borraron (0 si el chat no existía; sin error): es el
// dato útil para el caller (cuánto historial local se perdió). Solo si deleteMedia
// borra además el directorio de media descargada en <storeDir>/<safeChat>/,
// replicando la sanitización de DownloadMedia (wa.Service) para apuntar al mismo dir.
// os.RemoveAll ya trata "no existe" como éxito; un RemoveAll fallido SÍ se propaga,
// pero la DB ya quedó comiteada: la poda de filas es la parte autoritativa local.
func (store *MessageStore) DeleteChat(chatJID string, deleteMedia bool) (int64, error) {
	tx, err := store.db.Begin()
	if err != nil {
		return 0, err
	}
	// Rollback defensivo: si algo falla antes del Commit, deshace todo; tras un Commit
	// exitoso el Rollback es un no-op (devuelve ErrTxDone, que ignoramos).
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec("DELETE FROM messages WHERE chat_jid = ?", chatJID)
	if err != nil {
		return 0, err
	}
	deleted, _ := res.RowsAffected()

	if _, err := tx.Exec("DELETE FROM unread_messages WHERE chat_jid = ?", chatJID); err != nil {
		return 0, err
	}
	// El chat se borra al final: como messages tiene FK a chats(jid), borrar primero
	// los mensajes evita cualquier problema de orden con foreign_keys=on.
	if _, err := tx.Exec("DELETE FROM chats WHERE jid = ?", chatJID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}

	if deleteMedia {
		mediaDir, ok := mediaDirForChat(store.storeDir, chatJID)
		if !ok {
			// Guarda anti-traversal: un chat_jid vacío o con ".." haría que la ruta
			// resuelva al store mismo o a su directorio padre, y os.RemoveAll borraría
			// la sesión entera (whatsapp.db/messages.db/token) o incluso el $HOME en
			// modo plugin. No borramos media en ese caso; el borrado de filas ya quedó hecho.
			return deleted, fmt.Errorf("delete_chat: ruta de media fuera del store para chat_jid=%q; se omite el borrado de media", chatJID)
		}
		if err := os.RemoveAll(mediaDir); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// mediaDirForChat resuelve el directorio de media de un chat y valida que quede
// ESTRICTAMENTE por debajo de storeDir. Sanitiza el jid igual que DownloadMedia
// (`:` `/` `\` -> `_`), pero eso NO neutraliza ".." ni el string vacío; por eso además
// exige contención real (filepath.Clean + prefijo con separador). Devuelve ok=false si la
// ruta es el propio store o escapa de él, evitando un os.RemoveAll catastrófico.
func mediaDirForChat(storeDir, chatJID string) (string, bool) {
	safeChat := strings.NewReplacer(":", "_", "/", "_", "\\", "_").Replace(chatJID)
	base := filepath.Clean(storeDir)
	target := filepath.Clean(filepath.Join(base, safeChat))
	if target == base || !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", false
	}
	return target, true
}

// ApplyMessageEdit refleja un edit entrante: reemplaza el contenido por el texto
// editado (marca "(editado)") in-place SIN tocar el timestamp. Devuelve filas afectadas.
func (store *MessageStore) ApplyMessageEdit(id, chatJID, newText string) (int64, error) {
	res, err := store.db.Exec(
		"UPDATE messages SET content = ? WHERE id = ? AND chat_jid = ?",
		newText+" (editado)", id, chatJID,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- T3-3: tracking de no leídos (solo mensajes entrantes EN VIVO; el history-sync NO
// puebla esta tabla, así que el conteo empieza desde que el bridge está corriendo). ---

// AddUnread registra un mensaje entrante como no leído.
func (store *MessageStore) AddUnread(chatJID, messageID string, ts time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR IGNORE INTO unread_messages (chat_jid, message_id, timestamp) VALUES (?, ?, ?)",
		chatJID, messageID, dbTime(ts),
	)
	return err
}

// ClearChatUnread marca todo un chat como leído (al recibir read-receipt propio, al
// responder, o vía mark_read). Devuelve cuántos no-leídos se limpiaron.
func (store *MessageStore) ClearChatUnread(chatJID string) (int64, error) {
	res, err := store.db.Exec("DELETE FROM unread_messages WHERE chat_jid = ?", chatJID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UnreadChat resume un chat con mensajes sin leer. LastTime es string porque MAX(timestamp)
// pierde el tipo de columna y modernc lo devuelve como texto (no convertible a time.Time).
type UnreadChat struct {
	ChatJID     string `json:"chat_jid"`
	UnreadCount int    `json:"unread_count"`
	LastTime    string `json:"last_time"`
}

// GetUnreadChats lista los chats con no-leídos, más recientes primero.
// Excluye status@broadcast (Novedades) y newsletters: no son chats conversacionales.
func (store *MessageStore) GetUnreadChats() ([]UnreadChat, error) {
	rows, err := store.db.Query(
		`SELECT chat_jid, COUNT(*), MAX(timestamp) FROM unread_messages
		 WHERE chat_jid != 'status@broadcast' AND chat_jid NOT LIKE '%@newsletter'
		 GROUP BY chat_jid ORDER BY MAX(timestamp) DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UnreadChat
	for rows.Next() {
		var c UnreadChat
		if err := rows.Scan(&c.ChatJID, &c.UnreadCount, &c.LastTime); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetMessages returns the most recent messages from a chat.
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// GetChats returns all chats keyed by JID with their last-message time.
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// StoreMediaInfo stores additional media info for a message in the database.
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// GetMediaInfo returns the stored media info for a message from the database.
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url, directPath string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, COALESCE(direct_path, ''), media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &directPath, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// MessageSender devuelve el sender crudo y is_from_me del primer mensaje con ese id.
// Lo usa /api/star para reconstruir el target del app-state. Best-effort: si no hay
// fila, devuelve sql.ErrNoRows y el caller decide (star cae al chat propio).
func (store *MessageStore) MessageSender(id string) (senderRaw string, fromMe bool, err error) {
	err = store.db.QueryRow(
		"SELECT sender, is_from_me FROM messages WHERE id = ? LIMIT 1", id,
	).Scan(&senderRaw, &fromMe)
	return senderRaw, fromMe, err
}

// PollSender devuelve el sender crudo y is_from_me del poll (media_type='poll') con
// ese id en ese chat. Lo usa /api/poll_vote para reconstruir el MessageInfo del poll
// original; err != nil significa que el poll no fue capturado y no se puede votar.
func (store *MessageStore) PollSender(id, chatJID string) (senderRaw string, fromMe bool, err error) {
	err = store.db.QueryRow(
		"SELECT sender, is_from_me FROM messages WHERE id = ? AND chat_jid = ? AND media_type = 'poll' LIMIT 1",
		id, chatJID,
	).Scan(&senderRaw, &fromMe)
	return senderRaw, fromMe, err
}
