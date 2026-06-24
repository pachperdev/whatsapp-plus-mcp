package main

import (
	"context"
	crand "crypto/rand"
	"crypto/subtle"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mdp/qrterminal"
	_ "modernc.org/sqlite"

	"bytes"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0755); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages.
	// WAL: permite lecturas concurrentes (el server Python lee mientras el bridge escribe)
	//      sin bloquear, eliminando "database is locked" entre ambos procesos.
	// busy_timeout: una escritura reintenta hasta 5s antes de fallar por lock.
	// synchronous=NORMAL: seguro bajo WAL y mucho mas rapido que FULL.
	db, err := sql.Open("sqlite",
		"file:store/messages.db?_pragma=foreign_keys(on)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
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
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	// Migracion idempotente: agrega direct_path a DBs creadas antes de este campo.
	// Si la columna ya existe, el error ("duplicate column name") se ignora a proposito.
	// El directPath nativo del protobuf es necesario para descargar media: whatsmeow.Download
	// usa solo GetDirectPath, y reconstruirlo de la URL falla con el formato nuevo (mms3) -> 403.
	_, _ = db.Exec("ALTER TABLE messages ADD COLUMN direct_path TEXT")

	return &MessageStore{db: db}, nil
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

// execer abstrae *sql.DB y *sql.Tx para reusar la logica de INSERT tanto en escrituras
// sueltas (store.db) como dentro de una transaccion (batch de history sync), sin duplicar SQL.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// Store a chat in the database
func storeChatExec(e execer, jid, name string, lastMessageTime time.Time) error {
	_, err := e.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, dbTime(lastMessageTime),
	)
	return err
}

func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	return storeChatExec(store.db, jid, name, lastMessageTime)
}

// Touch a chat: create it if missing (with empty name) or just bump its
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

// Store a message in the database
func storeMessageExec(e execer, id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
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
	return storeMessageExec(store.db, id, chatJID, sender, content, timestamp, isFromMe,
		mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength)
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Desenrollar wrappers (consistente con extractMediaInfo) para no perder el texto adentro.
	if w := msg.GetEphemeralMessage(); w.GetMessage() != nil {
		return extractTextContent(w.GetMessage())
	}
	if w := msg.GetViewOnceMessage(); w.GetMessage() != nil {
		return extractTextContent(w.GetMessage())
	}
	if w := msg.GetDocumentWithCaptionMessage(); w.GetMessage() != nil {
		return extractTextContent(w.GetMessage())
	}

	// Texto plano / extendido (con link preview)
	if text := msg.GetConversation(); text != "" {
		return text
	}
	if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// Captions de media: el texto que acompana imagen/video/documento (para leer/analizar).
	if img := msg.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return vid.GetCaption()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetCaption()
	}

	// Encuesta: el texto legible es la pregunta.
	if poll := msg.GetPollCreationMessage(); poll != nil {
		return poll.GetName()
	}

	// Invitación a grupo (por mensaje de invitación, no link).
	if inv := msg.GetGroupInviteMessage(); inv != nil {
		name := inv.GetGroupName()
		if name == "" {
			name = inv.GetGroupJID()
		}
		return "📨 Invitación a grupo: " + name
	}

	return ""
}

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient       string   `json:"recipient"`
	Message         string   `json:"message"`
	MediaPath       string   `json:"media_path,omitempty"`
	QuotedMessageID string   `json:"quoted_message_id,omitempty"`
	Mentions        []string `json:"mentions,omitempty"`
}

// MarkReadRequest marks one or more messages as read
type MarkReadRequest struct {
	ChatJID    string   `json:"chat_jid"`
	MessageIDs []string `json:"message_ids"`
	Sender     string   `json:"sender,omitempty"`
}

// ReactRequest adds an emoji reaction to a message
type ReactRequest struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
	Sender    string `json:"sender,omitempty"`
	Emoji     string `json:"emoji"`
}

// EditRequest edits a previously sent message (own message, ~20 min window)
type EditRequest struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
	NewText   string `json:"new_text"`
}

// RevokeRequest deletes a message for everyone
type RevokeRequest struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
	Sender    string `json:"sender,omitempty"`
}

// TypingRequest sends a chat presence (typing / recording)
type TypingRequest struct {
	ChatJID string `json:"chat_jid"`
	State   string `json:"state"`           // composing | paused
	Media   string `json:"media,omitempty"` // "" (text) | audio
}

// PollRequest sends a poll message
type PollRequest struct {
	ChatJID         string   `json:"chat_jid"`
	Question        string   `json:"question"`
	Options         []string `json:"options"`
	SelectableCount int      `json:"selectable_count,omitempty"`
}

// CheckWhatsAppRequest checks if phone numbers are registered on WhatsApp
type CheckWhatsAppRequest struct {
	Phones []string `json:"phones"`
}

// ProfilePictureRequest gets a profile picture URL
type ProfilePictureRequest struct {
	JID     string `json:"jid"`
	Preview bool   `json:"preview,omitempty"`
}

// UserInfoRequest gets user info (status/about, business flag)
type UserInfoRequest struct {
	JIDs []string `json:"jids"`
}

// GroupActionRequest for participants / invite_link / leave
type GroupActionRequest struct {
	GroupJID string `json:"group_jid"`
	Reset    bool   `json:"reset,omitempty"`
}

// SetGroupNameRequest renames a group
type SetGroupNameRequest struct {
	GroupJID string `json:"group_jid"`
	Name     string `json:"name"`
}

// SetGroupTopicRequest sets a group description/topic
type SetGroupTopicRequest struct {
	GroupJID string `json:"group_jid"`
	Topic    string `json:"topic"`
}

// JoinGroupRequest joins a group via invite link/code
type JoinGroupRequest struct {
	Code string `json:"code"`
}

// BlockRequest blocks/unblocks a contact
type BlockRequest struct {
	JID    string `json:"jid"`
	Action string `json:"action"` // block | unblock
}

// ChatStateRequest toggles mute/pin/archive/read on a chat
type ChatStateRequest struct {
	ChatJID  string `json:"chat_jid"`
	Enable   bool   `json:"enable"`                   // true = mute/pin/archive/read ; false = lo contrario
	Duration int    `json:"duration_hours,omitempty"` // solo mute: 0 = indefinido
}

// StarRequest stars/unstars a message
type StarRequest struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
	Starred   bool   `json:"starred"`
}

// HistoryRequest pide mas historia (mensajes anteriores al mas viejo que tenemos) de un chat
type HistoryRequest struct {
	ChatJID string `json:"chat_jid"`
	Count   int    `json:"count"`
}

// CreateGroupRequest crea un grupo nuevo
type CreateGroupRequest struct {
	Name         string   `json:"name"`
	Participants []string `json:"participants"`
}

// UpdateParticipantsRequest agrega/quita/promueve/degrada participantes de un grupo
type UpdateParticipantsRequest struct {
	GroupJID     string   `json:"group_jid"`
	Participants []string `json:"participants"`
	Action       string   `json:"action"` // add | remove | promote | demote
}

// DisappearingRequest setea el timer de mensajes temporales de un chat
type DisappearingRequest struct {
	ChatJID  string `json:"chat_jid"`
	Duration string `json:"duration"` // off | 24h | 7d | 90d
}

// --- Lote A1: perfil & cuenta ---

// SetStatusRequest cambia el mensaje de estado ("about") propio
type SetStatusRequest struct {
	Message string `json:"message"`
}

// BusinessProfileRequest pide el perfil de negocio de un contacto
type BusinessProfileRequest struct {
	JID string `json:"jid"`
}

// UserDevicesRequest pide los dispositivos de uno o varios contactos
type UserDevicesRequest struct {
	JIDs []string `json:"jids"`
}

// DefaultDisappearingRequest setea el timer de mensajes temporales por defecto (chats nuevos)
type DefaultDisappearingRequest struct {
	Duration string `json:"duration"` // off | 24h | 7d | 90d
}

// --- Lote A2: administración de grupos ---

// SetGroupDescriptionRequest cambia la descripción de un grupo
type SetGroupDescriptionRequest struct {
	GroupJID    string `json:"group_jid"`
	Description string `json:"description"`
}

// GroupToggleRequest activa/desactiva un modo de grupo (announce/locked)
type GroupToggleRequest struct {
	GroupJID string `json:"group_jid"`
	Enable   bool   `json:"enable"`
}

// SetGroupPhotoRequest cambia la foto de un grupo (lee la imagen del path; debe ser JPEG)
type SetGroupPhotoRequest struct {
	GroupJID  string `json:"group_jid"`
	ImagePath string `json:"image_path"`
}

// PollVoteRequest vota en una encuesta existente (el poll debe estar capturado en la DB)
type PollVoteRequest struct {
	ChatJID       string   `json:"chat_jid"`
	PollMessageID string   `json:"poll_message_id"`
	Options       []string `json:"options"`
}

