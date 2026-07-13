"""Definicion de las tools MCP (@mcp.tool). Wrappers delgados sobre db/ bridge."""
import base64
import os
from typing import Any, Dict, List, Optional

from mcp.server.fastmcp import FastMCP, Image
from mcp.types import ToolAnnotations

from whatsapp_mcp import bridge, db
from whatsapp_mcp.models import Chat, Contact, MessageContext

# Initialize FastMCP server
mcp = FastMCP("whatsapp")

# Perfiles de anotación MCP reutilizables (hints para el cliente: qué tools solo leen,
# cuáles modifican, cuáles son destructivas). readOnly=True => el cliente puede no pedir
# confirmación; destructive=True => acción irreversible/peligrosa, el cliente debería confirmar.
_READ_LOCAL = ToolAnnotations(readOnlyHint=True, openWorldHint=False)
_READ_REMOTE = ToolAnnotations(readOnlyHint=True, openWorldHint=True)
_WRITE = ToolAnnotations(readOnlyHint=False, destructiveHint=False, idempotentHint=False, openWorldHint=True)
_WRITE_IDEMPOTENT = ToolAnnotations(readOnlyHint=False, destructiveHint=False, idempotentHint=True, openWorldHint=True)
_DESTRUCTIVE = ToolAnnotations(readOnlyHint=False, destructiveHint=True, idempotentHint=True, openWorldHint=True)
_DESTRUCTIVE_NONIDEM = ToolAnnotations(readOnlyHint=False, destructiveHint=True, idempotentHint=False, openWorldHint=True)

@mcp.tool(annotations=_READ_LOCAL)
def search_contacts(query: str) -> List[Contact]:
    """Search WhatsApp contacts by name or phone number.
    
    Args:
        query: Search term to match against contact names or phone numbers
    """
    contacts = db.search_contacts(query)
    return contacts

@mcp.tool(annotations=_READ_LOCAL)
def refresh_contacts() -> Dict[str, Any]:
    """Refresh the contact-name index from the WhatsApp address book.

    Call this after adding or renaming contacts so list_chats / search_contacts
    pick up the changes without restarting the server.
    """
    return db.refresh_contacts()

@mcp.tool(annotations=_READ_LOCAL)
def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1
) -> List[Dict[str, Any]]:
    """Get WhatsApp messages matching specified criteria with optional context.
    
    Args:
        after: Optional ISO-8601 formatted string to only return messages after this date
        before: Optional ISO-8601 formatted string to only return messages before this date
        sender_phone_number: Optional phone number to filter messages by sender
        chat_jid: Optional chat JID to filter messages by chat
        query: Optional search term to filter messages by content
        limit: Maximum number of messages to return (default 20)
        page: Page number for pagination (default 0)
        include_context: Whether to include messages before and after matches (default True)
        context_before: Number of messages to include before each match (default 1)
        context_after: Number of messages to include after each match (default 1)
    """
    messages = db.list_messages(
        after=after,
        before=before,
        sender_phone_number=sender_phone_number,
        chat_jid=chat_jid,
        query=query,
        limit=limit,
        page=page,
        include_context=include_context,
        context_before=context_before,
        context_after=context_after
    )
    return messages

@mcp.tool(annotations=_READ_LOCAL)
def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Chat]:
    """Get WhatsApp chats matching specified criteria.
    
    Args:
        query: Optional search term to filter chats by name or JID
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
        include_last_message: Whether to include the last message in each chat (default True)
        sort_by: Field to sort results by, either "last_active" or "name" (default "last_active")
    """
    chats = db.list_chats(
        query=query,
        limit=limit,
        page=page,
        include_last_message=include_last_message,
        sort_by=sort_by
    )
    return chats

