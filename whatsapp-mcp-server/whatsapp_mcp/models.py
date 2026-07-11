"""Dataclasses del dominio: Message, Chat, Contact, MessageContext."""
from dataclasses import dataclass
from datetime import datetime
from typing import List, Optional


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

    def __post_init__(self):
        # SQLite guarda los booleanos como 0/1 (int). mcp >=1.10 valida el structured output
        # contra el schema y rechaza un int donde se espera bool -> normalizar a bool.
        self.is_from_me = bool(self.is_from_me)

@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None

    def __post_init__(self):
        # Igual que Message: normaliza el 0/1 de SQLite a bool, preservando None (campo opcional).
        if self.last_is_from_me is not None:
            self.last_is_from_me = bool(self.last_is_from_me)

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