// InviteActionRequest opera sobre una invitación de grupo capturada (por su message_id)
type InviteActionRequest struct {
	ChatJID         string `json:"chat_jid"`
	InviteMessageID string `json:"invite_message_id"`
}

// SetPresenceRequest cambia la presencia propia (available/unavailable)
type SetPresenceRequest struct {
	State string `json:"state"` // available | unavailable
}

// presenceInfo es el último estado de presencia conocido de un contacto.
type presenceInfo struct {
	Online    bool
	LastSeen  time.Time
	Typing    bool
	UpdatedAt time.Time
}

// presenceTracker guarda en memoria la presencia de terceros: online/last-seen (events.Presence)
// y typing (events.ChatPresence). Thread-safe; lo escribe el event handler.
type presenceTracker struct {
	mu     sync.RWMutex
	states map[string]presenceInfo
}

var presences = &presenceTracker{states: make(map[string]presenceInfo)}

func (p *presenceTracker) onPresence(from string, unavailable bool, lastSeen time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.states[from]
	cur.Online = !unavailable
	if !lastSeen.IsZero() {
		cur.LastSeen = lastSeen
	}
	cur.UpdatedAt = time.Now()
	p.states[from] = cur
}

func (p *presenceTracker) onChatPresence(from string, composing bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.states[from]
	cur.Typing = composing
	cur.UpdatedAt = time.Now()
	p.states[from] = cur
}

func (p *presenceTracker) get(jid string) (presenceInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	info, ok := p.states[jid]
	return info, ok
}

// canonicalPresenceKey normaliza un JID a una clave estable para el tracker: si es @lid lo
// resuelve a su número (PN). Así la presencia (que suele llegar por @lid) se guarda y se
// consulta por la misma clave, sin importar si el caller usa el número o el lid.
func canonicalPresenceKey(client *whatsmeow.Client, jid types.JID) string {
	if jid.Server == types.HiddenUserServer {
		if pn, err := client.Store.LIDs.GetPNForLID(context.Background(), jid); err == nil && pn.User != "" {
			return pn.ToNonAD().String()
		}
	}
	return jid.ToNonAD().String()
}

// botStatus mantiene el estado de conexion/sesion/ban del cliente para /api/status
// y para pausar envios ante un ban temporal. Thread-safe (lo escribe el event handler).
type botStatus struct {
	mu                 sync.RWMutex
	lastConnected      time.Time
	lastDisconnected   time.Time
	tempBanned         bool
	tempBanCode        int
	tempBanReason      string
	tempBanExpiresAt   time.Time
	loggedOut          bool
	loggedOutReason    string
	lastConnectFailure string
	lastConnectFailAt  time.Time
}

var status = &botStatus{}

func (s *botStatus) onConnected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastConnected = time.Now()
	s.loggedOut = false // si reconecto OK, ya no necesita re-escanear QR
}

func (s *botStatus) onDisconnected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastDisconnected = time.Now()
}

func (s *botStatus) onTempBan(code int, reason string, expire time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tempBanned = true
	s.tempBanCode = code
	s.tempBanReason = reason
	if expire > 0 {
		s.tempBanExpiresAt = time.Now().Add(expire)
	}
}

func (s *botStatus) onLoggedOut(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loggedOut = true
	s.loggedOutReason = reason
}

func (s *botStatus) onConnectFailure(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastConnectFailure = msg
	s.lastConnectFailAt = time.Now()
}

// isTempBanned indica si hay un ban temporal vigente (no expirado).
func (s *botStatus) isTempBanned() (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.tempBanned && (s.tempBanExpiresAt.IsZero() || s.tempBanExpiresAt.After(time.Now())) {
		return true, s.tempBanReason
	}
	return false, ""
}

// snapshot arma el estado actual para /api/status (conexion/login en vivo desde el client).
func (s *botStatus) snapshot(client *whatsmeow.Client) map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := map[string]interface{}{
		"connected":   client.IsConnected(),
		"logged_in":   client.Store.ID != nil,
		"temp_banned": s.tempBanned && (s.tempBanExpiresAt.IsZero() || s.tempBanExpiresAt.After(time.Now())),
		"needs_qr":    client.Store.ID == nil || s.loggedOut,
	}
	if client.Store.ID != nil {
		m["jid"] = client.Store.ID.String()
	}
	if s.tempBanned {
		m["ban_code"] = s.tempBanCode
		m["ban_reason"] = s.tempBanReason
		if !s.tempBanExpiresAt.IsZero() {
			m["ban_expires_at"] = s.tempBanExpiresAt.Format(time.RFC3339)
		}
	}
	if s.loggedOut {
		m["logged_out_reason"] = s.loggedOutReason
	}
	if s.lastConnectFailure != "" {
		m["last_connect_failure"] = s.lastConnectFailure
	}
	if !s.lastConnected.IsZero() {
		m["last_connected_at"] = s.lastConnected.Format(time.RFC3339)
	}
	if !s.lastDisconnected.IsZero() {
		m["last_disconnected_at"] = s.lastDisconnected.Format(time.RFC3339)
	}
	return m
}

// lastMsgKey arma el MessageKey + timestamp del ultimo mensaje de un chat,
// requerido por BuildArchive y BuildMarkChatAsRead.
func lastMsgKey(store *MessageStore, chatJID types.JID) (*waCommon.MessageKey, time.Time) {
	var id, sender string
	var isFromMe bool
	var ts time.Time
	err := store.db.QueryRow(
		"SELECT id, sender, is_from_me, timestamp FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT 1",
		chatJID.String(),
	).Scan(&id, &sender, &isFromMe, &ts)
	if err != nil {
		return nil, time.Now()
	}
	key := &waCommon.MessageKey{
		RemoteJID: proto.String(chatJID.String()),
		FromMe:    proto.Bool(isFromMe),
		ID:        proto.String(id),
	}
	if chatJID.Server == types.GroupServer && !isFromMe && sender != "" {
		p := sender
		if !strings.Contains(p, "@") {
			p += "@lid"
		}
		key.Participant = proto.String(p)
	}
	return key, ts
}

// buildQuotedContext arma el ContextInfo para citar (reply) un mensaje previo,
// buscandolo en messages.db. En chats directos el Participant (autor del citado) es
// uno mismo si el mensaje es propio, o el chat (contacto) si fue recibido.
// Nota: en grupos el autor real puede no ser el chat_jid (limitacion conocida).
func buildQuotedContext(store *MessageStore, client *whatsmeow.Client, quotedID string) *waProto.ContextInfo {
	var chatJID, sender, content string
	var isFromMe bool
	err := store.db.QueryRow(
		"SELECT chat_jid, sender, content, is_from_me FROM messages WHERE id = ? LIMIT 1", quotedID,
	).Scan(&chatJID, &sender, &content, &isFromMe)
	if err != nil {
		return nil
	}
	var participant string
	switch {
	case isFromMe && client.Store.ID != nil:
		participant = client.Store.ID.ToNonAD().String()
	case strings.HasSuffix(chatJID, "@g.us"):
		// En grupos el autor del citado es el participante (sender), no el grupo.
		// Los participantes se referencian por LID en multidevice.
		if strings.Contains(sender, "@") {
			participant = sender
		} else {
			participant = sender + "@lid"
		}
	default:
		// Chat directo: el autor es el contacto (el propio chat).
		participant = chatJID
	}
	ctxInfo := &waProto.ContextInfo{
		StanzaID:    proto.String(quotedID),
		Participant: proto.String(participant),
	}
	if content != "" {
		ctxInfo.QuotedMessage = &waProto.Message{Conversation: proto.String(content)}
	}
	return ctxInfo
}

// Function to send a WhatsApp message
// mentionRegex detecta menciones @<numero> (7-15 digitos) en el texto del mensaje.
var mentionRegex = regexp.MustCompile(`@(\d{7,15})`)

// resolveMentions arma la lista de JIDs mencionados a partir de menciones explicitas
// (numeros o JIDs) y de auto-detectar @<numero> en el texto. Dedup conservando orden.
func resolveMentions(text string, explicit []string) []string {
	seen := map[string]bool{}
	var jids []string
	add := func(j string) {
		if j != "" && !seen[j] {
			seen[j] = true
			jids = append(jids, j)
		}
	}
	for _, m := range explicit {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if strings.Contains(m, "@") {
			add(m)
		} else {
			add(strings.TrimLeft(m, "+") + "@s.whatsapp.net")
		}
	}
	for _, match := range mentionRegex.FindAllStringSubmatch(text, -1) {
		add(match[1] + "@s.whatsapp.net")
	}
	return jids
}

