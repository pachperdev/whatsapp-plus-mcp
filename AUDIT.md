# WhatsApp MCP — Estado y Plan de Trabajo

Fork `mauricioDevApp/whatsapp-mcp`. Bridge Go (`whatsmeow`) + server Python (MCP).
Última actualización: 2026-06-23.

---

## ⚠️ Riesgo de ban — LEER antes de agregar funcionalidad

Este MCP opera sobre una **cuenta de WhatsApp personal real** con `whatsmeow`, un cliente
**NO oficial** (reverse-engineered de WhatsApp Web). **Usarlo viola los ToS de WhatsApp** y
puede costar el número. No es paranoia: está documentado.

- **Reactivo (responder a quien te escribe): <2% de ban/año. Proactivo (escribir primero,
  outreach en frío, envío en lote): 15–30% de ban/año.** Esta es la decisión que más mueve la aguja.
- La detección principal es **fingerprint de protocolo** (capa 1): no se evita desde el cliente,
  ni con delays. Se mitiga el resto (comportamiento, reportes) pero el riesgo residual existe.
- **Reglas duras:**
  - NO mensajería proactiva / outreach en frío / envío en lote.
  - NO el mismo texto repetido (dispara `TempBanSentTooManySameMessage=104`).
  - Una cuenta = una sesión = una IP (no correr varias instancias).
  - Mantener `whatsmeow` actualizado (versión vieja → `ClientOutdated 405` / `BadUserAgent 409`).
  - Pro-humano: `typing` + `mark_read` + jitter real antes de responder.
- **Códigos de ban (fuente primaria, `whatsmeow/types/events`):** `TempBan` 101/102/103/104/106;
  `ConnectFailure` 401 LoggedOut, 402 TempBanned, 403 MainDeviceGone, 405 ClientOutdated, 409 BadUserAgent.
- whatsmeow **no soporta**: llamadas voz/video, broadcast lists. Status = experimental.

➡️ Por esto, **manejar los eventos de ban (Fase 2) es casi tan prioritario como las features**:
hoy el bridge no escucha `TemporaryBan`/`ConnectFailure`/`LoggedOut` → vuela a ciegas.

Fuentes: pkg.go.dev/go.mau.fi/whatsmeow · github.com/tulir/whatsmeow/discussions/567 · whatsapp.com/legal/messaging-guidelines

---

## ✅ Ya hecho (ver `git log`; 20 commits)

**Base/infra:** T1 captura en vivo · T2 WAL · T3 salientes persistidos · T4 timeout · T5 logging stderr ·
T6 seguridad (loopback+token+sandbox) · T7 cache TTL · T8 salida estructurada ·
resolución de nombres + unificación lid/número (list_chats, list_messages, search_contacts,
get_direct_chat_by_contact) · no-críticos M1/M3/M4/M7 + limpieza (rand/min/temp/makedirs).

**FASE 1 Tier 1 — COMPLETO:** edit_message, delete_message (lo dispara el usuario), send_typing,
check_whatsapp, get_profile_picture, get_user_info, list_all_contacts, send_poll,
get_group_participants, get_group_invite_link, join_group, leave_group, set_group_name,
set_group_topic, block_contact/unblock_contact (RESUELTO el 400 vía `blockViaLID`).

**FASE 1 Tier 2 — COMPLETO:** ✅ reply/quote (`send_message` reply_to) · ✅ estado de chat
(mute/pin/archive/mark/star/get_chat_settings, commit `04a3e87`) · ✅ request_more_history
(on-demand history sync, best-effort, commit `e88ec56`) · ✅ menciones (@tag en send_message,
combinables con reply, commit `a30d453`) · ✅ crear grupo + gestionar participantes
(create_group, update_group_participants add/remove/promote/demote, commit `280a5d4`) ·
✅ set_disappearing_messages (off/24h/7d/90d, commit `e193a13`).

