import sqlite3
import sys
import time
import logging
from datetime import datetime
from dataclasses import dataclass
from typing import Optional, List, Tuple, Dict, Any
import os.path
import requests
import json
import audio

# Logging SIEMPRE a stderr: en transporte stdio, stdout es el canal del protocolo
# MCP (JSON-RPC). Un logger.error() a stdout inyecta texto crudo y corrompe el stream.
logger = logging.getLogger("whatsapp_mcp")
if not logger.handlers:
    _handler = logging.StreamHandler(sys.stderr)
    _handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(name)s: %(message)s"))
    logger.addHandler(_handler)
    logger.setLevel(logging.INFO)

# Timeout (connect, read) para las llamadas a la REST API del bridge.
# Sin esto, si el bridge se cuelga (no caído) el server MCP queda bloqueado para siempre.
REQUEST_TIMEOUT = (5, 30)

MESSAGES_DB_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store', 'messages.db')
# whatsmeow guarda la libreta de contactos y el mapeo lid<->numero aqui
WHATSAPP_DB_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store', 'whatsapp.db')
WHATSAPP_API_BASE_URL = "http://localhost:8080/api"
# Token de auth compartido con el bridge (el bridge lo genera en store/.bridge_token).
BRIDGE_TOKEN_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store', '.bridge_token')


def _bridge_token() -> str:
    """Lee el token compartido que el bridge persiste en store/.bridge_token."""
    try:
        with open(BRIDGE_TOKEN_PATH) as f:
            return f.read().strip()
    except OSError:
        return ""

@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: Optional[str] = None
    media_type: Optional[str] = None

@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")

@dataclass
class Contact:
    phone_number: str
    name: Optional[str]
    jid: str

@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]

def _normalize_phone(jid_or_local: str) -> str:
    """Extrae la parte numerica/local de un JID, quitando sufijo (@...) y device id (:NN)."""
    if not jid_or_local:
        return ""
    local = str(jid_or_local).split('@')[0]
    return local.split(':')[0]


def _load_contact_index():
    """Carga indices de nombres desde la libreta de whatsmeow (whatsapp.db).

    Devuelve (names, lid_to_pn):
      names:     { numero_telefono: mejor_nombre_para_mostrar }
      lid_to_pn: { lid: numero_telefono }
    """
    names = {}
    lid_to_pn = {}
    try:
        conn = sqlite3.connect(f"file:{WHATSAPP_DB_PATH}?mode=ro", uri=True)
        cursor = conn.cursor()
        try:
            for their_jid, first_name, full_name, push_name, business_name in cursor.execute(
                "SELECT their_jid, first_name, full_name, push_name, business_name FROM whatsmeow_contacts"
            ):
                pn = _normalize_phone(their_jid)
                # Preferimos como esta guardado en la libreta del usuario, luego push name
                name = (full_name or first_name or push_name or business_name or "").strip()
                if pn and name:
                    names[pn] = name
        except sqlite3.Error:
            pass
        try:
            for lid, pn in cursor.execute("SELECT lid, pn FROM whatsmeow_lid_map"):
                lid_to_pn[_normalize_phone(str(lid))] = _normalize_phone(str(pn))
        except sqlite3.Error:
            pass
        conn.close()
    except sqlite3.Error as e:
        logger.error(f"Database error loading contacts: {e}")
    return names, lid_to_pn


_CONTACT_INDEX = None
_CONTACT_INDEX_TS = 0.0
_CONTACT_INDEX_TTL = 300.0  # 5 min


def _get_contact_index(refresh: bool = False):
    """Cache en memoria del indice de contactos, con TTL para no quedar obsoleto.

    El server MCP es de larga vida; sin TTL, un contacto agregado/renombrado en
    WhatsApp no aparecia hasta reiniciar. Se recarga si pasa el TTL o si refresh=True.
    """
    global _CONTACT_INDEX, _CONTACT_INDEX_TS
    now = time.monotonic()
    if _CONTACT_INDEX is None or refresh or (now - _CONTACT_INDEX_TS) > _CONTACT_INDEX_TTL:
        _CONTACT_INDEX = _load_contact_index()
        _CONTACT_INDEX_TS = now
    return _CONTACT_INDEX