// parseParticipantJIDs convierte una lista de numeros o JIDs a []types.JID
// (un numero suelto se interpreta como <numero>@s.whatsapp.net).
func parseParticipantJIDs(raw []string) ([]types.JID, error) {
	var jids []types.JID
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !strings.Contains(r, "@") {
			r = strings.TrimLeft(r, "+") + "@s.whatsapp.net"
		}
		j, err := types.ParseJID(r)
		if err != nil {
			return nil, fmt.Errorf("invalid participant %q: %w", r, err)
		}
		jids = append(jids, j)
	}
	return jids, nil
}

// loadGroupInvite recupera de la DB los datos de una invitacion de grupo capturada
// (media_type="group_invite"): group_jid/code/expiration del JSON en filename; inviter = sender.
func loadGroupInvite(store *MessageStore, chatJID, msgID string) (groupJID types.JID, inviter types.JID, code string, expiration int64, err error) {
	var filename string
	if e := store.db.QueryRow(
		"SELECT filename FROM messages WHERE id = ? AND chat_jid = ? AND media_type = 'group_invite' LIMIT 1",
		msgID, chatJID,
	).Scan(&filename); e != nil {
		return groupJID, inviter, "", 0, fmt.Errorf("invitacion de grupo no encontrada en la DB")
	}
	var data struct {
		GroupJID   string `json:"group_jid"`
		Code       string `json:"code"`
		Expiration int64  `json:"expiration"`
	}
	if e := json.Unmarshal([]byte(filename), &data); e != nil {
		return groupJID, inviter, "", 0, fmt.Errorf("datos de invitacion corruptos: %w", e)
	}
	groupJID, e := types.ParseJID(data.GroupJID)
	if e != nil {
		return groupJID, inviter, "", 0, fmt.Errorf("group_jid invalido: %w", e)
	}
	// El inviter (atributo "admin" del IQ) es quien envió la invitación. Las invitaciones de
	// grupo llegan por mensaje PRIVADO, así que el chat donde llegó ES el JID del inviter.
	// (Reconstruirlo del sender fallaba: el sender se guarda como LID sin sufijo y terminaba
	// como @s.whatsapp.net falso -> el server respondía 410 gone.)
	inviter, e = types.ParseJID(chatJID)
	if e != nil {
		return groupJID, inviter, "", 0, fmt.Errorf("chat_jid (inviter) invalido: %w", e)
	}
	return groupJID, inviter, data.Code, data.Expiration, nil
}

func sendWhatsAppMessage(client *whatsmeow.Client, messageStore *MessageStore, recipient string, message string, mediaPath string, quotedMessageID string, mentions []string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}
	// Pausar envios si hay un ban temporal vigente (evita empeorar la situacion con el server).
	if banned, reason := status.isTempBanned(); banned {
		return false, fmt.Sprintf("envio bloqueado: cuenta con ban temporal (%s). Espera a que expire; ver /api/status", reason)
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	msg := &waProto.Message{}
	// Tipo/nombre de media para persistir el saliente en la DB (ver final de la funcion)
	var dbMediaType, dbFilename string

	// Check if we have media to send
	if mediaPath != "" {
		// Sandbox: evitar exfiltracion de archivos sensibles via media_path
		if err := validateMediaPath(mediaPath); err != nil {
			return false, fmt.Sprintf("media path rejected: %v", err)
		}
		// Read media file
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err)
		}

		// Determine media type and mime type based on file extension
		fileExt := strings.ToLower(mediaPath[strings.LastIndex(mediaPath, ".")+1:])
		var mediaType whatsmeow.MediaType
		var mimeType string

		// Handle different media types
		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types (for any other file type)
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/octet-stream"
		}

		// Tipo y nombre para persistir el saliente en la DB
		dbFilename = mediaPath[strings.LastIndex(mediaPath, "/")+1:]
		switch mediaType {
		case whatsmeow.MediaImage:
			dbMediaType = "image"
		case whatsmeow.MediaAudio:
			dbMediaType = "audio"
		case whatsmeow.MediaVideo:
			dbMediaType = "video"
		default:
			dbMediaType = "document"
		}

		// Upload media to WhatsApp servers
		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		fmt.Println("Media uploaded successfully")

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Handle ogg audio files
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			// Try to analyze the ogg file
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waProto.DocumentMessage{
				Title:         proto.String(mediaPath[strings.LastIndex(mediaPath, "/")+1:]),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else {
		// Texto: puede llevar reply (ContextInfo con QuotedMessage) y/o menciones (MentionedJID).
		// Si no hay ninguno, se envia como Conversation plano.
		var ctxInfo *waProto.ContextInfo
		if quotedMessageID != "" {
			ctxInfo = buildQuotedContext(messageStore, client, quotedMessageID)
		}
		if mentionJIDs := resolveMentions(message, mentions); len(mentionJIDs) > 0 {
			if ctxInfo == nil {
				ctxInfo = &waProto.ContextInfo{}
			}
			ctxInfo.MentionedJID = mentionJIDs
		}
		if ctxInfo != nil {
			msg.ExtendedTextMessage = &waProto.ExtendedTextMessage{
				Text:        proto.String(message),
				ContextInfo: ctxInfo,
			}
		} else {
			msg.Conversation = proto.String(message)
		}
	}

	// Send message
	sendResp, err := client.SendMessage(context.Background(), recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	// Persistir el saliente: WhatsApp NO lo reenvia como events.Message, asi que sin
	// esto el historial solo reflejaria los mensajes recibidos, no los enviados.
	if messageStore != nil {
		chatJID := recipientJID.String()
		var senderUser string
		if client.Store != nil && client.Store.ID != nil {
			senderUser = client.Store.ID.User
		}
		// Asegurar que el chat existe (FK) y subirlo al tope sin pisar su nombre.
		if err := messageStore.TouchChat(chatJID, sendResp.Timestamp); err != nil {
			fmt.Printf("warn: failed to touch chat for outgoing message: %v\n", err)
		}
		if err := messageStore.StoreMessage(
			sendResp.ID, chatJID, senderUser, message, sendResp.Timestamp, true,
			dbMediaType, dbFilename, "", "", nil, nil, nil, 0,
		); err != nil {
			fmt.Printf("warn: message sent but failed to persist outgoing copy: %v\n", err)
		}
	}

	return true, fmt.Sprintf("Message sent to %s", recipient)
}

// Extract media info from a message
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, directPath string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", "", nil, nil, nil, 0
	}

	// Desenrollar wrappers que envuelven el mensaje real; sin esto se pierde la media de
	// dentro: mensajes temporales (ephemeral), ver-una-vez (view once) y documento+caption.
	if w := msg.GetEphemeralMessage(); w.GetMessage() != nil {
		return extractMediaInfo(w.GetMessage())
	}
	if w := msg.GetViewOnceMessage(); w.GetMessage() != nil {
		return extractMediaInfo(w.GetMessage())
	}
	if w := msg.GetDocumentWithCaptionMessage(); w.GetMessage() != nil {
		return extractMediaInfo(w.GetMessage())
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetDirectPath(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetDirectPath(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetDirectPath(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			doc.GetURL(), doc.GetDirectPath(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	// Check for sticker message (estaticos y animados; .webp). whatsmeow los descarga como imagen.
	if stk := msg.GetStickerMessage(); stk != nil {
		return "sticker", "sticker_" + time.Now().Format("20060102_150405") + ".webp",
			stk.GetURL(), stk.GetDirectPath(), stk.GetMediaKey(), stk.GetFileSHA256(), stk.GetFileEncSHA256(), stk.GetFileLength()
	}

	// Encuesta: NO es media descargable, pero se persiste (media_type="poll"). Las opciones
	// se guardan como JSON en el campo "filename" (no usado por polls) para poder mapear
	// despues los votos entrantes (que llegan como hashes) a sus nombres legibles.
	if poll := msg.GetPollCreationMessage(); poll != nil {
		names := make([]string, 0, len(poll.GetOptions()))
		for _, o := range poll.GetOptions() {
			names = append(names, o.GetOptionName())
		}
		b, _ := json.Marshal(names)
		return "poll", string(b), "", "", nil, nil, nil, 0
	}

	// Invitación a grupo: se persiste (media_type="group_invite") con los datos necesarios
	// para unirse (group_jid/code/expiration) como JSON en "filename"; el inviter es el sender.
	if inv := msg.GetGroupInviteMessage(); inv != nil {
		data, _ := json.Marshal(map[string]interface{}{
			"group_jid":  inv.GetGroupJID(),
			"code":       inv.GetInviteCode(),
			"expiration": inv.GetInviteExpiration(),
			"group_name": inv.GetGroupName(),
		})
		return "group_invite", string(data), "", "", nil, nil, nil, 0
	}

	return "", "", "", "", nil, nil, nil, 0
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Save message to database
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		directPath,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log based on message type
		if mediaType != "" {
			fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
		} else if content != "" {
			fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
		}
	}
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
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

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetURL implements the DownloadableMessage interface
func (d *MediaDownloader) GetURL() string {
	return d.URL
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// Function to download media from a message
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	// Query the database for the message
	var mediaType, filename, url, directPath string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	// First, check if we already have this file
	chatDir := fmt.Sprintf("store/%s", strings.ReplaceAll(chatJID, ":", "_"))
	localPath := ""

	// Get media info from the database
	mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)

	if err != nil {
		// Try to get basic info if extended info isn't available
		err = messageStore.db.QueryRow(
			"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
			messageID, chatJID,
		).Scan(&mediaType, &filename)

		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
	}

	// Check if this is a media message
	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	// Create directory for the chat if it doesn't exist
	if err := os.MkdirAll(chatDir, 0755); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Sanitizar el filename (proviene del remitente del mensaje) para evitar
	// path traversal: un filename tipo "../../x" escaparia de chatDir.
	filename = filepath.Base(filepath.Clean(filename))
	if filename == "." || filename == ".." || filename == string(os.PathSeparator) {
		filename = "file_" + messageID
	}
	// Generate a local path for the file
	localPath = fmt.Sprintf("%s/%s", chatDir, filename)

	// Get absolute path
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Check if file already exists
	if _, err := os.Stat(localPath); err == nil {
		// File exists, return it
		return true, mediaType, filename, absPath, nil
	}

	// If we don't have all the media info we need, we can't download
	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}

	fmt.Printf("Attempting to download media for message %s in chat %s...\n", messageID, chatJID)

	// Preferir el directPath NATIVO guardado (necesario para el formato nuevo mms3);
	// reconstruirlo desde la URL solo como fallback para mensajes viejos sin direct_path.
	if directPath == "" {
		directPath = extractDirectPathFromURL(url)
	}

	// Create a downloader that implements DownloadableMessage
	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	case "sticker":
		// whatsmeow clasifica StickerMessage como MediaImage (download.go).
		waMediaType = whatsmeow.MediaImage
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    directPath,
		MediaKey:      mediaKey,
		FileLength:    fileLength,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	// Download the media using whatsmeow client
	mediaData, err := client.Download(context.Background(), downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}

	// Save the downloaded media to file
	if err := os.WriteFile(localPath, mediaData, 0644); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	fmt.Printf("Successfully downloaded %s media to %s (%d bytes)\n", mediaType, absPath, len(mediaData))
	return true, mediaType, filename, absPath, nil
}

// Extract direct path from a WhatsApp media URL
func extractDirectPathFromURL(url string) string {
	// The direct path is typically in the URL, we need to extract it
	// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/13812002_698058036224062_3424455886509161511_n.enc?ccb=11-4&oh=...

	// Find the path part after the domain
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url // Return original URL if parsing fails
	}

	pathPart := parts[1]

	// Remove query parameters
	pathPart = strings.SplitN(pathPart, "?", 2)[0]

	// Create proper direct path format
	return "/" + pathPart
}

// getOrCreateBridgeToken devuelve un token compartido entre el bridge y el MCP server.
// Se persiste en store/.bridge_token (0600); el server Python lo lee del mismo archivo.
// Asi la auth es automatica (sin config manual) y protege ante otros procesos locales.
func getOrCreateBridgeToken() (string, error) {
	path := filepath.Join("store", ".bridge_token")
	if data, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := crand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate token: %v", err)
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok), 0600); err != nil {
		return "", fmt.Errorf("failed to persist token: %v", err)
	}
	return tok, nil
}