➡️ **FASE 1 (Tier 1 + Tier 2) cerrada.** Próximo foco: **FASE 2 — Robustez** (lo más prioritario:
🔴 manejo de eventos de ban + `/api/status`). Tier 3 (captura no-texto, presencia, etc.) queda como ampliación opcional de FASE 1.

**Tools actuales (43):** search_contacts, list_messages, list_chats, get_chat,
get_direct_chat_by_contact, get_contact_chats, get_last_interaction, get_message_context,
send_message (con reply_to), send_file, send_audio_message, download_media, list_groups, mark_as_read,
react_to_message, refresh_contacts, edit_message, delete_message, send_typing, check_whatsapp,
get_profile_picture, get_user_info, list_all_contacts, send_poll, get_group_participants,
get_group_invite_link, join_group, leave_group, set_group_name, set_group_topic,
block_contact, unblock_contact, mute_chat, pin_chat, archive_chat, mark_chat, star_message,
get_chat_settings, request_more_history, create_group, update_group_participants,
set_disappearing_messages, get_status.

---

## 🗺️ Plan de trabajo (orden acordado)

> Orden: **Fase 1 Funcionalidades → Fase 2 Robustez → Fase 3 Performance → Fase 4 Calidad/infra.**
> La Fase 4 (tests/CI) se hace al final, cuando esté todo pulido a nivel funcional.

### FASE 1 — Funcionalidades (tools nuevas)  ✅ Tier 1 + Tier 2 COMPLETOS

Patrón clave: las tools basadas en `Build*` (Edit/Revoke/Poll/Reaction) o método directo del
Client son **wrappers de baja complejidad** (handler REST en el bridge + tool Python).

**Tier 1 — quick wins (valor alto · complejidad baja):**
| Tool | API whatsmeow |
|---|---|
| `edit_message` | `BuildEdit(chat, id, newContent)` (ventana ~20 min) |
| `delete_message` (endpoint; lo ejecuta el usuario) | `BuildRevoke(chat, sender, id)` |
| `send_typing` (✋ mejora verosimilitud humana) | `SendChatPresence(jid, composing/paused, media)` |
| `check_whatsapp` | `IsOnWhatsApp(phones[])` |
| `get_profile_picture` | `GetProfilePictureInfo(jid, params)` |
| `get_user_info` (about/status, nombre verificado) | `GetUserInfo(jids[])` |
| `list_all_contacts` | `Store.Contacts.GetAllContacts()` |
| `send_poll` | `BuildPollCreation(name, options[], multi)` |
| `get_group_participants` (exponer `Participants` ya disponible) | sobre `GetGroupInfo` |
| `get_group_invite_link` / `join_group` / `leave_group` | `GetGroupInviteLink` / `JoinGroupWithLink` / `LeaveGroup` |
| `set_group_name` / `set_group_topic` | `SetGroupName` / `SetGroupTopic` |
| `block_contact` / `unblock_contact` | `UpdateBlocklist(jid, action)` |

