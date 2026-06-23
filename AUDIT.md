# Auditoría WhatsApp MCP — 2026-06-22

Auditoría a fondo de `whatsapp-bridge` (Go, 1367 ln) + `whatsapp-mcp-server` (Python, ~1200 ln) y comparación con el upstream `lharries/whatsapp-mcp`.

---

## ✅ Estado de implementación (2026-06-22, fork `mauricioDevApp/whatsapp-mcp`)

Resueltos en esta tanda de mantenimiento — todos commiteados en `main` y validados:

| Ítem | Estado | Commit |
|---|---|---|
| whatsmeow 2026 + fix 405 | ✅ | `dec7d74` |
| Resolución de nombres (lid→nombre) | ✅ validado | `303b9d5` |
| **T1** captura en vivo (history-sync async) | ✅ validado en vivo | `46eb0bf` |
| **T2** SQLite WAL + busy_timeout | ✅ validado en vivo | `46eb0bf` |
| **T3** persistir salientes | ✅ validado en vivo | `0340f28` |
| **T4** requests con timeout | ✅ | `889bcfa` |
| **T5** logging a stderr (no rompe stdio MCP) | ✅ | `889bcfa` |
| **T6** seguridad (loopback + token + sandbox media + timeouts) | ✅ validado en vivo | `577117b` |
| **T7** cache de contactos con TTL + `refresh_contacts` | ✅ validado | `cd8f55b` |
| **T8** salida estructurada de `list_messages` + consistencia de nombres | ✅ validado | `cd8f55b` |
| Unificar chats lid/número en `list_chats` | ✅ validado en vivo | `e71de32` |
| Tools nuevas: `list_groups`, `mark_as_read`, `react_to_message` | ✅ validado en vivo | `f845430` |

### Resuelto en la 2da tanda (no críticos)
| Ítem | Commit |
|---|---|
| **M1** reconexión sin race (solo `EnableAutoReconnect`) | `6d46db2` |
| **M3** índices `messages(chat_jid, timestamp)` + `chats(last_message_time)` | `33afa7f` |
| **M4** path traversal en `downloadMedia` (`filepath.Base` del filename) | `33afa7f` |
| **M7** `get_direct_chat_by_contact` por jid exacto + lid mapeado (sin `LIKE %phone%`) | `d9c37d3` |
| **B** `rand.Seed`→RNG local, `min()` builtin | `6d46db2` |
| **B** temp `.ogg` cleanup + `makedirs(exist_ok=True)` | `d9c37d3` |
| `list_chats(query)` busca por nombre resuelto (no lid crudo) | `cb23d93` |
| `search_contacts` deduplica contacto y `list_messages` une la conversación (lid+número) | `982d8f4` |

### Pendiente (no crítico, bajo impacto)
- **M2** batch transaction en history-sync (inserts uno por uno). **Ya mitigado** por T1 (goroutine) + T2 (WAL): no bloquea; es optimización, no se hizo por riesgo/beneficio.
- **M6** `_load_contact_index` ignora `our_jid` — latente, **sin impacto con una sola cuenta** vinculada.
- Menores: adaptador `datetime`→str de sqlite deprecado en Python 3.12; paginación `OFFSET` sin tie-breaker.
- Tools extra: `edit_message`, `get_profile_picture`, presencia/typing, `get_unread`.
- **`delete_message`** (revoke): whatsmeow lo soporta (`RevokeMessage`/`BuildRevoke`), pero **NO se implementó a propósito** — eliminar mensajes "para todos" es una acción irreversible que debe disparar el usuario, no el asistente (política de no-borrado). Si se quiere la capacidad, el endpoint queda por hacer pero el borrado lo ejecuta el usuario.
- **Nota de alcance:** el sandbox de media, `mark_as_read` y `react_to_message` asumen **chats directos**; en grupos el `sender` del participante queda acotado.

---

## 0. Hallazgo de contexto (acción urgente)