// validateMediaPath protege contra exfiltracion de archivos: resuelve symlinks,
// rechaza componentes ocultos (donde viven secretos: ~/.ssh, ~/.aws, ~/.gnupg...)
// y exige que sea un archivo regular existente.
func validateMediaPath(p string) error {
	abs, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("invalid path: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("cannot resolve path: %v", err)
	}
	for _, part := range strings.Split(resolved, string(os.PathSeparator)) {
		if len(part) > 1 && strings.HasPrefix(part, ".") {
			return fmt.Errorf("hidden path component %q not allowed", part)
		}
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("cannot stat file: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	return nil
}

// blockViaLID actualiza la blocklist replicando el formato NUEVO del protocolo de
// WhatsApp (item por LID + pn_jid + dhash). El UpdateBlocklist de whatsmeow aun
// envia el formato viejo (solo jid+action) y el server responde 400. Como sendIQ es
// privado, enviamos el IQ con DangerousInternals().SendNode (no espera respuesta) y
// verificamos el resultado con GetBlocklist. Ref: whatsmeow PR #1137.
func blockViaLID(client *whatsmeow.Client, jid types.JID, action events.BlocklistChangeAction) (bool, error) {
	ctx := context.Background()
	var lidJID, pnJID types.JID
	switch jid.Server {
	case types.DefaultUserServer: // @s.whatsapp.net
		pnJID = jid
		lid, err := client.Store.LIDs.GetLIDForPN(ctx, jid)
		if err != nil || lid.IsEmpty() {
			info, ierr := client.GetUserInfo(ctx, []types.JID{jid})
			if ierr != nil {
				return false, fmt.Errorf("could not resolve LID: %v", ierr)
			}
			lid = info[jid].LID
		}
		if lid.IsEmpty() {
			return false, fmt.Errorf("no LID found for %s", jid)
		}
		lidJID = lid
	case types.HiddenUserServer: // @lid
		lidJID = jid
		if pn, err := client.Store.LIDs.GetPNForLID(ctx, jid); err == nil {
			pnJID = pn
		}
	default:
		return false, fmt.Errorf("unsupported jid server: %s", jid.Server)
	}

	attrs := waBinary.Attrs{
		"jid":    lidJID,
		"action": string(action),
		"dhash":  strconv.FormatInt(time.Now().UnixMilli(), 10),
	}
	if action == events.BlocklistChangeActionBlock && !pnJID.IsEmpty() {
		attrs["pn_jid"] = pnJID
	}
	node := waBinary.Node{
		Tag: "iq",
		Attrs: waBinary.Attrs{
			"id":    client.GenerateMessageID(),
			"xmlns": "blocklist",
			"type":  "set",
			"to":    types.ServerJID,
		},
		Content: []waBinary.Node{{Tag: "item", Attrs: attrs}},
	}
	if err := client.DangerousInternals().SendNode(ctx, node); err != nil {
		return false, fmt.Errorf("send blocklist iq: %v", err)
	}

	// SendNode no espera la respuesta del IQ -> verificamos con GetBlocklist.
	time.Sleep(900 * time.Millisecond)
	bl, err := client.GetBlocklist(ctx)
	if err != nil {
		return false, fmt.Errorf("verify via GetBlocklist: %v", err)
	}
	inList := false
	for _, b := range bl.JIDs {
		if (lidJID.User != "" && b.User == lidJID.User) || (pnJID.User != "" && b.User == pnJID.User) {
			inList = true
			break
		}
	}
	if action == events.BlocklistChangeActionBlock {
		return inList, nil
	}
	return !inList, nil // unblock: exito si ya NO esta en la lista
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, port int) {
	token, tokErr := getOrCreateBridgeToken()
	if tokErr != nil {
		fmt.Printf("WARNING: could not set up auth token: %v\n", tokErr)
	}
	// Middleware: exige el token compartido (X-Auth-Token) en cada request.
	withAuth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-Auth-Token")
			if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}

	// Handler for sending messages
	http.HandleFunc("/api/send", withAuth(func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}

		fmt.Println("Received request to send message", req.Message, req.MediaPath)

		// Send the message
		success, message := sendWhatsAppMessage(client, messageStore, req.Recipient, req.Message, req.MediaPath, req.QuotedMessageID, req.Mentions)
		fmt.Println("Message sent", success, message)
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		json.NewEncoder(w).Encode(SendMessageResponse{
			Success: success,
			Message: message,
		})
	}))

	// Handler for downloading media
	http.HandleFunc("/api/download", withAuth(func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Download the media
		success, mediaType, filename, path, err := downloadMedia(client, messageStore, req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	}))

	// Handler: list joined groups
	http.HandleFunc("/api/groups", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		groups, err := client.GetJoinedGroups(context.Background())
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		type groupOut struct {
			JID              string `json:"jid"`
			Name             string `json:"name"`
			ParticipantCount int    `json:"participant_count"`
			Owner            string `json:"owner,omitempty"`
		}
		out := make([]groupOut, 0, len(groups))
		for _, g := range groups {
			// GetJoinedGroups no siempre popula ParticipantCount; usar len(Participants) de fallback.
			pc := g.ParticipantCount
			if pc == 0 {
				pc = len(g.Participants)
			}
			out = append(out, groupOut{
				JID:              g.JID.String(),
				Name:             g.Name,
				ParticipantCount: pc,
				Owner:            g.OwnerJID.String(),
			})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "groups": out})
	}))

	// Handler: mark messages as read
	http.HandleFunc("/api/mark_read", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req MarkReadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		sender := chat // chats directos: el remitente es el propio chat
		if req.Sender != "" {
			if s, perr := types.ParseJID(req.Sender); perr == nil {
				sender = s
			}
		}
		ids := make([]types.MessageID, 0, len(req.MessageIDs))
		for _, id := range req.MessageIDs {
			ids = append(ids, types.MessageID(id))
		}
		if err := client.MarkRead(context.Background(), ids, time.Now(), chat, sender); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "marked as read"})
	}))

	// Handler: react to a message with an emoji
	http.HandleFunc("/api/react", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ReactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		sender := chat
		if req.Sender != "" {
			if s, perr := types.ParseJID(req.Sender); perr == nil {
				sender = s
			}
		}
		reaction := client.BuildReaction(chat, sender, types.MessageID(req.MessageID), req.Emoji)
		if _, err := client.SendMessage(context.Background(), chat, reaction); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "reaction sent"})
	}))

	// Handler: edit a previously sent message
	http.HandleFunc("/api/edit", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req EditRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		newContent := &waProto.Message{Conversation: proto.String(req.NewText)}
		edit := client.BuildEdit(chat, types.MessageID(req.MessageID), newContent)
		if _, err := client.SendMessage(context.Background(), chat, edit); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "message edited"})
	}))

	// Handler: revoke (delete for everyone) a message
	http.HandleFunc("/api/revoke", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req RevokeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		var sender types.JID // vacio = revocar mensaje propio
		if req.Sender != "" {
			if s, perr := types.ParseJID(req.Sender); perr == nil {
				sender = s
			}
		}
		revoke := client.BuildRevoke(chat, sender, types.MessageID(req.MessageID))
		if _, err := client.SendMessage(context.Background(), chat, revoke); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "message revoked"})
	}))

	// Handler: send chat presence (typing / recording)
	http.HandleFunc("/api/typing", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req TypingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		state := types.ChatPresenceComposing
		if req.State == "paused" {
			state = types.ChatPresencePaused
		}
		media := types.ChatPresenceMediaText
		if req.Media == "audio" {
			media = types.ChatPresenceMediaAudio
		}
		if err := client.SendChatPresence(context.Background(), chat, state, media); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "presence sent"})
	}))

	// Handler: send a poll
	http.HandleFunc("/api/poll", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req PollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		if req.Question == "" || len(req.Options) < 2 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "poll needs a question and at least 2 options"})
			return
		}
		selectable := req.SelectableCount
		if selectable < 1 {
			selectable = 1
		}
		poll := client.BuildPollCreation(req.Question, req.Options, selectable)
		resp, err := client.SendMessage(context.Background(), chat, poll)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		// Persistir el poll saliente (media_type="poll", opciones en filename JSON) para poder
		// votarlo con vote_poll y mapear los votos entrantes a nombres legibles.
		if messageStore != nil {
			optsB, _ := json.Marshal(req.Options)
			var senderUser string
			if client.Store != nil && client.Store.ID != nil {
				senderUser = client.Store.ID.User
			}
			_ = messageStore.TouchChat(chat.String(), resp.Timestamp)
			_ = messageStore.StoreMessage(resp.ID, chat.String(), senderUser, req.Question, resp.Timestamp, true,
				"poll", string(optsB), "", "", nil, nil, nil, 0)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "poll sent", "message_id": resp.ID})
	}))

	// Handler: check if phone numbers are on WhatsApp
	http.HandleFunc("/api/check_whatsapp", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req CheckWhatsAppRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		resp, err := client.IsOnWhatsApp(context.Background(), req.Phones)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		normPhone := func(s string) string {
			var b strings.Builder
			for _, r := range s {
				if r >= '0' && r <= '9' {
					b.WriteRune(r)
				}
			}
			return b.String()
		}
		out := make([]map[string]interface{}, 0, len(req.Phones))
		seen := make(map[string]bool, len(resp))
		for _, item := range resp {
			seen[normPhone(item.Query)] = true
			out = append(out, map[string]interface{}{
				"query":          item.Query,
				"jid":            item.JID.String(),
				"is_on_whatsapp": item.IsIn,
				"is_business":    item.VerifiedName != nil,
			})
		}
		// IsOnWhatsApp omite los numeros con formato invalido. Reportarlos explicitamente
		// para que el caller sepa el resultado de CADA numero que envio (no desaparecen).
		for _, p := range req.Phones {
			if !seen[normPhone(p)] {
				out = append(out, map[string]interface{}{
					"query":          p,
					"jid":            "",
					"is_on_whatsapp": false,
					"is_business":    false,
					"error":          "invalid or unverifiable number",
				})
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "results": out})
	}))

	// Handler: get a profile picture URL
	http.HandleFunc("/api/profile_picture", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ProfilePictureRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.JID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid jid"})
			return
		}
		info, err := client.GetProfilePictureInfo(context.Background(), jid, &whatsmeow.GetProfilePictureParams{Preview: req.Preview})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if info == nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "has_picture": false})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "has_picture": true,
			"url": info.URL, "id": info.ID, "type": info.Type,
		})
	}))

	// Handler: get user info (status/about, business flag)
	http.HandleFunc("/api/user_info", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req UserInfoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jids := make([]types.JID, 0, len(req.JIDs))
		for _, j := range req.JIDs {
			if jid, perr := types.ParseJID(j); perr == nil {
				jids = append(jids, jid)
			}
		}
		info, err := client.GetUserInfo(context.Background(), jids)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		out := make(map[string]interface{})
		for jid, ui := range info {
			out[jid.String()] = map[string]interface{}{
				"status":      ui.Status,
				"picture_id":  ui.PictureID,
				"is_business": ui.VerifiedName != nil,
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "users": out})
	}))

	// Handler: get group participants
	http.HandleFunc("/api/group_participants", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		info, err := client.GetGroupInfo(context.Background(), jid)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		parts := make([]map[string]interface{}, 0, len(info.Participants))
		for _, p := range info.Participants {
			parts = append(parts, map[string]interface{}{
				"jid":            p.JID.String(),
				"phone":          p.PhoneNumber.String(),
				"is_admin":       p.IsAdmin,
				"is_super_admin": p.IsSuperAdmin,
			})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "name": info.Name, "participant_count": len(parts), "participants": parts,
		})
	}))

	// Handler: get / reset group invite link
	http.HandleFunc("/api/group_invite_link", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		link, err := client.GetGroupInviteLink(context.Background(), jid, req.Reset)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "link": link})
	}))

	// Handler: join a group via invite link/code
	http.HandleFunc("/api/join_group", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req JoinGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		code := req.Code
		if idx := strings.LastIndex(code, "/"); idx >= 0 {
			code = code[idx+1:] // aceptar link completo o solo el codigo
		}
		jid, err := client.JoinGroupWithLink(context.Background(), code)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "group_jid": jid.String()})
	}))

	// Handler: leave a group
	http.HandleFunc("/api/leave_group", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.LeaveGroup(context.Background(), jid); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "left group"})
	}))

	// Handler: set group name
	http.HandleFunc("/api/set_group_name", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupNameRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.SetGroupName(context.Background(), jid, req.Name); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "group name updated"})
	}))

	// Handler: set group topic/description
	http.HandleFunc("/api/set_group_topic", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupTopicRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.SetGroupTopic(context.Background(), jid, "", "", req.Topic); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "group topic updated"})
	}))

	// Handler: block / unblock a contact
	http.HandleFunc("/api/block", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BlockRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.JID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid jid"})
			return
		}
		action := events.BlocklistChangeActionBlock
		if req.Action == "unblock" {
			action = events.BlocklistChangeActionUnblock
		}
		ok, err := blockViaLID(client, jid, action)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "blocklist update not reflected (verified via GetBlocklist)"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": string(action) + "ed"})
	}))

	// Handler: mute / unmute chat
	http.HandleFunc("/api/mute", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		if err := client.SendAppState(context.Background(), appstate.BuildMute(jid, req.Enable, time.Duration(req.Duration)*time.Hour)); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "mute updated"})
	}))

	// Handler: pin / unpin chat
	http.HandleFunc("/api/pin", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		if err := client.SendAppState(context.Background(), appstate.BuildPin(jid, req.Enable)); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "pin updated"})
	}))

	// Handler: archive / unarchive chat
	http.HandleFunc("/api/archive", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		key, ts := lastMsgKey(messageStore, jid)
		if err := client.SendAppState(context.Background(), appstate.BuildArchive(jid, req.Enable, ts, key)); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "archive updated"})
	}))

	// Handler: mark chat read / unread
	http.HandleFunc("/api/mark_chat", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		key, ts := lastMsgKey(messageStore, jid)
		if err := client.SendAppState(context.Background(), appstate.BuildMarkChatAsRead(jid, req.Enable, ts, key)); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "chat read-state updated"})
	}))

	// Handler: star / unstar a message
	http.HandleFunc("/api/star", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req StarRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		var senderRaw string
		var fromMe bool
		_ = messageStore.db.QueryRow("SELECT sender, is_from_me FROM messages WHERE id = ? LIMIT 1", req.MessageID).Scan(&senderRaw, &fromMe)
		// BuildStar mapea sender==target -> "0" en el index, que es lo que WhatsApp
		// espera en chats directos y para mensajes propios. Por eso el default es el
		// propio chat (jid). Solo en grupos con mensaje de OTRO se usa el participante real.
		senderJID := jid
		if !fromMe && jid.Server == types.GroupServer && senderRaw != "" {
			if strings.Contains(senderRaw, "@") {
				if s, perr := types.ParseJID(senderRaw); perr == nil {
					senderJID = s
				}
			} else {
				senderJID = types.NewJID(senderRaw, types.HiddenUserServer)
			}
		}
		if err := client.SendAppState(context.Background(), appstate.BuildStar(jid, senderJID, types.MessageID(req.MessageID), fromMe, req.Starred)); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "star updated"})
	}))

	// Handler: get chat settings (muted/pinned/archived)
	http.HandleFunc("/api/chat_settings", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		s, err := client.Store.ChatSettings.GetChatSettings(context.Background(), jid)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		muted := false
		mutedUntil := ""
		if !s.MutedUntil.IsZero() {
			muted = s.MutedUntil.After(time.Now())
			mutedUntil = s.MutedUntil.Format(time.RFC3339)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "found": s.Found,
			"muted": muted, "muted_until": mutedUntil,
			"pinned": s.Pinned, "archived": s.Archived,
		})
	}))

	// Handler: request more history for a chat (on-demand history sync)
	http.HandleFunc("/api/request_history", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req HistoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		count := req.Count
		if count <= 0 {
			count = 50
		}
		if err := requestMoreHistory(client, messageStore, jid, count); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "history requested (best-effort); si el telefono primario esta online y conserva mensajes anteriores, llegan async via history sync y quedan en la DB"})
	}))

	// Handler: create group
	http.HandleFunc("/api/create_group", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req CreateGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "group name required"})
			return
		}
		if len([]rune(name)) > 25 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "group name max 25 chars"})
			return
		}
		parts, err := parseParticipantJIDs(req.Participants)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if len(parts) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "at least one participant required"})
			return
		}
		info, err := client.CreateGroup(context.Background(), whatsmeow.ReqCreateGroup{Name: name, Participants: parts})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "group_jid": info.JID.String(),
			"name": info.Name, "participant_count": len(info.Participants),
		})
	}))

	// Handler: update group participants (add/remove/promote/demote)
	http.HandleFunc("/api/update_participants", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req UpdateParticipantsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		gjid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		var action whatsmeow.ParticipantChange
		switch req.Action {
		case "add":
			action = whatsmeow.ParticipantChangeAdd
		case "remove":
			action = whatsmeow.ParticipantChangeRemove
		case "promote":
			action = whatsmeow.ParticipantChangePromote
		case "demote":
			action = whatsmeow.ParticipantChangeDemote
		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "action must be add/remove/promote/demote"})
			return
		}
		parts, err := parseParticipantJIDs(req.Participants)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if len(parts) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "at least one participant required"})
			return
		}
		result, err := client.UpdateGroupParticipants(context.Background(), gjid, parts, action)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		results := make([]map[string]interface{}, 0, len(result))
		for _, p := range result {
			results = append(results, map[string]interface{}{
				"jid": p.JID.String(), "error_code": p.Error,
				"is_admin": p.IsAdmin,
			})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "action": req.Action, "results": results})
	}))

	// Handler: set disappearing-messages timer (off/24h/7d/90d)
	http.HandleFunc("/api/disappearing", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req DisappearingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		timer, ok := whatsmeow.ParseDisappearingTimerString(req.Duration)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "duration must be one of: off, 24h, 7d, 90d"})
			return
		}
		if err := client.SetDisappearingTimer(context.Background(), jid, timer, time.Time{}); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "disappearing timer set", "duration": req.Duration})
	}))

	// Handler: estado de conexion/sesion/ban del cliente
	http.HandleFunc("/api/status", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		m := status.snapshot(client)
		m["success"] = true
		json.NewEncoder(w).Encode(m)
	}))

	// --- Lote A1: perfil & cuenta ---

	// Handler: set status message ("about" propio)
	http.HandleFunc("/api/set_status", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		if err := client.SetStatusMessage(context.Background(), req.Message); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "status updated"})
	}))

	// Handler: get business profile de un contacto
	http.HandleFunc("/api/business_profile", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BusinessProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.JID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid jid"})
			return
		}
		bp, err := client.GetBusinessProfile(context.Background(), jid)
		if err != nil {
			// Contacto NO business: whatsmeow devuelve "missing jid"/not-found. No es error real.
			if strings.Contains(err.Error(), "missing jid") || strings.Contains(err.Error(), "not found") {
				json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "is_business": false})
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if bp == nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "is_business": false})
			return
		}
		cats := make([]map[string]string, 0, len(bp.Categories))
		for _, c := range bp.Categories {
			cats = append(cats, map[string]string{"id": c.ID, "name": c.Name})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "is_business": true, "jid": bp.JID.String(),
			"address": bp.Address, "email": bp.Email, "categories": cats,
			"business_hours_timezone": bp.BusinessHoursTimeZone,
		})
	}))

	// Handler: get user devices (dispositivos vinculados de un contacto)
	http.HandleFunc("/api/user_devices", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req UserDevicesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jids, err := parseParticipantJIDs(req.JIDs)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		devices, err := client.GetUserDevices(context.Background(), jids)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		out := make([]string, 0, len(devices))
		for _, d := range devices {
			out = append(out, d.String())
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "devices": out, "count": len(out)})
	}))

	// Handler: set default disappearing timer (aplica a chats NUEVOS)
	http.HandleFunc("/api/default_disappearing", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req DefaultDisappearingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		timer, ok := whatsmeow.ParseDisappearingTimerString(req.Duration)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "duration must be one of: off, 24h, 7d, 90d"})
			return
		}
		if err := client.SetDefaultDisappearingTimer(context.Background(), timer); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "default disappearing timer set", "duration": req.Duration})
	}))

	// --- Lote A2: administración de grupos (requieren ser admin) ---

	// Handler: set group description
	http.HandleFunc("/api/set_group_description", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupDescriptionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.SetGroupDescription(context.Background(), jid, req.Description); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "group description updated"})
	}))

	// Handler: set group announce (true = solo admins pueden enviar mensajes)
	http.HandleFunc("/api/set_group_announce", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupToggleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.SetGroupAnnounce(context.Background(), jid, req.Enable); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "group announce updated"})
	}))

	// Handler: set group locked (true = solo admins pueden editar info del grupo)
	http.HandleFunc("/api/set_group_locked", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupToggleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.SetGroupLocked(context.Background(), jid, req.Enable); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "group locked updated"})
	}))

	// Handler: set group photo (lee la imagen del path; WhatsApp requiere JPEG)
	http.HandleFunc("/api/set_group_photo", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupPhotoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		avatar, err := os.ReadFile(req.ImagePath)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": fmt.Sprintf("cannot read image: %v", err)})
			return
		}
		pictureID, err := client.SetGroupPhoto(context.Background(), jid, avatar)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "group photo updated", "picture_id": pictureID})
	}))

	// --- Lote B2: presencia ---

	// Handler: set own presence (available/unavailable). available es requisito para RECIBIR
	// la presencia de otros.
	http.HandleFunc("/api/set_presence", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetPresenceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		var p types.Presence
		switch req.State {
		case "available":
			p = types.PresenceAvailable
		case "unavailable":
			p = types.PresenceUnavailable
		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "state must be available/unavailable"})
			return
		}
		if err := client.SendPresence(context.Background(), p); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "presence sent", "state": req.State})
	}))

	// Handler: subscribe to a contact's presence (necesario para recibir su online/last-seen)
	http.HandleFunc("/api/subscribe_presence", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BusinessProfileRequest // {jid}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.JID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid jid"})
			return
		}
		if err := client.SubscribePresence(context.Background(), jid); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "subscribed to presence"})
	}))

	// Handler: get last known presence of a contact (del tracker en memoria)
	http.HandleFunc("/api/get_presence", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BusinessProfileRequest // {jid}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.JID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid jid"})
			return
		}
		info, ok := presences.get(canonicalPresenceKey(client, jid))
		if !ok {
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "tracked": false, "message": "sin datos de presencia aún (subscribe_presence + esperar a que cambie de estado)"})
			return
		}
		out := map[string]interface{}{
			"success": true, "tracked": true,
			"online": info.Online, "typing": info.Typing,
			"updated_at": info.UpdatedAt.Format(time.RFC3339),
		}
		if !info.LastSeen.IsZero() {
			out["last_seen"] = info.LastSeen.Format(time.RFC3339)
		}
		json.NewEncoder(w).Encode(out)
	}))

	// --- Lote B1: unirse por código de invitación ---

	// Handler: get group info from invite (inspeccionar sin unirse)
	http.HandleFunc("/api/group_info_from_invite", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req InviteActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		gjid, inviter, code, exp, err := loadGroupInvite(messageStore, req.ChatJID, req.InviteMessageID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		info, err := client.GetGroupInfoFromInvite(context.Background(), gjid, inviter, code, exp)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "group_jid": info.JID.String(),
			"name": info.Name, "participant_count": len(info.Participants),
		})
	}))

	// Handler: join group with invite (unirse por código de invitación)
	http.HandleFunc("/api/join_group_with_invite", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req InviteActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		gjid, inviter, code, exp, err := loadGroupInvite(messageStore, req.ChatJID, req.InviteMessageID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if err := client.JoinGroupWithInvite(context.Background(), gjid, inviter, code, exp); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "joined group via invite", "group_jid": gjid.String()})
	}))

	// --- Lote A3: solicitudes de ingreso a grupos (requieren admin) ---

	// Handler: set group join approval mode (true = los ingresos requieren aprobación de admin)
	http.HandleFunc("/api/set_group_join_approval", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupToggleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.SetGroupJoinApprovalMode(context.Background(), jid, req.Enable); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "join approval mode updated"})
	}))

	// Handler: get group join requests (solicitudes pendientes de ingreso)
	http.HandleFunc("/api/group_join_requests", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		reqs, err := client.GetGroupRequestParticipants(context.Background(), jid)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		out := make([]map[string]interface{}, 0, len(reqs))
		for _, p := range reqs {
			out = append(out, map[string]interface{}{"jid": p.JID.String(), "requested_at": p.RequestedAt.Format(time.RFC3339)})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "requests": out, "count": len(out)})
	}))

	// Handler: review group join request (approve/reject)
	http.HandleFunc("/api/review_group_join_request", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req UpdateParticipantsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		var action whatsmeow.ParticipantRequestChange
		switch req.Action {
		case "approve":
			action = whatsmeow.ParticipantChangeApprove
		case "reject":
			action = whatsmeow.ParticipantChangeReject
		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "action must be approve/reject"})
			return
		}
		parts, err := parseParticipantJIDs(req.Participants)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if len(parts) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "at least one participant required"})
			return
		}
		result, err := client.UpdateGroupRequestParticipants(context.Background(), jid, parts, action)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		results := make([]map[string]interface{}, 0, len(result))
		for _, p := range result {
			results = append(results, map[string]interface{}{"jid": p.JID.String(), "error_code": p.Error})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "action": req.Action, "results": results})
	}))

	// --- Lote A4: votar en encuestas ---

	// Handler: vote in a poll
	http.HandleFunc("/api/poll_vote", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req PollVoteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		if len(req.Options) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "at least one option required"})
			return
		}
		// Reconstruir el MessageInfo del poll original desde la DB (debe haber sido capturado).
		var senderRaw string
		var fromMe bool
		err = messageStore.db.QueryRow(
			"SELECT sender, is_from_me FROM messages WHERE id = ? AND chat_jid = ? AND media_type = 'poll' LIMIT 1",
			req.PollMessageID, req.ChatJID,
		).Scan(&senderRaw, &fromMe)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "poll not found in DB (no fue capturado); no se puede votar"})
			return
		}
		senderJID := jid
		if fromMe && client.Store.ID != nil {
			senderJID = client.Store.ID.ToNonAD()
		} else if senderRaw != "" {
			if strings.Contains(senderRaw, "@") {
				if s, perr := types.ParseJID(senderRaw); perr == nil {
					senderJID = s
				}
			} else if jid.Server == types.GroupServer {
				senderJID = types.NewJID(senderRaw, types.HiddenUserServer)
			}
		}
		pollInfo := &types.MessageInfo{
			MessageSource: types.MessageSource{Chat: jid, Sender: senderJID, IsFromMe: fromMe},
			ID:            types.MessageID(req.PollMessageID),
		}
		voteMsg, err := client.BuildPollVote(context.Background(), pollInfo, req.Options)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if _, err := client.SendMessage(context.Background(), jid, voteMsg); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "poll vote sent", "options": req.Options})
	}))

	// --- Logout: desvincula la sesión (requiere re-escanear QR para volver) ---
	http.HandleFunc("/api/logout", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := client.Logout(context.Background()); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		status.onLoggedOut("logout solicitado por el usuario")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "logged out; reiniciar el bridge y re-escanear el QR para volver a vincular"})
	}))

	// Bind SOLO a loopback (no exponer a la LAN) + timeouts (anti cliente lento/DoS).
	serverAddr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{
		Addr:         serverAddr,
		Handler:      nil, // usa el DefaultServeMux donde registramos los handlers
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
}

