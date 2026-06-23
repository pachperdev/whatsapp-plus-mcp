from typing import List, Dict, Any, Optional
from mcp.server.fastmcp import FastMCP
from whatsapp import (
    search_contacts as whatsapp_search_contacts,
    list_messages as whatsapp_list_messages,
    list_chats as whatsapp_list_chats,
    get_chat as whatsapp_get_chat,
    get_direct_chat_by_contact as whatsapp_get_direct_chat_by_contact,
    get_contact_chats as whatsapp_get_contact_chats,
    get_last_interaction as whatsapp_get_last_interaction,
    get_message_context as whatsapp_get_message_context,
    send_message as whatsapp_send_message,
    send_file as whatsapp_send_file,
    send_audio_message as whatsapp_audio_voice_message,
    download_media as whatsapp_download_media,
    refresh_contacts as whatsapp_refresh_contacts,
    list_groups as whatsapp_list_groups,
    mark_as_read as whatsapp_mark_as_read,
    react_to_message as whatsapp_react_to_message,
    edit_message as whatsapp_edit_message,
    delete_message as whatsapp_delete_message,
    send_typing as whatsapp_send_typing,
    send_poll as whatsapp_send_poll,
    list_all_contacts as whatsapp_list_all_contacts,
    check_whatsapp as whatsapp_check_whatsapp,
    get_profile_picture as whatsapp_get_profile_picture,
    get_user_info as whatsapp_get_user_info,
    get_group_participants as whatsapp_get_group_participants,
    get_group_invite_link as whatsapp_get_group_invite_link,
    join_group as whatsapp_join_group,
    leave_group as whatsapp_leave_group,
    set_group_name as whatsapp_set_group_name,
    set_group_topic as whatsapp_set_group_topic,
    block_contact as whatsapp_block_contact,
    unblock_contact as whatsapp_unblock_contact,
    mute_chat as whatsapp_mute_chat,
    pin_chat as whatsapp_pin_chat,
    archive_chat as whatsapp_archive_chat,
    mark_chat as whatsapp_mark_chat,
    star_message as whatsapp_star_message,
    get_chat_settings as whatsapp_get_chat_settings,
    request_more_history as whatsapp_request_more_history
)

# Initialize FastMCP server
mcp = FastMCP("whatsapp")

@mcp.tool()
def search_contacts(query: str) -> List[Dict[str, Any]]:
    """Search WhatsApp contacts by name or phone number.
    
    Args:
        query: Search term to match against contact names or phone numbers
    """
    contacts = whatsapp_search_contacts(query)
    return contacts

@mcp.tool()
def refresh_contacts() -> Dict[str, Any]:
    """Refresh the contact-name index from the WhatsApp address book.

    Call this after adding or renaming contacts so list_chats / search_contacts
    pick up the changes without restarting the server.
    """
    return whatsapp_refresh_contacts()

@mcp.tool()
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
    messages = whatsapp_list_messages(
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

@mcp.tool()
def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Dict[str, Any]]:
    """Get WhatsApp chats matching specified criteria.
    
    Args:
        query: Optional search term to filter chats by name or JID
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
        include_last_message: Whether to include the last message in each chat (default True)
        sort_by: Field to sort results by, either "last_active" or "name" (default "last_active")
    """
    chats = whatsapp_list_chats(
        query=query,
        limit=limit,
        page=page,
        include_last_message=include_last_message,
        sort_by=sort_by
    )
    return chats

@mcp.tool()
def get_chat(chat_jid: str, include_last_message: bool = True) -> Dict[str, Any]:
    """Get WhatsApp chat metadata by JID.
    
    Args:
        chat_jid: The JID of the chat to retrieve
        include_last_message: Whether to include the last message (default True)
    """
    chat = whatsapp_get_chat(chat_jid, include_last_message)
    return chat

