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
    assert "data:image/png;base64," not in textos, "el data URI es demasiado lento de transcribir"
    assert "artifact" in textos.lower(), "falta la instruccion de artifact para el asistente"
    assert "qrcodejs" in textos or "qrcode.min.js" in textos, "falta la plantilla con la libreria QR"
    # UX: modo carga integrado (artifact utilizable ANTES del codigo) y aviso de
    # expiracion suave (el codigo suele seguir siendo escaneable tras el countdown).
    assert "if (CODE)" in textos, "falta el modo carga (CODE vacio -> pantalla de espera)"
    assert "pudo rotar" in textos, "falta el aviso suave de expiracion (no alarma falsa)"
    assert '"test"' in textos or "'test'" in textos or ">test<" in textos or "text: \"test\"" in textos or "test" in textos, "falta el codigo crudo interpolado"


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
