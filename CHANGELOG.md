# Changelog

All notable changes to this fork are documented here, grouped by milestone.

This fork does not yet use tagged semantic versions; the format loosely follows
[Keep a Changelog](https://keepachangelog.com).

Forked from [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp).

---

## [Unreleased]

Open work — **Phase 4 (Quality/infra)**:
MCP SDK `1.6 → 1.28` upgrade, Pydantic structured output, MCP resources/prompts,
automated tests + CI + linters.

---

## Tier 3 — Rich live capture — 2026-06

### Added
- **Non-media metadata capture** (`T3-1`): incoming `LocationMessage` /
  `LiveLocationMessage` (coords + name/address) and `ContactMessage` /
  `ContactsArrayMessage` (display name + phone parsed from the vCard) are now
  stored and surfaced in `list_messages`.
- **Incoming edits & revokes** (`T3-2`): edits and "delete for everyone" are
  reflected in place without reordering history. Modern WhatsApp edits arrive as
  an encrypted `SecretEncryptedMessage` and are decrypted via
  `DecryptSecretEncryptedMessage`.
- **`get_unread_chats`** (`T3-3`): live unread tracking in a dedicated table
  (history sync deliberately skips it so counts aren't inflated), cleared by
  self read-receipts, replying, or `mark_as_read`. Excludes Status & newsletters.

## Tier A & B + Logout — Coverage backlog — 2026-06

### Added
- **Profile & account**: `set_status_message`, `get_business_profile`,
  `get_user_devices`, `set_default_disappearing`.
- **Group administration**: `set_group_description`, `set_group_announce`,
  `set_group_locked`, `set_group_photo`.
- **Polls**: `vote_poll` + incoming-vote capture (`DecryptPollVote`).
- **Group join requests**: `set_group_join_approval`,
  `get_group_join_requests`, `review_group_join_request`.
- **Invite-code flows**: `get_group_info_from_invite`, `join_group_with_invite`.
- **Presence**: `set_presence`, `subscribe_presence`, `get_presence` (with
  `@lid` ↔ phone-number unification).
- **Call logging**: incoming `events.CallOffer` captured as a `call` message.
- **`logout`**: unlink the session from chat (re-scan QR to relink).

### Fixed
- Richer **downloadable media**: stickers (`.webp`), unwrapped
  ephemeral / view-once / document-with-caption messages, and captions.
- **`download_media` 403**: persist and use the native protobuf `direct_path`
  instead of reconstructing it from the URL (broken by the new `mms3` format).

## Phase 3 — Performance — 2026-06

### Changed
- Migrated the SQLite driver to **`modernc.org/sqlite` (pure Go, no CGO)** —
  the bridge now cross-compiles to darwin/linux/windows with no C toolchain.
  A `dbTime` valuer pins the canonical timestamp format across drivers.
- **Batched history-sync writes** into one transaction per conversation.
- **Reused HTTP connections** from the Python server to the bridge via a
  global `requests.Session()`.

## Phase 2 — Robustness — 2026-06

### Added
- **Ban / health awareness**: handling for `events.TemporaryBan` /
  `ConnectFailure` / `LoggedOut` / `Connected` / `Disconnected`, a thread-safe
  status, the `/api/status` endpoint and `get_status` tool, and **outgoing-send
  pause while temp-banned**.

## Phase 1 — Functionality — 2026-06

### Added
- **Tier 1**: `edit_message`, `delete_message`, `send_typing`, `check_whatsapp`,
  `get_profile_picture`, `get_user_info`, `list_all_contacts`, `send_poll`,
  `get_group_participants`, `get_group_invite_link`, `join_group`, `leave_group`,
  `set_group_name`, `set_group_topic`, `block_contact` / `unblock_contact`.
- **Tier 2**: replies + `@mentions` in `send_message`; chat state
  (`mute_chat`, `pin_chat`, `archive_chat`, `mark_chat`, `star_message`,
  `get_chat_settings`); `request_more_history`; `create_group` +
  `update_group_participants`; `set_disappearing_messages`.

## Foundation — Hardening over upstream — 2026-06

### Added
- **Token-secured bridge**: loopback-only bind + `X-Auth-Token` on every route.
- Live message capture in a goroutine; outgoing messages persisted;
  contact-name resolution with `@lid` ↔ phone-number unification;
  structured tool output; contact cache with TTL + `refresh_contacts`.

### Changed
- Updated **whatsmeow to the 2026 line** (fixes `405 ClientOutdated`).
- SQLite tuned with WAL + busy-timeout + single writer connection.