**Tier 2 — alto valor · complejidad media:**
| Tool | API whatsmeow |
|---|---|
| ✅ `send_message` con **reply/quote** + **menciones** (commit `a30d453`, RESUELTO) | `ContextInfo` único combina QuotedMessage (reply) + MentionedJID (menciones). `resolveMentions` auto-detecta `@<número>` en el texto y acepta JIDs explícitos. Menciones por número se renderizan resaltadas. |
| ✅ Estado de chat: `mute_chat` / `pin_chat` / `archive_chat` / `mark_chat` (read/unread) / `star_message` / `get_chat_settings` (commit `04a3e87`, RESUELTO) | `appstate.Build*` + `SendAppState` + `Store.ChatSettings.GetChatSettings`. Nota fix: `BuildStar` mapea `sender==target → "0"` en el index; en directos/mensajes propios se pasa el chat como sender (sin esto la estrella no se ve). `mark_chat` con read=false solo pinta badge si el último mensaje es entrante. |
| ✅ `create_group` / `update_group_participants` (add/remove/promote/demote) (commit `280a5d4`, RESUELTO) | `CreateGroup` (nombre ≤25 chars, no incluir el JID propio) / `UpdateGroupParticipants`. Validado en vivo: crear con N participantes, promote/demote, remove/add. **Nota op:** para *eliminar* un grupo hay que remover a todos los participantes y recién después salir — si salís primero, ya no podés administrarlo. |
| ✅ `request_more_history` (commit `e88ec56`, RESUELTO) | peer message al propio JID con `Peer:true` (NO a `status@s.whatsapp.net`, eso cuelga en usync). **Best-effort**: WhatsApp es E2E, el server no guarda historial; lo sirve el teléfono primario (debe estar online) y solo lo que él conserve. Validado: el teléfono respondió eventos ON_DEMAND, `Stored 0` por no tener mensajes más viejos. |
| ✅ `set_disappearing_messages` (commit `e193a13`, RESUELTO) | `SetDisappearingTimer` + `ParseDisappearingTimerString` (valida presets off/24h/7d/90d, rechaza el resto). Directos y grupos. Validado en vivo. |

**Tier 3 — requieren event handler nuevo o estado en SQLite (mayor esfuerzo):**
- 🔴 **Captura de mensajes NO-texto** (detectado 2026-06-23): `handleMessage`/`extractMediaInfo` solo guardan image/video/audio/document. **Se descartan: stickers, ubicación, contactos (vCard), polls, botones/listas, mensajes de sistema** → no quedan en la DB (no se pueden listar, citar con `reply`, ni descargar). Ampliar `extractMediaInfo` + `extractTextContent` para soportar `StickerMessage`, `LocationMessage`, `LiveLocationMessage`, `ContactMessage`/`ContactsArrayMessage`, `PollCreationMessage`. Además, `buildQuotedContext` solo cita texto (`Conversation`); citar un sticker/imagen requeriría reconstruir ese `QuotedMessage`.
- `get_unread_chats` — procesar `events.Receipt` y trackear read-state en SQLite (no hay "unread count" directo).
- Capturar **edits/revokes entrantes** (`events.Message.IsEdit`, `ProtocolMessage` REVOKE) → reflejar en la DB.
- Presencia de terceros (`SubscribePresence` + handler `events.Presence` last-seen/online).
- Votos de encuesta entrantes (`DecryptPollVote`).

