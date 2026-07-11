"""Entrypoint del server MCP: arranca el transporte stdio."""
from whatsapp_mcp import prompts as _prompts  # noqa: F401  # registra los @mcp.prompt en `mcp`
from whatsapp_mcp.tools import mcp


def main():
    # Initialize and run the server
    mcp.run(transport="stdio")
