"""Cliente HTTP del bridge: auth por token, sesion keep-alive y las acciones/escrituras."""
import hashlib
import json
import os.path
import platform
import shutil
from typing import Any, Dict, List, Optional, Tuple

import requests

from whatsapp_mcp import audio
from whatsapp_mcp.config import (
    BRIDGE_TOKEN_PATH,
    REQUEST_TIMEOUT,
    WHATSAPP_API_BASE_URL,
    logger,
)

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


def _bridge_get(path: str) -> Dict[str, Any]:
    """GET a un endpoint del bridge con auth + timeout; devuelve el JSON o {success:False}."""
    try:
        resp = _SESSION.get(
            f"{WHATSAPP_API_BASE_URL}/{path}",
            headers={"X-Auth-Token": _bridge_token()},
            timeout=REQUEST_TIMEOUT,
        )
        return resp.json()
    except (requests.RequestException, ValueError) as e:
        logger.error(f"bridge GET /{path} error: {e}")
        return {"success": False, "message": str(e)}

# send_message / send_file / send_audio_message NO se migran a _bridge_post a proposito:
# devuelven Tuple[bool, str] (no un dict), distinguen el status_code y usan mensajes de error
# propios ('Error: HTTP ...', 'Request error', 'Error parsing response', 'Unexpected error') que
# _bridge_post no reproduce. Ademas validan input y send_audio_message limpia el .ogg temporal en
# un finally. Reusar _bridge_post cambiaria esos mensajes/manejo observables, asi que se preservan.
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
            
    # Desde requests>=2.27, response.json() lanza requests.exceptions.JSONDecodeError,
    # que hereda de RequestException Y de json.JSONDecodeError (herencia doble). Debe
    # capturarse ANTES que RequestException: si no, un 200 con body no-JSON caia en
    # 'Request error' y esta rama de parsing quedaba muerta (bug historico). La tupla
    # conserva json.JSONDecodeError puro para cuando la excepcion no viene de requests.
    except (requests.exceptions.JSONDecodeError, json.JSONDecodeError):
        return False, f"Error parsing response: {response.text if response is not None else ''}"
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
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
            
    # JSONDecodeError de requests ANTES que RequestException: herencia doble desde
    # requests 2.27 (ver el comentario en send_message).
    except (requests.exceptions.JSONDecodeError, json.JSONDecodeError):
        return False, f"Error parsing response: {response.text if response is not None else ''}"
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
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
            
    # JSONDecodeError de requests ANTES que RequestException: herencia doble desde
    # requests 2.27 (ver el comentario en send_message).
    except (requests.exceptions.JSONDecodeError, json.JSONDecodeError):
        return False, f"Error parsing response: {response.text if response is not None else ''}"
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
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
            
    # JSONDecodeError de requests ANTES que RequestException: herencia doble desde
    # requests 2.27 (ver el comentario en send_message).
    except (requests.exceptions.JSONDecodeError, json.JSONDecodeError):
        logger.error(f"Error parsing response: {response.text if response is not None else ''}")
        return None
    except requests.RequestException as e:
        logger.error(f"Request error: {str(e)}")
        return None
    except Exception as e:
        logger.error(f"Unexpected error: {str(e)}")
        return None


