// Package wa: dispatcher de eventos entrantes de WhatsApp. Es lógica de dominio
// (decide QUÉ hacer con cada evento), así que vive junto a los handlers de events.go
// en vez de en main.go; main solo registra client.AddEventHandler(svc.HandleEvent).
package wa

import (
	"fmt"
	"runtime/debug"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

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

// HandleEvent es el dispatcher central de eventos de whatsmeow: se registra con
// client.AddEventHandler(svc.HandleEvent). Los eventos no contemplados son no-op.
func (s *Service) HandleEvent(evt interface{}) {
	logger := s.Log
	switch v := evt.(type) {
	case *events.Message:
		// Voto de encuesta entrante: descifrar y registrar (goroutine; no bloquea el dispatch).
		if v.Message.GetPollUpdateMessage() != nil {
			goSafe(logger, "handlePollVote", func() { s.HandlePollVote(v) })
		}
		// Process regular messages
		s.HandleMessage(v)

	case *events.HistorySync:
		// Procesar en goroutine para NO bloquear el dispatch de eventos en vivo.
		// El history-sync puede tardar minutos (cientos de mensajes + lookups de red);
		// si corre sincronico en el handler, los *events.Message en vivo quedan
		// encolados detras y no se guardan hasta que termina (bug observado).
		goSafe(logger, "handleHistorySync", func() { s.HandleHistorySync(v) })

	case *events.Receipt:
		// T3-3: read-receipt PROPIO (leí el chat desde el teléfono u otro device) ->
		// marcar ese chat como leído. (ReceiptTypeRead = otros leyeron MIS mensajes, no aplica.)
		if v.Type == types.ReceiptTypeReadSelf {
			if n, _ := s.Store.ClearChatUnread(v.Chat.String()); n > 0 {
				logger.Infof("Chat %s marcado como leído (read-self): %d no-leídos limpiados", v.Chat.String(), n)
			}
		}

	case *events.Connected:
		logger.Infof("Connected to WhatsApp")
		s.OnConnected()

	case *events.Disconnected:
		// EnableAutoReconnect (configurado en main) ya gestiona la reconexion. Un reconnect
		// manual aqui competiria con el interno de whatsmeow (race / StreamReplaced).
		logger.Warnf("Disconnected from WhatsApp; auto-reconnect en curso...")
		s.OnDisconnected()

	case *events.LoggedOut:
		logger.Warnf("Device logged out (reason=%s, onConnect=%v); please scan QR code to log in again", v.Reason.String(), v.OnConnect)
		s.OnLoggedOut(v.Reason.String())

	case *events.TemporaryBan:
		// 🔴 Ban temporal: loggear fuerte y pausar envios (svc.SendMessage chequea isTempBanned).
		logger.Errorf("⚠️ TEMPORARY BAN: code=%d (%s); expira en %s. ENVIOS PAUSADOS.", int(v.Code), v.Code.String(), v.Expire)
		s.OnTempBan(int(v.Code), v.Code.String(), v.Expire)

	case *events.ConnectFailure:
		logger.Errorf("Connect failure: reason=%d (%s) msg=%s", int(v.Reason), v.Reason.String(), v.Message)
		s.OnConnectFailure(fmt.Sprintf("%d %s: %s", int(v.Reason), v.Reason.String(), v.Message))
		if v.Reason.IsLoggedOut() {
			s.OnLoggedOut(v.Reason.String())
		}

	case *events.Presence:
		// Presencia de terceros: online/offline + last-seen (requiere SubscribePresence + SendPresence).
		s.OnPresenceEvent(v.From, v.Unavailable, v.LastSeen)

	case *events.ChatPresence:
		// Typing de terceros (composing/paused) en un chat.
		s.OnChatPresenceEvent(v.Sender, v.State == types.ChatPresenceComposing)

	case *events.CallOffer:
		// Llamada entrante: solo se registra (whatsmeow no maneja audio). Sin auto-rechazo.
		goSafe(logger, "handleCallOffer", func() { s.HandleCallOffer(v) })
	}
}
