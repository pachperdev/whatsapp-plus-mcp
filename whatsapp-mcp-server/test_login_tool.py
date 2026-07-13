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
    # Determinismo de la instruccion: la orden del artifact debe ser el PRIMER contenido
    # del resultado (primacia) y debe haber recordatorio al cierre (recencia). Con la
    # orden al final, el modelo a veces narraba sin crear el artifact (visto en Desktop).
    text_contents = [c.text for c in contents if isinstance(c, TextContent)]
    assert "artifact" in text_contents[0].lower() and "ACCI" in text_contents[0], (
        "la orden del artifact debe abrir el resultado"
    )
    assert "RECUERDA" in text_contents[-1], "falta el recordatorio de cierre"
    assert "artifact" in textos.lower(), "falta la instruccion de artifact para el asistente"
    # QR EN LA CONVERSACION: data URI mini (PNG 1-bit ~800B) que el modelo copia en
    # segundos, con la herramienta de visualizacion inline como via preferida y artifact
    # de conversacion solo como fallback. Sin CDN (CSP lo bloqueaba: QR en blanco).
    assert "cdnjs" not in textos, "nada de CDNs (CSP los bloquea)"
    assert "data:image/png;base64," in textos, "falta el data URI mini del QR"
    import re as _re
    m = _re.search(r"data:image/png;base64,([A-Za-z0-9+/=]+)", textos)
    assert m and len(m.group(1)) < 2500, f"data URI demasiado largo para copiarlo rapido: {len(m.group(1)) if m else 0}"
    assert "visualiz" in textos.lower(), "falta la via de visualizacion inline en el chat"
    assert "pudo rotar" in textos, "falta el aviso suave de expiracion (no alarma falsa)"


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


def test_qr_png_data_uri_compacto_y_valido():
    """El PNG mini debe ser un PNG real (magic+IHDR) y lo bastante corto para copiarlo rapido."""
    import base64
    import struct

    from whatsapp_mcp.tools import _qr_png_data_uri

    code = "https://wa.me/settings/linked_devices#2@" + "A" * 230
    uri = _qr_png_data_uri(code)
    assert uri.startswith("data:image/png;base64,")
    b64 = uri.split(",", 1)[1]
    assert len(b64) < 1500, f"demasiado grande: {len(b64)}"
    png = base64.b64decode(b64)
    assert png[:8] == b"\x89PNG\r\n\x1a\n", "magic PNG invalido"
    w, h = struct.unpack(">II", png[16:24])
    assert w == h and w >= 120, f"dimensiones sospechosas: {w}x{h}"
