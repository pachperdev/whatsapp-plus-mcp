"""Entrypoint del server MCP: arranca el transporte stdio."""
from whatsapp_mcp.tools import mcp


def main():
    # Initialize and run the server
    mcp.run(transport="stdio")
