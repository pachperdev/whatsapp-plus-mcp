package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatsapp-client/internal/store"
	"whatsapp-client/internal/wa"
)

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

// withAuth exige el token compartido (X-Auth-Token) en cada request antes de delegar en next.
// Fail-closed: si token=="" o el header no coincide (comparación en tiempo constante) responde 401.
func withAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Auth-Token")
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// banBlocked responde 503 y devuelve true si hay un ban temporal vigente. Los
// envios salientes que NO pasan por svc.SendMessage (react/edit/revoke/poll/
// poll_vote usan client.SendMessage directo con Build*) deben chequearlo igual:
// son stanzas salientes que pueden empeorar un ban en curso. Preserva el
// invariante anti-ban del proyecto en todos los caminos de envio.
func banBlocked(w http.ResponseWriter, svc *wa.Service) bool {
	if banned, reason := svc.IsTempBanned(); banned {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("envio bloqueado: cuenta con ban temporal (%s). Espera a que expire; ver /api/status", reason),
		})
		return true
	}
	return false
}

// decodeJSON limita el body a 1 MiB (defensa DoS) y decodifica el JSON en dst.
// Si el body es inválido responde 400 con el contrato estándar
// {"success":false,"message":"invalid request"} y devuelve false; el handler
// debe cortar. El Content-Type ya lo fija el handler antes de llamar.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		respondErr(w, http.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

// parseJID valida raw con types.ParseJID. Si falla responde 400
// {"success":false,"message":"invalid "+field} y devuelve ok=false; el mensaje
// reproduce el de cada handler ("invalid chat_jid", "invalid group_jid", etc.),
// por eso field debe ser el correcto en cada call.
func parseJID(w http.ResponseWriter, raw, field string) (types.JID, bool) {
	jid, err := types.ParseJID(raw)
	if err != nil {
		respondErr(w, http.StatusBadRequest, "invalid "+field)
		return jid, false
	}
	return jid, true
}

// respondErr escribe el status y el cuerpo de error estándar
// {"success":false,"message":msg}. El Content-Type debe fijarse antes.
func respondErr(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]interface{}{"success": false, "message": msg})
}

// respondOK escribe una respuesta de éxito {"success":true, ...extra} con status
// 200 implícito. json ordena las claves del mapa, así que el resultado es
// byte-idéntico a construir el literal con "success":true ya incluido.
func respondOK(w http.ResponseWriter, extra map[string]interface{}) {
	m := map[string]interface{}{"success": true}
	for k, v := range extra {
		m[k] = v
	}
	writeJSON(w, m)
}

