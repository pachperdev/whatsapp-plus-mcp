"""Configuracion central: logger a stderr, timeouts y paths (DB/bridge/token)."""
import logging
import os.path
import sys

# Logging SIEMPRE a stderr: en transporte stdio, stdout es el canal del protocolo
# MCP (JSON-RPC). Un logger.error() a stdout inyecta texto crudo y corrompe el stream.
logger = logging.getLogger("whatsapp_mcp")
if not logger.handlers:
    _handler = logging.StreamHandler(sys.stderr)
    _handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(name)s: %(message)s"))
    logger.addHandler(_handler)
    logger.setLevel(logging.INFO)

# Timeout (connect, read) para las llamadas a la REST API del bridge.
# Sin esto, si el bridge se cuelga (no caído) el server MCP queda bloqueado para siempre.
REQUEST_TIMEOUT = (5, 30)

# Config por variables de entorno con defaults = layout actual del repo. Permite
# apuntar el server a otra ubicacion (tests, despliegue empaquetado, Claude Desktop)
# sin tocar codigo. Los defaults preservan el comportamiento historico.
# NOTA: este modulo vive en whatsapp_mcp/ (un nivel mas profundo que el viejo whatsapp.py),
# por eso son DOS ".." para resolver al mismo <repo>/whatsapp-bridge/store de siempre.
_STORE_DIR = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "..", "..", "whatsapp-bridge", "store"
)

MESSAGES_DB_PATH = os.environ.get("WHATSAPP_MESSAGES_DB", os.path.join(_STORE_DIR, "messages.db"))
# whatsmeow guarda la libreta de contactos y el mapeo lid<->numero aqui
WHATSAPP_DB_PATH = os.environ.get("WHATSAPP_SESSION_DB", os.path.join(_STORE_DIR, "whatsapp.db"))
WHATSAPP_API_BASE_URL = os.environ.get("WHATSAPP_BRIDGE_URL", "http://localhost:8080/api")
# Token de auth compartido con el bridge (el bridge lo genera en store/.bridge_token).
BRIDGE_TOKEN_PATH = os.environ.get(
    "WHATSAPP_BRIDGE_TOKEN_FILE", os.path.join(_STORE_DIR, ".bridge_token")
)

# --- Modo supervisor (login autogestionado / plug-and-play) ---
# Directorio del store que se le pasa al bridge lanzado por el supervisor. Respeta la
# misma env var que entiende el bridge, para que ambos procesos apunten al mismo store.
STORE_DIR = os.environ.get("WHATSAPP_STORE_DIR", os.path.normpath(_STORE_DIR))
# Binario del bridge. Default: el compilado junto al repo (`go build -o whatsapp-bridge`);
# el empaquetado plug-and-play (.mcpb) lo apunta a su binario precompilado.
BRIDGE_BIN_PATH = os.environ.get(
    "WHATSAPP_BRIDGE_BIN",
    os.path.normpath(os.path.join(_STORE_DIR, "..", "whatsapp-bridge")),
)
# Log del bridge cuando lo lanza el supervisor (su stdout deja de ser una terminal).
BRIDGE_LOG_PATH = os.environ.get("WHATSAPP_BRIDGE_LOG", os.path.join(STORE_DIR, "bridge.log"))
