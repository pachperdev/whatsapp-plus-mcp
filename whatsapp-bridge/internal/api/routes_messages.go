// Package api: rutas del dominio MENSAJES (enviar, reaccionar, editar, revocar,
// destacar, marcar leído, descargar media, encuestas y typing). Extraídas de
// server.go para que cada dominio sea navegable por separado; los handlers son
// movimiento mecánico, sin cambios de lógica ni de contrato HTTP.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"whatsapp-client/internal/store"
	"whatsapp-client/internal/wa"
)

// registerMessageRoutes registra las rutas de mensajes sobre el mux. Los handlers
// capturan svc/client/st/token por closure, igual que cuando vivían en NewServer.
func registerMessageRoutes(mux *http.ServeMux, svc *wa.Service, client *whatsmeow.Client, st *store.MessageStore, token string) {
	// Handler for sending messages
	mux.HandleFunc("/api/send", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Only allow POST requests
		if r.Method != http.MethodPost {
			respondErr(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}

		// Parse the request body
		var req SendMessageRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		// Validate request
		if req.Recipient == "" {
			respondErr(w, http.StatusBadRequest, "Recipient is required")
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			respondErr(w, http.StatusBadRequest, "Message or media path is required")
			return
		}

		fmt.Println("Received request to send message", req.Message, req.MediaPath)

		// Send the message
		success, message := svc.SendMessage(req.Recipient, req.Message, req.MediaPath, req.QuotedMessageID, req.Mentions)
		fmt.Println("Message sent", success, message)

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
	mux.HandleFunc("/api/download", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body (body acotado a 1 MiB, igual que decodeJSON; este
		// handler conserva el contrato de error en texto plano de upstream).
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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

	// Handler: mark messages as read
	mux.HandleFunc("/api/mark_read", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req MarkReadRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		chat, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
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
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// T3-3: el chat queda leído localmente.
		_, _ = st.ClearChatUnread(req.ChatJID)
		respondOK(w, map[string]interface{}{"message": "marked as read"})
	}))

	// Handler: react to a message with an emoji
	mux.HandleFunc("/api/react", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if banBlocked(w, svc) {
			return
		}
		var req ReactRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		chat, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
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
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "reaction sent"})
	}))

	// Handler: edit a previously sent message
	mux.HandleFunc("/api/edit", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if banBlocked(w, svc) {
			return
		}
		var req EditRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		chat, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
			return
		}
		newContent := &waE2E.Message{Conversation: proto.String(req.NewText)}
		edit := client.BuildEdit(chat, types.MessageID(req.MessageID), newContent)
		if _, err := client.SendMessage(context.Background(), chat, edit); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "message edited"})
	}))

	// Handler: revoke (delete for everyone) a message
	mux.HandleFunc("/api/revoke", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if banBlocked(w, svc) {
			return
		}
		var req RevokeRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		chat, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
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
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "message revoked"})
	}))

	// Handler: send chat presence (typing / recording)
	mux.HandleFunc("/api/typing", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req TypingRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		chat, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
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
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "presence sent"})
	}))

	// Handler: send a poll
	mux.HandleFunc("/api/poll", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if banBlocked(w, svc) {
			return
		}
		var req PollRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		chat, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
			return
		}
		if req.Question == "" || len(req.Options) < 2 {
			respondErr(w, http.StatusBadRequest, "poll needs a question and at least 2 options")
			return
		}
		selectable := req.SelectableCount
		if selectable < 1 {
			selectable = 1
		}
		poll := client.BuildPollCreation(req.Question, req.Options, selectable)
		resp, err := client.SendMessage(context.Background(), chat, poll)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Persistir el poll saliente (media_type="poll", opciones en filename JSON) para poder
		// votarlo con vote_poll y mapear los votos entrantes a nombres legibles.
		if st != nil {
			optsB, _ := json.Marshal(req.Options)
			var senderUser string
			if client.Store != nil && client.Store.ID != nil {
				senderUser = client.Store.ID.User
			}
			_ = st.TouchChat(chat.String(), resp.Timestamp)
			_ = st.StoreMessage(resp.ID, chat.String(), senderUser, req.Question, resp.Timestamp, true,
				"poll", string(optsB), "", "", nil, nil, nil, 0)
		}
		respondOK(w, map[string]interface{}{"message": "poll sent", "message_id": resp.ID})
	}))

	// Handler: star / unstar a message
	mux.HandleFunc("/api/star", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req StarRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
			return
		}
		senderRaw, fromMe, _ := st.MessageSender(req.MessageID)
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
		if err := sendAppState(client, appstate.BuildStar(jid, senderJID, types.MessageID(req.MessageID), fromMe, req.Starred)); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "star updated"})
	}))

	// --- Lote A4: votar en encuestas ---

	// Handler: vote in a poll
	mux.HandleFunc("/api/poll_vote", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if banBlocked(w, svc) {
			return
		}
		var req PollVoteRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
			return
		}
		if len(req.Options) == 0 {
			respondErr(w, http.StatusBadRequest, "at least one option required")
			return
		}
		// Reconstruir el MessageInfo del poll original desde la DB (debe haber sido capturado).
		senderRaw, fromMe, err := st.PollSender(req.PollMessageID, req.ChatJID)
		if err != nil {
			respondErr(w, http.StatusBadRequest, "poll not found in DB (no fue capturado); no se puede votar")
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
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if _, err := client.SendMessage(context.Background(), jid, voteMsg); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "poll vote sent", "options": req.Options})
	}))
}
