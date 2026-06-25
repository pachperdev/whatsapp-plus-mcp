# WhatsApp MCP Server

A [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server that connects an LLM (Claude, Cursor, etc.) to your **personal WhatsApp account**: search and read your chats — text, images, video, audio, documents, stickers, locations and shared contacts — and send messages, media and polls to people or groups.

It talks to WhatsApp directly through the WhatsApp Web multi-device API using the [whatsmeow](https://github.com/tulir/whatsmeow) library. **All your data stays local** in a SQLite database; nothing is sent anywhere except to the LLM, and only when it explicitly calls a tool you control.

![WhatsApp MCP](./example-use.png)

> **This is a substantially enhanced fork** of [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp). It exposes **62 MCP tools** (vs ~12 upstream), runs on a **modernized whatsmeow** (2026), builds **without CGO**, and adds a token-secured bridge, ban/health awareness, richer media capture, and live capture of edits, revokes, polls, presence and unread state. See [Differences from upstream](#differences-from-upstream).

---

## ⚠️ Read this first

- **This uses an unofficial WhatsApp client.** Using whatsmeow **violates WhatsApp's Terms of Service** and can get your number banned. Replying to people who message you is low-risk; **proactive / cold / bulk messaging is high-risk** (see [Ban awareness](#ban-awareness)).
- **It runs on your real, personal number.** Treat it accordingly: one account = one session = one IP.
- Like any MCP server with access to private data, this is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/) — untrusted content + private data + exfiltration paths. Be deliberate about what you let the agent do.

---

## Features

- 📥 **Read & search** your message history, chats and contacts (with `@lid` ↔ phone-number unification and real contact-name resolution).
- 📤 **Send** text (with replies and `@mentions`), files, voice notes, and polls.
- 🗳️ **Polls**: create them, vote, and capture incoming votes.
- 🖼️ **Rich media**: download images, video, audio, documents and **stickers**; capture captions, view-once / ephemeral wrappers, **locations** and **vCard contacts**.
- ✏️ **Live capture** of incoming **edits** and **revokes** ("deleted for everyone"), reflected in place.
- 👀 **Presence** (online / typing / last-seen) and **unread-chat** tracking.
- 👥 **Full group admin**: create groups, manage participants, name/topic/description/photo, announce & locked modes, invite links, and join-request approval.
- 🛡️ **Health & ban awareness**: the bridge tracks connection / login / temp-ban state and **pauses sends while temp-banned**.
- 🔒 **Local-first & token-secured**: data lives in local SQLite; the bridge binds to loopback and requires an auth token.

For the complete, grouped tool catalogue see [MCP tools](#mcp-tools-62).

---

## Architecture

Two cooperating processes:

```
                  ┌─────────────────────────┐         ┌──────────────────┐
  LLM (Claude) ◄──┤  Python MCP server      │         │  Go WhatsApp     │
   via MCP    ────►  (whatsapp-mcp-server/)  │         │  bridge          │
                  │                          │         │ (whatsapp-bridge/)│
                  │  reads  ── SQLite ───────┼────────►│                  │
                  │  writes ── HTTP /api/* ──┼────────►│  whatsmeow ◄────► WhatsApp
                  └─────────────────────────┘  token  └──────────────────┘
                                                          │
                                            live events + history sync
                                                          ▼
                                                  SQLite (messages.db)
```

1. **Go WhatsApp bridge** (`whatsapp-bridge/`) — connects to WhatsApp via whatsmeow, handles QR auth, persists chats/messages to SQLite, listens for live events, and serves a **token-authenticated REST API on `127.0.0.1:8080`**.
2. **Python MCP server** (`whatsapp-mcp-server/`) — exposes the MCP tools. **Reads** query the SQLite DB directly; **writes/actions** are forwarded to the bridge over HTTP.

> Deeper architecture notes, the database schema, the "how to add a tool" pattern and gotchas live in [`CLAUDE.md`](./CLAUDE.md).

---

## Requirements

- **Go** 1.25+ (the bridge builds **without CGO** — no C compiler needed on any platform)
- **Python** 3.11+
- **[uv](https://docs.astral.sh/uv/)** — `curl -LsSf https://astral.sh/uv/install.sh | sh`
- An MCP client: **Claude Desktop**, **Claude Code**, **Cursor**, etc.
- **FFmpeg** *(optional)* — only to send arbitrary audio files as playable voice notes; without it you can still send `.ogg` Opus directly or use `send_file`.

---

## Installation

### 1. Clone

```bash
git clone https://github.com/mauricioDevApp/whatsapp-mcp.git
cd whatsapp-mcp
```

### 2. Run the WhatsApp bridge

```bash
cd whatsapp-bridge
go run .            # or: go build -o whatsapp-bridge . && ./whatsapp-bridge
```

On first run it prints a **QR code** — scan it from your phone (**WhatsApp → Settings → Linked Devices → Link a device**). The session is saved in `whatsapp-bridge/store/`; you typically only re-scan after ~20 days of inactivity or an explicit logout.

> Keep the bridge running in its own terminal. **Without it running, the database stops updating** (no new messages, no live actions).

### 3. Register the MCP server with your client

Point your client at the Python server via `uv`. Replace the paths:

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "{{PATH_TO_UV}}",
      "args": [
        "--directory",
        "{{PATH_TO_REPO}}/whatsapp-mcp/whatsapp-mcp-server",
        "run",
        "main.py"
      ]
    }
  }
}
```

- `{{PATH_TO_UV}}` → output of `which uv`
- `{{PATH_TO_REPO}}` → output of `pwd` in the repo root

**Config file location:**

| Client | Path |
|---|---|
| Claude Desktop | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Cursor | `~/.cursor/mcp.json` |
| Claude Code | `~/.mcp.json` (or `claude mcp add`) |

### 4. Restart / reconnect your client

Restart Claude Desktop / Cursor, or in Claude Code run `/mcp` to (re)connect. WhatsApp should now appear as an available integration.

> **After you rebuild or restart the bridge, reconnect the MCP server** (`/mcp` in Claude Code) so the Python side picks up the current auth token.

---

## MCP tools (62)

Signatures live in [`whatsapp-mcp-server/main.py`](./whatsapp-mcp-server/main.py) (`@mcp.tool()`). Grouped by area:

| Area | Tools |
|---|---|
| **Read / search** | `search_contacts`, `list_all_contacts`, `refresh_contacts`, `list_messages`, `list_chats`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`, `get_message_context`, `get_unread_chats`, `list_groups` |
| **Send** | `send_message` (supports `reply_to` + `@number` mentions), `send_file`, `send_audio_message`, `send_poll`, `vote_poll`, `send_typing` |
| **Message ops** | `react_to_message`, `edit_message`, `delete_message`, `star_message`, `mark_as_read`, `download_media` |
| **Chat state** | `mute_chat`, `pin_chat`, `archive_chat`, `mark_chat`, `get_chat_settings`, `set_disappearing_messages`, `request_more_history` |
| **Groups** | `create_group`, `update_group_participants`, `get_group_participants`, `get_group_invite_link`, `join_group`, `leave_group`, `set_group_name`, `set_group_topic`, `set_group_description`, `set_group_announce`, `set_group_locked`, `set_group_photo`, `set_group_join_approval`, `get_group_join_requests`, `review_group_join_request`, `get_group_info_from_invite`, `join_group_with_invite` |
| **Contacts / identity / presence** | `check_whatsapp`, `get_user_info`, `get_user_devices`, `get_profile_picture`, `get_business_profile`, `block_contact`, `unblock_contact`, `set_presence`, `subscribe_presence`, `get_presence` |
| **Account / session** | `get_status`, `set_status_message`, `set_default_disappearing`, `logout` |

Some incoming data is captured passively (no dedicated tool) and surfaces through `list_messages`: **call logs**, **edits/revokes**, **locations**, **shared contacts**, and **poll votes**.

---

## Media handling

- **Sending** — images / video / documents via `send_file`; voice notes via `send_audio_message` (needs `.ogg` Opus; FFmpeg auto-converts other formats — see `audio.py`).
- **Receiving** — only metadata is stored at capture time; fetch the bytes on demand with `download_media(message_id, chat_jid)`, which returns a local file path you can open or pass to another tool. Capture covers images, video, audio, documents, **stickers** (`.webp`), unwrapped ephemeral / view-once / document-with-caption messages, captions, **locations** and **vCard contacts**.

---

## Security & privacy

- **Local-first.** Messages and media live in SQLite under `whatsapp-bridge/store/` and are only read when the LLM calls a tool.
- **Loopback only.** The bridge binds `127.0.0.1:8080`, never `0.0.0.0`.
- **Token auth.** Every `/api/*` route requires an `X-Auth-Token` header. The bridge generates a random token on startup and writes it to `whatsapp-bridge/store/.bridge_token` (mode `0600`); the Python side reads the same file and sends it on every request.
- **Nothing private is committed.** `whatsapp-bridge/store/` (DBs, token, downloaded media) and `.mcp.json` (machine-specific paths) are git-ignored.

---

## Ban awareness

whatsmeow is an **unofficial** client and using it **violates WhatsApp's ToS**. The bridge handles `events.TemporaryBan` / `ConnectFailure` / `LoggedOut` / `Connected` / `Disconnected`, keeps a thread-safe status, exposes it via `get_status`, and **pauses outgoing sends while temp-banned**.

Hard rules:

- **No bulk, cold, or proactive outreach.** Reactive replies are far lower-risk.
- **Never send the same text repeatedly** (triggers temp-ban code `104`).
- **One account = one session = one IP.** Don't run multiple instances.
- **Keep whatsmeow current** (an outdated client triggers `405 ClientOutdated` / `409 BadUserAgent`).

---

## Troubleshooting

- **QR won't show / expired** — restart the bridge. The bridge also logs the raw QR as `QR_RAW>>>...<<<`, so you can render it to a PNG (e.g. with [`segno`](https://pypi.org/project/segno/)) and open it when the terminal can't display it.
- **`405 Client outdated`** — whatsmeow is too old: `go get go.mau.fi/whatsmeow@latest && go mod tidy && go build -o whatsapp-bridge .`
- **No messages loading** — after first auth, history sync can take several minutes for large accounts.
- **Out of sync** — delete `whatsapp-bridge/store/messages.db` and restart (this only drops the local mirror; the WhatsApp session in `whatsapp-bridge/store/whatsapp.db` is preserved). Deleting both files forces a fresh QR.
- **Duplicate / orphan linked devices** — every QR scan creates a new linked device; `logout` only unlinks the current one. Audit with `get_user_devices` on your own number and remove stale ones from **Phone → Linked Devices**.
- **Tool changes not picked up** — reconnect the MCP server after rebuilding the bridge (`/mcp` in Claude Code).

---

## Differences from upstream

| | upstream `lharries/whatsapp-mcp` | this fork |
|---|---|---|
| MCP tools | ~12 | **62** |
| whatsmeow | 2025 | **2026 (current)** |
| SQLite driver | `mattn/go-sqlite3` (CGO) | **`modernc.org/sqlite` (pure Go, no CGO)** — cross-compiles to darwin/linux/windows |
| Bridge API | unauthenticated | **loopback + `X-Auth-Token`** |
| Ban / health | none | **event handling + `get_status` + send-pause** |
| Media capture | basic | **stickers, wrappers, captions, locations, vCards, native `direct_path`** |
| Live capture | messages | **+ edits, revokes, polls & votes, presence, calls, unread state** |
| Groups | basic | **full admin + join-request approval + invite flows** |

---

## Project status & development

Phases 1–3 + Tier A/B + Tier 3 are complete; **Phase 4** (MCP SDK upgrade, Pydantic structured output, resources/prompts, tests + CI + linters) is the only open work.

- **Changelog:** [`CHANGELOG.md`](./CHANGELOG.md) — what changed, grouped by milestone.
- **Working in this repo with Claude Code:** read [`CLAUDE.md`](./CLAUDE.md) first — it covers the two-process model, the database schema, the canonical "add a tool" pattern, and the non-obvious gotchas (`dbTime`, `direct_path`, encrypted edits, `@lid` unification).

### Quick dev commands

```bash
# Bridge
cd whatsapp-bridge && go build -o whatsapp-bridge . && ./whatsapp-bridge

# MCP server (needs the bridge running)
cd whatsapp-mcp-server && uv run main.py
```

---

## Credits & license

- Built on [whatsmeow](https://github.com/tulir/whatsmeow) by Tulir Asokan.
- Forked from [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp).

Licensed under the [MIT License](./LICENSE).
