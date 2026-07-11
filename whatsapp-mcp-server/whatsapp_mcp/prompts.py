"""Prompts MCP reutilizables: flujos comunes sobre WhatsApp.

Se registran en la misma instancia `mcp` que las tools. Todos son de SOLO LECTURA
por diseño: guían al modelo a leer/resumir y le prohíben enviar o marcar nada sin
confirmación explícita del usuario (coherente con la cautela anti-ban del proyecto).
"""
from whatsapp_mcp.tools import mcp


@mcp.prompt(title="Ponerme al día con los no leídos")
def catch_up_unread() -> str:
    """Resume los chats de WhatsApp con mensajes sin leer."""
    return (
        "Ponme al día con mi WhatsApp. Usá get_unread_chats para ver qué chats tienen "
        "mensajes sin leer y, para cada uno, list_messages para leer esos mensajes. "
        "Después dame un resumen breve por chat (quién escribió y de qué), ordenado por "
        "lo que parezca más urgente. NO marques nada como leído ni respondas: solo resumime."
    )


@mcp.prompt(title="Resumir una conversación")
def summarize_chat(contact_or_group: str) -> str:
    """Resume la conversación reciente con un contacto o grupo.

    Args:
        contact_or_group: Nombre o número del contacto, o nombre del grupo a resumir.
    """
    return (
        f"Resumime la conversación reciente de WhatsApp con '{contact_or_group}'. "
        "Encontrá el chat (search_contacts / list_chats), traé los últimos mensajes con "
        "list_messages y dame un resumen de los temas tratados y de cualquier cosa que "
        "requiera una respuesta de mi parte. NO envíes ningún mensaje."
    )


@mcp.prompt(title="Ayudarme a redactar una respuesta")
def draft_reply(contact_or_group: str) -> str:
    """Ayuda a redactar (SIN enviar) una respuesta para un contacto o grupo.

    Args:
        contact_or_group: Nombre o número del contacto, o nombre del grupo.
    """
    return (
        f"Ayudame a redactar una respuesta para '{contact_or_group}' en WhatsApp. "
        "Leé los últimos mensajes del chat (list_messages) para entender el contexto y "
        "proponeme 2 o 3 borradores de respuesta con tonos distintos. NO envíes nada: "
        "mostrame los borradores y esperá mi confirmación antes de usar send_message."
    )
