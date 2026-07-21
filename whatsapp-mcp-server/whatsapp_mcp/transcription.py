"""Transcripcion local de notas de voz (STT) con faster-whisper.

Decision: faster-whisper (Whisper sobre CTranslate2) corre 100% local con compute
int8 en CPU — mas que suficiente para notas de voz — y su decodificador (PyAV, con
FFmpeg embebido en el wheel) abre los .ogg Opus de WhatsApp sin necesitar un ffmpeg
del sistema. Es un extra opcional (`transcription`): el import es lazy y el server
completo sigue funcionando sin el.
"""
import logging
import os
import threading
import time
from typing import Any, Dict, Optional, Tuple

from whatsapp_mcp import config

# Hijo del logger "whatsapp_mcp" (config.py): propaga a su handler de stderr, pero
# identifica este modulo en cada linea. Todas las ramas de fallo loguean (como bridge.py).
logger = logging.getLogger(__name__)

_GIB = 1024**3

# Allowlist de modelos: faster-whisper trata cualquier string desconocido como repo-id
# de Hugging Face y lo DESCARGA — un caller podria bajar un repo arbitrario, rompiendo
# la promesa "100% local". Solo estos tiers estan permitidos.
_ALLOWED_MODELS = ("tiny", "base", "small", "large-v3-turbo")

# Tope de espera por el lock del modelo (segundos, parcheable en tests): si otra
# transcripcion — o la descarga de pesos, que puede tardar — lo retiene mas que esto,
# devolvemos "busy" accionable en vez de encolar llamadas indefinidamente.
_BUSY_TIMEOUT = 15.0

_BUSY_ERROR = (
    "Another transcription or model download is in progress; retry in a few seconds."
)

# Fallback conservador cuando el hardware no se puede detectar (contenedores raros,
# plataformas exoticas): asumir poca maquina y elegir un tier liviano.
_FALLBACK_RAM = 4 * _GIB
_FALLBACK_CPUS = 2

# beam_size por tier cuando WHATSAPP_TRANSCRIPTION_BEAM=0: greedy (1) en los tiers
# chicos (~2x mas rapido en maquinas flojas), beam 5 donde el hardware lo banca.
_BEAM_BY_MODEL = {"large-v3-turbo": 5, "small": 5, "base": 1, "tiny": 1}
_DEFAULT_BEAM = 5

# Error accionable cuando falta el extra opcional (el server sigue vivo sin la feature).
_MISSING_EXTRA_ERROR = (
    "faster-whisper is not installed (optional extra). Enable it with "
    "`uv sync --extra transcription`, or launch the server with "
    "`uv run --extra transcription main.py` (for the plugin, add "
    "`--extra transcription` to the launch command). "
    "The rest of the server keeps working without this feature."
)

# Cache del modelo: UNA sola instancia residente (importa en maquinas con poca RAM),
# keyed por (modelo, device, compute). Pedir otro modelo libera el anterior antes de
# cargar el nuevo. El lock evita cargas dobles concurrentes.
_model_lock = threading.Lock()
_cached_key: Optional[Tuple[str, str, str]] = None
_cached_model: Any = None


class _ModelBusyError(Exception):
    """El lock del modelo no se consiguio dentro de _BUSY_TIMEOUT (otra transcripcion
    o descarga en curso). transcribe_file lo convierte en {"success": False, ...}."""


def _windows_total_ram() -> int:
    """RAM fisica total en Windows via GlobalMemoryStatusEx (no hay os.sysconf)."""
    import ctypes

    class MEMORYSTATUSEX(ctypes.Structure):
        _fields_ = [
            ("dwLength", ctypes.c_ulong),
            ("dwMemoryLoad", ctypes.c_ulong),
            ("ullTotalPhys", ctypes.c_ulonglong),
            ("ullAvailPhys", ctypes.c_ulonglong),
            ("ullTotalPageFile", ctypes.c_ulonglong),
            ("ullAvailPageFile", ctypes.c_ulonglong),
            ("ullTotalVirtual", ctypes.c_ulonglong),
            ("ullAvailVirtual", ctypes.c_ulonglong),
            ("ullAvailExtendedVirtual", ctypes.c_ulonglong),
        ]

    stat = MEMORYSTATUSEX()
    stat.dwLength = ctypes.sizeof(MEMORYSTATUSEX)
    if not ctypes.windll.kernel32.GlobalMemoryStatusEx(ctypes.byref(stat)):
        raise OSError("GlobalMemoryStatusEx failed")
    return int(stat.ullTotalPhys)