func main() {
	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0755); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	container, err := sqlstore.New(context.Background(), "sqlite", "file:store/whatsapp.db?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Create client instance with extended timeout settings
	clientLog := waLog.Stdout("Client/Socket", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Enable automatic reconnection
	client.EnableAutoReconnect = true
	client.EmitAppStateEventsOnFullSync = false

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Voto de encuesta entrante: descifrar y registrar (goroutine; no bloquea el dispatch).
			if v.Message.GetPollUpdateMessage() != nil {
				go handlePollVote(client, messageStore, v, logger)
			}
			// Process regular messages
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			// Procesar en goroutine para NO bloquear el dispatch de eventos en vivo.
			// El history-sync puede tardar minutos (cientos de mensajes + lookups de red);
			// si corre sincronico en el handler, los *events.Message en vivo quedan
			// encolados detras y no se guardan hasta que termina (bug observado).
			go handleHistorySync(client, messageStore, v, logger)

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")
			status.onConnected()

		case *events.Disconnected:
			// EnableAutoReconnect (mas arriba) ya gestiona la reconexion. Un reconnect
			// manual aqui competiria con el interno de whatsmeow (race / StreamReplaced).
			logger.Warnf("Disconnected from WhatsApp; auto-reconnect en curso...")
			status.onDisconnected()

		case *events.LoggedOut:
			logger.Warnf("Device logged out (reason=%s, onConnect=%v); please scan QR code to log in again", v.Reason.String(), v.OnConnect)
			status.onLoggedOut(v.Reason.String())

		case *events.TemporaryBan:
			// 🔴 Ban temporal: loggear fuerte y pausar envios (sendWhatsAppMessage chequea isTempBanned).
			logger.Errorf("⚠️ TEMPORARY BAN: code=%d (%s); expira en %s. ENVIOS PAUSADOS.", int(v.Code), v.Code.String(), v.Expire)
			status.onTempBan(int(v.Code), v.Code.String(), v.Expire)

		case *events.ConnectFailure:
			logger.Errorf("Connect failure: reason=%d (%s) msg=%s", int(v.Reason), v.Reason.String(), v.Message)
			status.onConnectFailure(fmt.Sprintf("%d %s: %s", int(v.Reason), v.Reason.String(), v.Message))
			if v.Reason.IsLoggedOut() {
				status.onLoggedOut(v.Reason.String())
			}

		case *events.Presence:
			// Presencia de terceros: online/offline + last-seen (requiere SubscribePresence + SendPresence).
			presences.onPresence(canonicalPresenceKey(client, v.From), v.Unavailable, v.LastSeen)

		case *events.ChatPresence:
			// Typing de terceros (composing/paused) en un chat.
			presences.onChatPresence(canonicalPresenceKey(client, v.MessageSource.Sender), v.State == types.ChatPresenceComposing)

		case *events.CallOffer:
			// Llamada entrante: solo se registra (whatsmeow no maneja audio). Sin auto-rechazo.
			go handleCallOffer(client, messageStore, v, logger)
		}
	})

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Print QR code for pairing with phone
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				// Code crudo para poder generar un PNG nitido fuera de la terminal (el ASCII
				// half-block renderiza muy lento en algunos clientes y el QR expira antes).
				fmt.Printf("QR_RAW>>>%s<<<\n", evt.Code)
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp! Type 'help' for commands.")

	// Start REST API server
	startRESTServer(client, messageStore, 8080)

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// Disconnect client
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" {
		// Chat exists with a name, use that
		logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		logger.Infof("Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Infof("Using group name: %s", name)
	} else {
		// This is an individual contact
		logger.Infof("Getting name for contact: %s", chatJID)

		// Just use contact info (full name)
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			// Fallback to sender
			name = sender
		} else {
			// Last fallback to JID
			name = jid.User
		}

		logger.Infof("Using contact name: %s", name)
	}

	return name
}

