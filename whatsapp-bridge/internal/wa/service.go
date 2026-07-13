// Package wa: este archivo agrupa la lógica STATEFUL de WhatsApp del bridge en un
// Service que encapsula el cliente whatsmeow, el store de mensajes, el logger y el
// estado en memoria (presencia de terceros y estado de conexión/ban). Es el reemplazo
// de las variables globales (presences/status) y de las funciones sueltas que vivían
// en main.go: ahora son métodos de *Service con el estado inyectado.
package wa

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"whatsapp-client/internal/auth"
	"whatsapp-client/internal/media"
	"whatsapp-client/internal/store"
)

// Service encapsula el cliente whatsmeow, el store de mensajes, el logger y el estado
// en memoria del bridge (presencia y estado de conexión/ban). Reemplaza las globales
// que vivían en main.go: el estado vive en las instancias inyectadas, no en variables
// globales del paquete.
type Service struct {
	Client *whatsmeow.Client
	Store  *store.MessageStore
	Log    waLog.Logger

	storeDir  string          // directorio absoluto del store (media descargada, DBs)
	validator *auth.Validator // validacion anti-exfiltracion de rutas de media

	presence *presenceTracker // estado inyectado, NO global
	status   *botStatus       // estado inyectado, NO global
}

// NewService arma un Service con el estado en memoria recién inicializado. storeDir
// es el directorio del store (inyectado, absoluto); validator valida las rutas de
// media salientes. Ambos pueden ser cero/nil en tests que no ejerciten media.
func NewService(client *whatsmeow.Client, st *store.MessageStore, log waLog.Logger, storeDir string, validator *auth.Validator) *Service {
	return &Service{
		Client: client, Store: st, Log: log,
		storeDir:  storeDir,
		validator: validator,
		presence:  &presenceTracker{states: make(map[string]PresenceInfo)},
		status:    &botStatus{},
	}
}

// ValidateMediaPath expone la validacion anti-exfiltracion del Service para los
// handlers HTTP que leen archivos del disco (p. ej. set_group_photo).
func (s *Service) ValidateMediaPath(p string) (string, error) {
	return s.validator.Validate(p)
}

// PresenceInfo es el estado de presencia conocido de un contacto (online/last-seen/typing).
type PresenceInfo struct {
	Online    bool
	LastSeen  time.Time
	Typing    bool
	UpdatedAt time.Time
}

// presenceTracker guarda en memoria la presencia de terceros: online/last-seen (events.Presence)
// y typing (events.ChatPresence). Thread-safe; lo escribe el event handler.
type presenceTracker struct {
	mu     sync.RWMutex
	states map[string]PresenceInfo
}

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

func (p *presenceTracker) get(jid string) (PresenceInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	info, ok := p.states[jid]
	return info, ok
}

