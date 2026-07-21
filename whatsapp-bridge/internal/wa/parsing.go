// Package wa contiene la lógica de dominio de WhatsApp del bridge. Este archivo
// agrupa las funciones PURAS de parsing/normalización (sin cliente, store ni estado
// global): extracción de texto y metadata de media desde los protobufs, resolución
// de menciones y de JIDs de participantes. Son la base testeable del dominio.
package wa

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// ExtractTextContent extrae el texto legible de un mensaje (conversación, texto
// extendido, captions, encuesta, invitación, ubicación, contactos). "" si no hay.
func ExtractTextContent(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}

	// Desenrollar wrappers (consistente con ExtractMediaInfo) para no perder el texto adentro.
	if w := msg.GetEphemeralMessage(); w.GetMessage() != nil {
		return ExtractTextContent(w.GetMessage())
	}
	if w := msg.GetViewOnceMessage(); w.GetMessage() != nil {
		return ExtractTextContent(w.GetMessage())
	}
	if w := msg.GetDocumentWithCaptionMessage(); w.GetMessage() != nil {
		return ExtractTextContent(w.GetMessage())
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

	// Ubicación: no hay archivo que descargar; guardamos coords + nombre/dirección legibles.
	if loc := msg.GetLocationMessage(); loc != nil {
		s := fmt.Sprintf("📍 Ubicación: %.6f, %.6f", loc.GetDegreesLatitude(), loc.GetDegreesLongitude())
		if n := loc.GetName(); n != "" {
			s += " — " + n
		}
		if a := loc.GetAddress(); a != "" {
			s += " (" + a + ")"
		}
		return s
	}
	if live := msg.GetLiveLocationMessage(); live != nil {
		s := fmt.Sprintf("📍 Ubicación en vivo: %.6f, %.6f", live.GetDegreesLatitude(), live.GetDegreesLongitude())
		if c := live.GetCaption(); c != "" {
			s += " — " + c
		}
		return s
	}

	// Contacto(s) compartido(s): guardamos nombre + teléfono (parseado del vCard).
	if c := msg.GetContactMessage(); c != nil {
		s := "👤 Contacto: " + c.GetDisplayName()
		if p := vcardPhone(c.GetVcard()); p != "" {
			s += " · " + p
		}
		return s
	}
	if ca := msg.GetContactsArrayMessage(); ca != nil {
		names := make([]string, 0, len(ca.GetContacts()))
		for _, c := range ca.GetContacts() {
			n := c.GetDisplayName()
			if p := vcardPhone(c.GetVcard()); p != "" {
				n += " (" + p + ")"
			}
			names = append(names, n)
		}
		s := fmt.Sprintf("👤 %d contactos", len(ca.GetContacts()))
		if len(names) > 0 {
			s += ": " + strings.Join(names, ", ")
		}
		return s
	}

	return ""
}

// ExtractEditedText obtiene el texto nuevo de un edit descifrado. El plaintext puede venir
// como contenido directo, anidado en ProtocolMessage.EditedMessage, o en un EditedMessage wrapper.
func ExtractEditedText(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	if t := ExtractTextContent(m); t != "" {
		return t
	}
	if pm := m.GetProtocolMessage(); pm != nil {
		if t := ExtractTextContent(pm.GetEditedMessage()); t != "" {
			return t
		}
	}
	if em := m.GetEditedMessage().GetMessage(); em != nil {
		return ExtractTextContent(em)
	}
	return ""
}

