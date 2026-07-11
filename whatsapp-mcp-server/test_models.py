"""Tests de contrato de los modelos Pydantic (whatsapp_mcp.models).

Blindan el structured output de las tools MCP contra cambios accidentales:
  (a) nombres / presencia / tipos de cada campo al serializar,
  (b) la coerción 0/1 -> bool que hace SQLite al leer enteros,
  (c) que las filas con opcionales en None (chat_name, media_type, last_*, name) no rompan,
  (d) un golden del JSON Schema de cada modelo (lo que ve el LLM) para detectar drift.

Todo es determinista: no tocan red, bridge ni DB; usan datetimes fijos.
"""

import json
from datetime import datetime

from whatsapp_mcp.models import Chat, Contact, Message, MessageContext

# datetime fijo (naive) para que la serialización sea reproducible entre corridas.
_TS = datetime(2026, 7, 1, 10, 0, 0)
_TS_ISO = "2026-07-01T10:00:00"


def _sample_message(**overrides) -> Message:
    base = dict(
        timestamp=_TS,
        sender="111",
        content="hola",
        is_from_me=False,
        chat_jid="111@s.whatsapp.net",
        id="MSGID1",
    )
    base.update(overrides)
    return Message(**base)


# --- (a) Contrato de serialización: campos, presencia y tipos ---------------------------


def test_message_serialization_contract():
    m = _sample_message(chat_name="Ana", media_type="image")
    data = m.model_dump()
    assert set(data.keys()) == {
        "timestamp",
        "sender",
        "content",
        "is_from_me",
        "chat_jid",
        "id",
        "chat_name",
        "media_type",
    }
    assert isinstance(data["timestamp"], datetime)
    assert isinstance(data["sender"], str)
    assert isinstance(data["content"], str)
    assert isinstance(data["is_from_me"], bool)
    assert isinstance(data["chat_jid"], str)
    assert isinstance(data["id"], str)
    assert data["chat_name"] == "Ana"
    assert data["media_type"] == "image"
    # En modo JSON el timestamp sale como string ISO-8601 (lo que consume el cliente MCP).
    jdata = m.model_dump(mode="json")
    assert isinstance(jdata["timestamp"], str)
    assert jdata["timestamp"] == _TS_ISO


def test_chat_serialization_contract():
    c = Chat(
        jid="111@s.whatsapp.net",
        name="Ana",
        last_message_time=_TS,
        last_message="hola",
        last_sender="111",
        last_is_from_me=False,
    )
    data = c.model_dump()
    assert set(data.keys()) == {
        "jid",
        "name",
        "last_message_time",
        "last_message",
        "last_sender",
        "last_is_from_me",
    }
    assert isinstance(data["jid"], str)
    assert isinstance(data["name"], str)
    assert isinstance(data["last_message_time"], datetime)
    assert isinstance(data["last_message"], str)
    assert isinstance(data["last_sender"], str)
    assert isinstance(data["last_is_from_me"], bool)
    # is_group es una property derivada del jid; NO forma parte del dump serializado.
    assert "is_group" not in data
    assert c.is_group is False
    assert Chat(jid="123-456@g.us", name=None, last_message_time=None).is_group is True


def test_contact_serialization_contract():
    ct = Contact(phone_number="111", name="Ana", jid="111@s.whatsapp.net")
    data = ct.model_dump()
    assert set(data.keys()) == {"phone_number", "name", "jid"}
    assert isinstance(data["phone_number"], str)
    assert isinstance(data["name"], str)
    assert isinstance(data["jid"], str)


def test_messagecontext_serialization_contract():
    target = _sample_message(id="T")
    mc = MessageContext(
        message=target,
        before=[_sample_message(id="B1")],
        after=[_sample_message(id="A1"), _sample_message(id="A2")],
    )
    data = mc.model_dump()
    assert set(data.keys()) == {"message", "before", "after"}
    assert data["message"]["id"] == "T"
    assert [m["id"] for m in data["before"]] == ["B1"]
    assert [m["id"] for m in data["after"]] == ["A1", "A2"]
    # Los mensajes anidados conservan el mismo contrato que un Message suelto.
    assert set(data["message"].keys()) == {
        "timestamp",
        "sender",
        "content",
        "is_from_me",
        "chat_jid",
        "id",
        "chat_name",
        "media_type",
    }


