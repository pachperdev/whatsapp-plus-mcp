# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

A Model Context Protocol (MCP) server for a **personal WhatsApp account**, built on two cooperating processes:

1. **Go WhatsApp Bridge** (`whatsapp-bridge/main.go`, ~4000 lines): connects to WhatsApp Web's multidevice API via [whatsmeow](https://github.com/tulir/whatsmeow), handles QR auth, persists message/chat history to SQLite, listens for live events, and exposes a token-authenticated REST API on `127.0.0.1:8080`.

2. **Python MCP Server** (`whatsapp-mcp-server/`): exposes WhatsApp functionality as MCP tools. **Reads** come straight from the SQLite DB; **writes/actions** go to the Go bridge over HTTP.

> **This fork is far ahead of upstream.** It exposes **62 MCP tools** (upstream had ~12). `AUDIT.md` is the authoritative log of project state, roadmap, and per-feature implementation notes — **read it before planning any feature work.** Commits, comments, and `AUDIT.md` are written in Spanish.

## Architecture

### Two-process split (the core mental model)
- **Reads** (`list_messages`, `list_chats`, `search_contacts`, context lookups, `get_unread_chats`): Python queries the SQLite DB **directly** — the bridge is not in the path.
- **Writes/actions** (send, react, edit, group admin, presence, etc.): Python → `POST http://localhost:8080/api/<endpoint>` → bridge → WhatsApp.
- **Incoming data**: WhatsApp → bridge event handlers → SQLite (live capture + history sync).

### Adding a new tool (the canonical pattern)
Every action-style tool is three small pieces, all following the same shape:
1. **Bridge handler** in `main.go`: `http.HandleFunc("/api/<name>", withAuth(func(...)))` calling the relevant whatsmeow `Client` method.
2. **Client function** in `whatsapp.py`: a thin wrapper, almost always `_bridge_post("<name>", payload)`.
3. **MCP tool** in `main.py`: `@mcp.tool()` that calls the `whatsapp.py` function.

Event-driven features (incoming edits, votes, presence, ban status) instead add a `case` to the bridge's event handler and persist to SQLite. `Build*`-based sends (`BuildEdit`, `BuildRevoke`, `BuildPollCreation`, `BuildPollVote`) all go out via `SendMessage`. See `AUDIT.md` → "Referencia rápida" for the whatsmeow method map.

### Security model (important — not optional)
- The bridge binds **loopback only** (`127.0.0.1:8080`), never `0.0.0.0`.
- Every `/api/*` route is wrapped in `withAuth`, which requires the `X-Auth-Token` header. The bridge generates a random token on startup and writes it to `whatsapp-bridge/store/.bridge_token` (mode `0600`); the Python side reads that same file in `_bridge_token()` and sends it on every request via `_bridge_post`. If you add an endpoint, wrap it in `withAuth`.

### Database (`whatsapp-bridge/store/messages.db`)
- `chats`: `jid` (PK), `name`, `last_message_time`.
- `messages`: PK `(id, chat_jid)`; columns include `sender`, `content`, `timestamp`, `is_from_me`, `media_type`, `filename`, `url`, **`direct_path`**, `media_key`, `file_sha256`, `file_enc_sha256`, `file_length`.
  - **`direct_path` is required for media downloads.** whatsmeow's `Download` uses the native protobuf direct path; reconstructing it from `url` fails with the new `mms3` format (403). A column migration (`ALTER TABLE ... ADD COLUMN direct_path`) is applied idempotently on startup for old DBs.