**El upstream está ABANDONADO.** El `main` de `github.com/lharries/whatsapp-mcp` sigue en el mismo commit `7d6a06d` (13-jul-2025) que nuestro clone — **0 commits de código en ~11 meses**, 186 issues abiertos, sin merges desde abril 2025 (el propio issue #220 pide traspaso de mantenimiento). Su whatsmeow sigue en marzo-2025 → cualquiera que clone hoy obtiene el error **"405 Client outdated"** (app rota).

**Nuestro clone es la versión ADELANTADA**: ya tiene whatsmeow 2026 (fix 405), migración a `context.Context`, reconexión automática y la resolución de nombres lid→número. **Pero esos cambios viven solo en el working tree, SIN commitear** → un `git checkout` accidental los borra.

➡️ **Acción inmediata:** commitear los cambios locales en una branch propia y apuntar `origin` a un fork. NO actualizar al upstream (sería un downgrade). **✅ HECHO:** fork `mauricioDevApp/whatsapp-mcp` creado, `origin`→fork, push a `upstream` deshabilitado, todo commiteado en `main`.

---

## 1. Críticos (ALTA) — agrupados por tema

### 🔴 T1. Captura de mensajes en vivo se ahoga con el history-sync `[bridge]`
`main.go:843-873` (handler único secuencial) · `main.go:1028-1167` (`handleHistorySync`) · `main.go:1048` (`GetChatName` en loop)

Causa raíz (verificada contra el fuente de whatsmeow): el handler procesa `*events.HistorySync` y `*events.Message` en la **misma función bloqueante**. El history-sync itera cientos de mensajes y por cada conversación hace round-trips de red (`GetGroupInfo`/`GetContact`) + inserts uno por uno → acapara el lock de escritura de SQLite por minutos. Los mensajes en vivo quedan esperando ese lock. Es exactamente el bug que vimos ("llegaba el historial pero no los mensajes nuevos hasta reiniciar").

**Fix (combinado con T2):**
1. Procesar history-sync en goroutine: `case *events.HistorySync: go handleHistorySync(...)`.
2. Patrón **single-writer**: una goroutine única consume un `chan func()` para TODAS las escrituras a SQLite (elimina la contención de raíz).
3. Quitar el N+1 de red en `GetChatName` dentro del loop (usar solo datos del `conversation`, cachear).

### 🔴 T2. SQLite sin WAL ni busy_timeout → `database is locked` `[bridge + server]`
`main.go:57, 803` (Go abre sin WAL) · `whatsapp.py:130,192,287,382,486,517,563,612,660` (Python abre `messages.db` **read-write** sin timeout)

El bridge escribe en modo journal DELETE (lock exclusivo) mientras el server Python lee la **misma** DB. Combinado con T1, produce `SQLITE_BUSY`. Es un problema **cruzado**: hay que arreglar ambos lados.

**Fix:**
- Bridge: DSN `...?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL` + `db.SetMaxOpenConns(1)`.
- Server: abrir `messages.db` con `file:...?mode=ro` + `timeout=10` (hoy solo lo hace para `whatsapp.db`).

### 🔴 T3. Mensajes salientes NO se persisten `[bridge]`
`main.go:206-372` (`sendWhatsAppMessage` nunca llama `StoreMessage`)

WhatsApp no rebota los mensajes propios como `events.Message`, así que el historial queda sesgado solo a entrantes. Capturar el `SendResponse` (trae `ID` y `Timestamp`) y persistir con `IsFromMe=true` + actualizar `chats.last_message_time`. Cuidado con el early-return de `StoreMessage` cuando no hay contenido ni media (caption de media).

### 🔴 T4. `requests.post` sin timeout congela todo el MCP `[server]`
`whatsapp.py:711,745,785,818`

Las 4 llamadas a la REST API del bridge no tienen `timeout`. Si el bridge se **cuelga** (no caído), el thread del MCP se bloquea para siempre → congela la sesión stdio completa del LLM. **Fix:** `requests.post(..., timeout=(5, 30))`.

### 🔴 T5. `print()` a stdout corrompe el protocolo MCP `[server]`
`whatsapp.py` (líneas 90,142,165,273,366,441,482,502,553,602,650,693 + download_media)

El server corre en `transport='stdio'`: stdout **es** el canal JSON-RPC. Cualquier `print()` de error puede inyectar texto crudo y romper el parsing del cliente. Además tragan errores devolviendo `[]`/`None` (un lock se ve como "sin resultados"). **Fix:** migrar a `logging` configurado a **stderr**.

### 🔴 T6. Seguridad: REST API abierta + exfiltración de archivos `[bridge + server]`
`main.go:783` (`:8080` = 0.0.0.0, sin auth) · `main.go:237` (`media_path` arbitrario) · `whatsapp.py:736,770` (server reenvía path crudo)

- API sin autenticación y bind a todas las interfaces → cualquier proceso local **o de la LAN** puede enviar WhatsApp en tu nombre.
- `media_path` arbitrario permite adjuntar y exfiltrar **cualquier archivo legible** (`~/.ssh/id_rsa`, secrets) a un número. Relevante en contexto fintech.

**Fix:** bind `127.0.0.1`, token `X-Auth-Token` (env var), sandbox de `media_path` a un directorio permitido (en ambos lados), timeouts del `http.Server`.

### 🔴 T7. Cache de contactos nunca se invalida `[server]`
`whatsapp.py:94-102` (`_get_contact_index`)

`_CONTACT_INDEX` se carga una vez por proceso (server de larga vida). Contactos nuevos/renombrados **no aparecen** hasta reiniciar. El flag `refresh=True` existe pero nadie lo llama. **Fix:** TTL (5 min) o comparar `mtime` de `whatsapp.db`; opcional tool `refresh_contacts`. *(Limitación de la mejora que implementamos hoy.)*

### 🔴 T8. Contrato de salida mentiroso + filtros rotos `[server]`
- `whatsapp.py:267,270` — `list_messages` declara `List[Dict]` pero devuelve **string** → el LLM no puede extraer `message_id`/`chat_jid` para encadenar con `download_media`. Devolver estructura.
- `whatsapp.py:220` — filtro `sender_phone_number` usa igualdad exacta contra un campo `sender` heterogéneo (`@lid`, número crudo, `@s.whatsapp.net`) → resultados vacíos silenciosos.
- `whatsapp.py:178/280` — `include_context=True` (default) abre **una conexión SQLite por mensaje** (21 conexiones por llamada) y aplana/duplica resultados.

---

## 2. Importantes (MEDIA)

| # | Comp. | Ubicación | Problema | Fix |
|---|---|---|---|---|
| M1 | bridge | `main.go:856-872` | Reconexión manual duplica el auto-reconnect de whatsmeow (race); `LoggedOut` solo loguea | Confiar en `EnableAutoReconnect` o backoff exponencial; endpoint `/api/status` |
| M2 | bridge | `main.go:1048,1134` | N+1 de red en history-sync + inserts uno por uno | Batch en una transacción (`Prepare`+`Stmt`) |
| M3 | bridge | `main.go:70-86` | Faltan índices en `messages` | `CREATE INDEX idx_messages_chat_time ON messages(chat_jid, timestamp DESC)` |
| M4 | bridge | `main.go:594` | Path traversal en `downloadMedia` (`filename` viene del remitente) | `filepath.Base(filepath.Clean(filename))` |
| M5 | bridge | `main.go:1206-1310` | `analyzeOggOpus`: posible lectura fuera de límites | `if i+pageSize > len(data) { break }` |
| M6 | server | `whatsapp.py:60-91` | `_load_contact_index` ignora `our_jid` (rompe con multi-cuenta) | Filtrar por cuenta activa |
| M7 | server | `whatsapp.py:136,674` | `LIKE '%phone%'` da falsos positivos (`get_direct_chat_by_contact` con `LIMIT 1` puede devolver chat equivocado) | Anclar: `LIKE '{pn}@%'` |
| M8 | server | `whatsapp.py:454` | `search_contacts` crashea con `query=None` | Guard `if not query: return []` |
| M9 | ambos | `main.go` / `whatsapp.py` | `is_from_me` queda como `int` (0/1) no `bool` | `bool(msg[4])` |

---

## 3. Menores (BAJA)
- **bridge:** `rand.Seed` deprecado + reseed por llamada (`main.go:1328`); `min()` redefinido (builtin desde Go 1.21); `fmt.Println(resp)` filtra URLs/media-keys a logs; `requestHistorySync` es código muerto (nunca expuesto).
- **server:** type hints incorrectos (`-> None` que retornan str); `datetime.fromisoformat` frágil + adaptador sqlite deprecado en Python 3.12; temp `.ogg` nunca se borra tras enviar audio (`audio.py`/`whatsapp.py:775` → fuga en /tmp); `os.makedirs` sin `exist_ok=True`; paginación `OFFSET` sin tie-breaker.

---

## 4. Funcionalidades faltantes (tools MCP)

El server es **read + send-only**. Faltan operaciones que whatsmeow ya soporta:

| Tool | Valor |
|---|---|
| `mark_as_read` | Alto — gestionar inbox |
| `react_to_message` (emoji) | Alto |
| `list_groups` / `get_group_info` / participantes | Alto — hoy los grupos son opacos |
| `get_unread_messages` / `list_unread_chats` | Alto |
| `edit_message` / `delete_message` | Medio |
| `get_profile_picture` | Medio |
| `refresh_contacts` | Medio (mitiga T7) |
| `send_typing` / presence | Bajo (UX) |

---

## 5. Plan de remediación recomendado

1. **Commitear los cambios actuales** en branch propia (antes de tocar nada — hoy están sin guardar).
2. **T1 + T2 juntos** (single-writer + WAL/busy_timeout + history-sync async) → resuelve el bug de captura en vivo sin introducir locks. *Lo más impactante.*
3. **T4 + T5** (timeouts en `requests` + `logging` a stderr) → estabilidad del MCP. *Barato y alto impacto.*
4. **T3** (persistir salientes) → historial completo.
5. **T6** (auth + bind loopback + sandbox media) → seguridad. *Importante en fintech.*
6. **T7 + T8** (cache TTL + salida estructurada + filtros).
7. **M1–M9**, luego tools faltantes (`mark_as_read`, `react`, grupos), luego BAJA.

> Nota: la mejora de nombres lid→número que implementamos hoy fue auditada y está **bien diseñada** (read-only, maneja grupos/desconocidos). Sus únicas debilidades: el cache sin invalidar (T7) y que ignora `our_jid` (M6).
