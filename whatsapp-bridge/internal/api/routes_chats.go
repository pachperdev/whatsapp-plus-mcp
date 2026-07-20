// Package api: rutas del dominio CHATS (estado del chat: archivar, fijar, silenciar,
// leído/no-leído, ajustes, mensajes temporales, no-leídos e historial). Extraídas de
// server.go por dominio; movimiento mecánico sin cambios de lógica ni de contrato.
package api

import (
	"context"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"

	"whatsapp-client/internal/store"
	"whatsapp-client/internal/wa"
)

// registerChatRoutes registra las rutas de estado de chat sobre el mux. Los handlers
// capturan svc/client/st/token por closure, igual que cuando vivían en NewServer.
func registerChatRoutes(mux *http.ServeMux, svc *wa.Service, client *whatsmeow.Client, st *store.MessageStore, token string) {
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
		if err := sendAppState(client, appstate.BuildMute(jid, req.Enable, time.Duration(req.Duration)*time.Hour)); err != nil {
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
		if err := sendAppState(client, appstate.BuildPin(jid, req.Enable)); err != nil {
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
		if err := sendAppState(client, appstate.BuildArchive(jid, req.Enable, ts, key)); err != nil {
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
		if err := sendAppState(client, appstate.BuildMarkChatAsRead(jid, req.Enable, ts, key)); err != nil {
			respondErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondOK(w, map[string]interface{}{"message": "chat read-state updated"})
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
}