@mcp.tool()
def get_direct_chat_by_contact(sender_phone_number: str) -> Dict[str, Any]:
    """Get WhatsApp chat metadata by sender phone number.
    
    Args:
        sender_phone_number: The phone number to search for
    """
    chat = whatsapp_get_direct_chat_by_contact(sender_phone_number)
    return chat

@mcp.tool()
def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Dict[str, Any]]:
    """Get all WhatsApp chats involving the contact.
    
    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    chats = whatsapp_get_contact_chats(jid, limit, page)
    return chats

@mcp.tool()
def get_last_interaction(jid: str) -> str:
    """Get most recent WhatsApp message involving the contact.
    
    Args:
        jid: The JID of the contact to search for
    """
    message = whatsapp_get_last_interaction(jid)
    return message

@mcp.tool()
def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> Dict[str, Any]:
    """Get context around a specific WhatsApp message.
    
    Args:
        message_id: The ID of the message to get context for
        before: Number of messages to include before the target message (default 5)
        after: Number of messages to include after the target message (default 5)
    """
    context = whatsapp_get_message_context(message_id, before, after)
    return context

@mcp.tool()
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

    # Call the whatsapp_send_message function with the unified recipient parameter
    success, status_message = whatsapp_send_message(recipient, message, reply_to, mentions)
    return {
        "success": success,
        "message": status_message
    }

@mcp.tool()
def send_file(recipient: str, media_path: str) -> Dict[str, Any]:
    """Send a file such as a picture, raw audio, video or document via WhatsApp to the specified recipient. For group messages use the JID.
    
    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the media file to send (image, video, document)
    
    Returns:
        A dictionary containing success status and a status message
    """
    
    # Call the whatsapp_send_file function
    success, status_message = whatsapp_send_file(recipient, media_path)
    return {
        "success": success,
        "message": status_message
    }

@mcp.tool()
def send_audio_message(recipient: str, media_path: str) -> Dict[str, Any]:
    """Send any audio file as a WhatsApp audio message to the specified recipient. For group messages use the JID. If it errors due to ffmpeg not being installed, use send_file instead.
    
    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the audio file to send (will be converted to Opus .ogg if it's not a .ogg file)
    
    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_audio_voice_message(recipient, media_path)
    return {
        "success": success,
        "message": status_message
    }

@mcp.tool()
def download_media(message_id: str, chat_jid: str) -> Dict[str, Any]:
    """Download media from a WhatsApp message and get the local file path.
    
    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message
    
    Returns:
        A dictionary containing success status, a status message, and the file path if successful
    """
    file_path = whatsapp_download_media(message_id, chat_jid)
    
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

@mcp.tool()
def list_groups() -> List[Dict[str, Any]]:
    """List all WhatsApp groups you are a member of.

    Returns each group's jid, name and participant_count.
    """
    return whatsapp_list_groups()

@mcp.tool()
def mark_as_read(chat_jid: str, message_ids: List[str]) -> Dict[str, Any]:
    """Mark one or more messages as read in a chat (works for direct chats).

    Args:
        chat_jid: The JID of the chat
        message_ids: List of message IDs to mark as read
    """
    success, status_message = whatsapp_mark_as_read(chat_jid, message_ids)
    return {"success": success, "message": status_message}

@mcp.tool()
def react_to_message(chat_jid: str, message_id: str, emoji: str) -> Dict[str, Any]:
    """React to a message with an emoji (works for direct chats / received messages).

    Args:
        chat_jid: The JID of the chat containing the message
        message_id: The ID of the message to react to
        emoji: The emoji to react with (e.g. "\U0001f44d", "❤️")
    """
    success, status_message = whatsapp_react_to_message(chat_jid, message_id, emoji)
    return {"success": success, "message": status_message}

@mcp.tool()
def edit_message(chat_jid: str, message_id: str, new_text: str) -> Dict[str, Any]:
    """Edit one of your own previously sent messages (within WhatsApp's ~20 min window).

    Args:
        chat_jid: The JID of the chat containing the message
        message_id: The ID of the message to edit (must be your own)
        new_text: The new text content
    """
    success, status_message = whatsapp_edit_message(chat_jid, message_id, new_text)
    return {"success": success, "message": status_message}

@mcp.tool()
def delete_message(chat_jid: str, message_id: str, sender: str = "") -> Dict[str, Any]:
    """Delete a message for everyone (revoke). Leave sender empty for your own message.

    Args:
        chat_jid: The JID of the chat containing the message
        message_id: The ID of the message to delete
        sender: JID of the original sender (only needed to delete someone else's message as group admin)
    """
    success, status_message = whatsapp_delete_message(chat_jid, message_id, sender)
    return {"success": success, "message": status_message}

@mcp.tool()
def send_typing(chat_jid: str, state: str = "composing", media: str = "") -> Dict[str, Any]:
    """Send a chat presence indicator (shows "typing…" / "recording audio…").

    Args:
        chat_jid: The JID of the chat
        state: "composing" (typing) or "paused" (stopped)
        media: "" for text typing, or "audio" for recording-audio indicator
    """
    success, status_message = whatsapp_send_typing(chat_jid, state, media)
    return {"success": success, "message": status_message}

@mcp.tool()
def send_poll(chat_jid: str, question: str, options: List[str], selectable_count: int = 1) -> Dict[str, Any]:
    """Send a poll to a chat or group.

    Args:
        chat_jid: The JID of the chat/group
        question: The poll question
        options: List of answer options (at least 2)
        selectable_count: 1 for single-choice (default), >1 to allow multiple answers
    """
    success, status_message = whatsapp_send_poll(chat_jid, question, options, selectable_count)
    return {"success": success, "message": status_message}

@mcp.tool()
def list_all_contacts(limit: int = 0) -> List[Dict[str, Any]]:
    """List all contacts from your WhatsApp address book (unified by number, sorted by name).

    Args:
        limit: Max contacts to return (0 = all). The address book can be large; set a limit if you only need a sample.
    """
    return whatsapp_list_all_contacts(limit)

@mcp.tool()
def check_whatsapp(phones: List[str]) -> List[Dict[str, Any]]:
    """Check whether phone numbers are registered on WhatsApp.

    Args:
        phones: List of phone numbers in international format (e.g. "+573001234567")
    Returns: per number — query, jid, is_on_whatsapp, is_business.
    """
    return whatsapp_check_whatsapp(phones)

@mcp.tool()
def get_profile_picture(jid: str, preview: bool = False) -> Dict[str, Any]:
    """Get the profile-picture URL of a user or group.

    Args:
        jid: The user/group JID
        preview: True for a small thumbnail, False for full resolution
    """
    return whatsapp_get_profile_picture(jid, preview)

@mcp.tool()
def get_user_info(jids: List[str]) -> Dict[str, Any]:
    """Get info (status/"about" text, business flag) for one or more users.

    Args:
        jids: List of user JIDs
    """
    return whatsapp_get_user_info(jids)

@mcp.tool()
def get_group_participants(group_jid: str) -> Dict[str, Any]:
    """List the participants of a group (jid, phone, is_admin, is_super_admin).

    Args:
        group_jid: The group JID (e.g. "123456789-...@g.us")
    """
    return whatsapp_get_group_participants(group_jid)

@mcp.tool()
def get_group_invite_link(group_jid: str, reset: bool = False) -> Dict[str, Any]:
    """Get a group's invite link.

    Args:
        group_jid: The group JID
        reset: True to revoke the previous link and generate a new one
    """
    return whatsapp_get_group_invite_link(group_jid, reset)

@mcp.tool()
def join_group(code: str) -> Dict[str, Any]:
    """Join a group via its invite link or code.

    Args:
        code: The full invite link (https://chat.whatsapp.com/...) or just the code
    """
    return whatsapp_join_group(code)

@mcp.tool()
def leave_group(group_jid: str) -> Dict[str, Any]:
    """Leave a group.

    Args:
        group_jid: The group JID to leave
    """
    success, status_message = whatsapp_leave_group(group_jid)
    return {"success": success, "message": status_message}

@mcp.tool()
def set_group_name(group_jid: str, name: str) -> Dict[str, Any]:
    """Rename a group (you must be admin; max 25 chars).

    Args:
        group_jid: The group JID
        name: The new group name
    """
    success, status_message = whatsapp_set_group_name(group_jid, name)
    return {"success": success, "message": status_message}

@mcp.tool()
def set_group_topic(group_jid: str, topic: str) -> Dict[str, Any]:
    """Set a group's topic/description (you must be admin).

    Args:
        group_jid: The group JID
        topic: The new topic/description text
    """
    success, status_message = whatsapp_set_group_topic(group_jid, topic)
    return {"success": success, "message": status_message}

@mcp.tool()
def block_contact(jid: str) -> Dict[str, Any]:
    """Block a contact.

    Args:
        jid: The contact JID to block
    """
    success, status_message = whatsapp_block_contact(jid)
    return {"success": success, "message": status_message}

@mcp.tool()
def unblock_contact(jid: str) -> Dict[str, Any]:
    """Unblock a contact.

    Args:
        jid: The contact JID to unblock
    """
    success, status_message = whatsapp_unblock_contact(jid)
    return {"success": success, "message": status_message}

@mcp.tool()
def mute_chat(chat_jid: str, mute: bool = True, duration_hours: int = 0) -> Dict[str, Any]:
    """Mute or unmute a chat.

    Args:
        chat_jid: The chat/group JID
        mute: True to mute, False to unmute
        duration_hours: How long to mute (0 = indefinitely)
    """
    success, status_message = whatsapp_mute_chat(chat_jid, mute, duration_hours)
    return {"success": success, "message": status_message}

@mcp.tool()
def pin_chat(chat_jid: str, pin: bool = True) -> Dict[str, Any]:
    """Pin or unpin a chat to the top of the chat list.

    Args:
        chat_jid: The chat/group JID
        pin: True to pin, False to unpin
    """
    success, status_message = whatsapp_pin_chat(chat_jid, pin)
    return {"success": success, "message": status_message}

@mcp.tool()
def archive_chat(chat_jid: str, archive: bool = True) -> Dict[str, Any]:
    """Archive or unarchive a chat.

    Args:
        chat_jid: The chat/group JID
        archive: True to archive, False to unarchive
    """
    success, status_message = whatsapp_archive_chat(chat_jid, archive)
    return {"success": success, "message": status_message}

@mcp.tool()
def mark_chat(chat_jid: str, read: bool = True) -> Dict[str, Any]:
    """Mark an entire chat as read or unread.

    Args:
        chat_jid: The chat/group JID
        read: True to mark as read, False to mark as unread
    """
    success, status_message = whatsapp_mark_chat(chat_jid, read)
    return {"success": success, "message": status_message}

@mcp.tool()
def star_message(chat_jid: str, message_id: str, starred: bool = True) -> Dict[str, Any]:
    """Star or unstar a message.

    Args:
        chat_jid: The chat JID containing the message
        message_id: The ID of the message
        starred: True to star, False to unstar
    """
    success, status_message = whatsapp_star_message(chat_jid, message_id, starred)
    return {"success": success, "message": status_message}

@mcp.tool()
def get_chat_settings(chat_jid: str) -> Dict[str, Any]:
    """Get a chat's settings: muted, muted_until, pinned, archived.

    Args:
        chat_jid: The chat/group JID
    """
    return whatsapp_get_chat_settings(chat_jid)

@mcp.tool()
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
    success, status_message = whatsapp_request_more_history(chat_jid, count)
    return {"success": success, "message": status_message}

if __name__ == "__main__":
    # Initialize and run the server
    mcp.run(transport='stdio')