"""Cliente HTTP del bridge: auth por token, sesion keep-alive y las acciones/escrituras."""
import json
import os.path
from typing import Any, Dict, List, Optional, Tuple

import requests

from whatsapp_mcp import audio
from whatsapp_mcp.config import (
    BRIDGE_TOKEN_PATH,
    REQUEST_TIMEOUT,
    WHATSAPP_API_BASE_URL,
    logger,
)
from whatsapp_mcp.db import resolve_contact_name

# Sesion HTTP reutilizable: mantiene un pool de conexiones keep-alive al bridge en vez de
# abrir/cerrar un socket TCP por request. urllib3 (debajo) es thread-safe para el uso
# concurrente de tools del MCP. Reduce latencia por llamada y evita sockets en TIME_WAIT.
_SESSION = requests.Session()


def _bridge_token() -> str:
    """Lee el token compartido que el bridge persiste en store/.bridge_token."""
    try:
        with open(BRIDGE_TOKEN_PATH) as f:
            return f.read().strip()
    except OSError:
        return ""


def _bridge_post(path: str, payload: dict) -> Dict[str, Any]:
    """POST a un endpoint del bridge con auth + timeout; devuelve el JSON o {success:False}."""
    try:
        resp = _SESSION.post(
            f"{WHATSAPP_API_BASE_URL}/{path}",
            json=payload,
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        return resp.json()
    except (requests.RequestException, ValueError) as e:
        logger.error(f"bridge POST /{path} error: {e}")
        return {"success": False, "message": str(e)}

def send_message(recipient: str, message: str, reply_to: str = "", mentions: Optional[List[str]] = None) -> Tuple[bool, str]:
    response = None
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"

        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload: Dict[str, Any] = {
            "recipient": recipient,
            "message": message,
        }
        if reply_to:
            payload["quoted_message_id"] = reply_to
        if mentions:
            payload["mentions"] = mentions

        response = _SESSION.post(url, json=payload, headers={"X-Auth-Token": _bridge_token()}, timeout=REQUEST_TIMEOUT)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text if response is not None else ''}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_file(recipient: str, media_path: str) -> Tuple[bool, str]:
    response = None
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
        
        response = _SESSION.post(url, json=payload, headers={"X-Auth-Token": _bridge_token()}, timeout=REQUEST_TIMEOUT)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text if response is not None else ''}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_audio_message(recipient: str, media_path: str) -> Tuple[bool, str]:
    temp_to_cleanup = None
    response = None
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
        
        response = _SESSION.post(url, json=payload, headers={"X-Auth-Token": _bridge_token()}, timeout=REQUEST_TIMEOUT)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text if response is not None else ''}"
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
    response = None
    try:
        url = f"{WHATSAPP_API_BASE_URL}/download"
        payload = {
            "message_id": message_id,
            "chat_jid": chat_jid
        }
        
        response = _SESSION.post(url, json=payload, headers={"X-Auth-Token": _bridge_token()}, timeout=REQUEST_TIMEOUT)
        
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
        logger.error(f"Error parsing response: {response.text if response is not None else ''}")
        return None
    except Exception as e:
        logger.error(f"Unexpected error: {str(e)}")
        return None