### FASE 2 — Robustez / correctness
- ✅ **Manejo de eventos de ban** (commit `761b532`, RESUELTO): el handler captura `events.TemporaryBan` (loggea code+reason+expire y **pausa envíos** vía guard `isTempBanned` en `sendWhatsAppMessage`), `events.ConnectFailure` (registra fallo; `IsLoggedOut()` → marca logout), `events.LoggedOut` (guarda razón), `events.Connected`/`Disconnected` (timestamps). Estado thread-safe (`botStatus` + `sync.RWMutex`). **Validado en vivo:** corte de red (Disconnected+reconnect), cierre de sesión (LoggedOut→needs_qr), re-vinculación por QR.
- ✅ **`/api/status` + tool `get_status`** (commit `761b532`): connected, logged_in, jid, temp_banned (code/reason/expires_at), needs_qr, last_connect_failure, timestamps. *Helper de re-vinculación: el bridge loggea el code crudo del QR (`QR_RAW`) → se puede generar el QR como imagen (PNG) y abrirlo, sin depender del ASCII en terminal.*
- 🔲 Procesar `events.Receipt` (delivered/read) → saber si leyeron + base para `get_unread_chats`.
- **M6** `_load_contact_index` ignora `our_jid` (latente con multi-cuenta).
- ✅ **`block_contact`/`unblock_contact`** RESUELTO (commit `05509a4`): WhatsApp cambió el protocolo de blocklist (el `<item>` va por **LID + `pn_jid` + `dhash`**, no por número); `UpdateBlocklist` de whatsmeow envía el formato viejo → 400 (fix upstream en PR #1137, sin mergear). Workaround `blockViaLID`: resuelve LID (`GetLIDForPN` → fallback `GetUserInfo`) + `pn_jid`, arma el IQ nuevo vía `DangerousInternals().SendNode` y verifica con `GetBlocklist`. Validado en vivo (block + unblock). *Si whatsmeow mergea #1137, volver a `UpdateBlocklist` nativo.*
- Menores: adaptador `datetime`→str deprecado en Python 3.12; paginación `OFFSET` sin tie-breaker.
- Alcance: `mark_as_read`/`react`/sandbox de media asumen chats directos; grupos = sender acotado.

### FASE 3 — Performance
- **M2** batch transaction en history-sync (hoy inserts uno por uno; ya mitigado por T1+T2, es optimización).
- Migrar driver SQLite a `modernc.org/sqlite` (puro Go, **sin CGO**) → builds reproducibles y cross-compile triviales. Penalidad de runtime irrelevante a esta escala. Cambio: import + `sql.Open("sqlite", ...)`.
- Reusar conexión `httpx`/cliente al bridge (lifespan) en el server.

### FASE 4 — Calidad / infra (AL FINAL)
- **Actualizar el SDK `mcp` 1.6.0 → `>=1.28,<2`** (`uv lock --upgrade`). Desbloquea lo de abajo. (El bridge Go ya está al día.)
- **Structured output (Pydantic):** convertir `Message/Chat/Contact/MessageContext` a modelos y tipar los retornos de las tools → schema explícito para el LLM. *Mayor ROI de calidad.*
- **Resources** (`whatsapp://chats`, `whatsapp://contacts`) navegables + 2-3 **prompts** de flujos comunes.
- **Tests** automatizados (Go + Python) + **CI** (GitHub Actions en el fork: build + test + lint) + **README** actualizado + linters (golangci-lint, ruff).

---

## 📚 Referencia rápida — capacidades whatsmeow (de la investigación)

`Build*` → se mandan con `SendMessage` (mismo patrón que reactions ya implementado):
`BuildEdit`, `BuildRevoke`, `BuildPollCreation`, `BuildPollVote`.
Estado de chat: `appstate.BuildMute/BuildPin/BuildArchive/BuildMarkChatAsRead/BuildStar` + `client.SendAppState`.
Lectura de estado: `Store.ChatSettings.GetChatSettings` → `{MutedUntil, Pinned, Archived}`.
Directos: `SendChatPresence`, `SendPresence`, `SubscribePresence`, `IsOnWhatsApp`, `GetProfilePictureInfo`,
`GetUserInfo`, `GetBusinessProfile`, `SetStatusMessage`, `UpdateBlocklist`, `GetBlocklist`, `SetDisappearingTimer`.
Grupos: `CreateGroup`, `UpdateGroupParticipants`, `SetGroupName/Topic/Photo/Announce/Locked`,
`GetGroupInviteLink`, `JoinGroupWithLink`, `GetGroupInfoFromLink`, `LeaveGroup`, `GetGroupRequestParticipants`.
Contactos: `Store.Contacts.GetAllContacts`, `GetUserDevices`.
Eventos a procesar: `events.Receipt`, `events.ChatPresence`, `events.Presence`, `events.TemporaryBan`,
`events.ConnectFailure`, `events.LoggedOut`, `events.Message{IsEdit}`.

## 📦 Estado de dependencias
- Bridge Go: **al día** (whatsmeow 2026-06-22, Go 1.25, protobuf 1.36.11). Refrescar con `go get -u` periódicamente.
- Server Python: SDK **`mcp` 1.6.0 desactualizado → 1.28.0** (subir en Fase 4, tope `<2`). Resto al día.
