# Changelog

All notable changes to this fork are documented here, grouped by milestone.

This fork does not yet use tagged semantic versions; the format loosely follows
[Keep a Changelog](https://keepachangelog.com).

Forked from [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp).

---

## [Unreleased]

### Added

- **`transcribe_audio_message` tool (#67)**: local speech-to-text for voice notes /
  audio messages, behind the new optional `transcription` extra (faster-whisper on
  CTranslate2, int8 CPU; PyAV decodes WhatsApp's `.ogg` Opus without a system ffmpeg —
  nothing leaves the machine). Reuses the idempotent `download_media` path, auto-selects
  the Whisper tier from RAM/cores (`tiny` → `large-v3-turbo`; override via the `model`
  param or `WHATSAPP_TRANSCRIPTION_MODEL`), derives `beam_size` from the tier (greedy on
  small tiers, `WHATSAPP_TRANSCRIPTION_BEAM` overrides), keeps a single resident model
  cached per (model, device, compute) and caps duration with
  `WHATSAPP_TRANSCRIPTION_MAX_SECONDS` (default 900 s, `0` = unlimited) **before** paying
  for the transcription. Model weights download once to
  `WHATSAPP_TRANSCRIPTION_MODELS_DIR` (`~/.whatsapp-mcp/models` in plugin mode,
  `<store>/models` in repo mode); real download sizes: ~75 MB (`tiny`), ~145 MB
  (`base`), ~484 MB (`small`), ~1.6 GB (`large-v3-turbo`). The `model` param and env
  var are validated against that allowlist — any other string would be treated by
  faster-whisper as an arbitrary Hugging Face repo-id and downloaded, breaking the
  100% local promise (invalid param → actionable error; invalid env → warning + auto
  heuristic). Model loading/transcription holds the model lock with a bounded timeout
  (a busy result instead of queueing forever) and the weight download itself is
  bounded via `HF_HUB_DOWNLOAD_TIMEOUT` (defaulted to 30 s, user value respected), so
  a dead connection can never wedge the server. The lazy import keeps the server
  fully functional without the extra: the tool returns an actionable install hint
  instead of crashing.
  Gotcha documented in the README: `uv run main.py` does not install extras — launch
  with `uv run --extra transcription main.py`.

- **`delete_chat` tool (#66)**: delete an entire chat from the chat list on all your
  devices, via the WhatsApp app-state sync (`appstate.BuildDeleteChat`, unlocked by the
  whatsmeow bump above). Optional `delete_media` also removes the chat's downloaded media
  under `store/<chat>/`. Same 3-piece pattern as the other chat-state tools; being an
  app-state mutation it may transiently return the `409 LTHash` recovery and need a retry.
  After a successful app-state delete the bridge also **prunes the local DB**
  (`MessageStore.DeleteChat`): the chat's `messages`, `unread_messages` and `chats` rows are
  removed in one transaction so local reads (`list_chats` / `list_messages`) stop showing it
  (best-effort — a failed prune logs a warning but never fails the authoritative remote delete).

### Security

- **`delete_chat` media removal hardened against path traversal** (`MessageStore.DeleteChat`):
  the new local-prune's `os.RemoveAll` could escape the store — `types.ParseJID` accepts any
  string without `@` unvalidated and the sanitizer left `.`/`..`/empty untouched, so
  `chat_jid=""` resolved to the store dir itself (wiping session + DBs + token) and `".."` to
  its parent (`$HOME` in plugin mode). Now `mediaDirForChat` requires the resolved path to stay
  strictly inside the store (`filepath.Clean` + separator-prefix check); anything else is
  refused and the `RemoveAll` is skipped. Caught by the risk review before release.
- **Token regeneration no longer trusts `chmod`** (`GetOrCreateBridgeToken`):
  regenerating over a pre-existing lax-permission `.bridge_token` (e.g. `0644`
  left by a crash) used to keep the live token world-readable — `os.WriteFile`
  only applies the mode on file *creation*. The file is now removed and recreated
  (so `0600` applies at creation), failing closed only if a `Stat` check confirms
  the effective mode is still insecure. Found by the new test campaign.
- **Media exfiltration guard hardened** (`ValidateMediaPath`): the `store/`
  protection now compares **by inode** (`os.SameFile`) instead of a case-sensitive
  string prefix. Closes a bypass on case-insensitive filesystems (APFS/NTFS) where
  `STORE/whatsapp.db` leaked the session keys via `send_file` / `set_group_photo`,
  plus a hardlink bypass. The validator returns the canonical path and callers read
  from it, closing the TOCTOU window between validation and read.
- **Anti-ban check extended** to `react` / `edit` / `revoke` / `poll` / `poll_vote`
  (they send via `client.SendMessage` directly and previously skipped the temp-ban
  gate). Outgoing stanzas are now paused on all send paths while temp-banned.
- **Opt-in media allowlist** (`WHATSAPP_MEDIA_ALLOWED_DIRS`): confines `send_file` /
  `set_group_photo` to an allowlist of directories, mitigating prompt-injection
  exfiltration. Unset = historic behavior.
- **Loopback bind validated** (`WHATSAPP_BRIDGE_ADDR`): non-loopback addresses are
  rejected at startup. Auth token re-`chmod`ed to `0600` on reuse. Request bodies
  capped at 1 MiB (`MaxBytesReader`).
- **OGG parser bounds fix**: `AnalyzeOggOpus` no longer panics on a truncated
  `OpusHead` (off-by-4 in the `SampleRate` read).

### Fixed

- **Media filename collisions no longer serve the wrong bytes** (`internal/wa`): captured
  media filenames were generated from the capture timestamp at second granularity
  (`audio_YYYYMMDD_HHMMSS.ogg`), so history-sync bursts stored different messages under the
  **same** `filename` — and `DownloadMedia` blindly reused any existing file in
  `store/<chat>/`, returning the bytes of the *wrong* message (a voice note's transcription
  came back with another note's text). Fixed in two layers: (1) generated filenames now
  carry a unique suffix derived from the message ID (`mediaFilename`:
  `audio_YYYYMMDD_HHMMSS_<id8>.ogg`, sanitized to `[A-Za-z0-9]` with a short-hash fallback);
  (2) before reusing an existing file, `DownloadMedia` validates it against the requested
  message's persisted metadata (`canReuseMediaFile`: size vs `file_length`, then sha256 vs
  `file_sha256`; mismatch → logged and re-downloaded, no metadata → reused as before), so
  even pre-existing colliding rows now return the correct media.
- **Dead `"Error parsing response"` branch restored** (`bridge.py`): since
  requests ≥ 2.27, `response.json()` raises `requests.exceptions.JSONDecodeError`,
  which inherits from `RequestException` — the earlier `except` swallowed invalid
  JSON as `"Request error"`. The five affected functions (`send_message`,
  `send_file`, `send_audio_message`, `download_media`, `get_status`) now catch
  `JSONDecodeError` first, honoring the documented error contract.

### Changed

- **Media reuse skips the sha256 pass for collision-proof filenames** (`internal/wa`):
  `DownloadMedia` re-hashed the whole file on every cache hit to guard against the
  historic filename collision — negligible for voice notes, real cost for large videos
  on repeated `download_media` calls. Bridge-generated filenames already embed the
  `_<id8>` suffix derived from the message ID, so the file↔message link is unambiguous:
  when the media type is one whose filename is always bridge-generated
  (audio/image/video/sticker) and the stored filename carries the suffix matching the
  requested message (`shouldSkipSHA` → `filenameHasMessageSuffix`, sharing the exact
  sanitization/fallback via the extracted `sanitizedID8` helper), the sha256 check is
  skipped and only the cheap size check runs. Documents are deliberately excluded —
  their filename is sender-provided (`doc.GetFileName()`) and could mimic the suffix,
  so they always keep the full size + sha256 validation, as do legacy filenames
  (no suffix) and unknown media types (fail-safe).
- **Dependencies**: whatsmeow bumped to 2026-07-20 (protocol protobuf updates,
  new `IsOnWhatsApp` query format backing `check_whatsapp`, DMs always sent via
  LID — already covered by the `@lid`↔PN unification in `db.py`);
  `modernc.org/sqlite` 1.53 → 1.54 (SQLite 3.53.3). `mcp` stays pinned `<2` on
  purpose (v2 is still pre-release).
- **Bridge configuration by environment** (`internal/config`): `WHATSAPP_BRIDGE_ADDR`
  / `WHATSAPP_STORE_DIR` / `WHATSAPP_MEDIA_ALLOWED_DIRS`, all paths resolved to
  absolute at startup (no more accidental CWD-relative `store/`).
- **`download_media → send_file`** works again: only the `store/` **root** (session /
  history / token) is protected; downloaded media in `store/<chat>/` is forwardable.
- **MCP contract refinements**: `edit_message` marked destructive; `request_more_history`
  reclassified non-idempotent; `get_group_invite_link` split into a pure-read tool and
  a destructive `reset_group_invite_link` (**63 tools**).

### Internal / quality

- **Test-quality follow-ups**: the revoke-scenario boilerplate shared between
  `dispatcher_test.go` and `events_test.go` extracted to a single helper
  (`internal/wa/helpers_test.go`); `acquire_login_qr`'s fixed recycle `sleep(2.0)` made
  injectable (`recycle_wait_s`, default unchanged) so the 3 recycle tests no longer pay ~2s
  each (`test_bridge.py` 8.2s → 1.6s). `.codegraph/` (local CodeGraph index) gitignored.
- **Test campaign (+122 cases)**: bridge coverage 20.4% → 29.7% (media/edit
  protobuf parsing, store revoke/edit invariants, bridge token, group invites,
  quoted context, event dispatcher, `sendAppState` LTHash recovery 0 → 100%);
  Python coverage 37% → 56% (`@lid`↔number resolution, contact index + TTL cache,
  message listing with sibling JIDs, supervisor state machine, HTTP error
  contract, release binary download). Full 4R review (risk / reliability /
  resilience / readability) run over the whole diff.
- **`api/server.go` split by domain** (1535 → 202 lines): the 51 handlers now
  register per domain in `routes_{messages,chats,groups,contacts,session}.go`
  (byte-identical bodies, HTTP contracts untouched).
- **Event dispatcher extracted from `main.go`** to `wa.Service.HandleEvent`
  (`internal/wa/dispatcher.go`, now testable; `main.go` 310 → 229 lines is pure
  bootstrap).
- **`sendAppState` seam** (`appStateSender` interface) makes the two-level
  LTHash-conflict recovery unit-testable; **`_ro_connect`** context manager
  unifies the 10 read-only SQLite openings in `db.py`.
- Bridge: 49 REST handlers deduplicated behind `decodeJSON` / `parseJID` /
  `respondErr` / `respondOK` (`-176` lines); `/api/send` errors unified to JSON; the
  two remaining ad-hoc SQL queries moved out of the `api` layer into `store` methods.
- Python: layer leak in `bridge.get_unread_chats` removed (name resolution moved to
  the tools layer); `_bridge_get` helper; contract tests for the Pydantic models.
- Tests: `httptest` harness for `NewServer`; regression tests for every security fix.

Previously completed under **Phase 4 (Quality/infra)**: MCP SDK `1.6 → 1.28`,
Pydantic structured output, MCP prompts, CI + linters.

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