def resolve_contact_name(jid: str) -> Optional[str]:
    """Resuelve el nombre real de un contacto cruzando lid -> numero -> nombre.

    Devuelve None si no hay nombre en la libreta (p.ej. grupos o desconocidos).
    """
    if not jid:
        return None
    suffix = jid.split('@', 1)[1] if '@' in jid else ''
    if suffix.startswith('g.us'):
        return None  # los grupos usan su propio nombre, no la libreta
    names, lid_to_pn = _get_contact_index()
    local = _normalize_phone(jid)
    pn = lid_to_pn.get(local) if suffix.startswith('lid') else local
    # El sender crudo a veces es un lid SIN sufijo @lid: mapearlo a su numero real
    # para resolver el nombre de la libreta (consistencia con list_chats).
    if pn and pn in lid_to_pn:
        pn = lid_to_pn[pn]
    if pn and pn in names:
        return names[pn]
    return None


def _canonical_chat_key(jid: str) -> str:
    """Clave para unificar chats que son la misma persona bajo distintos JIDs.

    Un contacto aparece a veces como <lid>@lid (mensajes en vivo) y como
    <numero>@s.whatsapp.net (history sync); ambos colapsan al mismo numero.
    Grupos y broadcast NO se unifican (devuelven su jid tal cual).
    """
    if not jid:
        return jid
    suffix = jid.split('@', 1)[1] if '@' in jid else ''
    if suffix.startswith('g.us') or suffix.startswith('broadcast'):
        return jid
    names, lid_to_pn = _get_contact_index()
    local = _normalize_phone(jid)
    pn = lid_to_pn.get(local) if suffix.startswith('lid') else local
    return pn or jid


def refresh_contacts() -> dict:
    """Fuerza recargar el indice de nombres desde la libreta de WhatsApp.

    Util tras agregar o renombrar contactos para que list_chats / search_contacts
    los reflejen sin reiniciar el server.
    """
    names, _ = _get_contact_index(refresh=True)
    return {"success": True, "contacts_loaded": len(names)}


def _sibling_chat_jids(chat_jid: str) -> List[str]:
    """Todos los jids del mismo contacto (lid + numero) para unir su conversacion.

    Un contacto tiene mensajes bajo su @lid (entrantes en vivo) y bajo
    numero@s.whatsapp.net (salientes/history). list_messages debe traer ambos.
    Grupos/broadcast devuelven solo su propio jid.
    """
    jids = {chat_jid}
    suffix = chat_jid.split('@', 1)[1] if '@' in chat_jid else ''
    if suffix.startswith('g.us') or suffix.startswith('broadcast'):
        return list(jids)
    _, lid_to_pn = _get_contact_index()
    local = _normalize_phone(chat_jid)
    if suffix.startswith('lid'):
        pn = lid_to_pn.get(local)
        if pn:
            jids.add(f"{pn}@s.whatsapp.net")
    else:
        pn = local
        for lid, mapped_pn in lid_to_pn.items():
            if mapped_pn == pn:
                jids.add(f"{lid}@lid")
    return list(jids)


def get_sender_name(sender_jid: str) -> str:
    # 1) Nombre real desde la libreta de WhatsApp (lid -> numero -> nombre)
    name = resolve_contact_name(sender_jid)
    if name:
        return name
    # 2) Fallback: nombre guardado en la tabla chats de messages.db
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        cursor.execute("SELECT name FROM chats WHERE jid = ? LIMIT 1", (sender_jid,))
        result = cursor.fetchone()
        if not result:
            phone_part = sender_jid.split('@')[0] if '@' in sender_jid else sender_jid
            cursor.execute("SELECT name FROM chats WHERE jid LIKE ? LIMIT 1", (f"%{phone_part}%",))
            result = cursor.fetchone()
        if result and result[0]:
            return result[0]
        return sender_jid
    except sqlite3.Error as e:
        logger.error(f"Database error while getting sender name: {e}")
        return sender_jid
    finally:
        if 'conn' in locals():
            conn.close()

