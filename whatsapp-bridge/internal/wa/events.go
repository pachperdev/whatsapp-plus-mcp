// Package wa: este archivo agrupa los handlers de eventos entrantes de WhatsApp como
// métodos de *Service. Persisten a la DB el mensaje/edit/revoke, el history-sync, los
// votos de encuesta y las llamadas entrantes. Se invocan desde el dispatcher de main.
package wa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatsapp-client/internal/store"
)

// HandleMessage procesa un mensaje entrante (o edit/revoke) y lo persiste en la DB.
func (s *Service) HandleMessage(msg *events.Message) {
	client := s.Client
	messageStore := s.Store
	logger := s.Log
	// Save message to database
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	// T3-2: edits y revokes entrantes -> se reflejan in-place en la DB SIN reordenar el
	// historial (no se toca el timestamp ni el last_message_time del chat).
	tsLog := msg.Info.Timestamp.Format("2006-01-02 15:04:05")

	// Revoke ("borrar para todos"): llega como ProtocolMessage REVOKE intacto.
	if pm := msg.Message.GetProtocolMessage(); pm != nil && pm.GetType() == waE2E.ProtocolMessage_REVOKE {
		if targetID := pm.GetKey().GetID(); targetID != "" {
			if _, err := messageStore.MarkMessageRevoked(targetID, chatJID); err != nil {
				logger.Warnf("Failed to apply revoke for %s: %v", targetID, err)
			} else {
				fmt.Printf("[%s] 🗑️ %s eliminó el mensaje %s\n", tsLog, sender, targetID)
			}
		}
		return
	}

	// Edit moderno: llega como SecretEncryptedMessage (contenido CIFRADO) con
	// secretEncType=MESSAGE_EDIT y targetMessageKey apuntando al mensaje original.
	// Hay que descifrarlo con la clave secreta del mensaje target (igual que poll votes).
	if sec := msg.Message.GetSecretEncryptedMessage(); sec != nil {
		// binary/proto no re-exporta la constante del enum; comparamos por nombre.
		if sec.GetSecretEncType().String() == "MESSAGE_EDIT" {
			targetID := sec.GetTargetMessageKey().GetID()
			if decrypted, err := client.DecryptSecretEncryptedMessage(context.Background(), msg); err != nil {
				logger.Warnf("Failed to decrypt edit for %s: %v", targetID, err)
			} else if newText := ExtractEditedText(decrypted); targetID != "" && newText != "" {
				if n, err := messageStore.ApplyMessageEdit(targetID, chatJID, newText); err != nil {
					logger.Warnf("Failed to apply edit for %s: %v", targetID, err)
				} else if n > 0 {
					fmt.Printf("[%s] ✏️ %s editó el mensaje %s\n", tsLog, sender, targetID)
				}
			}
		}
		return
	}

	// Edit ya desenvuelto (history-sync/webMsg): IsEdit=true, msg.Info.ID = original.
	if msg.IsEdit {
		if newText := ExtractTextContent(msg.Message); newText != "" {
			if n, err := messageStore.ApplyMessageEdit(msg.Info.ID, chatJID, newText); err != nil {
				logger.Warnf("Failed to apply edit for %s: %v", msg.Info.ID, err)
				return
			} else if n > 0 {
				fmt.Printf("[%s] ✏️ %s editó el mensaje %s\n", tsLog, sender, msg.Info.ID)
				return
			}
			// n == 0: no teníamos el original -> cae al flujo normal e inserta el texto editado.
		} else {
			return
		}
	}

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := s.GetChatName(msg.Info.Chat, chatJID, nil, sender)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := ExtractTextContent(msg.Message)

	// Extract media info (el ID participa en el filename generado para evitar colisiones).
	mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength := ExtractMediaInfo(msg.Message, msg.Info.ID)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		directPath,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// T3-3: tracking de no leídos. Entrante -> no leído; propio (respondí/envié desde
		// cualquier device) -> el chat queda leído. Se excluyen Novedades/newsletters
		// (no son chats conversacionales).
		isTrackable := chatJID != "status@broadcast" && !strings.HasSuffix(chatJID, "@newsletter")
		if msg.Info.IsFromMe {
			_, _ = messageStore.ClearChatUnread(chatJID)
		} else if isTrackable {
			_ = messageStore.AddUnread(chatJID, msg.Info.ID, msg.Info.Timestamp)
		}

		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log based on message type
		if mediaType != "" {
			fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
		} else if content != "" {
			fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
		}
	}
}