// canonicalPresenceKey normaliza un JID a una clave estable para el tracker: si es @lid lo
// resuelve a su número (PN). Así la presencia (que suele llegar por @lid) se guarda y se
// consulta por la misma clave, sin importar si el caller usa el número o el lid.
func (s *Service) canonicalPresenceKey(jid types.JID) string {
	if jid.Server == types.HiddenUserServer {
		if pn, err := s.Client.Store.LIDs.GetPNForLID(context.Background(), jid); err == nil && pn.User != "" {
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

	// Estado del flujo de login por QR, publicado en /api/qr para que el supervisor
	// (server MCP) pueda mostrar el codigo sin raspar el stdout del proceso.
	qrStatus    string // "" (=none) | "active" | "success" | "timeout"
	qrCode      string
	qrExpiresAt time.Time
}

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

// --- Accesores/dispatchers para el estado en memoria (usados por main). ---

// OnConnected registra una conexión exitosa.
func (s *Service) OnConnected() { s.status.onConnected() }

// OnDisconnected registra una desconexión.
func (s *Service) OnDisconnected() { s.status.onDisconnected() }

// OnTempBan registra un ban temporal (pausa los envíos hasta que expire).
func (s *Service) OnTempBan(code int, reason string, expire time.Duration) {
	s.status.onTempBan(code, reason, expire)
}

// OnLoggedOut registra que la sesión quedó desvinculada (requiere re-escanear QR).
func (s *Service) OnLoggedOut(reason string) { s.status.onLoggedOut(reason) }

// OnConnectFailure registra el último fallo de conexión.
func (s *Service) OnConnectFailure(msg string) { s.status.onConnectFailure(msg) }

// IsTempBanned indica si hay un ban temporal vigente (no expirado).
func (s *Service) IsTempBanned() (bool, string) { return s.status.isTempBanned() }

// StatusSnapshot arma el estado actual para /api/status.
func (s *Service) StatusSnapshot() map[string]interface{} { return s.status.snapshot(s.Client) }

// SetQRCode publica un código QR de login vigente (con su ventana de validez) para /api/qr.
func (s *Service) SetQRCode(code string, timeout time.Duration) {
	s.status.mu.Lock()
	defer s.status.mu.Unlock()
	s.status.qrStatus = "active"
	s.status.qrCode = code
	s.status.qrExpiresAt = time.Now().Add(timeout)
}

// SetQRStatus marca el desenlace del flujo QR ("success", "timeout") y limpia el código:
// un código consumido o vencido no debe volver a mostrarse.
func (s *Service) SetQRStatus(status string) {
	s.status.mu.Lock()
	defer s.status.mu.Unlock()
	s.status.qrStatus = status
	s.status.qrCode = ""
	s.status.qrExpiresAt = time.Time{}
}

// QRInfo devuelve el estado del flujo QR: ("none"|"active"|"success"|"timeout", código, expiración).
// Si el código activo ya venció, degrada a "timeout" (el supervisor debe reiniciar el login).
func (s *Service) QRInfo() (string, string, time.Time) {
	s.status.mu.RLock()
	defer s.status.mu.RUnlock()
	status := s.status.qrStatus
	if status == "" {
		status = "none"
	}
	if status == "active" && time.Now().After(s.status.qrExpiresAt) {
		return "timeout", "", s.status.qrExpiresAt
	}
	return status, s.status.qrCode, s.status.qrExpiresAt
}

// OnPresenceEvent actualiza la presencia (online/last-seen) de un contacto.
func (s *Service) OnPresenceEvent(from types.JID, unavailable bool, lastSeen time.Time) {
	s.presence.onPresence(s.canonicalPresenceKey(from), unavailable, lastSeen)
}

// OnChatPresenceEvent actualiza el estado de typing de un contacto.
func (s *Service) OnChatPresenceEvent(sender types.JID, composing bool) {
	s.presence.onChatPresence(s.canonicalPresenceKey(sender), composing)
}

// GetPresence devuelve la última presencia conocida de un contacto (normaliza el JID a
// la clave canónica del tracker antes de consultar).
func (s *Service) GetPresence(jid types.JID) (PresenceInfo, bool) {
	return s.presence.get(s.canonicalPresenceKey(jid))
}

// LastMsgKey arma el MessageKey + timestamp del ultimo mensaje de un chat,
// requerido por BuildArchive y BuildMarkChatAsRead.
func (s *Service) LastMsgKey(chatJID types.JID) (*waCommon.MessageKey, time.Time) {
	var id, sender string
	var isFromMe bool
	var ts time.Time
	err := s.Store.DB().QueryRow(
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
func (s *Service) buildQuotedContext(quotedID string) *waE2E.ContextInfo {
	var chatJID, sender, content string
	var isFromMe bool
	err := s.Store.DB().QueryRow(
		"SELECT chat_jid, sender, content, is_from_me FROM messages WHERE id = ? LIMIT 1", quotedID,
	).Scan(&chatJID, &sender, &content, &isFromMe)
	if err != nil {
		return nil
	}
	var participant string
	switch {
	case isFromMe && s.Client.Store.ID != nil:
		participant = s.Client.Store.ID.ToNonAD().String()
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
	ctxInfo := &waE2E.ContextInfo{
		StanzaID:    proto.String(quotedID),
		Participant: proto.String(participant),
	}
	if content != "" {
		ctxInfo.QuotedMessage = &waE2E.Message{Conversation: proto.String(content)}
	}
	return ctxInfo
}

// LoadGroupInvite recupera de la DB los datos de una invitacion de grupo capturada
// (media_type="group_invite"): group_jid/code/expiration del JSON en filename; inviter = sender.
func (s *Service) LoadGroupInvite(chatJID, msgID string) (groupJID types.JID, inviter types.JID, code string, expiration int64, err error) {
	var filename string
	if e := s.Store.DB().QueryRow(
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

// SendMessage envía un mensaje (texto con reply/menciones o media) al destinatario y
// persiste el saliente en la DB. Devuelve (ok, mensaje).
func (s *Service) SendMessage(recipient string, message string, mediaPath string, quotedMessageID string, mentions []string) (bool, string) {
	client := s.Client
	messageStore := s.Store
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}
	// Pausar envios si hay un ban temporal vigente (evita empeorar la situacion con el server).
	if banned, reason := s.status.isTempBanned(); banned {
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

	msg := &waE2E.Message{}
	// Metadatos de media para persistir el saliente en la DB (ver final de la funcion).
	// url/direct_path/media_key/hashes salen del UploadResponse: sin ellos download_media
	// no puede descargar los medios que este mismo bridge envio.
	var dbMediaType, dbFilename, dbURL, dbDirectPath string
	var dbMediaKey, dbFileSHA256, dbFileEncSHA256 []byte
	var dbFileLength uint64

	// Check if we have media to send
	if mediaPath != "" {
		// Sandbox: evitar exfiltracion de archivos sensibles via media_path.
		// Se lee de la ruta canonica devuelta (no del string original) para no
		// reabrir un path que pudo cambiar entre la validacion y la lectura.
		resolvedPath, err := s.validator.Validate(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("media path rejected: %v", err)
		}
		// Read media file
		mediaData, err := os.ReadFile(resolvedPath)
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

		dbURL = resp.URL
		dbDirectPath = resp.DirectPath
		dbMediaKey = resp.MediaKey
		dbFileSHA256 = resp.FileSHA256
		dbFileEncSHA256 = resp.FileEncSHA256
		dbFileLength = resp.FileLength

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waE2E.ImageMessage{
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
				analyzedSeconds, analyzedWaveform, err := media.AnalyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waE2E.AudioMessage{
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
			msg.VideoMessage = &waE2E.VideoMessage{
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
			msg.DocumentMessage = &waE2E.DocumentMessage{
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
		var ctxInfo *waE2E.ContextInfo
		if quotedMessageID != "" {
			ctxInfo = s.buildQuotedContext(quotedMessageID)
		}
		if mentionJIDs := ResolveMentions(message, mentions); len(mentionJIDs) > 0 {
			if ctxInfo == nil {
				ctxInfo = &waE2E.ContextInfo{}
			}
			ctxInfo.MentionedJID = mentionJIDs
		}
		if ctxInfo != nil {
			msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{
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
			dbMediaType, dbFilename, dbURL, dbDirectPath, dbMediaKey, dbFileSHA256, dbFileEncSHA256, dbFileLength,
		); err != nil {
			fmt.Printf("warn: message sent but failed to persist outgoing copy: %v\n", err)
		}
	}

	return true, fmt.Sprintf("Message sent to %s", recipient)
}

// DownloadMedia descarga (o reutiliza si ya existe en disco) la media de un mensaje y la
// guarda en store/<chat>/. Devuelve (ok, mediaType, filename, path, err).
func (s *Service) DownloadMedia(messageID, chatJID string) (bool, string, string, string, error) {
	client := s.Client
	messageStore := s.Store
	// Query the database for the message
	var mediaType, filename, url, directPath string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	// First, check if we already have this file. Sanitizamos el chatJID a un unico
	// componente de directorio: ademas de ":" reemplazamos separadores de ruta para
	// que no pueda escapar de store/ (defensa en profundidad; el handler ya valida el JID).
	safeChat := strings.NewReplacer(":", "_", "/", "_", "\\", "_").Replace(chatJID)
	chatDir := filepath.Join(s.storeDir, safeChat)
	localPath := ""

	// Get media info from the database
	mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)

	if err != nil {
		// Try to get basic info if extended info isn't available
		err = messageStore.DB().QueryRow(
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
		directPath = ExtractDirectPathFromURL(url)
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

	downloader := &media.MediaDownloader{
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

// BlockViaLID actualiza la blocklist replicando el formato NUEVO del protocolo de
// WhatsApp (item por LID + pn_jid + dhash). El UpdateBlocklist de whatsmeow aun
// envia el formato viejo (solo jid+action) y el server responde 400. Como sendIQ es
// privado, enviamos el IQ con DangerousInternals().SendNode (no espera respuesta) y
// verificamos el resultado con GetBlocklist. Ref: whatsmeow PR #1137.
func (s *Service) BlockViaLID(jid types.JID, action events.BlocklistChangeAction) (bool, error) {
	client := s.Client
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
	// DangerousInternals es la única vía para enviar este IQ con el formato nuevo: sendIQ de
	// whatsmeow es privado y UpdateBlocklist aún manda el formato viejo (ver doc de la función).
	if err := client.DangerousInternals().SendNode(ctx, node); err != nil { //nolint:staticcheck // API interna necesaria, sin equivalente público
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

// GetChatName determines the appropriate name for a chat based on JID and other info
func (s *Service) GetChatName(jid types.JID, chatJID string, conversation interface{}, sender string) string {
	client := s.Client
	messageStore := s.Store
	logger := s.Log
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.DB().QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
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
			if v.Kind() == reflect.Pointer && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Pointer && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Pointer && !nameField.IsNil() {
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

// RequestMoreHistory pide al servidor los mensajes ANTERIORES al mas viejo que tenemos
// de un chat (el "cargar mensajes anteriores" de WhatsApp). El ancla es el mensaje mas
// antiguo en la DB para ese chat; sin ese ancla no se puede pedir (BuildHistorySyncRequest
// dereferencia el MessageInfo sin chequear nil). Los resultados llegan de forma asincrona
// via un evento HistorySync ON_DEMAND que HandleHistorySync persiste.
func (s *Service) RequestMoreHistory(chatJID types.JID, count int) error {
	client := s.Client
	if client == nil || !client.IsConnected() {
		return fmt.Errorf("client not connected")
	}
	if client.Store.ID == nil {
		return fmt.Errorf("client not logged in")
	}
	var id string
	var isFromMe bool
	var ts time.Time
	err := s.Store.DB().QueryRow(
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
