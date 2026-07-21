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

# --- Modo plugin vs modo repo ---
# WHATSAPP_PLUGIN_MODE=1 (lo setea el plugin de Claude Code en su mcpServers.env) mueve
# TODOS los datos mutables (store con sesion/mensajes, binario compilado, logs) a
# ~/.whatsapp-mcp/. Razon: el plugin instalado se REEMPLAZA en cada update — si el store
# viviera dentro del directorio del plugin, un update destruiria la sesion vinculada y
# el historial. En modo repo (default) se preserva el layout historico del proyecto.
PLUGIN_MODE = os.environ.get("WHATSAPP_PLUGIN_MODE", "") == "1"

# Raiz del codigo (el repo o el plugin instalado): whatsapp_mcp/ -> whatsapp-mcp-server/ -> raiz.
_CODE_ROOT = os.path.normpath(
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..")
)
# Fuente del bridge Go (para el auto-build del supervisor cuando falta el binario).
BRIDGE_SRC_DIR = os.path.join(_CODE_ROOT, "whatsapp-bridge")

if PLUGIN_MODE:
    _DATA_DIR = os.path.expanduser(os.path.join("~", ".whatsapp-mcp"))
    _STORE_DIR = os.path.join(_DATA_DIR, "store")
    _DEFAULT_BRIDGE_BIN = os.path.join(_DATA_DIR, "bin", "whatsapp-bridge")
    _DEFAULT_MODELS_DIR = os.path.join(_DATA_DIR, "models")
else:
    _STORE_DIR = os.path.join(BRIDGE_SRC_DIR, "store")
    _DEFAULT_BRIDGE_BIN = os.path.join(BRIDGE_SRC_DIR, "whatsapp-bridge")
    _DEFAULT_MODELS_DIR = os.path.join(_STORE_DIR, "models")

# Config por variables de entorno con defaults segun el modo. Los env vars explicitos
# siempre ganan (tests, layouts a medida, otros agentes MCP).
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
# Binario del bridge. En modo repo: el compilado junto al repo (`go build -o whatsapp-bridge`).
# En modo plugin: ~/.whatsapp-mcp/bin/ (sobrevive updates del plugin; el supervisor lo
# auto-compila desde BRIDGE_SRC_DIR si falta y hay toolchain Go).
BRIDGE_BIN_PATH = os.environ.get("WHATSAPP_BRIDGE_BIN", _DEFAULT_BRIDGE_BIN)
# Log del bridge cuando lo lanza el supervisor (su stdout deja de ser una terminal).
BRIDGE_LOG_PATH = os.environ.get("WHATSAPP_BRIDGE_LOG", os.path.join(STORE_DIR, "bridge.log"))
# Repo GitHub del que se descargan los binarios precompilados del bridge (GitHub
# Releases + checksums SHA256). La API sigue el redirect si el repo se transfiere.
RELEASE_REPO = os.environ.get("WHATSAPP_RELEASE_REPO", "pachperdev/whatsapp-plus-mcp")


def _env_int(name: str, default: int) -> int:
    """Entero desde env con fallback: un valor basura no debe tumbar el server al importar."""
    try:
        return int(os.environ.get(name, default))
    except (TypeError, ValueError):
        logger.warning(f"{name} invalido; usando default {default}")
        return default


# --- Transcripcion local de notas de voz (extra opcional `transcription`) ---
# STT 100% local via faster-whisper; estas vars controlan modelo, recursos y limites.
# "auto" = elegir el tier segun RAM/cores de la maquina (ver transcription.resolve_model).
# Solo se aceptan tiny/base/small/large-v3-turbo (allowlist en transcription.py: cualquier
# otro string seria tratado como repo-id de Hugging Face y descargado); un valor invalido
# se ignora con warning y cae a la heuristica auto.
TRANSCRIPTION_MODEL = os.environ.get("WHATSAPP_TRANSCRIPTION_MODEL", "auto")
# Donde se descargan/cachean los pesos del modelo: junto al resto de datos mutables
# (~/.whatsapp-mcp/models en modo plugin; <store>/models en modo repo).
TRANSCRIPTION_MODELS_DIR = os.environ.get(
    "WHATSAPP_TRANSCRIPTION_MODELS_DIR", _DEFAULT_MODELS_DIR
)
TRANSCRIPTION_DEVICE = os.environ.get("WHATSAPP_TRANSCRIPTION_DEVICE", "cpu")
TRANSCRIPTION_COMPUTE = os.environ.get("WHATSAPP_TRANSCRIPTION_COMPUTE", "int8")
# Tope de duracion del audio (segundos) antes de abortar la transcripcion; 0 = sin limite.
TRANSCRIPTION_MAX_SECONDS = _env_int("WHATSAPP_TRANSCRIPTION_MAX_SECONDS", 900)
# 0 = derivar beam_size del tier del modelo (greedy en tiers chicos, beam 5 en el resto).
TRANSCRIPTION_BEAM = _env_int("WHATSAPP_TRANSCRIPTION_BEAM", 0)