def list_groups() -> List[Dict[str, Any]]:
    """Lista los grupos de WhatsApp de los que el usuario es miembro."""
    data = _bridge_get("groups")
    return data.get("groups", []) if data.get("success") else []


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
    """Estado de conexion/sesion/ban del bridge (connected, logged_in, temp_banned, needs_qr, ...).

    NO usa _bridge_get a proposito: este endpoint diferencia el status_code (devuelve un mensaje
    'HTTP {code} - {text}' distinto en no-200) y usa prefijos de error propios ('Request error',
    'Error parsing response') que _bridge_get no reproduce. Migrarlo cambiaria esos mensajes
    observables (get_status es una tool de diagnostico), asi que se deja el try/except manual.
    """
    try:
        response = _SESSION.get(f"{WHATSAPP_API_BASE_URL}/status",
                                headers={"X-Auth-Token": _bridge_token()}, timeout=REQUEST_TIMEOUT)
        if response.status_code == 200:
            return response.json()
        return {"success": False, "message": f"HTTP {response.status_code} - {response.text}"}
    # JSONDecodeError de requests ANTES que RequestException: herencia doble desde
    # requests 2.27 (ver el comentario en send_message).
    except (requests.exceptions.JSONDecodeError, json.JSONDecodeError):
        return {"success": False, "message": "Error parsing response"}
    except requests.RequestException as e:
        return {"success": False, "message": f"Request error: {str(e)}"}


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
    """Devuelve los chats con mensajes sin leer, CRUDOS del bridge (sin resolver nombres).

    El conteo lo rastrea el bridge en vivo (el history-sync no lo puebla) y se limpia al leer
    el chat en el teléfono (read-receipt propio), al responder, o vía mark_as_read. Cada item
    trae chat_jid / unread_count / last_time tal cual los envía el bridge; el enriquecimiento
    con el nombre resuelto es lógica de dominio y vive en la capa tools (get_unread_chats),
    no acá en el transporte. Devuelve [] si no hay no-leídos o si el bridge falla.
    """
    data = _bridge_get("unread_chats")
    if not data.get("success"):
        return []
    return data.get("chats", [])


# --- Supervisor del bridge: login autogestionado (plug-and-play) ---
#
# El server MCP es el unico proceso que el cliente (Claude Desktop/Code) arranca. Estas
# funciones le permiten adoptar un bridge sano existente (nunca duplicar conexiones),
# lanzar uno si no hay, y reciclarlo cuando la sesion quedo zombie. El bind loopback del
# bridge actua de mutex natural: dos bridges sobre el mismo puerto son imposibles.

def get_qr() -> Dict[str, Any]:
    """Estado del flujo de login QR: qr_status (logged_in|none|active|success|timeout)
    y, con "active", el codigo crudo + png_base64 + expires_at."""
    return _bridge_get("qr")


def shutdown_bridge() -> Dict[str, Any]:
    """Pide al bridge un apagado ordenado (mismo camino que SIGTERM)."""
    return _bridge_post("shutdown", {})


def _run_go_build(src_dir: str, out_path: str) -> Tuple[bool, str]:
    """Compila el bridge Go desde src_dir hacia out_path. Separado para poder testearlo."""
    import subprocess

    try:
        proc = subprocess.run(
            ["go", "build", "-o", out_path, "."],
            cwd=src_dir,
            capture_output=True,
            text=True,
            timeout=300,
        )
    except (OSError, subprocess.TimeoutExpired) as e:
        return False, f"go build fallo: {e}"
    if proc.returncode != 0:
        return False, f"go build fallo: {proc.stderr.strip()[:500]}"
    return True, ""


def _platform_asset_name() -> str:
    """Nombre del asset de GitHub Releases para esta plataforma (convención GoReleaser)."""
    system = platform.system().lower()  # darwin | linux | windows
    machine = platform.machine().lower()
    arch = {"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(
        machine, machine
    )
    suffix = ".exe" if system == "windows" else ""
    return f"whatsapp-bridge-{system}-{arch}{suffix}"


def _sha256_matches(data: bytes, asset_name: str, checksums_txt: str) -> bool:
    """Verifica data contra la línea de asset_name en un checksums.txt (formato sha256sum)."""
    digest = hashlib.sha256(data).hexdigest()
    for line in checksums_txt.splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[1] == asset_name:
            return parts[0] == digest
    return False


def _gh_token() -> str:
    """Token para la API de GitHub (repo privado): env GITHUB_TOKEN/GH_TOKEN o `gh auth token`."""
    import subprocess

    token = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    if token:
        return token
    try:
        proc = subprocess.run(
            ["gh", "auth", "token"], capture_output=True, text=True, timeout=10
        )
        if proc.returncode == 0:
            return proc.stdout.strip()
    except (OSError, subprocess.TimeoutExpired):
        pass
    return ""


