package api

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
