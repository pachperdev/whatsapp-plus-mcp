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
    assert "data:image/png;base64," not in textos, "el data URI es demasiado lento de transcribir"
    assert "artifact" in textos.lower(), "falta la instruccion de artifact para el asistente"
    # Render AUTOCONTENIDO: la matriz pre-computada se pinta en un canvas con JS puro.
    # Un CDN (qrcodejs) fue bloqueado por la CSP del panel de Cowork -> QR en blanco
    # (visto en prueba real). Cero dependencias de red en la plantilla.
    assert "cdnjs" not in textos, "la plantilla no debe depender de un CDN (CSP lo bloquea)"
    assert "MATRIX" in textos and "canvas" in textos, "falta el render por matriz en canvas"
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


def test_qr_matrix_hex_roundtrip():
    """La matriz serializada debe reconstruir EXACTAMENTE el QR original (bit a bit)."""
    import qrcode as qrlib

    from whatsapp_mcp.tools import _qr_matrix_hex

    code = "https://wa.me/settings/linked_devices#2@" + "A" * 200
    serial = _qr_matrix_hex(code)
    rows = serial.split(";")
    n = len(rows)

    q = qrlib.QRCode(error_correction=qrlib.constants.ERROR_CORRECT_L, border=2)
    q.add_data(code)
    q.make(fit=True)
    matrix = q.get_matrix()
    assert n == len(matrix)
    # decodificar igual que el JS del artifact: bit j de la fila = hex[j>>2] >> (3-(j&3)) & 1
    for i, row in enumerate(matrix):
        for j, cell in enumerate(row):
            bit = (int(rows[i][j >> 2], 16) >> (3 - (j & 3))) & 1
            assert bit == (1 if cell else 0), f"bit ({i},{j}) no coincide"
    assert len(serial) < 1500, f"matriz demasiado grande para escribirla rapido: {len(serial)}"