def _download_release_binary(bin_path: str) -> Tuple[bool, str]:
    """Descarga el binario precompilado del último GitHub Release y verifica su SHA256.

    Instalación atómica (tmp + os.replace) para que un fallo a mitad de descarga jamás
    deje un binario corrupto en bin_path. Cualquier fallo devuelve (False, motivo) y el
    caller cae al siguiente nivel de la cascada (go build local).
    """
    from whatsapp_mcp.config import RELEASE_REPO

    asset_name = _platform_asset_name()
    headers = {"Accept": "application/vnd.github+json"}
    token = _gh_token()
    if token:
        headers["Authorization"] = f"Bearer {token}"
    try:
        rel = requests.get(
            f"https://api.github.com/repos/{RELEASE_REPO}/releases/latest",
            headers=headers,
            timeout=(5, 30),
        )
        if rel.status_code != 200:
            return False, f"release: HTTP {rel.status_code}"
        assets = {a["name"]: a for a in rel.json().get("assets", [])}
        if asset_name not in assets or "checksums.txt" not in assets:
            return False, f"release sin asset {asset_name} o checksums.txt"

        # Los assets de repos privados solo se descargan via su API url con Accept octet-stream.
        dl_headers = dict(headers)
        dl_headers["Accept"] = "application/octet-stream"
        checksums = requests.get(
            assets["checksums.txt"]["url"], headers=dl_headers, timeout=(5, 30)
        )
        binary = requests.get(assets[asset_name]["url"], headers=dl_headers, timeout=(5, 120))
        if checksums.status_code != 200 or binary.status_code != 200:
            return False, f"descarga: HTTP {checksums.status_code}/{binary.status_code}"
    except requests.RequestException as e:
        return False, f"release: {e}"

    if not _sha256_matches(binary.content, asset_name, checksums.text):
        return False, f"checksum SHA256 no coincide para {asset_name} (descarga descartada)"

    os.makedirs(os.path.dirname(bin_path) or ".", exist_ok=True)
    tmp_path = bin_path + ".tmp"
    with open(tmp_path, "wb") as f:
        f.write(binary.content)
    os.chmod(tmp_path, 0o755)
    os.replace(tmp_path, bin_path)
    return True, f"binario {asset_name} descargado y verificado (SHA256 OK)"


def ensure_bridge_binary(bin_path: str) -> Tuple[bool, str]:
    """Garantiza el binario del bridge resolviendo en cascada (plug-and-play).

    1. Ya existe en bin_path -> usarlo.
    2. Descargar el binario precompilado del último GitHub Release (verificado SHA256).
    3. Fallback: compilar con Go desde BRIDGE_SRC_DIR (el código incluido en el plugin).
    4. Error accionable con los motivos de ambos intentos.

    bin_path vive fuera del directorio del plugin (~/.whatsapp-mcp/bin en modo plugin)
    para sobrevivir updates.
    """
    from whatsapp_mcp.config import BRIDGE_SRC_DIR

    if os.path.isfile(bin_path):
        return True, ""

    dl_ok, dl_msg = _download_release_binary(bin_path)
    if dl_ok:
        logger.info(dl_msg)
        return True, dl_msg

    if not shutil.which("go"):
        return False, (
            f"no existe el binario del bridge ({bin_path}); la descarga del release fallo "
            f"({dl_msg}) y no hay toolchain Go para compilarlo. Instala Go "
            "(https://go.dev/dl/ o `brew install go`) y reintenta, o compila manualmente "
            "y apunta WHATSAPP_BRIDGE_BIN al binario."
        )
    logger.info(
        f"release no disponible ({dl_msg}); compilando el bridge "
        f"({BRIDGE_SRC_DIR} -> {bin_path})..."
    )
    os.makedirs(os.path.dirname(bin_path) or ".", exist_ok=True)
    ok, err = _run_go_build(BRIDGE_SRC_DIR, bin_path)
    if not ok:
        return False, err
    return True, "bridge compilado"


