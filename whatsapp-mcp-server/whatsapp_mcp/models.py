"""Modelos del dominio (Pydantic): Message, Chat, Contact, MessageContext.

Son el structured output de las tools MCP. Pydantic v2 genera el JSON Schema que ve
el LLM (con las descripciones de cada campo) y coacciona el 0/1 de SQLite a bool
automáticamente (antes se hacía a mano en __post_init__ con dataclasses).
"""
from datetime import datetime
from typing import List, Optional

from pydantic import BaseModel, Field


class Message(BaseModel):
    timestamp: datetime = Field(description="Cuándo se envió el mensaje (ISO-8601).")
    sender: str = Field(description="Identificador del remitente (número o parte local del JID).")
    content: str = Field(description="Texto del mensaje (o un marcador si es media/no-texto).")
    is_from_me: bool = Field(description="True si el mensaje lo envió el dueño de la cuenta.")
    chat_jid: str = Field(description="JID del chat al que pertenece el mensaje.")
    id: str = Field(description="ID único del mensaje dentro de su chat.")
    chat_name: Optional[str] = Field(default=None, description="Nombre resuelto del chat, si se conoce.")
    media_type: Optional[str] = Field(
        default=None,
        description="Tipo de media (image/video/audio/document/sticker/...) si el mensaje es media.",
    )


class Chat(BaseModel):
    jid: str = Field(description="JID del chat (individual @s.whatsapp.net / @lid, o grupo @g.us).")
    name: Optional[str] = Field(description="Nombre resuelto del chat.")
    last_message_time: Optional[datetime] = Field(description="Timestamp del último mensaje (ISO-8601).")
    last_message: Optional[str] = Field(default=None, description="Contenido del último mensaje, si se pidió incluirlo.")
    last_sender: Optional[str] = Field(default=None, description="Remitente del último mensaje.")
    last_is_from_me: Optional[bool] = Field(
        default=None, description="True si el último mensaje lo envió el dueño de la cuenta."
    )

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")


class Contact(BaseModel):
    phone_number: str = Field(description="Número de teléfono (con código de país, sin +).")
    name: Optional[str] = Field(description="Nombre del contacto en la libreta, si está guardado.")
    jid: str = Field(description="JID del contacto.")


class MessageContext(BaseModel):
    message: Message = Field(description="El mensaje objetivo.")
    before: List[Message] = Field(description="Mensajes inmediatamente anteriores al objetivo.")
    after: List[Message] = Field(description="Mensajes inmediatamente posteriores al objetivo.")