- `unread_messages`: tracks unread counts, populated **only from live events** (history sync deliberately skips it so counts aren't inflated). Cleared by self read-receipts, by replying, or via `mark_as_read`.
- Session/credential store lives separately in `whatsapp-bridge/store/whatsapp.db` (whatsmeow's own store).

### Identifiers & name resolution
- **JID**: `<number>@s.whatsapp.net` (individual), `<id>-<id>@g.us` (group), plus `@lid` (privacy-layer) identifiers WhatsApp now uses for some senders.
- `whatsapp.py` unifies `@lid` ↔ phone-number JIDs when resolving contact names and direct chats — keep this in mind; a sender may appear under either form. Name resolution (`resolve_contact_name`, `get_sender_name`) queries the contact index / `chats` table.

## Development Commands

### Go WhatsApp Bridge
```bash
cd whatsapp-bridge
go run main.go                 # first run prints a QR code to scan
go build -o whatsapp-bridge    # build binary
```
**No CGO required.** This fork migrated to the pure-Go `modernc.org/sqlite` driver (commit `caf5df3`), so it cross-compiles to darwin/linux/windows with no C toolchain. (Upstream's Windows `CGO_ENABLED=1` instructions no longer apply.)

> **Timestamp gotcha:** the `dbTime` type forces the SQLite format `2006-01-02 15:04:05-07:00`. modernc would otherwise serialize `time.Time` via `.String()`, silently breaking `ORDER BY` and Python's `fromisoformat`. Preserve `dbTime` when touching timestamp persistence.

### Python MCP Server
```bash
cd whatsapp-mcp-server
uv run main.py                 # expects the bridge running + DB at ../whatsapp-bridge/store/messages.db
```
Dependencies (`pyproject.toml`): `mcp[cli]`, `requests`, `httpx`; Python `>=3.11`. The server uses a global `requests.Session()` for keep-alive pooling to the bridge.

## MCP Tools (62)

Full list and signatures live in `whatsapp-mcp-server/main.py` (`@mcp.tool()` decorators). Grouped by area:
- **Read/search**: `search_contacts`, `list_all_contacts`, `refresh_contacts`, `list_messages`, `list_chats`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`, `get_message_context`, `get_unread_chats`, `list_groups`.
- **Send**: `send_message` (supports `reply_to` + `@number` mentions), `send_file`, `send_audio_message`, `send_poll`, `vote_poll`, `send_typing`.
- **Message ops**: `react_to_message`, `edit_message`, `delete_message`, `star_message`, `mark_as_read`, `download_media`.
- **Chat state**: `mute_chat`, `pin_chat`, `archive_chat`, `mark_chat`, `get_chat_settings`, `set_disappearing_messages`, `request_more_history`.
- **Groups**: `create_group`, `update_group_participants`, `get_group_participants`, `get_group_invite_link`, `join_group`, `leave_group`, `set_group_name/topic/description/announce/locked/photo`, join-approval + join-request tools, invite-link info/join.
- **Contacts/identity/presence**: `check_whatsapp`, `get_user_info`, `get_user_devices`, `get_profile_picture`, `get_business_profile`, `block_contact`/`unblock_contact`, `set/subscribe/get_presence`.
- **Account/session**: `get_status`, `set_status_message`, `set_default_disappearing`, `logout`.

### Key bridge HTTP endpoints
- `POST /api/send`, `/api/download` — messages and media (as in upstream).
- `GET /api/status` — connection/login/ban state (`temp_banned` with code/reason/expires, `needs_qr`, last connect failure). Backs `get_status`.
- Plus one `/api/<name>` per action tool above, each `withAuth`-wrapped.

## Media Handling
- **Send**: images/videos/documents via `send_file`; voice via `send_audio_message` (needs `.ogg` Opus — FFmpeg auto-converts other formats; see `audio.py`).
- **Receive**: metadata is stored at capture time; actual bytes are fetched on demand via `download_media` (`message_id` + `chat_jid`) hitting `/api/download`. Capture covers stickers (`.webp`), unwrapped ephemeral/view-once/document-with-caption wrappers, captions, locations, and vCard contacts.

## Authentication & ban awareness
- First bridge run requires a QR scan; session persists in `store/whatsapp.db`. Re-auth may be needed after ~20 days. The bridge logs the raw QR (`QR_RAW`) so it can be rendered to a PNG and opened when the terminal can't show it.
- The bridge handles `events.TemporaryBan` / `ConnectFailure` / `LoggedOut` / `Connected` / `Disconnected`, keeps thread-safe `botStatus`, and **pauses outgoing sends while temp-banned**.
- **whatsmeow is an unofficial client; using it violates WhatsApp's ToS and risks the number.** Per `AUDIT.md`: reactive use (replying to inbound) is low-risk; proactive/cold/bulk outreach is high-risk. Hard rules: no bulk or repeated-identical messages (triggers ban code 104), one account = one session = one IP, keep whatsmeow current. Read the ban section of `AUDIT.md` before adding anything that sends proactively.

## Configuration
- `.mcp.json` registers the server (runs `uv --directory <repo>/whatsapp-mcp-server run main.py`).
- `whatsapp-bridge/go.mod`: Go 1.25, whatsmeow (pinned, ~2026-06), `modernc.org/sqlite`.
- Roadmap status: Phases 1–3 + Tier A/B + Tier 3 are complete; **Phase 4 (Pydantic structured output, MCP SDK 1.6→1.28 upgrade, resources/prompts, tests + CI + linters) is the only open work** — see `AUDIT.md`.