// HandlePollVote descifra un voto de encuesta entrante y lo registra. Los votos llegan como
// hashes SHA256 de las opciones; se mapean a los nombres legibles usando las opciones del poll
// original (guardadas en la DB con el poll, en el campo filename como JSON). Se persiste el voto
// como un mensaje "poll_vote" para que sea consultable via list_messages.
func (s *Service) HandlePollVote(evt *events.Message) {
	client := s.Client
	logger := s.Log
	vote, err := client.DecryptPollVote(context.Background(), evt)
	if err != nil {
		logger.Warnf("poll vote: no se pudo descifrar: %v", err)
		return
	}
	if s.Store == nil {
		return
	}
	pollID := evt.Message.GetPollUpdateMessage().GetPollCreationMessageKey().GetID()
	var optsJSON string
	_ = s.Store.DB().QueryRow("SELECT filename FROM messages WHERE id = ? AND media_type = 'poll' LIMIT 1", pollID).Scan(&optsJSON)
	var pollOptions []string
	_ = json.Unmarshal([]byte(optsJSON), &pollOptions)

	selected := vote.GetSelectedOptions()
	var voted []string
	for _, name := range pollOptions {
		h := whatsmeow.HashPollOptions([]string{name})
		if len(h) == 0 {
			continue
		}
		for _, sel := range selected {
			if bytes.Equal(h[0], sel) {
				voted = append(voted, name)
			}
		}
	}
	label := strings.Join(voted, ", ")
	if label == "" {
		label = fmt.Sprintf("(%d opcion(es); poll original no capturado, no se pudo mapear)", len(selected))
	}
	logger.Infof("🗳️ Voto de %s en poll %s: %s", evt.Info.Sender.String(), pollID, label)

	// Persistir para que sea consultable via list_messages (TouchChat asegura el FK del chat).
	_ = s.Store.TouchChat(evt.Info.Chat.String(), evt.Info.Timestamp)
	if err := s.Store.StoreMessage(evt.Info.ID, evt.Info.Chat.String(), evt.Info.Sender.User,
		"🗳️ votó: "+label, evt.Info.Timestamp, evt.Info.IsFromMe,
		"poll_vote", "", "", "", nil, nil, nil, 0); err != nil {
		logger.Warnf("poll vote: no se pudo persistir: %v", err)
	}
}

// HandleCallOffer registra una llamada entrante como un mensaje "call" (whatsmeow no puede
// atender llamadas; solo las detecta). Queda consultable via list_messages. No se rechaza.
func (s *Service) HandleCallOffer(evt *events.CallOffer) {
	logger := s.Log
	caller := evt.CallCreator
	if caller.User == "" {
		caller = evt.From
	}
	logger.Infof("📞 Llamada entrante de %s (call %s)", caller.String(), evt.CallID)
	if s.Store == nil {
		return
	}
	chatJID := caller.String()
	if !evt.GroupJID.IsEmpty() {
		chatJID = evt.GroupJID.String() // llamada grupal
	}
	_ = s.Store.TouchChat(chatJID, evt.Timestamp)
	if err := s.Store.StoreMessage("CALL_"+evt.CallID, chatJID, caller.User, "📞 Llamada entrante",
		evt.Timestamp, false, "call", "", "", "", nil, nil, nil, 0); err != nil {
		logger.Warnf("call offer: no se pudo registrar: %v", err)
	}
}

// HandleHistorySync procesa un evento de history-sync y persiste sus conversaciones/mensajes.
func (s *Service) HandleHistorySync(historySync *events.HistorySync) {
	client := s.Client
	messageStore := s.Store
	logger := s.Log
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		// Get appropriate chat name by passing the history sync conversation directly
		name := s.GetChatName(jid, chatJID, conversation, "")

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			ts := latestMsg.Message.GetMessageTimestamp()
			if ts == 0 {
				continue
			}
			timestamp := time.Unix(int64(ts), 0)

			// Batch: una transaccion por conversacion -> 1 fsync en vez de N (WAL +
			// synchronous=NORMAL). Se abre DESPUES de GetChatName (la unica lectura de esta
			// iteracion): con SetMaxOpenConns(1) una tx abierta toma la unica conexion y
			// bloquearia cualquier lectura via store.db (deadlock).
			tx, err := messageStore.DB().Begin()
			if err != nil {
				logger.Warnf("history sync: no se pudo iniciar tx para %s: %v", chatJID, err)
				continue
			}
			if err := store.StoreChatExec(tx, chatJID, name, timestamp); err != nil {
				logger.Warnf("history sync: storeChat fallo (%s): %v", chatJID, err)
			}

			// Store messages (todos sobre la misma tx)
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// El message ID se extrae ANTES de la media porque también participa en el
				// filename generado: en ráfagas de history sync varios mensajes caen en el
				// mismo segundo y sin el sufijo del ID sus filenames colisionaban.
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Extract media info
				var mediaType, filename, url, directPath string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength = ExtractMediaInfo(msg.Message.Message, msgID)
				}

				// Log the message content for debugging
				logger.Infof("Message content: %v, Media Type: %v", content, mediaType)

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						sender = *msg.Message.Key.Participant
					} else if isFromMe {
						sender = client.Store.ID.User
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				// Get message timestamp
				ts := msg.Message.GetMessageTimestamp()
				if ts == 0 {
					continue
				}
				timestamp := time.Unix(int64(ts), 0)

				err = store.StoreMessageExec(
					tx,
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					directPath,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					// Log successful message storage
					if mediaType != "" {
						logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
					} else {
						logger.Infof("Stored message: [%s] %s -> %s: %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
					}
				}
			}
			if err := tx.Commit(); err != nil {
				logger.Warnf("history sync: commit fallo (%s): %v", chatJID, err)
				_ = tx.Rollback()
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}