def spawn_bridge() -> Tuple[bool, str]:
    """Lanza el binario del bridge como daemon independiente del server MCP.

    start_new_session=True lo desacopla: si el cliente MCP reinicia el server, el bridge
    (y la sesion WhatsApp) sobreviven y la proxima instancia lo adopta via health check.
    stdout/stderr van al log del store porque ya no hay terminal.
    """
    import subprocess

    from whatsapp_mcp.config import BRIDGE_BIN_PATH, BRIDGE_LOG_PATH, STORE_DIR

    ok, msg = ensure_bridge_binary(BRIDGE_BIN_PATH)
    if not ok:
        return False, msg
    env = dict(os.environ)
    env.setdefault("WHATSAPP_STORE_DIR", STORE_DIR)
    try:
        # El store y el log deben existir antes del primer arranque (modo plugin arranca
        # sin ~/.whatsapp-mcp; el token y las DBs se crean dentro).
        os.makedirs(STORE_DIR, exist_ok=True)
        with open(BRIDGE_LOG_PATH, "ab") as logf:
            subprocess.Popen(
                [BRIDGE_BIN_PATH],
                stdout=logf,
                stderr=logf,
                stdin=subprocess.DEVNULL,
                start_new_session=True,
                env=env,
                cwd=os.path.dirname(BRIDGE_BIN_PATH) or None,
            )
        return True, "bridge spawned"
    except OSError as e:
        return False, f"failed to spawn bridge: {e}"


def ensure_bridge(timeout_s: float = 20.0) -> Dict[str, Any]:
    """Garantiza un bridge respondiendo en el puerto configurado.

    Health check primero: si /api/status responde, se ADOPTA ese bridge (la validacion
    previa que evita conexiones duplicadas). Si no responde, lanza el binario y espera
    a que la API sirva. Devuelve {"ok", "spawned", "status", "message"}.
    """
    import time as _time

    st = get_status()
    if st.get("success"):
        return {"ok": True, "spawned": False, "status": st}
    ok, msg = spawn_bridge()
    if not ok:
        return {"ok": False, "spawned": False, "status": {}, "message": msg}
    deadline = _time.monotonic() + timeout_s
    while _time.monotonic() < deadline:
        _time.sleep(0.5)
        st = get_status()
        if st.get("success"):
            return {"ok": True, "spawned": True, "status": st}
    return {
        "ok": False,
        "spawned": True,
        "status": {},
        "message": f"bridge spawned pero /api/status no respondio en {timeout_s:.0f}s (ver bridge.log del store)",
    }


