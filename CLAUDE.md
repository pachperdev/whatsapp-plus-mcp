# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a Model Context Protocol (MCP) server for WhatsApp. It consists of two main components that work together:

1. **Go WhatsApp Bridge** (`whatsapp-bridge/`): Connects to WhatsApp's web API using [whatsmeow](https://github.com/tulir/whatsmeow), handles authentication, stores message history in SQLite, and provides a REST API on port 8080.

2. **Python MCP Server** (`whatsapp-mcp-server/`): Implements the MCP protocol to expose WhatsApp functionality as tools for Claude. Communicates with the Go bridge via HTTP and directly queries the SQLite database for read operations.

## Architecture

### Data Flow
- **Incoming messages**: WhatsApp → Go bridge → SQLite database
- **Outgoing messages**: Claude → Python MCP server → Go bridge HTTP API (`/api/send`) → WhatsApp
- **Media downloads**: Python MCP server → Go bridge HTTP API (`/api/download`) → WhatsApp
- **Message queries**: Python MCP server → SQLite database (direct)

### Database Schema
The Go bridge maintains `whatsapp-bridge/store/messages.db` with two tables:
- `chats`: JID (primary key), name, last_message_time
- `messages`: id, chat_jid (foreign key), sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length

### Key Identifiers
- **JID (Jabber ID)**: WhatsApp's unique identifier format (e.g., `1234567890@s.whatsapp.net` for individuals, `1234567890-1234567890@g.us` for groups)
- **Chat JID**: Identifies the conversation (individual or group)
- **Sender**: Phone number without `@` suffix

## Development Commands

### Go WhatsApp Bridge
```bash
cd whatsapp-bridge
go run main.go                    # Run the bridge (will show QR code on first run)
go build -o whatsapp-bridge      # Build binary
```

**Windows**: Requires CGO enabled and a C compiler (MSYS2 recommended):
```bash
go env -w CGO_ENABLED=1
go run main.go
```

### Python MCP Server
```bash
cd whatsapp-mcp-server
uv run main.py                   # Run the MCP server
```

The Python server expects the Go bridge to be running and the SQLite database to exist at `../whatsapp-bridge/store/messages.db`.

## Media Handling

### Sending Media
- **Images/Videos/Documents**: Use `send_file` tool with file path
- **Voice Messages**: Use `send_audio_message` tool
  - Requires `.ogg` Opus format
  - With FFmpeg installed, automatically converts other formats (see `audio.py`)
  - Without FFmpeg, use `send_file` for raw audio (won't be playable as voice message)

### Receiving Media
- Media metadata is stored in the database
- Actual media files are downloaded on-demand using `download_media` tool
- Downloads are handled by the Go bridge's `/api/download` endpoint

## MCP Tools

The Python MCP server exposes these tools (see `whatsapp-mcp-server/main.py`):
- `search_contacts`: Search by name or phone number
- `list_messages`: Get messages with filters (time range, sender, chat, content search) and context
- `list_chats`: Get available chats with metadata
- `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`: Chat lookup utilities
- `get_last_interaction`, `get_message_context`: Context retrieval
- `send_message`: Send text to phone number or group JID
- `send_file`: Send media files
- `send_audio_message`: Send voice messages (requires Opus format)
- `download_media`: Download media from messages

## Important Implementation Details

### Authentication
- First run of Go bridge requires QR code scan with WhatsApp mobile app
- Session is saved in `whatsapp-bridge/store/whatsapp.db`
- May need re-authentication after ~20 days

### Name Resolution
The `get_sender_name` function in `whatsapp.py` resolves JIDs to contact names by querying the `chats` table. For group messages, it extracts the phone number from the sender JID and looks up the corresponding chat name.

### HTTP API Endpoints (Go Bridge)
- `POST /api/send`: Send messages and media
  - Request: `{"recipient": "...", "message": "...", "media_path": "...", "is_voice_message": bool}`
  - Response: `{"success": bool, "message": "..."}`
- `POST /api/download`: Download media from messages
  - Request: `{"message_id": "...", "chat_jid": "..."}`
  - Response: `{"success": bool, "message": "...", "filename": "...", "path": "..."}`

### Media Storage
Media metadata (URL, encryption keys, hashes) is stored in the database. The Go bridge implements `MediaDownloader` (lines 510-549 in `main.go`) to download encrypted media from WhatsApp servers.

## Configuration Files

- `whatsapp-bridge/go.mod`: Go dependencies (uses whatsmeow library)
- `whatsapp-mcp-server/pyproject.toml`: Python dependencies (mcp, httpx, requests)
- `whatsapp-mcp-server/uv.lock`: Locked Python dependencies

## Security Considerations

As noted in the README, this project is subject to "the lethal trifecta" - project injection could lead to private data exfiltration since:
1. It has access to private data (WhatsApp messages)
2. It can make external requests (send messages, download media)
3. It's controlled by LLM prompts

All messages are stored locally and only sent to Claude when accessed through tools.
