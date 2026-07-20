// Package api: rutas del dominio GRUPOS (crear, listar, participantes, invitaciones,
// administración y solicitudes de ingreso). Extraídas de server.go por dominio;
// movimiento mecánico sin cambios de lógica ni de contrato HTTP.
package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"

	"whatsapp-client/internal/wa"
)

// registerGroupRoutes registra las rutas de grupos sobre el mux. Los handlers
// capturan svc/client/token por closure, igual que cuando vivían en NewServer.
func registerGroupRoutes(mux *http.ServeMux, svc *wa.Service, client *whatsmeow.Client, token string) {
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
		code := parseInviteCode(req.Code) // aceptar link completo (con ?query) o solo el codigo
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
}