// handlePollVote descifra un voto de encuesta entrante y lo registra. Los votos llegan como
// hashes SHA256 de las opciones; se mapean a los nombres legibles usando las opciones del poll
// original (guardadas en la DB con el poll, en el campo filename como JSON). Se persiste el voto
// como un mensaje "poll_vote" para que sea consultable via list_messages.
func handlePollVote(client *whatsmeow.Client, store *MessageStore, evt *events.Message, logger waLog.Logger) {
	vote, err := client.DecryptPollVote(context.Background(), evt)
	if err != nil {
		logger.Warnf("poll vote: no se pudo descifrar: %v", err)
		return
	}
	pollID := evt.Message.GetPollUpdateMessage().GetPollCreationMessageKey().GetID()
	var optsJSON string
	_ = store.db.QueryRow("SELECT filename FROM messages WHERE id = ? AND media_type = 'poll' LIMIT 1", pollID).Scan(&optsJSON)
	var pollOptions []string
	_ = json.Unmarshal([]byte(optsJSON), &pollOptions)

	selected := vote.GetSelectedOptions()
	var voted []string
	for _, name := range pollOptions {
		h := whatsmeow.HashPollOptions([]string{name})
		if len(h) == 0 {
			continue
		}
		for _, sel := range selected {
			if bytes.Equal(h[0], sel) {
				voted = append(voted, name)
			}
		}
	}
	label := strings.Join(voted, ", ")
	if label == "" {
		label = fmt.Sprintf("(%d opcion(es); poll original no capturado, no se pudo mapear)", len(selected))
	}
	logger.Infof("🗳️ Voto de %s en poll %s: %s", evt.Info.Sender.String(), pollID, label)

	// Persistir para que sea consultable via list_messages (TouchChat asegura el FK del chat).
	if store != nil {
		_ = store.TouchChat(evt.Info.Chat.String(), evt.Info.Timestamp)
		if err := store.StoreMessage(evt.Info.ID, evt.Info.Chat.String(), evt.Info.Sender.User,
			"🗳️ votó: "+label, evt.Info.Timestamp, evt.Info.IsFromMe,
			"poll_vote", "", "", "", nil, nil, nil, 0); err != nil {
			logger.Warnf("poll vote: no se pudo persistir: %v", err)
		}
	}
}