def list_groups() -> List[Dict[str, Any]]:
    """Lista los grupos de WhatsApp de los que el usuario es miembro."""
    try:
        resp = _SESSION.get(
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
        resp = _SESSION.post(
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
        resp = _SESSION.post(
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


def edit_message(chat_jid: str, message_id: str, new_text: str) -> Tuple[bool, str]:
    """Edita un mensaje propio ya enviado (ventana ~20 min)."""
    try:
        resp = _SESSION.post(
            f"{WHATSAPP_API_BASE_URL}/edit",
            json={"chat_jid": chat_jid, "message_id": message_id, "new_text": new_text},
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        data = resp.json()
        return data.get("success", False), data.get("message", "")
    except (requests.RequestException, ValueError) as e:
        logger.error(f"edit_message error: {e}")
        return False, str(e)


def delete_message(chat_jid: str, message_id: str, sender: str = "") -> Tuple[bool, str]:
    """Elimina un mensaje 'para todos' (revoke). sender vacio = mensaje propio."""
    try:
        resp = _SESSION.post(
            f"{WHATSAPP_API_BASE_URL}/revoke",
            json={"chat_jid": chat_jid, "message_id": message_id, "sender": sender},
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        data = resp.json()
        return data.get("success", False), data.get("message", "")
    except (requests.RequestException, ValueError) as e:
        logger.error(f"delete_message error: {e}")
        return False, str(e)


def send_typing(chat_jid: str, state: str = "composing", media: str = "") -> Tuple[bool, str]:
    """Envia presencia de chat: 'composing' (escribiendo) o 'paused'; media '' o 'audio'."""
    try:
        resp = _SESSION.post(
            f"{WHATSAPP_API_BASE_URL}/typing",
            json={"chat_jid": chat_jid, "state": state, "media": media},
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        data = resp.json()
        return data.get("success", False), data.get("message", "")
    except (requests.RequestException, ValueError) as e:
        logger.error(f"send_typing error: {e}")
        return False, str(e)


def send_poll(chat_jid: str, question: str, options: List[str], selectable_count: int = 1) -> Tuple[bool, str]:
    """Envia una encuesta. selectable_count=1 (opcion unica) o >1 (multiple)."""
    try:
        resp = _SESSION.post(
            f"{WHATSAPP_API_BASE_URL}/poll",
            json={"chat_jid": chat_jid, "question": question, "options": options, "selectable_count": selectable_count},
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        data = resp.json()
        return data.get("success", False), data.get("message", "")
    except (requests.RequestException, ValueError) as e:
        logger.error(f"send_poll error: {e}")
        return False, str(e)


def check_whatsapp(phones: List[str]) -> List[Dict[str, Any]]:
    """Verifica si numeros estan en WhatsApp (formato internacional con +)."""
    try:
        resp = _SESSION.post(
            f"{WHATSAPP_API_BASE_URL}/check_whatsapp",
            json={"phones": phones},
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        data = resp.json()
        return data.get("results", []) if data.get("success") else []
    except (requests.RequestException, ValueError) as e:
        logger.error(f"check_whatsapp error: {e}")
        return []


def get_profile_picture(jid: str, preview: bool = False) -> Dict[str, Any]:
    """Obtiene la URL de la foto de perfil de un usuario o grupo."""
    try:
        resp = _SESSION.post(
            f"{WHATSAPP_API_BASE_URL}/profile_picture",
            json={"jid": jid, "preview": preview},
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        return resp.json()
    except (requests.RequestException, ValueError) as e:
        logger.error(f"get_profile_picture error: {e}")
        return {"success": False, "message": str(e)}


def get_user_info(jids: List[str]) -> Dict[str, Any]:
    """Obtiene info (status/about, flag business) de uno o mas usuarios."""
    try:
        resp = _SESSION.post(
            f"{WHATSAPP_API_BASE_URL}/user_info",
            json={"jids": jids},
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        return resp.json()
    except (requests.RequestException, ValueError) as e:
        logger.error(f"get_user_info error: {e}")
        return {"success": False, "message": str(e)}


# --- Lote C: grupos + bloqueo ---

def get_group_participants(group_jid: str) -> Dict[str, Any]:
    """Lista los participantes de un grupo (jid, telefono, admin)."""
    return _bridge_post("group_participants", {"group_jid": group_jid})


def get_group_invite_link(group_jid: str, reset: bool = False) -> Dict[str, Any]:
    """Obtiene (o resetea con reset=True) el link de invitacion de un grupo."""
    return _bridge_post("group_invite_link", {"group_jid": group_jid, "reset": reset})


def join_group(code: str) -> Dict[str, Any]:
    """Une a un grupo via link o codigo de invitacion."""
    return _bridge_post("join_group", {"code": code})


def leave_group(group_jid: str) -> Tuple[bool, str]:
    """Sale de un grupo."""
    d = _bridge_post("leave_group", {"group_jid": group_jid})
    return d.get("success", False), d.get("message", "")


def set_group_name(group_jid: str, name: str) -> Tuple[bool, str]:
    """Renombra un grupo (max 25 chars)."""
    d = _bridge_post("set_group_name", {"group_jid": group_jid, "name": name})
    return d.get("success", False), d.get("message", "")


def set_group_topic(group_jid: str, topic: str) -> Tuple[bool, str]:
    """Cambia la descripcion/topic de un grupo."""
    d = _bridge_post("set_group_topic", {"group_jid": group_jid, "topic": topic})
    return d.get("success", False), d.get("message", "")


def block_contact(jid: str) -> Tuple[bool, str]:
    """Bloquea un contacto."""
    d = _bridge_post("block", {"jid": jid, "action": "block"})
    return d.get("success", False), d.get("message", "")


def unblock_contact(jid: str) -> Tuple[bool, str]:
    """Desbloquea un contacto."""
    d = _bridge_post("block", {"jid": jid, "action": "unblock"})
    return d.get("success", False), d.get("message", "")


# --- Estado de chat (mute/pin/archive/read/star/settings) ---

def mute_chat(chat_jid: str, mute: bool = True, duration_hours: int = 0) -> Tuple[bool, str]:
    """Silencia/desilencia un chat. duration_hours=0 = indefinido."""
    d = _bridge_post("mute", {"chat_jid": chat_jid, "enable": mute, "duration_hours": duration_hours})
    return d.get("success", False), d.get("message", "")


def pin_chat(chat_jid: str, pin: bool = True) -> Tuple[bool, str]:
    """Fija/desfija un chat al tope."""
    d = _bridge_post("pin", {"chat_jid": chat_jid, "enable": pin})
    return d.get("success", False), d.get("message", "")


def archive_chat(chat_jid: str, archive: bool = True) -> Tuple[bool, str]:
    """Archiva/desarchiva un chat."""
    d = _bridge_post("archive", {"chat_jid": chat_jid, "enable": archive})
    return d.get("success", False), d.get("message", "")


def mark_chat(chat_jid: str, read: bool = True) -> Tuple[bool, str]:
    """Marca un chat entero como leido (read=True) o no leido (read=False)."""
    d = _bridge_post("mark_chat", {"chat_jid": chat_jid, "enable": read})
    return d.get("success", False), d.get("message", "")


def star_message(chat_jid: str, message_id: str, starred: bool = True) -> Tuple[bool, str]:
    """Destaca/quita destacado a un mensaje."""
    d = _bridge_post("star", {"chat_jid": chat_jid, "message_id": message_id, "starred": starred})
    return d.get("success", False), d.get("message", "")


def get_chat_settings(chat_jid: str) -> Dict[str, Any]:
    """Lee el estado de un chat: muted, muted_until, pinned, archived."""
    return _bridge_post("chat_settings", {"chat_jid": chat_jid})


def request_more_history(chat_jid: str, count: int = 50) -> Tuple[bool, str]:
    """Pide mensajes anteriores de un chat (best-effort). WhatsApp es E2E: el server no guarda
    historial, vive en el telefono primario. Si esta online y los tiene, llegan async via
    history sync y quedan en la DB. Es normal que no llegue nada (telefono offline / sin mas)."""
    d = _bridge_post("request_history", {"chat_jid": chat_jid, "count": count})
    return d.get("success", False), d.get("message", "")


def create_group(name: str, participants: List[str]) -> Dict[str, Any]:
    """Crea un grupo nuevo (nombre max 25 chars) con los participantes dados (numeros o JIDs)."""
    return _bridge_post("create_group", {"name": name, "participants": participants})


def update_group_participants(group_jid: str, participants: List[str], action: str) -> Dict[str, Any]:
    """Agrega/quita/promueve/degrada participantes. action: add|remove|promote|demote (requiere admin)."""
    return _bridge_post("update_participants", {"group_jid": group_jid, "participants": participants, "action": action})


def set_disappearing_messages(chat_jid: str, duration: str = "off") -> Tuple[bool, str]:
    """Setea el timer de mensajes temporales. duration: off|24h|7d|90d (presets de WhatsApp)."""
    d = _bridge_post("disappearing", {"chat_jid": chat_jid, "duration": duration})
    return d.get("success", False), d.get("message", "")


def get_status() -> Dict[str, Any]:
    """Estado de conexion/sesion/ban del bridge (connected, logged_in, temp_banned, needs_qr, ...)."""
    try:
        response = _SESSION.get(f"{WHATSAPP_API_BASE_URL}/status",
                                headers={"X-Auth-Token": _bridge_token()}, timeout=REQUEST_TIMEOUT)
        if response.status_code == 200:
            return response.json()
        return {"success": False, "message": f"HTTP {response.status_code} - {response.text}"}
    except requests.RequestException as e:
        return {"success": False, "message": f"Request error: {str(e)}"}
    except json.JSONDecodeError:
        return {"success": False, "message": "Error parsing response"}


# --- Lote A1: perfil & cuenta ---

def set_status_message(message: str) -> Tuple[bool, str]:
    """Cambia el mensaje de estado ('about') propio."""
    d = _bridge_post("set_status", {"message": message})
    return d.get("success", False), d.get("message", "")


def get_business_profile(jid: str) -> Dict[str, Any]:
    """Perfil de negocio de un contacto (address, email, categories, business hours)."""
    return _bridge_post("business_profile", {"jid": jid})


def get_user_devices(jids: List[str]) -> Dict[str, Any]:
    """Dispositivos vinculados de uno o varios contactos."""
    return _bridge_post("user_devices", {"jids": jids})


def set_default_disappearing(duration: str = "off") -> Tuple[bool, str]:
    """Timer de mensajes temporales por defecto para chats NUEVOS. duration: off|24h|7d|90d."""
    d = _bridge_post("default_disappearing", {"duration": duration})
    return d.get("success", False), d.get("message", "")


# --- Lote A2: administración de grupos ---

def set_group_description(group_jid: str, description: str) -> Tuple[bool, str]:
    """Cambia la descripcion de un grupo (requiere admin)."""
    d = _bridge_post("set_group_description", {"group_jid": group_jid, "description": description})
    return d.get("success", False), d.get("message", "")


def set_group_announce(group_jid: str, enable: bool) -> Tuple[bool, str]:
    """Modo announce: si enable, solo admins pueden enviar mensajes (requiere admin)."""
    d = _bridge_post("set_group_announce", {"group_jid": group_jid, "enable": enable})
    return d.get("success", False), d.get("message", "")


def set_group_locked(group_jid: str, enable: bool) -> Tuple[bool, str]:
    """Modo locked: si enable, solo admins pueden editar la info del grupo (requiere admin)."""
    d = _bridge_post("set_group_locked", {"group_jid": group_jid, "enable": enable})
    return d.get("success", False), d.get("message", "")


def set_group_photo(group_jid: str, image_path: str) -> Dict[str, Any]:
    """Cambia la foto de un grupo desde una imagen local. Requiere admin.
    La imagen DEBE ser un JPEG CUADRADO (ej. 640x640); WhatsApp rechaza no-cuadradas y
    whatsmeow no redimensiona (recortar antes, ej. macOS `sips -z 640 640 in.jpg --out sq.jpg`)."""
    return _bridge_post("set_group_photo", {"group_jid": group_jid, "image_path": image_path})


# --- Lote A4: votar en encuestas ---

def vote_poll(chat_jid: str, poll_message_id: str, options: List[str]) -> Tuple[bool, str]:
    """Vota en una encuesta existente (el poll debe estar capturado en la DB). Los votos
    entrantes de otros se descifran y guardan solos como mensajes 'poll_vote'."""
    d = _bridge_post("poll_vote", {"chat_jid": chat_jid, "poll_message_id": poll_message_id, "options": options})
    return d.get("success", False), d.get("message", "")


# --- Lote A3: solicitudes de ingreso a grupos ---

def set_group_join_approval(group_jid: str, enable: bool) -> Tuple[bool, str]:
    """Activa/desactiva el modo de aprobación de ingresos al grupo (requiere admin)."""
    d = _bridge_post("set_group_join_approval", {"group_jid": group_jid, "enable": enable})
    return d.get("success", False), d.get("message", "")


def get_group_join_requests(group_jid: str) -> Dict[str, Any]:
    """Lista las solicitudes de ingreso pendientes de un grupo (jid + requested_at)."""
    return _bridge_post("group_join_requests", {"group_jid": group_jid})


def review_group_join_request(group_jid: str, participants: List[str], action: str) -> Dict[str, Any]:
    """Aprueba o rechaza solicitudes de ingreso. action: approve|reject (requiere admin)."""
    return _bridge_post("review_group_join_request", {"group_jid": group_jid, "participants": participants, "action": action})


# --- Lote B1: unirse por código de invitación ---

def get_group_info_from_invite(chat_jid: str, invite_message_id: str) -> Dict[str, Any]:
    """Inspecciona un grupo a partir de una invitacion recibida (sin unirse)."""
    return _bridge_post("group_info_from_invite", {"chat_jid": chat_jid, "invite_message_id": invite_message_id})


def join_group_with_invite(chat_jid: str, invite_message_id: str) -> Tuple[bool, str]:
    """Se une a un grupo usando una invitacion recibida (mensaje de invitacion, no link)."""
    d = _bridge_post("join_group_with_invite", {"chat_jid": chat_jid, "invite_message_id": invite_message_id})
    return d.get("success", False), d.get("message", "")


# --- Lote B2: presencia ---

def set_presence(state: str = "available") -> Tuple[bool, str]:
    """Cambia la presencia propia. state: available|unavailable. available es requisito para
    RECIBIR la presencia de otros."""
    d = _bridge_post("set_presence", {"state": state})
    return d.get("success", False), d.get("message", "")


def subscribe_presence(jid: str) -> Tuple[bool, str]:
    """Se suscribe a la presencia de un contacto (necesario para recibir su online/last-seen)."""
    d = _bridge_post("subscribe_presence", {"jid": jid})
    return d.get("success", False), d.get("message", "")


def get_presence(jid: str) -> Dict[str, Any]:
    """Ultimo estado de presencia conocido de un contacto (online, last_seen, typing)."""
    return _bridge_post("get_presence", {"jid": jid})


# --- Logout ---

def logout() -> Tuple[bool, str]:
    """Desvincula la sesion de WhatsApp. Requiere re-escanear el QR para volver a usar el MCP."""
    d = _bridge_post("logout", {})
    return d.get("success", False), d.get("message", "")


# --- Lote T3-3: chats no leídos ---

def get_unread_chats() -> List[Dict[str, Any]]:
    """Lista los chats con mensajes entrantes sin leer (rastreados en vivo por el bridge).

    El conteo se cuenta desde que el bridge está corriendo (el history-sync no lo puebla),
    y se limpia al leer el chat en el teléfono (read-receipt propio), al responder, o vía
    mark_as_read. Devuelve [] si no hay no-leídos.
    """
    try:
        response = _SESSION.get(f"{WHATSAPP_API_BASE_URL}/unread_chats",
                                headers={"X-Auth-Token": _bridge_token()}, timeout=REQUEST_TIMEOUT)
        if response.status_code != 200:
            return []
        data = response.json()
    except (requests.RequestException, json.JSONDecodeError):
        return []
    if not data.get("success"):
        return []
    out: List[Dict[str, Any]] = []
    for c in data.get("chats", []):
        jid = c.get("chat_jid", "")
        out.append({
            "chat_jid": jid,
            "name": resolve_contact_name(jid) or jid,
            "unread_count": c.get("unread_count", 0),
            "last_time": c.get("last_time", ""),
        })
    return out