def _hardware_profile() -> Tuple[int, int]:
    """(ram_bytes, cpu_count) de la maquina; NUNCA lanza (fallback conservador)."""
    try:
        if hasattr(os, "sysconf"):
            ram = os.sysconf("SC_PAGE_SIZE") * os.sysconf("SC_PHYS_PAGES")
        else:
            ram = _windows_total_ram()
        if ram <= 0:
            ram = _FALLBACK_RAM
    except Exception:
        ram = _FALLBACK_RAM
    try:
        cpus = os.cpu_count() or _FALLBACK_CPUS
    except Exception:
        cpus = _FALLBACK_CPUS
    return ram, cpus


def resolve_model(explicit: Optional[str] = None) -> Tuple[str, str, str]:
    """Resuelve que modelo Whisper usar: (model_name, source, reason).

    Precedencia: parametro explicito > env WHATSAPP_TRANSCRIPTION_MODEL (si != "auto")
    > heuristica por hardware. `source` es "param" | "env" | "auto" y `reason` explica
    la eleccion en una linea legible (queda en el resultado de la tool).

    Solo se aceptan modelos de _ALLOWED_MODELS: un parametro explicito invalido lanza
    ValueError (transcribe_file lo convierte en error de resultado); un env var
    invalido se ignora con warning y cae a la heuristica.
    """
    if explicit:
        if explicit not in _ALLOWED_MODELS:
            raise ValueError(
                f"Unknown Whisper model '{explicit}'. "
                f"Valid models: {', '.join(_ALLOWED_MODELS)}."
            )
        logger.info(f"Modelo Whisper: {explicit} (parametro explicito)")
        return explicit, "param", f"model requested explicitly: {explicit}"
    env_model = config.TRANSCRIPTION_MODEL
    if env_model and env_model != "auto":
        if env_model in _ALLOWED_MODELS:
            logger.info(f"Modelo Whisper: {env_model} (env WHATSAPP_TRANSCRIPTION_MODEL)")
            return env_model, "env", f"env WHATSAPP_TRANSCRIPTION_MODEL={env_model}"
        logger.warning(
            f"WHATSAPP_TRANSCRIPTION_MODEL={env_model} no es un modelo valido "
            f"({', '.join(_ALLOWED_MODELS)}); ignorado, usando la heuristica auto"
        )
    ram, cores = _hardware_profile()
    if ram >= 16 * _GIB and cores >= 8:
        name = "large-v3-turbo"
    elif ram >= 8 * _GIB and cores >= 4:
        name = "small"
    elif ram >= 4 * _GIB:
        name = "base"
    else:
        name = "tiny"
    reason = f"auto: {ram / _GIB:.1f} GiB RAM, {cores} cores → {name}"
    logger.info(f"Modelo Whisper resuelto — {reason}")
    return name, "auto", reason


def _beam_size(model_name: str) -> int:
    """beam_size efectivo: el override por env gana; si no, se deriva del tier."""
    if config.TRANSCRIPTION_BEAM > 0:
        return config.TRANSCRIPTION_BEAM
    return _BEAM_BY_MODEL.get(model_name, _DEFAULT_BEAM)


def _load_model(model_name: str) -> Any:
    """Carga (o reutiliza) el WhisperModel. Lazy import: faster_whisper solo se
    importa aca, para que el server arranque sin el extra instalado.

    Lanza _ModelBusyError si el lock no se consigue en _BUSY_TIMEOUT: construir el
    modelo puede descargar pesos (hasta ~1.6 GB) y sostener el lock sin tope
    convertiria una descarga colgada en un bloqueo permanente de TODAS las llamadas.
    """
    import faster_whisper  # lazy: extra opcional `transcription`

    key = (model_name, config.TRANSCRIPTION_DEVICE, config.TRANSCRIPTION_COMPUTE)
    global _cached_key, _cached_model
    if not _model_lock.acquire(timeout=_BUSY_TIMEOUT):
        raise _ModelBusyError(_BUSY_ERROR)
    try:
        if _cached_key == key and _cached_model is not None:
            return _cached_model
        # Evict-first deliberado: liberar el modelo residente ANTES de construir el
        # nuevo evita el pico doble de RAM (dos modelos simultaneos pueden no entrar
        # en las maquinas limitadas que son el target de la feature). Tradeoff
        # asumido: si la carga nueva falla no hay rollback al ultimo modelo bueno —
        # la proxima llamada simplemente recarga.
        _cached_model = None
        _cached_key = None
        models_dir = config.TRANSCRIPTION_MODELS_DIR
        os.makedirs(models_dir, exist_ok=True)
        _, cores = _hardware_profile()
        # Acotar la descarga misma: huggingface_hub respeta esta env como
        # read-timeout, asi una conexion muerta lanza excepcion en vez de colgar
        # indefinidamente (con el lock tomado, colgaria el server entero — mismo
        # criterio que los timeouts documentados en config.py). setdefault: un valor
        # ya configurado por el usuario no se pisa.
        os.environ.setdefault("HF_HUB_DOWNLOAD_TIMEOUT", "30")
        logger.info(
            f"Cargando modelo Whisper '{model_name}' (pesos en {models_dir}; "
            "si faltan, se descargan de Hugging Face)"
        )
        try:
            model = faster_whisper.WhisperModel(
                model_name,
                device=config.TRANSCRIPTION_DEVICE,
                compute_type=config.TRANSCRIPTION_COMPUTE,
                download_root=models_dir,
                cpu_threads=min(8, max(1, cores - 1)),
            )
        except Exception as e:
            logger.error(f"Fallo la carga del modelo Whisper '{model_name}': {e}")
            raise
        logger.info(f"Modelo Whisper '{model_name}' cargado y residente")
        _cached_key = key
        _cached_model = model
        return model
    finally:
        _model_lock.release()