def format_message(message: Message, show_chat_info: bool = True) -> None:
    """Print a single message with consistent formatting."""
    output = ""
    
    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "
        
    content_prefix = ""
    if hasattr(message, 'media_type') and message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "
    
    try:
        sender_name = get_sender_name(message.sender) if not message.is_from_me else "Me"
        output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    except Exception as e:
        logger.error(f"Error formatting message: {e}")
    return output

def format_messages_list(messages: List[Message], show_chat_info: bool = True) -> None:
    output = ""
    if not messages:
        output += "No messages to display."
        return output
    
    for message in messages:
        output += format_message(message, show_chat_info)
    return output


def message_to_dict(message: Message) -> Dict[str, Any]:
    """Convierte un Message a dict estructurado (consumible por el LLM).

    A diferencia del texto plano, expone message_id/chat_jid (para encadenar con
    download_media) y resuelve el nombre del remitente.
    """
    sender_name = "Me" if message.is_from_me else (resolve_contact_name(message.sender) or message.sender)
    return {
        "timestamp": message.timestamp.isoformat() if message.timestamp else None,
        "chat_jid": message.chat_jid,
        "chat_name": message.chat_name,
        "message_id": message.id,
        "sender": message.sender,
        "sender_name": sender_name,
        "is_from_me": bool(message.is_from_me),
        "content": message.content,
        "media_type": message.media_type or None,
    }


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
    """Get messages matching the specified criteria with optional context."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Build base query
        query_parts = ["SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type FROM messages"]
        query_parts.append("JOIN chats ON messages.chat_jid = chats.jid")
        where_clauses = []
        params = []
        
        # Add filters
        if after:
            try:
                after = datetime.fromisoformat(after)
            except ValueError:
                raise ValueError(f"Invalid date format for 'after': {after}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp > ?")
            params.append(after)

        if before:
            try:
                before = datetime.fromisoformat(before)
            except ValueError:
                raise ValueError(f"Invalid date format for 'before': {before}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp < ?")
            params.append(before)

        if sender_phone_number:
            where_clauses.append("messages.sender = ?")
            params.append(sender_phone_number)
            
        if chat_jid:
            # Unir la conversacion completa: un contacto tiene mensajes bajo su @lid
            # (entrantes en vivo) y bajo numero@s.whatsapp.net (salientes/history).
            siblings = _sibling_chat_jids(chat_jid)
            placeholders = ",".join(["?"] * len(siblings))
            where_clauses.append(f"messages.chat_jid IN ({placeholders})")
            params.extend(siblings)
            
        if query:
            where_clauses.append("LOWER(messages.content) LIKE LOWER(?)")
            params.append(f"%{query}%")
            
        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
            
        # Add pagination
        offset = page * limit
        query_parts.append("ORDER BY messages.timestamp DESC")
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])
        
        cursor.execute(" ".join(query_parts), tuple(params))
        messages = cursor.fetchall()
        
        result = []
        for msg in messages:
            message = Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            )
            result.append(message)
            
        if include_context and result:
            # Anexar contexto a cada match, deduplicando por (id, chat_jid) para no
            # repetir mensajes ni romper el orden cronologico.
            messages_with_context = []
            seen = set()
            for msg in result:
                context = get_message_context(msg.id, context_before, context_after)
                for m in [*context.before, context.message, *context.after]:
                    key = (m.id, m.chat_jid)
                    if key in seen:
                        continue
                    seen.add(key)
                    messages_with_context.append(m)
            return [message_to_dict(m) for m in messages_with_context]

        # Sin contexto
        return [message_to_dict(m) for m in result]
        
    except sqlite3.Error as e:
        logger.error(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> MessageContext:
    """Get context around a specific message."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Get the target message first
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.chat_jid, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.id = ?
        """, (message_id,))
        msg_data = cursor.fetchone()
        
        if not msg_data:
            raise ValueError(f"Message with ID {message_id} not found")
            
        target_message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[8]
        )
        
        # Get messages before
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp < ?
            ORDER BY messages.timestamp DESC
            LIMIT ?
        """, (msg_data[7], msg_data[0], before))
        
        before_messages = []
        for msg in cursor.fetchall():
            before_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        # Get messages after
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp > ?
            ORDER BY messages.timestamp ASC
            LIMIT ?
        """, (msg_data[7], msg_data[0], after))
        
        after_messages = []
        for msg in cursor.fetchall():
            after_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        return MessageContext(
            message=target_message,
            before=before_messages,
            after=after_messages
        )
        
    except sqlite3.Error as e:
        logger.error(f"Database error: {e}")
        raise
    finally:
        if 'conn' in locals():
            conn.close()


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Chat]:
    """Get chats matching the specified criteria."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Build base query
        query_parts = ["""
            SELECT 
                chats.jid,
                chats.name,
                chats.last_message_time,
                messages.content as last_message,
                messages.sender as last_sender,
                messages.is_from_me as last_is_from_me
            FROM chats
        """]
        
        if include_last_message:
            query_parts.append("""
                LEFT JOIN messages ON chats.jid = messages.chat_jid 
                AND chats.last_message_time = messages.timestamp
            """)
            
        where_clauses = []
        params = []

        # El filtro por `query` se aplica en Python (mas abajo) sobre el nombre
        # RESUELTO, no en SQL: chats.name guarda el lid crudo, asi que un LIKE en
        # SQL no encontraria por nombre real (ej. buscar "Esposa").

        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
            
        # Add sorting
        order_by = "chats.last_message_time DESC" if sort_by == "last_active" else "chats.name"
        query_parts.append(f"ORDER BY {order_by}")
        
        # NO paginamos en SQL: traemos todo ordenado y luego unificamos los chats
        # que son la misma persona bajo distintos JIDs (lid vs numero). Recien ahi
        # aplicamos limit/offset, para que el conteo sea correcto tras deduplicar.
        cursor.execute(" ".join(query_parts), tuple(params))
        chats = cursor.fetchall()

        seen_keys = set()
        deduped = []
        for chat_data in chats:
            key = _canonical_chat_key(chat_data[0])
            if key in seen_keys:
                continue  # ya tenemos la fila mas reciente de esta persona
            seen_keys.add(key)
            resolved = resolve_contact_name(chat_data[0])
            deduped.append(Chat(
                jid=chat_data[0],
                name=resolved or chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            ))

        # Filtro por `query` sobre el nombre RESUELTO o el jid (no el lid crudo)
        if query:
            q = query.lower().strip()
            deduped = [c for c in deduped if (c.name and q in c.name.lower()) or q in c.jid.lower()]

        # Paginacion en memoria sobre la lista ya unificada
        offset = page * limit
        return deduped[offset:offset + limit]
        
    except sqlite3.Error as e:
        logger.error(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def search_contacts(query: str) -> List[Contact]:
    """Search contacts by name or phone number.

    Busca primero en la libreta real de WhatsApp (whatsmeow_contacts) y luego
    complementa con los chats conocidos, resolviendo nombres reales.
    """
    search = f"%{query.lower().strip()}%"
    names_idx, lid_to_pn = _get_contact_index()
    results = {}  # numero_telefono CANONICO -> Contact (unifica lid + numero)

    def _canon_pn(jid_or_local: str) -> str:
        pn = _normalize_phone(jid_or_local)
        return lid_to_pn.get(pn, pn)  # si es un lid, mapearlo a su numero real

    # 1) Libreta real de WhatsApp (la fuente con todos los contactos guardados)
    try:
        conn = sqlite3.connect(f"file:{WHATSAPP_DB_PATH}?mode=ro", uri=True, timeout=10)
        cursor = conn.cursor()
        cursor.execute(
            """
            SELECT their_jid, first_name, full_name, push_name, business_name
            FROM whatsmeow_contacts
            WHERE LOWER(full_name) LIKE ? OR LOWER(first_name) LIKE ?
               OR LOWER(push_name) LIKE ? OR LOWER(business_name) LIKE ?
               OR their_jid LIKE ?
            """,
            (search, search, search, search, search),
        )
        for their_jid, first_name, full_name, push_name, business_name in cursor.fetchall():
            pn = _canon_pn(their_jid)
            if pn and pn not in results:
                # preferir el nombre canonico de la libreta (por numero)
                name = names_idx.get(pn) or (full_name or first_name or push_name or business_name or "").strip()
                results[pn] = Contact(
                    phone_number=pn,
                    name=name or None,
                    jid=f"{pn}@s.whatsapp.net",
                )
        conn.close()
    except sqlite3.Error as e:
        logger.error(f"Database error searching contacts: {e}")

    # 2) Complementar con chats (cubre nombres que solo existen ahi)
    try:
        conn = sqlite3.connect(f"file:{MESSAGES_DB_PATH}?mode=ro", uri=True, timeout=10)
        cursor = conn.cursor()
        cursor.execute(
            """
            SELECT DISTINCT jid, name FROM chats
            WHERE (LOWER(name) LIKE ? OR LOWER(jid) LIKE ?) AND jid NOT LIKE '%@g.us'
            LIMIT 50
            """,
            (search, search),
        )
        for jid, name in cursor.fetchall():
            pn = _canon_pn(jid)
            if pn not in results:
                resolved = names_idx.get(pn) or resolve_contact_name(jid) or name
                results[pn] = Contact(phone_number=pn, name=resolved, jid=jid)
        conn.close()
    except sqlite3.Error as e:
        logger.error(f"Database error searching chats: {e}")

    return list(results.values())[:50]


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact.
    
    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT DISTINCT
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
            JOIN messages m ON c.jid = m.chat_jid
            WHERE m.sender = ? OR c.jid = ?
            ORDER BY c.last_message_time DESC
            LIMIT ? OFFSET ?
        """, (jid, jid, limit, page * limit))
        
        chats = cursor.fetchall()
        
        result = []
        for chat_data in chats:
            resolved = resolve_contact_name(chat_data[0])
            chat = Chat(
                jid=chat_data[0],
                name=resolved or chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            )
            result.append(chat)
            
        return result
        
    except sqlite3.Error as e:
        logger.error(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def get_last_interaction(jid: str) -> str:
    """Get most recent message involving the contact."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT 
                m.timestamp,
                m.sender,
                c.name,
                m.content,
                m.is_from_me,
                c.jid,
                m.id,
                m.media_type
            FROM messages m
            JOIN chats c ON m.chat_jid = c.jid
            WHERE m.sender = ? OR c.jid = ?
            ORDER BY m.timestamp DESC
            LIMIT 1
        """, (jid, jid))
        
        msg_data = cursor.fetchone()
        
        if not msg_data:
            return None
            
        message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[7]
        )
        
        return format_message(message)
        
    except sqlite3.Error as e:
        logger.error(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get chat metadata by JID."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        query = """
            SELECT 
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
        """
        
        if include_last_message:
            query += """
                LEFT JOIN messages m ON c.jid = m.chat_jid 
                AND c.last_message_time = m.timestamp
            """
            
        query += " WHERE c.jid = ?"
        
        cursor.execute(query, (chat_jid,))
        chat_data = cursor.fetchone()
        
        if not chat_data:
            return None
            
        return Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )
        
    except sqlite3.Error as e:
        logger.error(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    """Get chat metadata by sender phone number.

    Busca por jid exacto (numero@s.whatsapp.net) y por el lid mapeado a ese numero,
    en vez de un LIKE '%phone%' (que daba falsos positivos y no cubria los @lid).
    """
    pn = _normalize_phone(sender_phone_number)
    _, lid_to_pn = _get_contact_index()
    candidate_jids = [f"{pn}@s.whatsapp.net"]
    for lid, mapped_pn in lid_to_pn.items():
        if mapped_pn == pn:
            candidate_jids.append(f"{lid}@lid")
    try:
        conn = sqlite3.connect(f"file:{MESSAGES_DB_PATH}?mode=ro", uri=True, timeout=10)
        cursor = conn.cursor()
        placeholders = ",".join(["?"] * len(candidate_jids))
        cursor.execute(f"""
            SELECT
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
            LEFT JOIN messages m ON c.jid = m.chat_jid
                AND c.last_message_time = m.timestamp
            WHERE c.jid IN ({placeholders})
            ORDER BY c.last_message_time DESC
            LIMIT 1
        """, tuple(candidate_jids))

        chat_data = cursor.fetchone()

        if not chat_data:
            return None

        return Chat(
            jid=chat_data[0],
            name=resolve_contact_name(chat_data[0]) or chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )
        
    except sqlite3.Error as e:
        logger.error(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()

def send_message(recipient: str, message: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "message": message,
        }
        
        response = requests.post(url, json=payload, headers={"X-Auth-Token": _bridge_token()}, timeout=REQUEST_TIMEOUT)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_file(recipient: str, media_path: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        if not media_path:
            return False, "Media path must be provided"
        
        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"
        
        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "media_path": media_path
        }
        
        response = requests.post(url, json=payload, headers={"X-Auth-Token": _bridge_token()}, timeout=REQUEST_TIMEOUT)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_audio_message(recipient: str, media_path: str) -> Tuple[bool, str]:
    temp_to_cleanup = None
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"

        if not media_path:
            return False, "Media path must be provided"

        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"

        if not media_path.endswith(".ogg"):
            try:
                media_path = audio.convert_to_opus_ogg_temp(media_path)
                temp_to_cleanup = media_path
            except Exception as e:
                return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {str(e)}"
        
        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "media_path": media_path
        }
        
        response = requests.post(url, json=payload, headers={"X-Auth-Token": _bridge_token()}, timeout=REQUEST_TIMEOUT)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"
    finally:
        # Borrar el .ogg temporal creado por la conversion (evita fuga en /tmp)
        if temp_to_cleanup:
            try:
                os.remove(temp_to_cleanup)
            except OSError:
                pass

def download_media(message_id: str, chat_jid: str) -> Optional[str]:
    """Download media from a message and return the local file path.
    
    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message
    
    Returns:
        The local file path if download was successful, None otherwise
    """
    try:
        url = f"{WHATSAPP_API_BASE_URL}/download"
        payload = {
            "message_id": message_id,
            "chat_jid": chat_jid
        }
        
        response = requests.post(url, json=payload, headers={"X-Auth-Token": _bridge_token()}, timeout=REQUEST_TIMEOUT)
        
        if response.status_code == 200:
            result = response.json()
            if result.get("success", False):
                path = result.get("path")
                logger.info(f"Media downloaded successfully: {path}")
                return path
            else:
                logger.error(f"Download failed: {result.get('message', 'Unknown error')}")
                return None
        else:
            logger.error(f"Error: HTTP {response.status_code} - {response.text}")
            return None
            
    except requests.RequestException as e:
        logger.error(f"Request error: {str(e)}")
        return None
    except json.JSONDecodeError:
        logger.error(f"Error parsing response: {response.text}")
        return None
    except Exception as e:
        logger.error(f"Unexpected error: {str(e)}")
        return None


def list_groups() -> List[Dict[str, Any]]:
    """Lista los grupos de WhatsApp de los que el usuario es miembro."""
    try:
        resp = requests.get(
            f"{WHATSAPP_API_BASE_URL}/groups",
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        data = resp.json()
        return data.get("groups", []) if data.get("success") else []
    except (requests.RequestException, ValueError) as e:
        logger.error(f"list_groups error: {e}")
        return []


def mark_as_read(chat_jid: str, message_ids: List[str]) -> Tuple[bool, str]:
    """Marca uno o mas mensajes como leidos (pensado para chats directos)."""
    try:
        resp = requests.post(
            f"{WHATSAPP_API_BASE_URL}/mark_read",
            json={"chat_jid": chat_jid, "message_ids": message_ids},
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        data = resp.json()
        return data.get("success", False), data.get("message", "")
    except (requests.RequestException, ValueError) as e:
        logger.error(f"mark_as_read error: {e}")
        return False, str(e)


def react_to_message(chat_jid: str, message_id: str, emoji: str) -> Tuple[bool, str]:
    """Reacciona a un mensaje con un emoji (chats directos / mensajes recibidos)."""
    try:
        resp = requests.post(
            f"{WHATSAPP_API_BASE_URL}/react",
            json={"chat_jid": chat_jid, "message_id": message_id, "emoji": emoji},
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        data = resp.json()
        return data.get("success", False), data.get("message", "")
    except (requests.RequestException, ValueError) as e:
        logger.error(f"react_to_message error: {e}")
        return False, str(e)