# --- (b) Coerción int(0/1) -> bool (SQLite guarda booleanos como 0/1) --------------------


def test_int_to_bool_coercion_message():
    assert _sample_message(is_from_me=1).is_from_me is True
    assert _sample_message(is_from_me=0).is_from_me is False


def test_int_to_bool_coercion_chat_last_is_from_me():
    assert Chat(jid="j", name=None, last_message_time=None, last_is_from_me=1).last_is_from_me is True
    assert Chat(jid="j", name=None, last_message_time=None, last_is_from_me=0).last_is_from_me is False


# --- (c) Opcionales en None no rompen la construcción -----------------------------------


def test_message_optional_none():
    m = _sample_message(chat_name=None, media_type=None)
    assert m.chat_name is None
    assert m.media_type is None


def test_chat_optional_none():
    c = Chat(jid="111@s.whatsapp.net", name=None, last_message_time=None)
    assert c.name is None
    assert c.last_message_time is None
    assert c.last_message is None
    assert c.last_sender is None
    assert c.last_is_from_me is None


def test_contact_optional_none():
    ct = Contact(phone_number="111", name=None, jid="111@s.whatsapp.net")
    assert ct.name is None


# --- (d) Golden del JSON Schema: detecta drift del contrato que ve el LLM ----------------
#
# El schema lo genera Pydantic v2 a partir de los modelos. Si alguien agrega/renombra/retira
# un campo, cambia un tipo, un default o una descripción, estas comparaciones fallan y obligan
# a actualizar el golden a conciencia. La igualdad de dicts es independiente del orden de claves.

_MESSAGE_SCHEMA = """
{
  "properties": {
    "timestamp": {
      "description": "Cuándo se envió el mensaje (ISO-8601).",
      "format": "date-time",
      "title": "Timestamp",
      "type": "string"
    },
    "sender": {
      "description": "Identificador del remitente (número o parte local del JID).",
      "title": "Sender",
      "type": "string"
    },
    "content": {
      "description": "Texto del mensaje (o un marcador si es media/no-texto).",
      "title": "Content",
      "type": "string"
    },
    "is_from_me": {
      "description": "True si el mensaje lo envió el dueño de la cuenta.",
      "title": "Is From Me",
      "type": "boolean"
    },
    "chat_jid": {
      "description": "JID del chat al que pertenece el mensaje.",
      "title": "Chat Jid",
      "type": "string"
    },
    "id": {
      "description": "ID único del mensaje dentro de su chat.",
      "title": "Id",
      "type": "string"
    },
    "chat_name": {
      "anyOf": [{"type": "string"}, {"type": "null"}],
      "default": null,
      "description": "Nombre resuelto del chat, si se conoce.",
      "title": "Chat Name"
    },
    "media_type": {
      "anyOf": [{"type": "string"}, {"type": "null"}],
      "default": null,
      "description": "Tipo de media (image/video/audio/document/sticker/...) si el mensaje es media.",
      "title": "Media Type"
    }
  },
  "required": ["timestamp", "sender", "content", "is_from_me", "chat_jid", "id"],
  "title": "Message",
  "type": "object"
}
"""