def transcribe_file(
    path: str, language: Optional[str] = None, model: Optional[str] = None
) -> Dict[str, Any]:
    """Transcribe un archivo de audio local a texto. Nunca deja escapar excepciones:
    siempre devuelve {"success": True, ...} o {"success": False, "error": ...}.

    Args:
        path: ruta local del audio (los .ogg Opus de WhatsApp se decodifican directo)
        language: codigo ISO 639-1 opcional; None = autodeteccion de Whisper
        model: modelo Whisper opcional; None = env/heuristica (ver resolve_model)
    """
    if not path or not os.path.isfile(path):
        logger.error(f"Transcripcion abortada: archivo de audio inexistente: {path}")
        return {"success": False, "error": f"Audio file not found: {path}"}

    try:
        model_name, source, reason = resolve_model(model)
    except ValueError as e:
        # Modelo explicito fuera de la allowlist: error de resultado, no excepcion cruda.
        logger.error(f"Transcripcion abortada: {e}")
        return {"success": False, "error": str(e)}
    try:
        whisper = _load_model(model_name)
    except _ModelBusyError as e:
        logger.error(f"Transcripcion abortada: lock del modelo ocupado ({e})")
        return {"success": False, "error": str(e)}
    except ImportError:
        logger.error("Transcripcion abortada: falta el extra opcional `transcription`")
        return {"success": False, "error": _MISSING_EXTRA_ERROR}
    except Exception as e:
        # La causa ya quedo logueada en _load_model; aca solo se convierte a resultado.
        return {"success": False, "error": f"Could not load Whisper model '{model_name}': {e}"}

    start = time.monotonic()
    try:
        segments, info = whisper.transcribe(
            path, language=language, vad_filter=True, beam_size=_beam_size(model_name)
        )
        # transcribe() devuelve (generator, info) y la transcripcion REAL ocurre al
        # consumir el generator: el tope de duracion se chequea ANTES de pagarla.
        duration = float(getattr(info, "duration", 0.0) or 0.0)
        max_seconds = config.TRANSCRIPTION_MAX_SECONDS
        if max_seconds > 0 and duration > max_seconds:
            logger.error(
                f"Transcripcion abortada: audio de {duration:.0f}s supera el tope "
                f"de {max_seconds}s (WHATSAPP_TRANSCRIPTION_MAX_SECONDS)"
            )
            return {
                "success": False,
                "error": (
                    f"Audio is {duration:.0f}s long, over the {max_seconds}s limit. "
                    "Raise WHATSAPP_TRANSCRIPTION_MAX_SECONDS (0 = no limit) to "
                    "transcribe longer audios."
                ),
            }
        parts = (seg.text.strip() for seg in segments)
        text = " ".join(" ".join(p.split()) for p in parts if p)
    except Exception as e:
        # Decode imposible (no es audio), archivo corrupto, fallo interno del modelo...
        logger.error(f"Fallo la transcripcion de '{path}': {e}")
        return {"success": False, "error": f"Could not transcribe '{path}': {e}"}

    return {
        "success": True,
        "text": text,
        "language": getattr(info, "language", None),
        "language_probability": getattr(info, "language_probability", None),
        "audio_seconds": duration,
        "elapsed_seconds": round(time.monotonic() - start, 3),
        "model": model_name,
        "model_source": source,
        "model_reason": reason,
    }