@mcp.tool(annotations=_READ_LOCAL)
def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get WhatsApp chat metadata by JID.
    
    Args:
        chat_jid: The JID of the chat to retrieve
        include_last_message: Whether to include the last message (default True)
    """
    chat = db.get_chat(chat_jid, include_last_message)
    return chat

@mcp.tool(annotations=_READ_LOCAL)
def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    """Get WhatsApp chat metadata by sender phone number.
    
    Args:
        sender_phone_number: The phone number to search for
    """
    chat = db.get_direct_chat_by_contact(sender_phone_number)
    return chat

@mcp.tool(annotations=_READ_LOCAL)
def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all WhatsApp chats involving the contact.
    
    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    chats = db.get_contact_chats(jid, limit, page)
    return chats

@mcp.tool(annotations=_READ_LOCAL)
def get_last_interaction(jid: str) -> Optional[str]:
    """Get most recent WhatsApp message involving the contact.
    
    Args:
        jid: The JID of the contact to search for
    """
    message = db.get_last_interaction(jid)
    return message

@mcp.tool(annotations=_READ_LOCAL)
def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> MessageContext:
    """Get context around a specific WhatsApp message.
    
    Args:
        message_id: The ID of the message to get context for
        before: Number of messages to include before the target message (default 5)
        after: Number of messages to include after the target message (default 5)
    """
    context = db.get_message_context(message_id, before, after)
    return context

@mcp.tool(annotations=_WRITE)
def send_message(
    recipient: str,
    message: str,
    reply_to: str = "",
    mentions: Optional[List[str]] = None
) -> Dict[str, Any]:
    """Send a WhatsApp message to a person or group. For group chats use the JID.

    To mention/tag someone, write "@<number>" in the message text (the number with country code,
    no + or symbols) AND list that same number (or JID) in `mentions`. The mention renders as the
    contact's name and notifies them — most useful in groups. Numbers written as "@<number>" in the
    text are auto-detected even if `mentions` is omitted; pass `mentions` to be explicit or to use a
    specific JID (e.g. a participant's "...@lid"). The "@<number>" in the text must match the JID.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        message: The message text to send. For a mention include "@<number>" (e.g. "Hola @573001234567")
        reply_to: Optional message_id to quote/reply to (the message will appear as a reply to it)
        mentions: Optional list of phone numbers or JIDs to tag (each should also appear as @<number> in message)

    Returns:
        A dictionary containing success status and a status message
    """
    # Validate input
    if not recipient:
        return {
            "success": False,
            "message": "Recipient must be provided"
        }

    success, status_message = bridge.send_message(recipient, message, reply_to, mentions)
    return {
        "success": success,
        "message": status_message
    }

@mcp.tool(annotations=_WRITE)
def send_file(recipient: str, media_path: str) -> Dict[str, Any]:
    """Send a file such as a picture, raw audio, video or document via WhatsApp to the specified recipient. For group messages use the JID.
    
    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the media file to send (image, video, document)
    
    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = bridge.send_file(recipient, media_path)
    return {
        "success": success,
        "message": status_message
    }

@mcp.tool(annotations=_WRITE)
def send_audio_message(recipient: str, media_path: str) -> Dict[str, Any]:
    """Send any audio file as a WhatsApp audio message to the specified recipient. For group messages use the JID. If it errors due to ffmpeg not being installed, use send_file instead.
    
    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the audio file to send (will be converted to Opus .ogg if it's not a .ogg file)
    
    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = bridge.send_audio_message(recipient, media_path)
    return {
        "success": success,
        "message": status_message
    }

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def download_media(message_id: str, chat_jid: str) -> Dict[str, Any]:
    """Download media from a WhatsApp message and get the local file path.
    
    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message
    
    Returns:
        A dictionary containing success status, a status message, and the file path if successful
    """
    file_path = bridge.download_media(message_id, chat_jid)
    
    if file_path:
        return {
            "success": True,
            "message": "Media downloaded successfully",
            "file_path": file_path
        }
    else:
        return {
            "success": False,
            "message": "Failed to download media"
        }

@mcp.tool(annotations=_READ_REMOTE)
def list_groups() -> List[Dict[str, Any]]:
    """List all WhatsApp groups you are a member of.

    Returns each group's jid, name and participant_count.
    """
    return bridge.list_groups()

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def mark_as_read(chat_jid: str, message_ids: List[str]) -> Dict[str, Any]:
    """Mark one or more messages as read in a chat (works for direct chats).

    Args:
        chat_jid: The JID of the chat
        message_ids: List of message IDs to mark as read
    """
    success, status_message = bridge.mark_as_read(chat_jid, message_ids)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def react_to_message(chat_jid: str, message_id: str, emoji: str) -> Dict[str, Any]:
    """React to a message with an emoji (works for direct chats / received messages).

    Args:
        chat_jid: The JID of the chat containing the message
        message_id: The ID of the message to react to
        emoji: The emoji to react with (e.g. "\U0001f44d", "❤️")
    """
    success, status_message = bridge.react_to_message(chat_jid, message_id, emoji)
    return {"success": success, "message": status_message}

# Destructivo = pisa contenido irrecuperable: editar reemplaza el texto anterior sin dejar
# copia recuperable vía API (no es additive-only). Sigue siendo idempotente: reenviar el mismo
# new_text converge al mismo estado (por eso _DESTRUCTIVE, que es destructive+idempotent).
@mcp.tool(annotations=_DESTRUCTIVE)
def edit_message(chat_jid: str, message_id: str, new_text: str) -> Dict[str, Any]:
    """Edit one of your own previously sent messages (within WhatsApp's ~20 min window).

    Args:
        chat_jid: The JID of the chat containing the message
        message_id: The ID of the message to edit (must be your own)
        new_text: The new text content
    """
    success, status_message = bridge.edit_message(chat_jid, message_id, new_text)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_DESTRUCTIVE)
def delete_message(chat_jid: str, message_id: str, sender: str = "") -> Dict[str, Any]:
    """Delete a message for everyone (revoke). Leave sender empty for your own message.

    Args:
        chat_jid: The JID of the chat containing the message
        message_id: The ID of the message to delete
        sender: JID of the original sender (only needed to delete someone else's message as group admin)
    """
    success, status_message = bridge.delete_message(chat_jid, message_id, sender)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def send_typing(chat_jid: str, state: str = "composing", media: str = "") -> Dict[str, Any]:
    """Send a chat presence indicator (shows "typing…" / "recording audio…").

    Args:
        chat_jid: The JID of the chat
        state: "composing" (typing) or "paused" (stopped)
        media: "" for text typing, or "audio" for recording-audio indicator
    """
    success, status_message = bridge.send_typing(chat_jid, state, media)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE)
def send_poll(chat_jid: str, question: str, options: List[str], selectable_count: int = 1) -> Dict[str, Any]:
    """Send a poll to a chat or group.

    Args:
        chat_jid: The JID of the chat/group
        question: The poll question
        options: List of answer options (at least 2)
        selectable_count: 1 for single-choice (default), >1 to allow multiple answers
    """
    success, status_message = bridge.send_poll(chat_jid, question, options, selectable_count)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_READ_LOCAL)
def list_all_contacts(limit: int = 0, saved_only: bool = False) -> List[Contact]:
    """List all contacts from your WhatsApp address book (unified by number, sorted by name).

    Args:
        limit: Max contacts to return (0 = all). The address book can be large; set a limit if you only need a sample.
        saved_only: If True, return only contacts truly saved in your address book (a name you
            assigned), excluding people captured solely by their push name from chatting with you.
    """
    return db.list_all_contacts(limit, saved_only)

@mcp.tool(annotations=_READ_REMOTE)
def check_whatsapp(phones: List[str]) -> List[Dict[str, Any]]:
    """Check whether phone numbers are registered on WhatsApp.

    Args:
        phones: List of phone numbers in international format (e.g. "+573001234567")
    Returns: per number — query, jid, is_on_whatsapp, is_business.
    """
    return bridge.check_whatsapp(phones)

@mcp.tool(annotations=_READ_REMOTE)
def get_profile_picture(jid: str, preview: bool = False) -> Dict[str, Any]:
    """Get the profile-picture URL of a user or group.

    Args:
        jid: The user/group JID
        preview: True for a small thumbnail, False for full resolution
    """
    return bridge.get_profile_picture(jid, preview)

@mcp.tool(annotations=_READ_REMOTE)
def get_user_info(jids: List[str]) -> Dict[str, Any]:
    """Get info (status/"about" text, business flag) for one or more users.

    Args:
        jids: List of user JIDs
    """
    return bridge.get_user_info(jids)

@mcp.tool(annotations=_READ_REMOTE)
def get_group_participants(group_jid: str) -> Dict[str, Any]:
    """List the participants of a group (jid, phone, is_admin, is_super_admin).

    Args:
        group_jid: The group JID (e.g. "123456789-...@g.us")
    """
    return bridge.get_group_participants(group_jid)

@mcp.tool(annotations=_READ_REMOTE)
def get_group_invite_link(group_jid: str) -> Dict[str, Any]:
    """Get a group's current invite link WITHOUT changing it (pure read).

    Returns the existing link; it does NOT revoke or regenerate anything. To revoke the
    current link and mint a new one, use reset_group_invite_link instead.

    Args:
        group_jid: The group JID
    """
    return bridge.get_group_invite_link(group_jid, reset=False)

@mcp.tool(annotations=_DESTRUCTIVE_NONIDEM)
def reset_group_invite_link(group_jid: str) -> Dict[str, Any]:
    """REVOKE the group's current invite link and generate a NEW one. Requires admin.

    ⚠️ Irreversible: the previous link stops working immediately for everyone who has it.
    Each call produces a different link (non-idempotent). To read the current link without
    changing it, use get_group_invite_link instead.

    Args:
        group_jid: The group JID
    """
    return bridge.get_group_invite_link(group_jid, reset=True)

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def join_group(code: str) -> Dict[str, Any]:
    """Join a group via its invite link or code.

    Args:
        code: The full invite link (https://chat.whatsapp.com/...) or just the code
    """
    return bridge.join_group(code)

@mcp.tool(annotations=_DESTRUCTIVE)
def leave_group(group_jid: str) -> Dict[str, Any]:
    """Leave a group.

    Args:
        group_jid: The group JID to leave
    """
    success, status_message = bridge.leave_group(group_jid)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def set_group_name(group_jid: str, name: str) -> Dict[str, Any]:
    """Rename a group (you must be admin; max 25 chars).

    Args:
        group_jid: The group JID
        name: The new group name
    """
    success, status_message = bridge.set_group_name(group_jid, name)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def set_group_topic(group_jid: str, topic: str) -> Dict[str, Any]:
    """Set a group's topic/description (you must be admin).

    Args:
        group_jid: The group JID
        topic: The new topic/description text
    """
    success, status_message = bridge.set_group_topic(group_jid, topic)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def block_contact(jid: str) -> Dict[str, Any]:
    """Block a contact.

    Args:
        jid: The contact JID to block
    """
    success, status_message = bridge.block_contact(jid)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def unblock_contact(jid: str) -> Dict[str, Any]:
    """Unblock a contact.

    Args:
        jid: The contact JID to unblock
    """
    success, status_message = bridge.unblock_contact(jid)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def mute_chat(chat_jid: str, mute: bool = True, duration_hours: int = 0) -> Dict[str, Any]:
    """Mute or unmute a chat.

    Args:
        chat_jid: The chat/group JID
        mute: True to mute, False to unmute
        duration_hours: How long to mute (0 = indefinitely)
    """
    success, status_message = bridge.mute_chat(chat_jid, mute, duration_hours)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def pin_chat(chat_jid: str, pin: bool = True) -> Dict[str, Any]:
    """Pin or unpin a chat to the top of the chat list.

    Args:
        chat_jid: The chat/group JID
        pin: True to pin, False to unpin
    """
    success, status_message = bridge.pin_chat(chat_jid, pin)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def archive_chat(chat_jid: str, archive: bool = True) -> Dict[str, Any]:
    """Archive or unarchive a chat.

    Args:
        chat_jid: The chat/group JID
        archive: True to archive, False to unarchive
    """
    success, status_message = bridge.archive_chat(chat_jid, archive)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def mark_chat(chat_jid: str, read: bool = True) -> Dict[str, Any]:
    """Mark an entire chat as read or unread.

    Args:
        chat_jid: The chat/group JID
        read: True to mark as read, False to mark as unread
    """
    success, status_message = bridge.mark_chat(chat_jid, read)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def star_message(chat_jid: str, message_id: str, starred: bool = True) -> Dict[str, Any]:
    """Star or unstar a message.

    Args:
        chat_jid: The chat JID containing the message
        message_id: The ID of the message
        starred: True to star, False to unstar
    """
    success, status_message = bridge.star_message(chat_jid, message_id, starred)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_READ_LOCAL)
def get_chat_settings(chat_jid: str) -> Dict[str, Any]:
    """Get a chat's settings: muted, muted_until, pinned, archived.

    Args:
        chat_jid: The chat/group JID
    """
    return bridge.get_chat_settings(chat_jid)

# NO idempotente: cada llamada ancla en el mensaje mas viejo ya sincronizado y
# pide el lote anterior, asi que llamadas repetidas traen historial progresivamente
# mas viejo (semantica "load more", con efecto acumulativo por invocacion).
@mcp.tool(annotations=_WRITE)
def request_more_history(chat_jid: str, count: int = 50) -> Dict[str, Any]:
    """Request older messages for a chat (like "load earlier messages"). BEST-EFFORT.

    WhatsApp is end-to-end encrypted: the server does NOT store message history; it lives on
    your primary phone. This sends an on-demand history-sync request (a peer message to your own
    account). The older messages, IF your primary phone is online and still holds them, arrive
    asynchronously and are stored in the local DB — query them afterwards with list_messages.

    It is normal for nothing to arrive (phone offline, or it no longer has messages before the
    oldest one already synced). Requires at least one existing message in the chat to anchor the
    request, and best to keep count <= 50.

    Args:
        chat_jid: The chat/group JID to fetch older history for
        count: How many older messages to request (default/recommended max 50)
    """
    success, status_message = bridge.request_more_history(chat_jid, count)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE)
def create_group(name: str, participants: List[str]) -> Dict[str, Any]:
    """Create a new WhatsApp group.

    Args:
        name: Group name (max 25 characters)
        participants: List of phone numbers (country code, no + or symbols) or JIDs to add.
                      Your own account is added automatically — do NOT include it.
    """
    return bridge.create_group(name, participants)

@mcp.tool(annotations=_DESTRUCTIVE_NONIDEM)
def update_group_participants(group_jid: str, participants: List[str], action: str) -> Dict[str, Any]:
    """Add, remove, promote, or demote participants in a group. Requires you to be an admin.

    Args:
        group_jid: The group JID (e.g. "123456789@g.us")
        participants: List of phone numbers or JIDs to act on
        action: One of "add", "remove", "promote", "demote"
    """
    return bridge.update_group_participants(group_jid, participants, action)

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def set_disappearing_messages(chat_jid: str, duration: str = "off") -> Dict[str, Any]:
    """Set the disappearing-messages timer for a chat or group.

    WhatsApp only allows preset values. New messages auto-delete after the chosen period.

    Args:
        chat_jid: The chat/group JID
        duration: One of "off", "24h", "7d", "90d"
    """
    success, status_message = bridge.set_disappearing_messages(chat_jid, duration)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_READ_LOCAL)
def get_status() -> Dict[str, Any]:
    """Get the WhatsApp bridge connection/session health.

    Returns connection state, login state, and any temporary-ban / logout info: connected,
    logged_in, temp_banned (with ban_code/ban_reason/ban_expires_at), needs_qr, and last
    connect failure. While temp_banned is true the bridge pauses outgoing messages.
    """
    return bridge.get_status()

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def set_status_message(message: str) -> Dict[str, Any]:
    """Set your own WhatsApp status message (the "about" text on your profile).

    Args:
        message: The new about/status text
    """
    success, status_message = bridge.set_status_message(message)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_READ_REMOTE)
def get_business_profile(jid: str) -> Dict[str, Any]:
    """Get the business profile of a WhatsApp Business contact.

    Returns address, email, categories and business hours timezone when the contact is a
    business account (is_business=false otherwise).

    Args:
        jid: The contact JID (e.g. "573001234567@s.whatsapp.net")
    """
    return bridge.get_business_profile(jid)

@mcp.tool(annotations=_READ_REMOTE)
def get_user_devices(jids: List[str]) -> Dict[str, Any]:
    """List the linked devices (companion devices) of one or more contacts.

    Args:
        jids: List of contact phone numbers or JIDs
    """
    return bridge.get_user_devices(jids)

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def set_default_disappearing(duration: str = "off") -> Dict[str, Any]:
    """Set the DEFAULT disappearing-messages timer applied to NEW chats you start.

    Does not change existing chats (use set_disappearing_messages for a specific chat).

    Args:
        duration: One of "off", "24h", "7d", "90d"
    """
    success, status_message = bridge.set_default_disappearing(duration)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def set_group_description(group_jid: str, description: str) -> Dict[str, Any]:
    """Set a group's description/topic text. Requires you to be a group admin.

    Args:
        group_jid: The group JID
        description: The new description text
    """
    success, status_message = bridge.set_group_description(group_jid, description)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def set_group_announce(group_jid: str, enable: bool = True) -> Dict[str, Any]:
    """Set group announce mode. When enabled, ONLY admins can send messages. Requires admin.

    Args:
        group_jid: The group JID
        enable: True = only admins can post; False = everyone can post
    """
    success, status_message = bridge.set_group_announce(group_jid, enable)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def set_group_locked(group_jid: str, enable: bool = True) -> Dict[str, Any]:
    """Set group locked mode. When enabled, ONLY admins can edit group info (name/photo/description). Requires admin.

    Args:
        group_jid: The group JID
        enable: True = only admins can edit info; False = everyone can edit
    """
    success, status_message = bridge.set_group_locked(group_jid, enable)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def set_group_photo(group_jid: str, image_path: str) -> Dict[str, Any]:
    """Set a group's photo from a local image file. Requires admin.

    The image MUST be a SQUARE JPEG (e.g. 640x640). WhatsApp rejects non-square images and
    whatsmeow does not resize, so crop/resize beforehand
    (e.g. macOS: `sips -z 640 640 in.jpg --out square.jpg`).

    Args:
        group_jid: The group JID
        image_path: Absolute path to a SQUARE JPEG image
    """
    return bridge.set_group_photo(group_jid, image_path)

@mcp.tool(annotations=_WRITE)
def vote_poll(chat_jid: str, poll_message_id: str, options: List[str]) -> Dict[str, Any]:
    """Vote in an existing WhatsApp poll.

    The poll must have been captured (sent or received in a recent session). Incoming votes from
    other people are decrypted and stored automatically as "poll_vote" messages — query them with
    list_messages on that chat.

    Args:
        chat_jid: The chat/group JID where the poll is
        poll_message_id: The message ID of the poll to vote on
        options: List of option names to select (exact text of the poll options)
    """
    success, status_message = bridge.vote_poll(chat_jid, poll_message_id, options)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def set_group_join_approval(group_jid: str, enable: bool = True) -> Dict[str, Any]:
    """Enable/disable group join-approval mode (new members need admin approval). Requires admin.

    Args:
        group_jid: The group JID
        enable: True = joins require approval; False = anyone with the link joins directly
    """
    success, status_message = bridge.set_group_join_approval(group_jid, enable)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_READ_REMOTE)
def get_group_join_requests(group_jid: str) -> Dict[str, Any]:
    """List pending join requests for a group (each with jid and requested_at). Requires admin.

    Args:
        group_jid: The group JID
    """
    return bridge.get_group_join_requests(group_jid)

@mcp.tool(annotations=_WRITE)
def review_group_join_request(group_jid: str, participants: List[str], action: str) -> Dict[str, Any]:
    """Approve or reject pending join requests for a group. Requires admin.

    Args:
        group_jid: The group JID
        participants: List of requester phone numbers or JIDs to act on
        action: "approve" or "reject"
    """
    return bridge.review_group_join_request(group_jid, participants, action)

@mcp.tool(annotations=_READ_REMOTE)
def get_group_info_from_invite(chat_jid: str, invite_message_id: str) -> Dict[str, Any]:
    """Inspect a group from a received group-invite message, WITHOUT joining.

    Use the message_id of a captured group-invite message (media_type "group_invite").

    Args:
        chat_jid: The chat JID where the invite message is
        invite_message_id: The message ID of the group-invite message
    """
    return bridge.get_group_info_from_invite(chat_jid, invite_message_id)

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def join_group_with_invite(chat_jid: str, invite_message_id: str) -> Dict[str, Any]:
    """Join a group using a received group-invite message (not a chat.whatsapp.com link).

    Use the message_id of a captured group-invite message (media_type "group_invite"). For
    chat.whatsapp.com links use join_group instead.

    Args:
        chat_jid: The chat JID where the invite message is
        invite_message_id: The message ID of the group-invite message
    """
    success, status_message = bridge.join_group_with_invite(chat_jid, invite_message_id)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def set_presence(state: str = "available") -> Dict[str, Any]:
    """Set your own presence. "available" (online) is REQUIRED to receive others' presence updates.

    Args:
        state: "available" or "unavailable"
    """
    success, status_message = bridge.set_presence(state)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_WRITE_IDEMPOTENT)
def subscribe_presence(jid: str) -> Dict[str, Any]:
    """Subscribe to a contact's presence so you start receiving their online/last-seen updates.

    Call set_presence("available") first. Updates arrive asynchronously; read them with get_presence.

    Args:
        jid: The contact JID
    """
    success, status_message = bridge.subscribe_presence(jid)
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_READ_LOCAL)
def get_presence(jid: str) -> Dict[str, Any]:
    """Get the last known presence of a contact: online, last_seen, typing.

    Requires having called subscribe_presence and set_presence("available") earlier, and waiting
    for the contact to change state. Returns tracked=false if no data yet.

    Args:
        jid: The contact JID
    """
    return bridge.get_presence(jid)

@mcp.tool(annotations=_DESTRUCTIVE)
def logout() -> Dict[str, Any]:
    """Log out / unlink this WhatsApp session.

    ⚠️ After this you will be DISCONNECTED and must re-scan the QR code (restart the bridge) to
    use the MCP again. Only use when the user explicitly wants to unlink the account.
    """
    success, status_message = bridge.logout()
    return {"success": success, "message": status_message}

@mcp.tool(annotations=_READ_LOCAL)
def get_unread_chats() -> List[Dict[str, Any]]:
    """List chats with unread incoming messages, most recent first.

    Each item: chat_jid, name, unread_count, last_time. Unread is tracked live by the bridge
    (counted from when the bridge started; history-sync does not populate it) and cleared when
    you read the chat on your phone, reply, or call mark_as_read. Returns [] if nothing unread.
    """
    # El bridge devuelve los chats crudos (capa de transporte). La resolución del nombre es
    # lógica de dominio (índice de contactos de db), así que el enriquecimiento vive acá.
    out: List[Dict[str, Any]] = []
    for c in bridge.get_unread_chats():
        jid = c.get("chat_jid", "")
        out.append({
            "chat_jid": jid,
            "name": db.resolve_contact_name(jid) or jid,
            "unread_count": c.get("unread_count", 0),
            "last_time": c.get("last_time", ""),
        })
    return out


# --- Login autogestionado (plug-and-play): el MCP gestiona el bridge y el QR ---

def _open_image_preview(png_bytes: bytes) -> str:
    """Escribe el PNG a un archivo temporal y lo abre con el visor del SO.

    Devuelve la ruta escrita, o "" si no se pudo abrir (headless, SO sin visor, etc.);
    el QR inline en la conversacion sigue disponible como fallback.
    """
    import subprocess
    import sys
    import tempfile

    path = os.path.join(tempfile.gettempdir(), "whatsapp_login_qr.png")
    try:
        with open(path, "wb") as f:
            f.write(png_bytes)
        if sys.platform == "darwin":
            subprocess.Popen(["open", path])
        elif sys.platform.startswith("linux"):
            subprocess.Popen(["xdg-open", path])
        elif sys.platform.startswith("win"):
            os.startfile(path)  # type: ignore[attr-defined]
        else:
            return ""
        return path
    except OSError:
        return ""


# structured_output=False: el retorno mezcla Image y texto; con structured output FastMCP
# intenta serializar el Image a JSON y falla ("Unable to serialize unknown type ... Image").
@mcp.tool(annotations=_WRITE_IDEMPOTENT, structured_output=False)
def login_with_qr(open_preview: bool = True) -> List[Any]:
    """Connect to WhatsApp, reusing the existing session or guiding a QR login.

    Self-managing: verifies the bridge process (adopts a healthy one, spawns or recycles
    it if needed — never duplicates connections), validates the current session, and only
    generates a QR when there is no valid session. The QR is returned INLINE as an image
    in the conversation; with open_preview=True it is also opened in the OS image viewer
    so the user can scan it comfortably.

    QR codes rotate every ~30s: if the user reports it expired, call this tool again to
    get the current one. After the user scans, call get_status to confirm login.

    Args:
        open_preview: Also open the QR in the local image viewer (default True)
    """
    result = bridge.acquire_login_qr()
    if not result.get("ok"):
        return [f"❌ No se pudo iniciar el login: {result.get('message', 'error desconocido')}"]
    if result.get("logged_in"):
        st = result.get("status", {})
        return [
            "✅ Sesión válida existente: conectado como "
            f"{st.get('jid', 'desconocido')}. No hace falta escanear QR."
        ]

    qr = result["qr"]
    png_bytes = base64.b64decode(qr["png_base64"])
    contents: List[Any] = []
    preview_note = ""
    if open_preview:
        path = _open_image_preview(png_bytes)
        preview_note = (
            f" También quedó abierto en tu visor de imágenes ({path})."
            if path
            else " (No se pudo abrir el visor local; usa la imagen de la conversación.)"
        )
    contents.append(Image(data=png_bytes, format="png"))
    contents.append(
        "📱 Escanea este QR desde WhatsApp → Ajustes → Dispositivos vinculados → "
        f"Vincular un dispositivo. Expira ~{qr.get('expires_at', 'en <1 min')}; si expira, "
        f"vuelve a llamar login_with_qr para obtener el vigente.{preview_note} "
        "Tras escanear, confirma con get_status."
    )
    return contents


@mcp.tool(annotations=_DESTRUCTIVE)
def shutdown_bridge() -> Dict[str, Any]:
    """Gracefully shut down the WhatsApp bridge process (the session is preserved).

    The next tool that needs the bridge (e.g. login_with_qr) will start it again. Useful
    to recycle a misbehaving bridge without touching the WhatsApp session/credentials.
    """
    return bridge.shutdown_bridge()
