"""La tool login_with_qr debe serializar el QR como ImageContent (no structured output).

Regresión: con structured output activo, FastMCP intenta serializar el objeto Image a
JSON y explota con "Unable to serialize unknown type ... Image" en el camino MCP real.
"""
import base64

import pytest
from mcp.types import ImageContent, TextContent

import whatsapp_mcp.bridge as bridge_mod
from whatsapp_mcp.tools import mcp

# PNG válido de 1x1 (transparente)
_PNG_1PX = base64.b64encode(
    base64.b64decode(
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGBgAAAABQAB"
        "h6FO1AAAAABJRU5ErkJggg=="
    )
)


@pytest.mark.anyio
async def test_login_with_qr_serializa_imagen_inline(monkeypatch):
    monkeypatch.setattr(
        bridge_mod,
        "acquire_login_qr",
        lambda: {
            "ok": True,
            "logged_in": False,
            "qr": {
                "qr_status": "active",
                "code": "test",
                "png_base64": _PNG_1PX.decode(),
                "expires_at": "2026-01-01T00:00:00Z",
            },
        },
    )
    result = await mcp.call_tool("login_with_qr", {"open_preview": False})
    contents = result[0] if isinstance(result, tuple) else result
    types_found = {type(c) for c in contents}
    assert ImageContent in types_found, f"sin ImageContent: {types_found}"
    assert TextContent in types_found, f"sin TextContent: {types_found}"
    # Clientes que colapsan los tool results (Claude Desktop/web): el asistente debe
    # poder re-mostrar el QR RAPIDO. Un data URI obliga al modelo a transcribir ~3000
    # tokens (>1 min, el codigo expira); la plantilla genera el QR en el navegador a
    # partir del codigo crudo (~270 chars) en segundos.
    textos = " ".join(c.text for c in contents if isinstance(c, TextContent))
    # QR EN LA CONVERSACION: texto de medio-bloque en un code block (se renderiza al
    # instante, sin herramientas). Descartadas por pruebas reales: markdown data URI
    # (Desktop lo bloquea), visualize/artifact (~1 min, el codigo expira), CDN (CSP).
    assert "cdnjs" not in textos, "nada de CDNs (CSP los bloquea)"
    assert "data:image/png;base64," not in textos, "el data URI en markdown lo bloquea Desktop"
    assert "```" in textos, "falta el bloque de codigo para el QR de texto"
    assert any(ch in textos for ch in "▀▄█"), "falta el QR en caracteres de medio-bloque"
    assert "Vista Previa" in textos or "visor de" in textos, "falta la Vista Previa como canal primario"


@pytest.mark.anyio
async def test_login_with_qr_sesion_valida_sin_imagen(monkeypatch):
    monkeypatch.setattr(
        bridge_mod,
        "acquire_login_qr",
        lambda: {"ok": True, "logged_in": True, "status": {"jid": "573001234567:26@s.whatsapp.net"}},
    )
    result = await mcp.call_tool("login_with_qr", {"open_preview": False})
    contents = result[0] if isinstance(result, tuple) else result
    assert all(isinstance(c, TextContent) for c in contents)
    assert "Sesión válida existente" in contents[0].text


@pytest.fixture
def anyio_backend():
    return "asyncio"


def test_qr_text_blocks_estructura_y_tamano():
    """El QR de texto debe ser cuadrado-ish en medio-bloque y compacto para copiarlo rapido."""
    from whatsapp_mcp.tools import _qr_text_blocks

    code = "https://wa.me/settings/linked_devices#2@" + "A" * 230
    txt = _qr_text_blocks(code)
    lines = txt.splitlines()
    # medio-bloque: ~la mitad de filas que columnas
    assert all(set(ln) <= set(" \u2580\u2584\u2588") for ln in lines), "caracteres invalidos"
    assert len(lines[0]) >= 2 * len(lines) - 4, "no parece medio-bloque (ratio alto/ancho)"
    assert len(txt) < 3000, f"QR de texto demasiado grande para copiarlo rapido: {len(txt)}"