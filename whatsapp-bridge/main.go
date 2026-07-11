package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/mdp/qrterminal"
	_ "modernc.org/sqlite"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"whatsapp-client/internal/auth"
	"whatsapp-client/internal/store"
	"whatsapp-client/internal/wa"
)

// MessageStore y Message viven ahora en internal/store. Estos aliases transicionales
// evitan reescribir las ~80 referencias existentes en este archivo mientras el resto
// del bridge se modulariza; se pueden retirar cuando todo referencie store.* directo.
type MessageStore = store.MessageStore

type Message = store.Message

// writeJSON serializa v como JSON en el ResponseWriter (el Content-Type y el status deben
// fijarse antes de llamar). Si la codificación falla —normalmente porque el cliente cerró la
// conexión— solo se registra: la respuesta ya está parcialmente enviada y no hay recuperación
// posible. Centraliza el manejo del error de Encode en los handlers HTTP.
func writeJSON(w http.ResponseWriter, v interface{}) {
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		fmt.Println("Error al codificar respuesta JSON:", err)
	}
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

// goSafe corre fn en una goroutine con recover. Los handlers de eventos que corren
// en goroutine (handleHistorySync, handlePollVote, handleCallOffer) procesan protobufs
// influidos por la red y viven FUERA del recover per-request de net/http: un panic ahi
// tumbaria todo el proceso. Aca lo logueamos con stack y seguimos vivos.
func goSafe(logger waLog.Logger, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("panic recuperado en %s: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(svc *wa.Service, client *whatsmeow.Client, messageStore *MessageStore, port int) {
	token, tokErr := auth.GetOrCreateBridgeToken()
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
		success, message := svc.SendMessage(req.Recipient, req.Message, req.MediaPath, req.QuotedMessageID, req.Mentions)
		fmt.Println("Message sent", success, message)
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		writeJSON(w, SendMessageResponse{
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
		// Validar el JID. Ojo: types.ParseJID es permisivo -> un string SIN "@"
		// (ej. "../../x") no da error, lo toma como user con server por defecto.
		// Por eso exigimos ademas la "@": rechaza basura con un 400 limpio en vez de
		// un 500 confuso. La proteccion REAL anti-traversal es el saneo de separadores
		// en downloadMedia (un chat_jid valido igual no puede escapar de store/).
		if _, err := types.ParseJID(req.ChatJID); err != nil || !strings.Contains(req.ChatJID, "@") {
			http.Error(w, "Invalid chat_jid", http.StatusBadRequest)
			return
		}

		// Download the media
		success, mediaType, filename, path, err := svc.DownloadMedia(req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		writeJSON(w, DownloadMediaResponse{
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
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
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
		writeJSON(w, map[string]interface{}{"success": true, "groups": out})
	}))

	// Handler: mark messages as read
	http.HandleFunc("/api/mark_read", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req MarkReadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
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
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		// T3-3: el chat queda leído localmente.
		_, _ = messageStore.ClearChatUnread(req.ChatJID)
		writeJSON(w, map[string]interface{}{"success": true, "message": "marked as read"})
	}))

	// Handler: list chats with unread (incoming) messages tracked live.
	http.HandleFunc("/api/unread_chats", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chats, err := messageStore.GetUnreadChats()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		out := make([]map[string]interface{}, 0, len(chats))
		for _, c := range chats {
			out = append(out, map[string]interface{}{
				"chat_jid":     c.ChatJID,
				"unread_count": c.UnreadCount,
				"last_time":    c.LastTime,
			})
		}
		writeJSON(w, map[string]interface{}{"success": true, "chats": out})
	}))

	// Handler: react to a message with an emoji
	http.HandleFunc("/api/react", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ReactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
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
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "reaction sent"})
	}))

	// Handler: edit a previously sent message
	http.HandleFunc("/api/edit", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req EditRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		newContent := &waE2E.Message{Conversation: proto.String(req.NewText)}
		edit := client.BuildEdit(chat, types.MessageID(req.MessageID), newContent)
		if _, err := client.SendMessage(context.Background(), chat, edit); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "message edited"})
	}))

	// Handler: revoke (delete for everyone) a message
	http.HandleFunc("/api/revoke", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req RevokeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
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
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "message revoked"})
	}))

	// Handler: send chat presence (typing / recording)
	http.HandleFunc("/api/typing", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req TypingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
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
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "presence sent"})
	}))

	// Handler: send a poll
	http.HandleFunc("/api/poll", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req PollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		if req.Question == "" || len(req.Options) < 2 {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "poll needs a question and at least 2 options"})
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
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
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
		writeJSON(w, map[string]interface{}{"success": true, "message": "poll sent", "message_id": resp.ID})
	}))

	// Handler: check if phone numbers are on WhatsApp
	http.HandleFunc("/api/check_whatsapp", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req CheckWhatsAppRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		resp, err := client.IsOnWhatsApp(context.Background(), req.Phones)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
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
		writeJSON(w, map[string]interface{}{"success": true, "results": out})
	}))

	// Handler: get a profile picture URL
	http.HandleFunc("/api/profile_picture", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ProfilePictureRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.JID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid jid"})
			return
		}
		info, err := client.GetProfilePictureInfo(context.Background(), jid, &whatsmeow.GetProfilePictureParams{Preview: req.Preview})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if info == nil {
			writeJSON(w, map[string]interface{}{"success": true, "has_picture": false})
			return
		}
		writeJSON(w, map[string]interface{}{
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
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
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
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
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
		writeJSON(w, map[string]interface{}{"success": true, "users": out})
	}))

	// Handler: get group participants
	http.HandleFunc("/api/group_participants", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		info, err := client.GetGroupInfo(context.Background(), jid)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
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
		writeJSON(w, map[string]interface{}{
			"success": true, "name": info.Name, "participant_count": len(parts), "participants": parts,
		})
	}))

	// Handler: get / reset group invite link
	http.HandleFunc("/api/group_invite_link", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		link, err := client.GetGroupInviteLink(context.Background(), jid, req.Reset)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "link": link})
	}))

	// Handler: join a group via invite link/code
	http.HandleFunc("/api/join_group", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req JoinGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		code := req.Code
		if idx := strings.LastIndex(code, "/"); idx >= 0 {
			code = code[idx+1:] // aceptar link completo o solo el codigo
		}
		jid, err := client.JoinGroupWithLink(context.Background(), code)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "group_jid": jid.String()})
	}))

	// Handler: leave a group
	http.HandleFunc("/api/leave_group", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.LeaveGroup(context.Background(), jid); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "left group"})
	}))

	// Handler: set group name
	http.HandleFunc("/api/set_group_name", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupNameRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.SetGroupName(context.Background(), jid, req.Name); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "group name updated"})
	}))

	// Handler: set group topic/description
	http.HandleFunc("/api/set_group_topic", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupTopicRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.SetGroupTopic(context.Background(), jid, "", "", req.Topic); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "group topic updated"})
	}))

	// Handler: block / unblock a contact
	http.HandleFunc("/api/block", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BlockRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.JID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid jid"})
			return
		}
		action := events.BlocklistChangeActionBlock
		if req.Action == "unblock" {
			action = events.BlocklistChangeActionUnblock
		}
		ok, err := svc.BlockViaLID(jid, action)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": "blocklist update not reflected (verified via GetBlocklist)"})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": string(action) + "ed"})
	}))

	// Handler: mute / unmute chat
	http.HandleFunc("/api/mute", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		if err := client.SendAppState(context.Background(), appstate.BuildMute(jid, req.Enable, time.Duration(req.Duration)*time.Hour)); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "mute updated"})
	}))

	// Handler: pin / unpin chat
	http.HandleFunc("/api/pin", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		if err := client.SendAppState(context.Background(), appstate.BuildPin(jid, req.Enable)); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "pin updated"})
	}))

	// Handler: archive / unarchive chat
	http.HandleFunc("/api/archive", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		key, ts := svc.LastMsgKey(jid)
		if err := client.SendAppState(context.Background(), appstate.BuildArchive(jid, req.Enable, ts, key)); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "archive updated"})
	}))

	// Handler: mark chat read / unread
	http.HandleFunc("/api/mark_chat", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		key, ts := svc.LastMsgKey(jid)
		if err := client.SendAppState(context.Background(), appstate.BuildMarkChatAsRead(jid, req.Enable, ts, key)); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "chat read-state updated"})
	}))

	// Handler: star / unstar a message
	http.HandleFunc("/api/star", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req StarRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		var senderRaw string
		var fromMe bool
		_ = messageStore.DB().QueryRow("SELECT sender, is_from_me FROM messages WHERE id = ? LIMIT 1", req.MessageID).Scan(&senderRaw, &fromMe)
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
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "star updated"})
	}))

	// Handler: get chat settings (muted/pinned/archived)
	http.HandleFunc("/api/chat_settings", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		s, err := client.Store.ChatSettings.GetChatSettings(context.Background(), jid)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		muted := false
		mutedUntil := ""
		if !s.MutedUntil.IsZero() {
			muted = s.MutedUntil.After(time.Now())
			mutedUntil = s.MutedUntil.Format(time.RFC3339)
		}
		writeJSON(w, map[string]interface{}{
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
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		count := req.Count
		if count <= 0 {
			count = 50
		}
		if err := svc.RequestMoreHistory(jid, count); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "history requested (best-effort); si el telefono primario esta online y conserva mensajes anteriores, llegan async via history sync y quedan en la DB"})
	}))

	// Handler: create group
	http.HandleFunc("/api/create_group", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req CreateGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "group name required"})
			return
		}
		if len([]rune(name)) > 25 {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "group name max 25 chars"})
			return
		}
		parts, err := wa.ParseParticipantJIDs(req.Participants)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if len(parts) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "at least one participant required"})
			return
		}
		info, err := client.CreateGroup(context.Background(), whatsmeow.ReqCreateGroup{Name: name, Participants: parts})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{
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
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		gjid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
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
			writeJSON(w, map[string]interface{}{"success": false, "message": "action must be add/remove/promote/demote"})
			return
		}
		parts, err := wa.ParseParticipantJIDs(req.Participants)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if len(parts) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "at least one participant required"})
			return
		}
		result, err := client.UpdateGroupParticipants(context.Background(), gjid, parts, action)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		results := make([]map[string]interface{}, 0, len(result))
		for _, p := range result {
			results = append(results, map[string]interface{}{
				"jid": p.JID.String(), "error_code": p.Error,
				"is_admin": p.IsAdmin,
			})
		}
		writeJSON(w, map[string]interface{}{"success": true, "action": req.Action, "results": results})
	}))

	// Handler: set disappearing-messages timer (off/24h/7d/90d)
	http.HandleFunc("/api/disappearing", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req DisappearingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		timer, ok := whatsmeow.ParseDisappearingTimerString(req.Duration)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "duration must be one of: off, 24h, 7d, 90d"})
			return
		}
		if err := client.SetDisappearingTimer(context.Background(), jid, timer, time.Time{}); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "disappearing timer set", "duration": req.Duration})
	}))

	// Handler: estado de conexion/sesion/ban del cliente
	http.HandleFunc("/api/status", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		m := svc.StatusSnapshot()
		m["success"] = true
		writeJSON(w, m)
	}))

	// --- Lote A1: perfil & cuenta ---

	// Handler: set status message ("about" propio)
	http.HandleFunc("/api/set_status", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		if err := client.SetStatusMessage(context.Background(), req.Message); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "status updated"})
	}))

	// Handler: get business profile de un contacto
	http.HandleFunc("/api/business_profile", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BusinessProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.JID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid jid"})
			return
		}
		bp, err := client.GetBusinessProfile(context.Background(), jid)
		if err != nil {
			// Contacto NO business: whatsmeow devuelve "missing jid"/not-found. No es error real.
			if strings.Contains(err.Error(), "missing jid") || strings.Contains(err.Error(), "not found") {
				writeJSON(w, map[string]interface{}{"success": true, "is_business": false})
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if bp == nil {
			writeJSON(w, map[string]interface{}{"success": true, "is_business": false})
			return
		}
		cats := make([]map[string]string, 0, len(bp.Categories))
		for _, c := range bp.Categories {
			cats = append(cats, map[string]string{"id": c.ID, "name": c.Name})
		}
		writeJSON(w, map[string]interface{}{
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
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jids, err := wa.ParseParticipantJIDs(req.JIDs)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		devices, err := client.GetUserDevices(context.Background(), jids)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		out := make([]string, 0, len(devices))
		for _, d := range devices {
			out = append(out, d.String())
		}
		writeJSON(w, map[string]interface{}{"success": true, "devices": out, "count": len(out)})
	}))

	// Handler: set default disappearing timer (aplica a chats NUEVOS)
	http.HandleFunc("/api/default_disappearing", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req DefaultDisappearingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		timer, ok := whatsmeow.ParseDisappearingTimerString(req.Duration)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "duration must be one of: off, 24h, 7d, 90d"})
			return
		}
		if err := client.SetDefaultDisappearingTimer(context.Background(), timer); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "default disappearing timer set", "duration": req.Duration})
	}))

	// --- Lote A2: administración de grupos (requieren ser admin) ---

	// Handler: set group description
	http.HandleFunc("/api/set_group_description", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupDescriptionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		// whatsmeow.SetGroupDescription envía el nodo sin versionar el cambio y el server
		// responde 409 conflict. En WhatsApp el "topic" ES la descripción del grupo, y
		// SetGroupTopic (con previous/new id vacíos) sí maneja el versionado, igual que el
		// handler set_group_topic. Por eso reusamos SetGroupTopic aquí.
		if err := client.SetGroupTopic(context.Background(), jid, "", "", req.Description); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "group description updated"})
	}))

	// Handler: set group announce (true = solo admins pueden enviar mensajes)
	http.HandleFunc("/api/set_group_announce", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupToggleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.SetGroupAnnounce(context.Background(), jid, req.Enable); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "group announce updated"})
	}))

	// Handler: set group locked (true = solo admins pueden editar info del grupo)
	http.HandleFunc("/api/set_group_locked", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupToggleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.SetGroupLocked(context.Background(), jid, req.Enable); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "group locked updated"})
	}))

	// Handler: set group photo (lee la imagen del path; WhatsApp requiere JPEG)
	http.HandleFunc("/api/set_group_photo", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupPhotoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		// Misma proteccion que el envio de media: sin esto un caller podria leer
		// cualquier archivo del disco (incluida la sesion en store/) y subirlo.
		if err := auth.ValidateMediaPath(req.ImagePath); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": fmt.Sprintf("invalid image_path: %v", err)})
			return
		}
		avatar, err := os.ReadFile(req.ImagePath)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": fmt.Sprintf("cannot read image: %v", err)})
			return
		}
		pictureID, err := client.SetGroupPhoto(context.Background(), jid, avatar)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "group photo updated", "picture_id": pictureID})
	}))

	// --- Lote B2: presencia ---

	// Handler: set own presence (available/unavailable). available es requisito para RECIBIR
	// la presencia de otros.
	http.HandleFunc("/api/set_presence", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetPresenceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
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
			writeJSON(w, map[string]interface{}{"success": false, "message": "state must be available/unavailable"})
			return
		}
		if err := client.SendPresence(context.Background(), p); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "presence sent", "state": req.State})
	}))

	// Handler: subscribe to a contact's presence (necesario para recibir su online/last-seen)
	http.HandleFunc("/api/subscribe_presence", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BusinessProfileRequest // {jid}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.JID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid jid"})
			return
		}
		if err := client.SubscribePresence(context.Background(), jid); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "subscribed to presence"})
	}))

	// Handler: get last known presence of a contact (del tracker en memoria)
	http.HandleFunc("/api/get_presence", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BusinessProfileRequest // {jid}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.JID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid jid"})
			return
		}
		info, ok := svc.GetPresence(jid)
		if !ok {
			writeJSON(w, map[string]interface{}{"success": true, "tracked": false, "message": "sin datos de presencia aún (subscribe_presence + esperar a que cambie de estado)"})
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
		writeJSON(w, out)
	}))

	// --- Lote B1: unirse por código de invitación ---

	// Handler: get group info from invite (inspeccionar sin unirse)
	http.HandleFunc("/api/group_info_from_invite", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req InviteActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		gjid, inviter, code, exp, err := svc.LoadGroupInvite(req.ChatJID, req.InviteMessageID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		info, err := client.GetGroupInfoFromInvite(context.Background(), gjid, inviter, code, exp)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{
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
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		gjid, inviter, code, exp, err := svc.LoadGroupInvite(req.ChatJID, req.InviteMessageID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if err := client.JoinGroupWithInvite(context.Background(), gjid, inviter, code, exp); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "joined group via invite", "group_jid": gjid.String()})
	}))

	// --- Lote A3: solicitudes de ingreso a grupos (requieren admin) ---

	// Handler: set group join approval mode (true = los ingresos requieren aprobación de admin)
	http.HandleFunc("/api/set_group_join_approval", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupToggleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		if err := client.SetGroupJoinApprovalMode(context.Background(), jid, req.Enable); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "join approval mode updated"})
	}))

	// Handler: get group join requests (solicitudes pendientes de ingreso)
	http.HandleFunc("/api/group_join_requests", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
			return
		}
		reqs, err := client.GetGroupRequestParticipants(context.Background(), jid)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		out := make([]map[string]interface{}, 0, len(reqs))
		for _, p := range reqs {
			out = append(out, map[string]interface{}{"jid": p.JID.String(), "requested_at": p.RequestedAt.Format(time.RFC3339)})
		}
		writeJSON(w, map[string]interface{}{"success": true, "requests": out, "count": len(out)})
	}))

	// Handler: review group join request (approve/reject)
	http.HandleFunc("/api/review_group_join_request", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req UpdateParticipantsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.GroupJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid group_jid"})
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
			writeJSON(w, map[string]interface{}{"success": false, "message": "action must be approve/reject"})
			return
		}
		parts, err := wa.ParseParticipantJIDs(req.Participants)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if len(parts) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "at least one participant required"})
			return
		}
		result, err := client.UpdateGroupRequestParticipants(context.Background(), jid, parts, action)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		results := make([]map[string]interface{}, 0, len(result))
		for _, p := range result {
			results = append(results, map[string]interface{}{"jid": p.JID.String(), "error_code": p.Error})
		}
		writeJSON(w, map[string]interface{}{"success": true, "action": req.Action, "results": results})
	}))

	// --- Lote A4: votar en encuestas ---

	// Handler: vote in a poll
	http.HandleFunc("/api/poll_vote", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req PollVoteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid request"})
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "invalid chat_jid"})
			return
		}
		if len(req.Options) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "at least one option required"})
			return
		}
		// Reconstruir el MessageInfo del poll original desde la DB (debe haber sido capturado).
		var senderRaw string
		var fromMe bool
		err = messageStore.DB().QueryRow(
			"SELECT sender, is_from_me FROM messages WHERE id = ? AND chat_jid = ? AND media_type = 'poll' LIMIT 1",
			req.PollMessageID, req.ChatJID,
		).Scan(&senderRaw, &fromMe)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"success": false, "message": "poll not found in DB (no fue capturado); no se puede votar"})
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
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		if _, err := client.SendMessage(context.Background(), jid, voteMsg); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"success": true, "message": "poll vote sent", "options": req.Options})
	}))

	// --- Logout: desvincula la sesión (requiere re-escanear QR para volver) ---
	http.HandleFunc("/api/logout", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := client.Logout(context.Background()); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		svc.OnLoggedOut("logout solicitado por el usuario")
		writeJSON(w, map[string]interface{}{"success": true, "message": "logged out; reiniciar el bridge y re-escanear el QR para volver a vincular"})
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
	messageStore, err := store.NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer func() { _ = messageStore.Close() }()

	// Servicio con la lógica stateful de WhatsApp (estado en memoria inyectado, no global).
	svc := wa.NewService(client, messageStore, logger)

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Voto de encuesta entrante: descifrar y registrar (goroutine; no bloquea el dispatch).
			if v.Message.GetPollUpdateMessage() != nil {
				goSafe(logger, "handlePollVote", func() { svc.HandlePollVote(v) })
			}
			// Process regular messages
			svc.HandleMessage(v)

		case *events.HistorySync:
			// Procesar en goroutine para NO bloquear el dispatch de eventos en vivo.
			// El history-sync puede tardar minutos (cientos de mensajes + lookups de red);
			// si corre sincronico en el handler, los *events.Message en vivo quedan
			// encolados detras y no se guardan hasta que termina (bug observado).
			goSafe(logger, "handleHistorySync", func() { svc.HandleHistorySync(v) })

		case *events.Receipt:
			// T3-3: read-receipt PROPIO (leí el chat desde el teléfono u otro device) ->
			// marcar ese chat como leído. (ReceiptTypeRead = otros leyeron MIS mensajes, no aplica.)
			if v.Type == types.ReceiptTypeReadSelf {
				if n, _ := messageStore.ClearChatUnread(v.Chat.String()); n > 0 {
					logger.Infof("Chat %s marcado como leído (read-self): %d no-leídos limpiados", v.Chat.String(), n)
				}
			}

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")
			svc.OnConnected()

		case *events.Disconnected:
			// EnableAutoReconnect (mas arriba) ya gestiona la reconexion. Un reconnect
			// manual aqui competiria con el interno de whatsmeow (race / StreamReplaced).
			logger.Warnf("Disconnected from WhatsApp; auto-reconnect en curso...")
			svc.OnDisconnected()

		case *events.LoggedOut:
			logger.Warnf("Device logged out (reason=%s, onConnect=%v); please scan QR code to log in again", v.Reason.String(), v.OnConnect)
			svc.OnLoggedOut(v.Reason.String())

		case *events.TemporaryBan:
			// 🔴 Ban temporal: loggear fuerte y pausar envios (svc.SendMessage chequea isTempBanned).
			logger.Errorf("⚠️ TEMPORARY BAN: code=%d (%s); expira en %s. ENVIOS PAUSADOS.", int(v.Code), v.Code.String(), v.Expire)
			svc.OnTempBan(int(v.Code), v.Code.String(), v.Expire)

		case *events.ConnectFailure:
			logger.Errorf("Connect failure: reason=%d (%s) msg=%s", int(v.Reason), v.Reason.String(), v.Message)
			svc.OnConnectFailure(fmt.Sprintf("%d %s: %s", int(v.Reason), v.Reason.String(), v.Message))
			if v.Reason.IsLoggedOut() {
				svc.OnLoggedOut(v.Reason.String())
			}

		case *events.Presence:
			// Presencia de terceros: online/offline + last-seen (requiere SubscribePresence + SendPresence).
			svc.OnPresenceEvent(v.From, v.Unavailable, v.LastSeen)

		case *events.ChatPresence:
			// Typing de terceros (composing/paused) en un chat.
			svc.OnChatPresenceEvent(v.Sender, v.State == types.ChatPresenceComposing)

		case *events.CallOffer:
			// Llamada entrante: solo se registra (whatsmeow no maneja audio). Sin auto-rechazo.
			goSafe(logger, "handleCallOffer", func() { svc.HandleCallOffer(v) })
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
	startRESTServer(svc, client, messageStore, 8080)

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