// NewServer registra todas las rutas REST del bridge sobre un mux propio (no el
// DefaultServeMux), cada una envuelta en withAuth con el token compartido, y devuelve el
// handler resultante. Los handlers capturan svc/client/st por closure.
func NewServer(svc *wa.Service, client *whatsmeow.Client, st *store.MessageStore, token string) http.Handler {
	mux := http.NewServeMux()

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

	// Handler: list joined groups
	mux.HandleFunc("/api/groups", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		groups, err := client.GetJoinedGroups(context.Background())
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
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
		respondOK(w, map[string]interface{}{"groups": out})
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

	// Handler: list chats with unread (incoming) messages tracked live.
	mux.HandleFunc("/api/unread_chats", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chats, err := st.GetUnreadChats()
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
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
		respondOK(w, map[string]interface{}{"chats": out})
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

	// Handler: check if phone numbers are on WhatsApp
	mux.HandleFunc("/api/check_whatsapp", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req CheckWhatsAppRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		resp, err := client.IsOnWhatsApp(context.Background(), req.Phones)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
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
		respondOK(w, map[string]interface{}{"results": out})
	}))

	// Handler: get a profile picture URL
	mux.HandleFunc("/api/profile_picture", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ProfilePictureRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.JID, "jid")
		if !ok {
			return
		}
		info, err := client.GetProfilePictureInfo(context.Background(), jid, &whatsmeow.GetProfilePictureParams{Preview: req.Preview})
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if info == nil {
			respondOK(w, map[string]interface{}{"has_picture": false})
			return
		}
		writeJSON(w, map[string]interface{}{
			"success": true, "has_picture": true,
			"url": info.URL, "id": info.ID, "type": info.Type,
		})
	}))

	// Handler: get user info (status/about, business flag)
	mux.HandleFunc("/api/user_info", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req UserInfoRequest
		if !decodeJSON(w, r, &req) {
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
			respondErr(w, http.StatusInternalServerError, err.Error())
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
		respondOK(w, map[string]interface{}{"users": out})
	}))

	// Handler: get group participants
	mux.HandleFunc("/api/group_participants", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		info, err := client.GetGroupInfo(context.Background(), jid)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
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
	mux.HandleFunc("/api/group_invite_link", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		link, err := client.GetGroupInviteLink(context.Background(), jid, req.Reset)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"link": link})
	}))

	// Handler: join a group via invite link/code
	mux.HandleFunc("/api/join_group", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req JoinGroupRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		code := req.Code
		if idx := strings.LastIndex(code, "/"); idx >= 0 {
			code = code[idx+1:] // aceptar link completo o solo el codigo
		}
		jid, err := client.JoinGroupWithLink(context.Background(), code)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"group_jid": jid.String()})
	}))

	// Handler: leave a group
	mux.HandleFunc("/api/leave_group", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		if err := client.LeaveGroup(context.Background(), jid); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "left group"})
	}))

	// Handler: set group name
	mux.HandleFunc("/api/set_group_name", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupNameRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		if err := client.SetGroupName(context.Background(), jid, req.Name); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "group name updated"})
	}))

	// Handler: set group topic/description
	mux.HandleFunc("/api/set_group_topic", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupTopicRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		if err := client.SetGroupTopic(context.Background(), jid, "", "", req.Topic); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "group topic updated"})
	}))

	// Handler: block / unblock a contact
	mux.HandleFunc("/api/block", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BlockRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.JID, "jid")
		if !ok {
			return
		}
		action := events.BlocklistChangeActionBlock
		if req.Action == "unblock" {
			action = events.BlocklistChangeActionUnblock
		}
		ok, err := svc.BlockViaLID(jid, action)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			respondErr(w, http.StatusInternalServerError, "blocklist update not reflected (verified via GetBlocklist)")
			return
		}
		respondOK(w, map[string]interface{}{"message": string(action) + "ed"})
	}))

	// Handler: mute / unmute chat
	mux.HandleFunc("/api/mute", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
			return
		}
		if err := client.SendAppState(context.Background(), appstate.BuildMute(jid, req.Enable, time.Duration(req.Duration)*time.Hour)); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "mute updated"})
	}))

	// Handler: pin / unpin chat
	mux.HandleFunc("/api/pin", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
			return
		}
		if err := client.SendAppState(context.Background(), appstate.BuildPin(jid, req.Enable)); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "pin updated"})
	}))

	// Handler: archive / unarchive chat
	mux.HandleFunc("/api/archive", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
			return
		}
		key, ts := svc.LastMsgKey(jid)
		if err := client.SendAppState(context.Background(), appstate.BuildArchive(jid, req.Enable, ts, key)); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "archive updated"})
	}))

	// Handler: mark chat read / unread
	mux.HandleFunc("/api/mark_chat", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
			return
		}
		key, ts := svc.LastMsgKey(jid)
		if err := client.SendAppState(context.Background(), appstate.BuildMarkChatAsRead(jid, req.Enable, ts, key)); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "chat read-state updated"})
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
		if err := client.SendAppState(context.Background(), appstate.BuildStar(jid, senderJID, types.MessageID(req.MessageID), fromMe, req.Starred)); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "star updated"})
	}))

	// Handler: get chat settings (muted/pinned/archived)
	mux.HandleFunc("/api/chat_settings", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req ChatStateRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
			return
		}
		s, err := client.Store.ChatSettings.GetChatSettings(context.Background(), jid)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
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
	mux.HandleFunc("/api/request_history", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req HistoryRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
			return
		}
		count := req.Count
		if count <= 0 {
			count = 50
		}
		if err := svc.RequestMoreHistory(jid, count); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "history requested (best-effort); si el telefono primario esta online y conserva mensajes anteriores, llegan async via history sync y quedan en la DB"})
	}))

	// Handler: create group
	mux.HandleFunc("/api/create_group", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req CreateGroupRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			respondErr(w, http.StatusBadRequest, "group name required")
			return
		}
		if len([]rune(name)) > 25 {
			respondErr(w, http.StatusBadRequest, "group name max 25 chars")
			return
		}
		parts, err := wa.ParseParticipantJIDs(req.Participants)
		if err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(parts) == 0 {
			respondErr(w, http.StatusBadRequest, "at least one participant required")
			return
		}
		info, err := client.CreateGroup(context.Background(), whatsmeow.ReqCreateGroup{Name: name, Participants: parts})
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{
			"success": true, "group_jid": info.JID.String(),
			"name": info.Name, "participant_count": len(info.Participants),
		})
	}))

	// Handler: update group participants (add/remove/promote/demote)
	mux.HandleFunc("/api/update_participants", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req UpdateParticipantsRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		gjid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
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
			respondErr(w, http.StatusBadRequest, "action must be add/remove/promote/demote")
			return
		}
		parts, err := wa.ParseParticipantJIDs(req.Participants)
		if err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(parts) == 0 {
			respondErr(w, http.StatusBadRequest, "at least one participant required")
			return
		}
		result, err := client.UpdateGroupParticipants(context.Background(), gjid, parts, action)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		results := make([]map[string]interface{}, 0, len(result))
		for _, p := range result {
			results = append(results, map[string]interface{}{
				"jid": p.JID.String(), "error_code": p.Error,
				"is_admin": p.IsAdmin,
			})
		}
		respondOK(w, map[string]interface{}{"action": req.Action, "results": results})
	}))

	// Handler: set disappearing-messages timer (off/24h/7d/90d)
	mux.HandleFunc("/api/disappearing", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req DisappearingRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.ChatJID, "chat_jid")
		if !ok {
			return
		}
		timer, ok := whatsmeow.ParseDisappearingTimerString(req.Duration)
		if !ok {
			respondErr(w, http.StatusBadRequest, "duration must be one of: off, 24h, 7d, 90d")
			return
		}
		if err := client.SetDisappearingTimer(context.Background(), jid, timer, time.Time{}); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "disappearing timer set", "duration": req.Duration})
	}))

	// Handler: estado de conexion/sesion/ban del cliente
	mux.HandleFunc("/api/status", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		m := svc.StatusSnapshot()
		m["success"] = true
		writeJSON(w, m)
	}))

	// --- Lote A1: perfil & cuenta ---

	// Handler: set status message ("about" propio)
	mux.HandleFunc("/api/set_status", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetStatusRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if err := client.SetStatusMessage(context.Background(), req.Message); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "status updated"})
	}))

	// Handler: get business profile de un contacto
	mux.HandleFunc("/api/business_profile", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BusinessProfileRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.JID, "jid")
		if !ok {
			return
		}
		bp, err := client.GetBusinessProfile(context.Background(), jid)
		if err != nil {
			// Contacto NO business: whatsmeow devuelve "missing jid"/not-found. No es error real.
			if strings.Contains(err.Error(), "missing jid") || strings.Contains(err.Error(), "not found") {
				respondOK(w, map[string]interface{}{"is_business": false})
				return
			}
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if bp == nil {
			respondOK(w, map[string]interface{}{"is_business": false})
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
	mux.HandleFunc("/api/user_devices", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req UserDevicesRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jids, err := wa.ParseParticipantJIDs(req.JIDs)
		if err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
		devices, err := client.GetUserDevices(context.Background(), jids)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]string, 0, len(devices))
		for _, d := range devices {
			out = append(out, d.String())
		}
		respondOK(w, map[string]interface{}{"devices": out, "count": len(out)})
	}))

	// Handler: set default disappearing timer (aplica a chats NUEVOS)
	mux.HandleFunc("/api/default_disappearing", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req DefaultDisappearingRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		timer, ok := whatsmeow.ParseDisappearingTimerString(req.Duration)
		if !ok {
			respondErr(w, http.StatusBadRequest, "duration must be one of: off, 24h, 7d, 90d")
			return
		}
		if err := client.SetDefaultDisappearingTimer(context.Background(), timer); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "default disappearing timer set", "duration": req.Duration})
	}))

	// --- Lote A2: administración de grupos (requieren ser admin) ---

	// Handler: set group description
	mux.HandleFunc("/api/set_group_description", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupDescriptionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		// whatsmeow.SetGroupDescription envía el nodo sin versionar el cambio y el server
		// responde 409 conflict. En WhatsApp el "topic" ES la descripción del grupo, y
		// SetGroupTopic (con previous/new id vacíos) sí maneja el versionado, igual que el
		// handler set_group_topic. Por eso reusamos SetGroupTopic aquí.
		if err := client.SetGroupTopic(context.Background(), jid, "", "", req.Description); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "group description updated"})
	}))

	// Handler: set group announce (true = solo admins pueden enviar mensajes)
	mux.HandleFunc("/api/set_group_announce", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupToggleRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		if err := client.SetGroupAnnounce(context.Background(), jid, req.Enable); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "group announce updated"})
	}))

	// Handler: set group locked (true = solo admins pueden editar info del grupo)
	mux.HandleFunc("/api/set_group_locked", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupToggleRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		if err := client.SetGroupLocked(context.Background(), jid, req.Enable); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "group locked updated"})
	}))

	// Handler: set group photo (lee la imagen del path; WhatsApp requiere JPEG)
	mux.HandleFunc("/api/set_group_photo", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetGroupPhotoRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		// Misma proteccion que el envio de media: sin esto un caller podria leer
		// cualquier archivo del disco (incluida la sesion en store/) y subirlo.
		// Se lee de la ruta canonica validada (no del string original) para cerrar
		// la ventana TOCTOU entre la validacion y la lectura.
		resolvedImage, err := svc.ValidateMediaPath(req.ImagePath)
		if err != nil {
			respondErr(w, http.StatusBadRequest, fmt.Sprintf("invalid image_path: %v", err))
			return
		}
		avatar, err := os.ReadFile(resolvedImage)
		if err != nil {
			respondErr(w, http.StatusBadRequest, fmt.Sprintf("cannot read image: %v", err))
			return
		}
		pictureID, err := client.SetGroupPhoto(context.Background(), jid, avatar)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "group photo updated", "picture_id": pictureID})
	}))

	// --- Lote B2: presencia ---

	// Handler: set own presence (available/unavailable). available es requisito para RECIBIR
	// la presencia de otros.
	mux.HandleFunc("/api/set_presence", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req SetPresenceRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		var p types.Presence
		switch req.State {
		case "available":
			p = types.PresenceAvailable
		case "unavailable":
			p = types.PresenceUnavailable
		default:
			respondErr(w, http.StatusBadRequest, "state must be available/unavailable")
			return
		}
		if err := client.SendPresence(context.Background(), p); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "presence sent", "state": req.State})
	}))

	// Handler: subscribe to a contact's presence (necesario para recibir su online/last-seen)
	mux.HandleFunc("/api/subscribe_presence", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BusinessProfileRequest // {jid}
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.JID, "jid")
		if !ok {
			return
		}
		if err := client.SubscribePresence(context.Background(), jid); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "subscribed to presence"})
	}))

	// Handler: get last known presence of a contact (del tracker en memoria)
	mux.HandleFunc("/api/get_presence", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req BusinessProfileRequest // {jid}
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.JID, "jid")
		if !ok {
			return
		}
		info, ok := svc.GetPresence(jid)
		if !ok {
			respondOK(w, map[string]interface{}{"tracked": false, "message": "sin datos de presencia aún (subscribe_presence + esperar a que cambie de estado)"})
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
	mux.HandleFunc("/api/group_info_from_invite", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req InviteActionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		gjid, inviter, code, exp, err := svc.LoadGroupInvite(req.ChatJID, req.InviteMessageID)
		if err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
		info, err := client.GetGroupInfoFromInvite(context.Background(), gjid, inviter, code, exp)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{
			"success": true, "group_jid": info.JID.String(),
			"name": info.Name, "participant_count": len(info.Participants),
		})
	}))

	// Handler: join group with invite (unirse por código de invitación)
	mux.HandleFunc("/api/join_group_with_invite", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req InviteActionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		gjid, inviter, code, exp, err := svc.LoadGroupInvite(req.ChatJID, req.InviteMessageID)
		if err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := client.JoinGroupWithInvite(context.Background(), gjid, inviter, code, exp); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "joined group via invite", "group_jid": gjid.String()})
	}))

	// --- Lote A3: solicitudes de ingreso a grupos (requieren admin) ---

	// Handler: set group join approval mode (true = los ingresos requieren aprobación de admin)
	mux.HandleFunc("/api/set_group_join_approval", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupToggleRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		if err := client.SetGroupJoinApprovalMode(context.Background(), jid, req.Enable); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "join approval mode updated"})
	}))

	// Handler: get group join requests (solicitudes pendientes de ingreso)
	mux.HandleFunc("/api/group_join_requests", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req GroupActionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		reqs, err := client.GetGroupRequestParticipants(context.Background(), jid)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]map[string]interface{}, 0, len(reqs))
		for _, p := range reqs {
			out = append(out, map[string]interface{}{"jid": p.JID.String(), "requested_at": p.RequestedAt.Format(time.RFC3339)})
		}
		respondOK(w, map[string]interface{}{"requests": out, "count": len(out)})
	}))

	// Handler: review group join request (approve/reject)
	mux.HandleFunc("/api/review_group_join_request", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req UpdateParticipantsRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		jid, ok := parseJID(w, req.GroupJID, "group_jid")
		if !ok {
			return
		}
		var action whatsmeow.ParticipantRequestChange
		switch req.Action {
		case "approve":
			action = whatsmeow.ParticipantChangeApprove
		case "reject":
			action = whatsmeow.ParticipantChangeReject
		default:
			respondErr(w, http.StatusBadRequest, "action must be approve/reject")
			return
		}
		parts, err := wa.ParseParticipantJIDs(req.Participants)
		if err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(parts) == 0 {
			respondErr(w, http.StatusBadRequest, "at least one participant required")
			return
		}
		result, err := client.UpdateGroupRequestParticipants(context.Background(), jid, parts, action)
		if err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		results := make([]map[string]interface{}, 0, len(result))
		for _, p := range result {
			results = append(results, map[string]interface{}{"jid": p.JID.String(), "error_code": p.Error})
		}
		respondOK(w, map[string]interface{}{"action": req.Action, "results": results})
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

	// --- Logout: desvincula la sesión (requiere re-escanear QR para volver) ---
	mux.HandleFunc("/api/logout", withAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := client.Logout(context.Background()); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		svc.OnLoggedOut("logout solicitado por el usuario")
		respondOK(w, map[string]interface{}{"message": "logged out; reiniciar el bridge y re-escanear el QR para volver a vincular"})
	}))

	return mux
}
