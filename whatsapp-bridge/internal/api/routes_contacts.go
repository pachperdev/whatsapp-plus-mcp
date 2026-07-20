// Package api: rutas del dominio CONTACTOS/IDENTIDAD/PRESENCIA (verificación de
// números, perfiles, dispositivos, bloqueo y presencia propia/de terceros).
// Extraídas de server.go por dominio; movimiento mecánico sin cambios de lógica.
package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatsapp-client/internal/wa"
)

// registerContactRoutes registra las rutas de contactos/identidad/presencia sobre el
// mux. Los handlers capturan svc/client/token por closure, igual que en NewServer.
func registerContactRoutes(mux *http.ServeMux, svc *wa.Service, client *whatsmeow.Client, token string) {
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

	// --- Lote A1: perfil & cuenta (parte de contactos) ---

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
}