_CHAT_SCHEMA = """
{
  "properties": {
    "jid": {
      "description": "JID del chat (individual @s.whatsapp.net / @lid, o grupo @g.us).",
      "title": "Jid",
      "type": "string"
    },
    "name": {
      "anyOf": [{"type": "string"}, {"type": "null"}],
      "description": "Nombre resuelto del chat.",
      "title": "Name"
    },
    "last_message_time": {
      "anyOf": [{"format": "date-time", "type": "string"}, {"type": "null"}],
      "description": "Timestamp del último mensaje (ISO-8601).",
      "title": "Last Message Time"
    },
    "last_message": {
      "anyOf": [{"type": "string"}, {"type": "null"}],
      "default": null,
      "description": "Contenido del último mensaje, si se pidió incluirlo.",
      "title": "Last Message"
    },
    "last_sender": {
      "anyOf": [{"type": "string"}, {"type": "null"}],
      "default": null,
      "description": "Remitente del último mensaje.",
      "title": "Last Sender"
    },
    "last_is_from_me": {
      "anyOf": [{"type": "boolean"}, {"type": "null"}],
      "default": null,
      "description": "True si el último mensaje lo envió el dueño de la cuenta.",
      "title": "Last Is From Me"
    }
  },
  "required": ["jid", "name", "last_message_time"],
  "title": "Chat",
  "type": "object"
}
"""

_CONTACT_SCHEMA = """
{
  "properties": {
    "phone_number": {
      "description": "Número de teléfono (con código de país, sin +).",
      "title": "Phone Number",
      "type": "string"
    },
    "name": {
      "anyOf": [{"type": "string"}, {"type": "null"}],
      "description": "Nombre del contacto en la libreta, si está guardado.",
      "title": "Name"
    },
    "jid": {
      "description": "JID del contacto.",
      "title": "Jid",
      "type": "string"
    }
  },
  "required": ["phone_number", "name", "jid"],
  "title": "Contact",
  "type": "object"
}
"""

_MESSAGECONTEXT_SCHEMA = """
{
  "$defs": {
    "Message": {
      "properties": {
        "timestamp": {
          "description": "Cuándo se envió el mensaje (ISO-8601).",
          "format": "date-time",
          "title": "Timestamp",
          "type": "string"
        },
        "sender": {
          "description": "Identificador del remitente (número o parte local del JID).",
          "title": "Sender",
          "type": "string"
        },
        "content": {
          "description": "Texto del mensaje (o un marcador si es media/no-texto).",
          "title": "Content",
          "type": "string"
        },
        "is_from_me": {
          "description": "True si el mensaje lo envió el dueño de la cuenta.",
          "title": "Is From Me",
          "type": "boolean"
        },
        "chat_jid": {
          "description": "JID del chat al que pertenece el mensaje.",
          "title": "Chat Jid",
          "type": "string"
        },
        "id": {
          "description": "ID único del mensaje dentro de su chat.",
          "title": "Id",
          "type": "string"
        },
        "chat_name": {
          "anyOf": [{"type": "string"}, {"type": "null"}],
          "default": null,
          "description": "Nombre resuelto del chat, si se conoce.",
          "title": "Chat Name"
        },
        "media_type": {
          "anyOf": [{"type": "string"}, {"type": "null"}],
          "default": null,
          "description": "Tipo de media (image/video/audio/document/sticker/...) si el mensaje es media.",
          "title": "Media Type"
        }
      },
      "required": ["timestamp", "sender", "content", "is_from_me", "chat_jid", "id"],
      "title": "Message",
      "type": "object"
    }
  },
  "properties": {
    "message": {"$ref": "#/$defs/Message", "description": "El mensaje objetivo."},
    "before": {
      "description": "Mensajes inmediatamente anteriores al objetivo.",
      "items": {"$ref": "#/$defs/Message"},
      "title": "Before",
      "type": "array"
    },
    "after": {
      "description": "Mensajes inmediatamente posteriores al objetivo.",
      "items": {"$ref": "#/$defs/Message"},
      "title": "After",
      "type": "array"
    }
  },
  "required": ["message", "before", "after"],
  "title": "MessageContext",
  "type": "object"
}
"""


def test_message_json_schema_golden():
    assert Message.model_json_schema() == json.loads(_MESSAGE_SCHEMA)


def test_chat_json_schema_golden():
    assert Chat.model_json_schema() == json.loads(_CHAT_SCHEMA)


def test_contact_json_schema_golden():
    assert Contact.model_json_schema() == json.loads(_CONTACT_SCHEMA)


def test_messagecontext_json_schema_golden():
    assert MessageContext.model_json_schema() == json.loads(_MESSAGECONTEXT_SCHEMA)