// handleCallOffer registra una llamada entrante como un mensaje "call" (whatsmeow no puede
// atender llamadas; solo las detecta). Queda consultable via list_messages. No se rechaza.
func handleCallOffer(client *whatsmeow.Client, store *MessageStore, evt *events.CallOffer, logger waLog.Logger) {
	caller := evt.CallCreator
	if caller.User == "" {
		caller = evt.From
	}
	logger.Infof("📞 Llamada entrante de %s (call %s)", caller.String(), evt.CallID)
	if store == nil {
		return
	}
	chatJID := caller.String()
	if !evt.GroupJID.IsEmpty() {
		chatJID = evt.GroupJID.String() // llamada grupal
	}
	_ = store.TouchChat(chatJID, evt.Timestamp)
	if err := store.StoreMessage("CALL_"+evt.CallID, chatJID, caller.User, "📞 Llamada entrante",
		evt.Timestamp, false, "call", "", "", "", nil, nil, nil, 0); err != nil {
		logger.Warnf("call offer: no se pudo registrar: %v", err)
	}
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			// Batch: una transaccion por conversacion -> 1 fsync en vez de N (WAL +
			// synchronous=NORMAL). Se abre DESPUES de GetChatName (la unica lectura de esta
			// iteracion): con SetMaxOpenConns(1) una tx abierta toma la unica conexion y
			// bloquearia cualquier lectura via store.db (deadlock).
			tx, err := messageStore.db.Begin()
			if err != nil {
				logger.Warnf("history sync: no se pudo iniciar tx para %s: %v", chatJID, err)
				continue
			}
			if err := storeChatExec(tx, chatJID, name, timestamp); err != nil {
				logger.Warnf("history sync: storeChat fallo (%s): %v", chatJID, err)
			}

			// Store messages (todos sobre la misma tx)
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// Extract media info
				var mediaType, filename, url, directPath string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				// Log the message content for debugging
				logger.Infof("Message content: %v, Media Type: %v", content, mediaType)

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						sender = *msg.Message.Key.Participant
					} else if isFromMe {
						sender = client.Store.ID.User
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = storeMessageExec(
					tx,
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					directPath,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					// Log successful message storage
					if mediaType != "" {
						logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
					} else {
						logger.Infof("Stored message: [%s] %s -> %s: %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
					}
				}
			}
			if err := tx.Commit(); err != nil {
				logger.Warnf("history sync: commit fallo (%s): %v", chatJID, err)
				tx.Rollback()
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}

// requestMoreHistory pide al servidor los mensajes ANTERIORES al mas viejo que tenemos
// de un chat (el "cargar mensajes anteriores" de WhatsApp). El ancla es el mensaje mas
// antiguo en la DB para ese chat; sin ese ancla no se puede pedir (BuildHistorySyncRequest
// dereferencia el MessageInfo sin chequear nil). Los resultados llegan de forma asincrona
// via un evento HistorySync ON_DEMAND que handleHistorySync persiste.
func requestMoreHistory(client *whatsmeow.Client, store *MessageStore, chatJID types.JID, count int) error {
	if client == nil || !client.IsConnected() {
		return fmt.Errorf("client not connected")
	}
	if client.Store.ID == nil {
		return fmt.Errorf("client not logged in")
	}
	var id string
	var isFromMe bool
	var ts time.Time
	err := store.db.QueryRow(
		"SELECT id, is_from_me, timestamp FROM messages WHERE chat_jid = ? ORDER BY timestamp ASC LIMIT 1",
		chatJID.String(),
	).Scan(&id, &isFromMe, &ts)
	if err != nil {
		return fmt.Errorf("no hay mensajes previos de %s para anclar la solicitud", chatJID)
	}
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chatJID, IsFromMe: isFromMe},
		ID:            id,
		Timestamp:     ts,
	}
	historyMsg := client.BuildHistorySyncRequest(info, count)
	if historyMsg == nil {
		return fmt.Errorf("failed to build history sync request")
	}
	// El history-sync on-demand es un PEER message (ProtocolMessage a tus propios
	// devices), por eso va a tu propio JID con Peer:true. Enviarlo como mensaje normal
	// a "status@s.whatsapp.net" dispara un usync (LID cache) que se cuelga (info query timed out).
	ownJID := client.Store.ID.ToNonAD()
	if _, err := client.SendMessage(context.Background(), ownJID, historyMsg, whatsmeow.SendRequestExtra{Peer: true}); err != nil {
		return fmt.Errorf("failed to request history: %w", err)
	}
	return nil
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position
	var lastGranule uint64
	var sampleRate uint32 = 48000 // Default Opus sample rate
	var preSkip uint16 = 0
	var foundOpusHead bool

	// Scan through the file looking for Ogg pages
	for i := 0; i < len(data); {
		// Check if we have enough data to read Ogg page header
		if i+27 >= len(data) {
			break
		}

		// Verify Ogg page signature
		if string(data[i:i+4]) != "OggS" {
			// Skip until next potential page
			i++
			continue
		}

		// Extract header fields
		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		// Extract segment table
		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		// Calculate page size
		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		// Check if we're looking at an OpusHead packet (should be in first few pages)
		if !foundOpusHead && pageSeqNum <= 1 {
			// Look for "OpusHead" marker in this page
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 && headPos+12 < len(pageData) {
				// Found OpusHead, extract sample rate and pre-skip
				// OpusHead format: Magic(8) + Version(1) + Channels(1) + PreSkip(2) + SampleRate(4) + ...
				headPos += 8 // Skip "OpusHead" marker
				// PreSkip is 2 bytes at offset 10
				if headPos+12 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
					fmt.Printf("Found OpusHead: sampleRate=%d, preSkip=%d\n", sampleRate, preSkip)
				}
			}
		}

		// Keep track of last valid granule position
		if granulePos != 0 {
			lastGranule = granulePos
		}

		// Move to next page
		i += pageSize
	}

	if !foundOpusHead {
		fmt.Println("Warning: OpusHead not found, using default values")
	}

	// Calculate duration based on granule position
	if lastGranule > 0 {
		// Formula for duration: (lastGranule - preSkip) / sampleRate
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		fmt.Printf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)\n",
			durationSeconds, lastGranule)
	} else {
		// Fallback to rough estimation if granule position not found
		fmt.Println("Warning: No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	fmt.Printf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes\n",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Seed the random number generator for consistent results with the same duration
	rng := rand.New(rand.NewSource(int64(duration)))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rng.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