def acquire_login_qr(max_recycles: int = 2, qr_wait_s: float = 15.0) -> Dict[str, Any]:
    """Consigue un QR de login vigente, validando/reciclando la sesion segun haga falta.

    Maquina de estados sobre /api/status y /api/qr:
      - logged_in && connected  -> sesion valida existente: NO se genera QR.
      - logged_in && needs_qr   -> sesion zombie (device invalidado remotamente): este
        proceso nunca emitira QR; se recicla (shutdown ordenado + respawn). whatsmeow
        borra la sesion rota en ese ciclo y el respawn siguiente entra en modo QR.
      - !logged_in              -> modo QR: esperar el primer codigo y devolverlo; si el
        canal se agoto ("timeout"), reciclar para obtener un canal fresco.
    """
    import time as _time

    ensured = ensure_bridge()
    if not ensured["ok"]:
        return {"ok": False, "message": ensured.get("message", "bridge unavailable")}

    for _ in range(max_recycles + 1):
        # Fase 1: dejar que el estado se asiente (un bridge con sesion valida tarda unos
        # segundos en conectar; uno zombie tarda lo mismo en descubrir el 401 remoto).
        st: Dict[str, Any] = ensured["status"]
        deadline = _time.monotonic() + qr_wait_s
        recycle = False
        while _time.monotonic() < deadline:
            if st.get("success"):
                if st.get("logged_in") and st.get("connected"):
                    return {"ok": True, "logged_in": True, "status": st}
                if st.get("logged_in") and st.get("needs_qr"):
                    recycle = True  # zombie: hay device guardado pero fue invalidado
                    break
                if not st.get("logged_in"):
                    break  # modo QR: pasar a esperar el codigo
            _time.sleep(0.5)  # poll agil: cada 500ms ahorra ~2s en la salida del QR
            st = get_status()

        # Fase 2: en modo QR, esperar a que el canal emita el codigo vigente.
        if not recycle:
            deadline = _time.monotonic() + qr_wait_s
            while _time.monotonic() < deadline:
                qr = get_qr()
                if qr.get("qr_status") == "logged_in":
                    return {"ok": True, "logged_in": True, "status": get_status()}
                if qr.get("qr_status") == "active":
                    return {"ok": True, "logged_in": False, "qr": qr}
                if qr.get("qr_status") == "timeout":
                    break  # canal agotado: reciclar para un canal fresco
                _time.sleep(0.5)

        shutdown_bridge()
        _time.sleep(2.0)
        ensured = ensure_bridge()
        if not ensured["ok"]:
            return {"ok": False, "message": ensured.get("message", "bridge unavailable")}

    return {
        "ok": False,
        "message": "no se pudo obtener un QR de login tras reciclar el bridge; ver bridge.log del store",
    }


# --- Watcher de rotación del QR (refresco proactivo de la Vista Previa) ---
#
# Los códigos QR de login rotan cada ~30-60 s. Este watcher (thread daemon del server
# MCP) consulta /api/qr y, en cada rotación, entrega el PNG nuevo al callback para que
# la imagen del visor local se regenere sola — el usuario siempre ve un código vigente
# sin pedirle nada al asistente. Termina al detectar login, canal muerto o timeout.

_qr_watcher_lock = __import__("threading").Lock()
_qr_watcher_running = False


def start_qr_preview_watcher(
    refresh_fn, initial_code: str = "", poll_interval_s: float = 2.5, max_seconds: float = 240.0
) -> bool:
    """Arranca (si no hay otro) un watcher que llama refresh_fn(png_bytes) en cada rotación.

    refresh_fn recibe los bytes PNG del código NUEVO. Devuelve False si ya hay un watcher
    activo (nunca hay dos). El thread es daemon: muere con el server MCP.
    """
    import base64 as _b64
    import threading
    import time as _time

    global _qr_watcher_running
    with _qr_watcher_lock:
        if _qr_watcher_running:
            return False
        _qr_watcher_running = True

    def _loop() -> None:
        global _qr_watcher_running
        last_code = initial_code
        deadline = _time.monotonic() + max_seconds
        # Entre la expiración del código N y la emisión del N+1, /api/qr reporta
        # "timeout" un instante: NO es terminal (ese bug congelaba el preview). Solo
        # rendirse si el estado no-activo persiste (canal muerto) o al confirmar login.
        strikes = 0
        max_strikes = 12  # ~30 s con el poll default
        try:
            while _time.monotonic() < deadline:
                _time.sleep(poll_interval_s)
                q = get_qr()
                status = q.get("qr_status")
                if status in ("logged_in", "success"):
                    return  # escaneado: misión cumplida
                if status != "active":
                    strikes += 1
                    if strikes >= max_strikes:
                        return  # canal muerto persistente; un nuevo login_with_qr rearma todo
                    continue
                strikes = 0
                code = q.get("code", "")
                png_b64 = q.get("png_base64", "")
                if code and code != last_code and png_b64:
                    last_code = code
                    try:
                        refresh_fn(_b64.b64decode(png_b64))
                    except Exception as e:  # el preview es best-effort; no matar el loop
                        logger.warning(f"qr preview refresh fallo: {e}")
        finally:
            with _qr_watcher_lock:
                _qr_watcher_running = False

    threading.Thread(target=_loop, daemon=True, name="qr-preview-watcher").start()
    return True