// vcardPhone extrae el primer teléfono (línea TEL) de un vCard. "" si no hay.
func vcardPhone(vcard string) string {
	if vcard == "" {
		return ""
	}
	for _, line := range strings.Split(vcard, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), "TEL") {
			if i := strings.LastIndex(line, ":"); i >= 0 && i+1 < len(line) {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}

// mediaFilename genera el nombre de archivo para media capturada:
// "<mediaType>_<YYYYMMDD_HHMMSS>_<id8><ext>". El sufijo id8 (primeros 8 caracteres
// alfanuméricos del message ID) garantiza unicidad: el timestamp solo tiene
// granularidad de segundo y en ráfagas de history sync dos mensajes distintos
// capturados en el mismo segundo colisionaban (mismo filename para bytes distintos).
// Los IDs de WhatsApp ya son alfanuméricos; igual se sanea defensivamente a
// [A-Za-z0-9] y, si queda vacío, cae a un hash corto determinista del ID original.
func mediaFilename(mediaType, ext string, ts time.Time, msgID string) string {
	id8 := make([]byte, 0, 8)
	for i := 0; i < len(msgID) && len(id8) < 8; i++ {
		c := msgID[i]
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') {
			id8 = append(id8, c)
		}
	}
	suffix := string(id8)
	if suffix == "" {
		sum := sha256.Sum256([]byte(msgID))
		suffix = hex.EncodeToString(sum[:4])
	}
	return mediaType + "_" + ts.Format("20060102_150405") + "_" + suffix + ext
}

// ExtractMediaInfo extrae la metadata de media descargable de un mensaje (tipo,
// filename sugerido, url/directPath y claves). El msgID entra en el filename generado
// para que dos mensajes capturados en el mismo segundo no colisionen (ver mediaFilename).
// Para poll/group_invite devuelve el tipo con los datos serializados en filename
// (no son media descargable).
func ExtractMediaInfo(msg *waE2E.Message, msgID string) (mediaType string, filename string, url string, directPath string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", "", nil, nil, nil, 0
	}

	// Desenrollar wrappers que envuelven el mensaje real; sin esto se pierde la media de
	// dentro: mensajes temporales (ephemeral), ver-una-vez (view once) y documento+caption.
	if w := msg.GetEphemeralMessage(); w.GetMessage() != nil {
		return ExtractMediaInfo(w.GetMessage(), msgID)
	}
	if w := msg.GetViewOnceMessage(); w.GetMessage() != nil {
		return ExtractMediaInfo(w.GetMessage(), msgID)
	}
	if w := msg.GetDocumentWithCaptionMessage(); w.GetMessage() != nil {
		return ExtractMediaInfo(w.GetMessage(), msgID)
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", mediaFilename("image", ".jpg", time.Now(), msgID),
			img.GetURL(), img.GetDirectPath(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", mediaFilename("video", ".mp4", time.Now(), msgID),
			vid.GetURL(), vid.GetDirectPath(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", mediaFilename("audio", ".ogg", time.Now(), msgID),
			aud.GetURL(), aud.GetDirectPath(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = mediaFilename("document", "", time.Now(), msgID)
		}
		return "document", filename,
			doc.GetURL(), doc.GetDirectPath(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	// Check for sticker message (estaticos y animados; .webp). whatsmeow los descarga como imagen.
	if stk := msg.GetStickerMessage(); stk != nil {
		return "sticker", mediaFilename("sticker", ".webp", time.Now(), msgID),
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

// mentionRegex detecta menciones @<numero> (7-15 digitos) en el texto del mensaje.
var mentionRegex = regexp.MustCompile(`@(\d{7,15})`)

// ResolveMentions arma la lista de JIDs mencionados a partir de menciones explicitas
// (numeros o JIDs) y de auto-detectar @<numero> en el texto. Dedup conservando orden.
func ResolveMentions(text string, explicit []string) []string {
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

// ParseParticipantJIDs convierte una lista de numeros o JIDs a []types.JID
// (un numero suelto se interpreta como <numero>@s.whatsapp.net).
func ParseParticipantJIDs(raw []string) ([]types.JID, error) {
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

// ExtractDirectPathFromURL reconstruye el direct path de una URL de media de WhatsApp.
// Solo fallback para mensajes viejos sin direct_path nativo.
func ExtractDirectPathFromURL(url string) string {
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
