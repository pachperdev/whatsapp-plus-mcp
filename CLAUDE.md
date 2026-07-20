# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

A Model Context Protocol (MCP) server for a **personal WhatsApp account**, built on two cooperating processes:

1. **Go WhatsApp Bridge** (`whatsapp-bridge/`): connects to WhatsApp Web's multidevice API via [whatsmeow](https://github.com/tulir/whatsmeow), handles QR auth, persists message/chat history to SQLite, listens for live events, and exposes a token-authenticated REST API on `127.0.0.1:8080`. Modularized into `main.go` (bootstrap + event dispatcher + HTTP lifecycle) and `internal/{config,media,auth,store,wa,api}`.

2. **Python MCP Server** (`whatsapp-mcp-server/`): the `whatsapp_mcp` package (`config` → `models` → `db` → `bridge` → `tools`/`prompts` → `server`) exposes WhatsApp functionality as MCP tools. **Reads** come straight from the SQLite DB (`db.py`); **writes/actions** go to the Go bridge over HTTP (`bridge.py`).

> **This fork is far ahead of upstream.** It exposes **65 MCP tools** (upstream had ~12). See `CHANGELOG.md` for the milestone history. Commits and code comments are written in Spanish.

## Architecture

### Two-process split (the core mental model)
- **Reads** (`list_messages`, `list_chats`, `search_contacts`, context lookups, `get_unread_chats`): Python queries the SQLite DB **directly** — the bridge is not in the path.
- **Writes/actions** (send, react, edit, group admin, presence, etc.): Python → `POST http://localhost:8080/api/<endpoint>` → bridge → WhatsApp.
- **Incoming data**: WhatsApp → bridge event handlers → SQLite (live capture + history sync).

### Adding a new tool (the canonical pattern)
Every action-style tool is three small pieces, all following the same shape:
1. **Bridge handler** in `internal/api/server.go`: `mux.HandleFunc("/api/<name>", withAuth(token, func(...)))`. Use the shared helpers — `decodeJSON(w, r, &req)`, `parseJID(w, raw, field)`, `respondErr`/`respondOK` — and call the relevant `wa.Service` method (or `client` directly). Wrap sends that bypass `svc.SendMessage` in `banBlocked(w, svc)`.
2. **Client function** in `whatsapp_mcp/bridge.py`: a thin wrapper, almost always `_bridge_post("<name>", payload)` (or `_bridge_get` for reads).
3. **MCP tool** in `whatsapp_mcp/tools.py`: `@mcp.tool(annotations=...)` that calls the `bridge.py` function.

