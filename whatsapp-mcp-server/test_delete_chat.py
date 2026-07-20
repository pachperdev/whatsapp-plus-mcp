"""Tool #66 delete_chat: borra un chat completo via app-state (appstate.BuildDeleteChat).

El borrado de chat es una mutacion de app-state (misma familia que archive/pin/mute),
asi que puede devolver el 409 LTHash transitorio y reintentarse, igual que las demas.
"""
import pytest
from mcp.types import TextContent

import whatsapp_mcp.bridge as bridge_mod
from whatsapp_mcp.tools import mcp


def test_bridge_delete_chat_postea_endpoint_y_payload(monkeypatch):
    """bridge.delete_chat postea a /delete_chat con chat_jid + delete_media y propaga (ok, msg)."""
    captured = {}

    def fake_post(path, payload):
        captured["path"] = path
        captured["payload"] = payload
        return {"success": True, "message": "chat deleted"}

    monkeypatch.setattr(bridge_mod, "_bridge_post", fake_post)
    ok, msg = bridge_mod.delete_chat("573156684893@s.whatsapp.net", delete_media=True)
    assert captured["path"] == "delete_chat"
    assert captured["payload"] == {
        "chat_jid": "573156684893@s.whatsapp.net",
        "delete_media": True,
    }
    assert ok is True
    assert msg == "chat deleted"


def test_bridge_delete_chat_delete_media_default_false(monkeypatch):
    """Por defecto delete_media=False: borra el chat pero conserva la media en disco."""
    captured = {}
    monkeypatch.setattr(
        bridge_mod,
        "_bridge_post",
        lambda p, pl: captured.update(payload=pl) or {"success": True, "message": "ok"},
    )
    bridge_mod.delete_chat("z@s.whatsapp.net")
    assert captured["payload"]["delete_media"] is False


@pytest.mark.anyio
async def test_delete_chat_registrada_como_tool_y_envuelve_bridge(monkeypatch):
    """La tool delete_chat existe en el server MCP y devuelve el resultado del bridge."""
    monkeypatch.setattr(
        bridge_mod,
        "delete_chat",
        lambda chat_jid, delete_media=False: (True, "chat deleted"),
    )
    result = await mcp.call_tool("delete_chat", {"chat_jid": "z@s.whatsapp.net"})
    contents = result[0] if isinstance(result, tuple) else result
    textos = " ".join(c.text for c in contents if isinstance(c, TextContent))
    assert "chat deleted" in textos


@pytest.fixture
def anyio_backend():
    return "asyncio"