Event-driven features (incoming edits, votes, presence, ban status) instead add a `case` to the dispatcher in `main.go` and a handler method in `internal/wa/events.go` that persists to SQLite. `Build*`-based sends (`BuildEdit`, `BuildRevoke`, `BuildPollCreation`, `BuildPollVote`) all go out via `SendMessage`. For available whatsmeow methods, see the [whatsmeow docs](https://pkg.go.dev/go.mau.fi/whatsmeow) or the existing handlers as templates.

### Security model (important — not optional)
- The bridge binds **loopback only** (`127.0.0.1:8080`), never `0.0.0.0`. The bind address (`WHATSAPP_BRIDGE_ADDR`) is validated to be loopback at startup (`internal/config`).
- Every `/api/*` route is wrapped in `withAuth` (constant-time token compare, fail-closed on empty token), which requires the `X-Auth-Token` header. The bridge generates a random token on startup and writes it to `<store>/.bridge_token` (mode `0600`, re-applied on reuse); the Python side reads that same file in `_bridge_token()` and sends it on every request via `_bridge_post`. If you add an endpoint, wrap it in `withAuth`. Request bodies are capped at 1 MiB via `decodeJSON`.
- **Media path validation** (`internal/auth`, the `Validator`): `send_file` / `set_group_photo` reject hidden paths and the bridge's own store secrets, compared **by inode** (`os.SameFile`) so case-variant paths and hardlinks can't exfiltrate the session DB. It returns the canonical path (callers read from it — no TOCTOU). `WHATSAPP_MEDIA_ALLOWED_DIRS` optionally confines sends to an allowlist. Downloaded media in `store/<chat>/` stays sendable.

### Database (`whatsapp-bridge/store/messages.db`)
- `chats`: `jid` (PK), `name`, `last_message_time`.
- `messages`: PK `(id, chat_jid)`; columns include `sender`, `content`, `timestamp`, `is_from_me`, `media_type`, `filename`, `url`, **`direct_path`**, `media_key`, `file_sha256`, `file_enc_sha256`, `file_length`.
  - **`direct_path` is required for media downloads.** whatsmeow's `Download` uses the native protobuf direct path; reconstructing it from `url` fails with the new `mms3` format (403). A column migration (`ALTER TABLE ... ADD COLUMN direct_path`) is applied idempotently on startup for old DBs.
- `unread_messages`: tracks unread counts, populated **only from live events** (history sync deliberately skips it so counts aren't inflated). Cleared by self read-receipts, by replying, or via `mark_as_read`.
- Session/credential store lives separately in `whatsapp-bridge/store/whatsapp.db` (whatsmeow's own store).

### Identifiers & name resolution
- **JID**: `<number>@s.whatsapp.net` (individual), `<id>-<id>@g.us` (group), plus `@lid` (privacy-layer) identifiers WhatsApp now uses for some senders.
- `whatsapp_mcp/db.py` unifies `@lid` ↔ phone-number JIDs when resolving contact names and direct chats — keep this in mind; a sender may appear under either form. Name resolution (`resolve_contact_name`, `get_sender_name`) queries the contact index / `chats` table.

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

## MCP Tools (65)

Full list and signatures live in `whatsapp-mcp-server/whatsapp_mcp/tools.py` (`@mcp.tool()` decorators). Grouped by area:
- **Read/search**: `search_contacts`, `list_all_contacts`, `refresh_contacts`, `list_messages`, `list_chats`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`, `get_message_context`, `get_unread_chats`, `list_groups`.
- **Send**: `send_message` (supports `reply_to` + `@number` mentions), `send_file`, `send_audio_message`, `send_poll`, `vote_poll`, `send_typing`.
- **Message ops**: `react_to_message`, `edit_message`, `delete_message`, `star_message`, `mark_as_read`, `download_media`.
- **Chat state**: `mute_chat`, `pin_chat`, `archive_chat`, `mark_chat`, `get_chat_settings`, `set_disappearing_messages`, `request_more_history`.
- **Groups**: `create_group`, `update_group_participants`, `get_group_participants`, `get_group_invite_link` (pure read) / `reset_group_invite_link` (revokes + regenerates), `join_group`, `leave_group`, `set_group_name/topic/description/announce/locked/photo`, join-approval + join-request tools, invite-link info/join (`get_group_info_from_invite`/`join_group_with_invite` operate on LEGACY native group-invite messages; modern WhatsApp clients share invites as plain-text links — route those to `join_group`, which accepts full links including `?mode=gi_t`).
- **Contacts/identity/presence**: `check_whatsapp`, `get_user_info`, `get_user_devices`, `get_profile_picture`, `get_business_profile`, `block_contact`/`unblock_contact`, `set/subscribe/get_presence`.
- **Account/session**: `get_status`, `set_status_message`, `set_default_disappearing`, `logout`.
- **Self-managed login (supervisor)**: `login_with_qr` (validates/reuses an existing session, spawns or recycles the bridge as needed), `shutdown_bridge` (graceful stop; session preserved). The Python server supervises the Go bridge: it adopts a healthy running bridge (never duplicates connections), spawns the binary (`WHATSAPP_BRIDGE_BIN`) when missing, and recycles zombie sessions via `/api/shutdown` + respawn (`bridge.ensure_bridge` / `bridge.acquire_login_qr`). The QR is delivered on two channels: the **local OS image viewer is primary** (instant, auto-refreshed on each rotation by `bridge.start_qr_preview_watcher`); the tool result also carries an MCP `Image` (native in Claude Code CLI) plus a minimal HTML snippet the assistant renders inline in the chat via its `visualize` tool. Markdown data-URIs, CDN-based rendering and text/block QRs were all rejected in real Claude Desktop testing (blocked, deformed, or too slow).

### Key bridge HTTP endpoints
- `POST /api/send`, `/api/download` — messages and media (as in upstream).
- `GET /api/status` — connection/login/ban state (`temp_banned` with code/reason/expires, `needs_qr`, last connect failure). Backs `get_status`.
- `GET /api/qr` — login-QR state machine (`logged_in|none|active|success|timeout` + raw code + `png_base64`). The HTTP server now starts BEFORE pairing so this route exists during QR mode.
- `POST /api/shutdown` — graceful shutdown (same path as SIGTERM); lets the supervisor recycle the process without OS signals.
- Plus one `/api/<name>` per action tool above, each `withAuth`-wrapped.

## Media Handling
- **Send**: images/videos/documents via `send_file`; voice via `send_audio_message` (needs `.ogg` Opus — FFmpeg auto-converts other formats; see `audio.py`).
- **Receive**: metadata is stored at capture time; actual bytes are fetched on demand via `download_media` (`message_id` + `chat_jid`) hitting `/api/download`. Capture covers stickers (`.webp`), unwrapped ephemeral/view-once/document-with-caption wrappers, captions, locations, and vCard contacts.

## Authentication & ban awareness
- First bridge run requires a QR scan; session persists in `store/whatsapp.db`. Re-auth may be needed after ~20 days. The bridge logs the raw QR (`QR_RAW`) so it can be rendered to a PNG and opened when the terminal can't show it.
- The bridge handles `events.TemporaryBan` / `ConnectFailure` / `LoggedOut` / `Connected` / `Disconnected`, keeps thread-safe `botStatus`, and **pauses outgoing sends while temp-banned**.
- **whatsmeow is an unofficial client; using it violates WhatsApp's ToS and risks the number.** Reactive use (replying to inbound) is low-risk; proactive / cold / bulk outreach is high-risk. Hard rules: no bulk or repeated-identical messages (triggers ban code 104), one account = one session = one IP, keep whatsmeow current. Be especially careful before adding anything that sends proactively.

## Configuration
- `.mcp.json` registers the server for standard MCP clients (runs `uv --directory whatsapp-mcp-server run main.py`, repo-relative).
- **Canonical repo**: `pachperdev/whatsapp-plus-mcp` (public, owner-maintained community upstream, MIT).
- **Claude Code plugin**: `.claude-plugin/plugin.json` declares the MCP server (`whatsapp-plus`) with `${CLAUDE_PLUGIN_ROOT}` paths and sets `WHATSAPP_PLUGIN_MODE=1`; `.claude-plugin/marketplace.json` makes this repo its own marketplace (`pachperdev`, plugin `whatsapp-plus`, source `./`). Install: `/plugin marketplace add pachperdev/whatsapp-plus-mcp` + `/plugin install whatsapp-plus@pachperdev`.
- **Plugin mode** (`WHATSAPP_PLUGIN_MODE=1`): all mutable data moves to `~/.whatsapp-mcp/` (store, compiled binary under `bin/`, logs) so plugin updates never destroy the session. When the binary is missing, the supervisor resolves it in cascade (`ensure_bridge_binary` in `bridge.py`): download the precompiled asset from the latest GitHub Release (SHA256-verified, GoReleaser naming `whatsapp-bridge-<os>-<arch>`, private-repo auth via `gh auth token`) → fallback to local `go build`. Releases are published by `.github/workflows/release.yml` (GoReleaser, tag `v*`).
- **Bridge env vars** (all optional, defaults = historic layout; `internal/config`): `WHATSAPP_BRIDGE_ADDR` (loopback-validated), `WHATSAPP_STORE_DIR`, `WHATSAPP_MEDIA_ALLOWED_DIRS` (opt-in send allowlist). **Python env vars**: `WHATSAPP_MESSAGES_DB`, `WHATSAPP_SESSION_DB`, `WHATSAPP_BRIDGE_TOKEN_FILE`, `WHATSAPP_API_BASE_URL`, `WHATSAPP_BRIDGE_BIN`, `WHATSAPP_BRIDGE_LOG`, `WHATSAPP_PLUGIN_MODE` (`whatsapp_mcp/config.py`).
- `whatsapp-bridge/go.mod`: Go 1.25, whatsmeow (pinned, ~2026-07), `modernc.org/sqlite`.
- Roadmap status: **Phases 0–5 complete** (security hardening, modularization Go + Python, MCP contract, tests + CI + linters, plug-and-play packaging as a Claude Code plugin + corporate marketplace with self-managed QR login). See `CHANGELOG.md`.
