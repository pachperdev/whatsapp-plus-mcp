"""Tests de la transcripción local de notas de voz: config, auto-selección de modelo
por hardware (con allowlist de modelos), cache singleton del modelo con lock acotado
(whatsapp_mcp.transcription) y la tool MCP transcribe_audio_message.

Ningún test requiere faster-whisper instalado (CI no instala el extra `transcription`):
el paquete se simula con módulos fake en sys.modules (monkeypatch.setitem) y su ausencia
se fuerza con setitem(None), que hace explotar el import lazy con ImportError.
"""

import gc
import importlib
import os
import sys
import threading
import time
import types
import weakref

import pytest
from mcp.types import TextContent

import whatsapp_mcp.bridge as bridge_mod
import whatsapp_mcp.config as config_mod
import whatsapp_mcp.transcription as trans_mod
from whatsapp_mcp.tools import mcp

_GIB = 1024**3

# Env vars de la feature (+ el switch de modo): el fixture de config las aísla del
# entorno real de la máquina para que los defaults sean deterministas.
_TRANS_ENV_VARS = (
    "WHATSAPP_TRANSCRIPTION_MODEL",
    "WHATSAPP_TRANSCRIPTION_MODELS_DIR",
    "WHATSAPP_TRANSCRIPTION_DEVICE",
    "WHATSAPP_TRANSCRIPTION_COMPUTE",
    "WHATSAPP_TRANSCRIPTION_MAX_SECONDS",
    "WHATSAPP_TRANSCRIPTION_BEAM",
    "WHATSAPP_PLUGIN_MODE",
)


@pytest.fixture(autouse=True)
def _cache_limpio(monkeypatch):
    """El cache del modelo es estado de módulo: cada test arranca sin modelo residente."""
    monkeypatch.setattr(trans_mod, "_cached_key", None)
    monkeypatch.setattr(trans_mod, "_cached_model", None)


@pytest.fixture
def config_reloaded():
    """Helper para recargar config con un entorno controlado; restaura al final.

    config.py evalúa los env vars al importar, así que probar defaults/overrides exige
    reload. El save/restore es manual (no monkeypatch) para garantizar que el reload
    final ocurra DESPUÉS de restaurar el entorno original.
    """
    saved = {k: os.environ.get(k) for k in _TRANS_ENV_VARS}

    def _reload(**env):
        for k in _TRANS_ENV_VARS:
            os.environ.pop(k, None)
        os.environ.update(env)
        return importlib.reload(config_mod)

    yield _reload
    for k, v in saved.items():
        if v is None:
            os.environ.pop(k, None)
        else:
            os.environ[k] = v
    importlib.reload(config_mod)


class _FakeSegment:
    def __init__(self, text):
        self.text = text


class _FakeInfo:
    def __init__(self, duration=3.5, language="es", language_probability=0.97):
        self.duration = duration
        self.language = language
        self.language_probability = language_probability


def _fake_faster_whisper(
    created, transcribe_calls, info=None, segments=None, transcribe_exc=None, consumed=None
):
    """Módulo fake de faster_whisper que registra instancias y llamadas a transcribe().

    Reproduce el contrato real: transcribe() devuelve (generator, info) y la
    transcripción ocurre al CONSUMIR el generator (la lista `consumed` lo delata).
    """
    modulo = types.ModuleType("faster_whisper")

    class _FakeWhisperModel:
        def __init__(
            self,
            model_size_or_path,
            device=None,
            compute_type=None,
            download_root=None,
            cpu_threads=None,
            **kwargs,
        ):
            created.append(
                {
                    "model": model_size_or_path,
                    "device": device,
                    "compute_type": compute_type,
                    "download_root": download_root,
                    "cpu_threads": cpu_threads,
                }
            )

        def transcribe(self, path, language=None, vad_filter=False, beam_size=5, **kwargs):
            transcribe_calls.append(
                {
                    "path": path,
                    "language": language,
                    "vad_filter": vad_filter,
                    "beam_size": beam_size,
                }
            )
            if transcribe_exc is not None:
                raise transcribe_exc
            segs = (
                segments
                if segments is not None
                else [_FakeSegment(" Hola  "), _FakeSegment(" mundo. ")]
            )

            def _gen():
                for s in segs:
                    if consumed is not None:
                        consumed.append(s.text)
                    yield s

            return _gen(), (info if info is not None else _FakeInfo())

    modulo.WhisperModel = _FakeWhisperModel
    return modulo


@pytest.fixture
def audio_file(tmp_path):
    """Un .ogg de mentira en disco (transcribe_file valida existencia antes de nada)."""
    path = tmp_path / "nota-de-voz.ogg"
    path.write_bytes(b"OggS-fake-opus")
    return str(path)


@pytest.fixture
def models_dir(tmp_path, monkeypatch):
    """Apunta el download_root a un dir temporal AÚN inexistente (debe crearse solo)."""
    destino = tmp_path / "profundo" / "models"
    monkeypatch.setattr(config_mod, "TRANSCRIPTION_MODELS_DIR", str(destino))
    return destino


class TestHardwareProfile:
    def test_fallback_conservador_si_sysconf_explota(self, monkeypatch):
        """Detección rota (contenedores raros, plataformas exóticas): nunca lanzar,
        devolver el fallback conservador (4 GiB, 2 cores)."""

        def _explota(_name):
            raise OSError("sysconf no disponible")

        monkeypatch.setattr(os, "sysconf", _explota)
        monkeypatch.setattr(os, "cpu_count", lambda: None)
        assert trans_mod._hardware_profile() == (4 * _GIB, 2)

    def test_lee_ram_y_cores_de_sysconf(self, monkeypatch):
        monkeypatch.setattr(
            os, "sysconf", lambda name: {"SC_PAGE_SIZE": 4096, "SC_PHYS_PAGES": 1024}[name]
        )
        monkeypatch.setattr(os, "cpu_count", lambda: 6)
        assert trans_mod._hardware_profile() == (4096 * 1024, 6)

    def test_valores_absurdos_caen_al_fallback(self, monkeypatch):
        """sysconf puede devolver -1 en plataformas sin el dato: RAM <= 0 no es RAM."""
        monkeypatch.setattr(
            os, "sysconf", lambda name: {"SC_PAGE_SIZE": 4096, "SC_PHYS_PAGES": -1}[name]
        )
        monkeypatch.setattr(os, "cpu_count", lambda: 6)
        ram, cpus = trans_mod._hardware_profile()
        assert ram == 4 * _GIB
        assert cpus == 6


class TestWindowsTotalRam:
    """_windows_total_ram se ejercita en POSIX inyectando un windll fake en ctypes
    (la función solo usa ctypes.windll.kernel32.GlobalMemoryStatusEx, que no existe
    fuera de Windows)."""

    @staticmethod
    def _windll_fake(total_phys=None, ret=1):
        """kernel32 fake: escribe ullTotalPhys en la struct (vía byref._obj) y
        devuelve `ret` (0 = fallo, como la API real)."""

        def _global_memory_status_ex(byref_stat):
            if total_phys is not None:
                byref_stat._obj.ullTotalPhys = total_phys
            return ret

        kernel32 = types.SimpleNamespace(GlobalMemoryStatusEx=_global_memory_status_ex)
        return types.SimpleNamespace(kernel32=kernel32)

    def test_exito_devuelve_ulltotalphys_en_bytes(self, monkeypatch):
        import ctypes

        monkeypatch.setattr(
            ctypes, "windll", self._windll_fake(total_phys=8 * _GIB), raising=False
        )
        assert trans_mod._windows_total_ram() == 8 * _GIB

    def test_fallo_de_la_api_lanza_oserror(self, monkeypatch):
        import ctypes

        monkeypatch.setattr(ctypes, "windll", self._windll_fake(ret=0), raising=False)
        with pytest.raises(OSError):
            trans_mod._windows_total_ram()

    def test_hardware_profile_cae_al_fallback_si_windows_falla(self, monkeypatch):
        """Sin os.sysconf (rama Windows) y con la API fallando, _hardware_profile
        debe caer al fallback conservador sin lanzar."""
        import ctypes

        monkeypatch.setattr(ctypes, "windll", self._windll_fake(ret=0), raising=False)
        monkeypatch.delattr(os, "sysconf")
        monkeypatch.setattr(os, "cpu_count", lambda: 4)
        ram, cpus = trans_mod._hardware_profile()
        assert ram == 4 * _GIB
        assert cpus == 4


class TestResolveModel:
    """Precedencia: parámetro explícito > env (si != "auto") > heurística por hardware."""

    def test_parametro_explicito_gana_sobre_env(self, monkeypatch):
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_MODEL", "small")
        name, source, reason = trans_mod.resolve_model("tiny")
        assert name == "tiny"
        assert source == "param"
        assert "tiny" in reason

    def test_env_gana_sobre_heuristica(self, monkeypatch):
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_MODEL", "small")
        monkeypatch.setattr(trans_mod, "_hardware_profile", lambda: (32 * _GIB, 16))
        name, source, reason = trans_mod.resolve_model(None)
        assert name == "small"
        assert source == "env"
        assert "WHATSAPP_TRANSCRIPTION_MODEL" in reason

    @pytest.mark.parametrize(
        "ram_gib,cores,esperado",
        [
            (16, 8, "large-v3-turbo"),
            (8, 4, "small"),
            (4, 2, "base"),
            (2, 1, "tiny"),
            # mucha RAM pero pocos cores: no califica para el tier grande
            (16, 4, "small"),
        ],
    )
    def test_heuristica_por_hardware(self, monkeypatch, ram_gib, cores, esperado):
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_MODEL", "auto")
        monkeypatch.setattr(trans_mod, "_hardware_profile", lambda: (ram_gib * _GIB, cores))
        name, source, reason = trans_mod.resolve_model(None)
        assert name == esperado, reason
        assert source == "auto"

    def test_reason_legible(self, monkeypatch):
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_MODEL", "auto")
        monkeypatch.setattr(trans_mod, "_hardware_profile", lambda: (16 * _GIB, 8))
        _, _, reason = trans_mod.resolve_model(None)
        assert reason == "auto: 16.0 GiB RAM, 8 cores → large-v3-turbo"

    def test_param_fuera_de_allowlist_lanza_valueerror_accionable(self):
        """Un string desconocido llegaría a faster-whisper como repo-id arbitrario de
        Hugging Face (descarga remota): fuera de la allowlist se rechaza, y el error
        debe listar los modelos válidos."""
        with pytest.raises(ValueError) as exc:
            trans_mod.resolve_model("evil-org/evil-model")
        mensaje = str(exc.value)
        assert "evil-org/evil-model" in mensaje
        for valido in ("tiny", "base", "small", "large-v3-turbo"):
            assert valido in mensaje, f"el error debe listar '{valido}': {mensaje}"

    def test_env_fuera_de_allowlist_warning_y_cae_a_heuristica(self, monkeypatch, caplog):
        """Un env var inválido no debe tumbar la tool ni descargar nada raro:
        logger.warning + heurística auto."""
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_MODEL", "evil-org/evil-model")
        monkeypatch.setattr(trans_mod, "_hardware_profile", lambda: (16 * _GIB, 8))
        with caplog.at_level("WARNING"):
            name, source, _ = trans_mod.resolve_model(None)
        assert name == "large-v3-turbo"
        assert source == "auto"
        assert any("evil-org/evil-model" in r.message for r in caplog.records), (
            "debe avisarse por log que el env var se ignoró"
        )


class TestTranscribeFile:
    def test_sin_faster_whisper_error_accionable(self, monkeypatch, audio_file, models_dir):
        """Sin el extra instalado, el resultado debe explicar CÓMO habilitarlo (y que
        el server sigue funcionando), nunca dejar escapar el ImportError crudo."""
        monkeypatch.setitem(sys.modules, "faster_whisper", None)
        result = trans_mod.transcribe_file(audio_file)
        assert result["success"] is False
        assert "--extra transcription" in result["error"], result["error"]
        assert "uv" in result["error"]

    def test_archivo_inexistente(self, tmp_path, monkeypatch):
        monkeypatch.setitem(sys.modules, "faster_whisper", None)
        fantasma = str(tmp_path / "no-existe.ogg")
        result = trans_mod.transcribe_file(fantasma)
        assert result["success"] is False
        assert fantasma in result["error"], "el error debe nombrar la ruta que falta"

    def test_happy_path(self, monkeypatch, audio_file, models_dir):
        created, calls = [], []
        fake = _fake_faster_whisper(created, calls)
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_DEVICE", "cpu")
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_COMPUTE", "int8")
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_BEAM", 0)
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_MAX_SECONDS", 900)
        monkeypatch.setattr(trans_mod, "_hardware_profile", lambda: (16 * _GIB, 10))

        result = trans_mod.transcribe_file(audio_file, model="tiny")

        assert result["success"] is True, result
        # segments concatenados con espacios normalizados (los fakes traen padding)
        assert result["text"] == "Hola mundo."
        assert result["language"] == "es"
        assert result["language_probability"] == 0.97
        assert result["audio_seconds"] == 3.5
        assert result["elapsed_seconds"] >= 0
        assert result["model"] == "tiny"
        assert result["model_source"] == "param"
        assert result["model_reason"]

        assert len(created) == 1
        assert created[0]["model"] == "tiny"
        assert created[0]["device"] == "cpu"
        assert created[0]["compute_type"] == "int8"
        # los pesos se descargan/cachean en el models dir, creado con parents si falta
        assert created[0]["download_root"] == str(models_dir)
        assert models_dir.is_dir(), "el models dir debe crearse solo"
        assert created[0]["cpu_threads"] == 8, "cpu_threads = min(8, cores - 1)"

        assert len(calls) == 1
        assert calls[0]["path"] == audio_file
        assert calls[0]["language"] is None, "sin language: autodetección de Whisper"
        assert calls[0]["vad_filter"] is True
        assert calls[0]["beam_size"] == 1, "tier tiny: greedy (beam 1)"

    def test_beam_por_tier_small(self, monkeypatch, audio_file, models_dir):
        created, calls = [], []
        fake = _fake_faster_whisper(created, calls)
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_BEAM", 0)
        result = trans_mod.transcribe_file(audio_file, model="small")
        assert result["success"] is True, result
        assert calls[0]["beam_size"] == 5, "tier small: beam 5"

    def test_beam_override_por_env(self, monkeypatch, audio_file, models_dir):
        created, calls = [], []
        fake = _fake_faster_whisper(created, calls)
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_BEAM", 3)
        result = trans_mod.transcribe_file(audio_file, model="tiny")
        assert result["success"] is True, result
        assert calls[0]["beam_size"] == 3, "WHATSAPP_TRANSCRIPTION_BEAM > 0 gana al tier"

    def test_language_explicito_se_propaga(self, monkeypatch, audio_file, models_dir):
        created, calls = [], []
        fake = _fake_faster_whisper(
            created, calls, info=_FakeInfo(language="en", language_probability=1.0)
        )
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        result = trans_mod.transcribe_file(audio_file, language="en", model="tiny")
        assert result["success"] is True, result
        assert calls[0]["language"] == "en"
        assert result["language"] == "en"

    def test_duracion_excedida_aborta_sin_transcribir(self, monkeypatch, audio_file, models_dir):
        """info.duration llega ANTES de consumir el generator: un audio sobre el límite
        debe abortar sin pagar la transcripción, con un error que mencione la env var."""
        created, calls, consumed = [], [], []
        fake = _fake_faster_whisper(created, calls, info=_FakeInfo(duration=25.0), consumed=consumed)
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_MAX_SECONDS", 10)
        result = trans_mod.transcribe_file(audio_file, model="tiny")
        assert result["success"] is False
        assert "WHATSAPP_TRANSCRIPTION_MAX_SECONDS" in result["error"], result["error"]
        assert consumed == [], "no debe consumirse ni un segment si la duración excede"

    def test_max_seconds_cero_sin_limite(self, monkeypatch, audio_file, models_dir):
        created, calls = [], []
        fake = _fake_faster_whisper(created, calls, info=_FakeInfo(duration=99999.0))
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        monkeypatch.setattr(config_mod, "TRANSCRIPTION_MAX_SECONDS", 0)
        result = trans_mod.transcribe_file(audio_file, model="tiny")
        assert result["success"] is True, result

    def test_decode_imposible_no_lanza(self, monkeypatch, audio_file, models_dir):
        """Un archivo que no es audio (PyAV no puede decodificar) debe producir un
        error de resultado, nunca una excepción cruda hacia la tool."""
        created, calls = [], []
        fake = _fake_faster_whisper(
            created, calls, transcribe_exc=RuntimeError("Invalid data found when processing input")
        )
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        result = trans_mod.transcribe_file(audio_file, model="tiny")
        assert result["success"] is False
        assert "Invalid data found" in result["error"], "debe propagar la causa original"

    def test_modelo_invalido_error_de_resultado_sin_instanciar(
        self, monkeypatch, audio_file, models_dir, caplog
    ):
        """El ValueError de la allowlist se propaga como {"success": False, ...}
        (nunca excepción cruda) y jamás llega a construirse un WhisperModel."""
        created, calls = [], []
        fake = _fake_faster_whisper(created, calls)
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        with caplog.at_level("ERROR"):
            result = trans_mod.transcribe_file(audio_file, model="evil-org/evil-model")
        assert result["success"] is False
        assert "evil-org/evil-model" in result["error"]
        assert "large-v3-turbo" in result["error"], "el error debe listar los válidos"
        assert created == [], "un modelo fuera de la allowlist jamás debe instanciarse"
        assert any(r.levelname == "ERROR" for r in caplog.records), "el fallo debe loguearse"


class TestLockDelModelo:
    """El lock del modelo se adquiere con timeout acotado: una descarga/transcripción
    colgada no debe bloquear todas las llamadas futuras para siempre."""

    def test_lock_ocupado_devuelve_busy_rapido(self, monkeypatch, audio_file, models_dir):
        created, calls = [], []
        fake = _fake_faster_whisper(created, calls)
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        monkeypatch.setattr(trans_mod, "_BUSY_TIMEOUT", 0.05)

        tomado = threading.Event()
        soltar = threading.Event()

        def _sostener_lock():
            trans_mod._model_lock.acquire()
            tomado.set()
            soltar.wait(timeout=10)
            trans_mod._model_lock.release()

        hilo = threading.Thread(target=_sostener_lock, daemon=True)
        hilo.start()
        assert tomado.wait(timeout=5), "el hilo dummy nunca tomó el lock"
        try:
            inicio = time.monotonic()
            result = trans_mod.transcribe_file(audio_file, model="tiny")
            transcurrido = time.monotonic() - inicio
        finally:
            soltar.set()
            hilo.join(timeout=5)

        assert result["success"] is False
        assert "in progress" in result["error"], result["error"]
        assert "retry" in result["error"], "el error debe ser accionable (reintentar)"
        assert transcurrido < 2, "el busy debe volver rápido, sin encolarse"
        assert created == [], "con el lock tomado no debe construirse ningún modelo"

    def test_load_acota_la_descarga_con_hf_hub_download_timeout(
        self, monkeypatch, audio_file, models_dir
    ):
        """Construir el modelo puede descargar pesos de Hugging Face: la carga debe
        dejar seteado HF_HUB_DOWNLOAD_TIMEOUT para que una conexión muerta lance en
        vez de colgar (con el lock tomado) para siempre."""
        # setenv + delenv: registra el estado original en monkeypatch para que el
        # valor que setee el código quede limpio al terminar el test.
        monkeypatch.setenv("HF_HUB_DOWNLOAD_TIMEOUT", "sentinel")
        monkeypatch.delenv("HF_HUB_DOWNLOAD_TIMEOUT")
        created, calls = [], []
        fake = _fake_faster_whisper(created, calls)
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        result = trans_mod.transcribe_file(audio_file, model="tiny")
        assert result["success"] is True, result
        assert os.environ.get("HF_HUB_DOWNLOAD_TIMEOUT") == "30"

    def test_load_no_pisa_hf_hub_download_timeout_del_usuario(
        self, monkeypatch, audio_file, models_dir
    ):
        """setdefault: si el usuario ya configuró su propio timeout, se respeta."""
        monkeypatch.setenv("HF_HUB_DOWNLOAD_TIMEOUT", "120")
        created, calls = [], []
        fake = _fake_faster_whisper(created, calls)
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        result = trans_mod.transcribe_file(audio_file, model="tiny")
        assert result["success"] is True, result
        assert os.environ.get("HF_HUB_DOWNLOAD_TIMEOUT") == "120"


class TestCacheDeModelo:
    def test_mismo_modelo_no_reinstancia(self, monkeypatch, audio_file, models_dir):
        created, calls = [], []
        fake = _fake_faster_whisper(created, calls)
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)
        r1 = trans_mod.transcribe_file(audio_file, model="tiny")
        r2 = trans_mod.transcribe_file(audio_file, model="tiny")
        assert r1["success"] and r2["success"]
        assert len(created) == 1, "el singleton debe reutilizar la instancia cargada"
        assert len(calls) == 2

    def test_cambio_de_modelo_reinstancia_y_libera_el_anterior(
        self, monkeypatch, audio_file, models_dir
    ):
        """Una sola instancia residente: pedir otro modelo crea uno nuevo y SUELTA el
        anterior (importante en máquinas con poca RAM). El weakref lo verifica."""
        created, calls = [], []
        fake = _fake_faster_whisper(created, calls)
        monkeypatch.setitem(sys.modules, "faster_whisper", fake)

        assert trans_mod.transcribe_file(audio_file, model="tiny")["success"]
        ref_anterior = weakref.ref(trans_mod._cached_model)
        assert trans_mod.transcribe_file(audio_file, model="base")["success"]

        assert [c["model"] for c in created] == ["tiny", "base"]
        gc.collect()
        assert ref_anterior() is None, "el modelo anterior debe liberarse al cambiar"


class TestConfigTranscripcion:
    """Defaults y overrides de las 6 env vars nuevas (config.py evalúa al importar)."""

    def test_defaults(self, config_reloaded):
        cfg = config_reloaded()
        assert cfg.TRANSCRIPTION_MODEL == "auto"
        assert cfg.TRANSCRIPTION_DEVICE == "cpu"
        assert cfg.TRANSCRIPTION_COMPUTE == "int8"
        assert cfg.TRANSCRIPTION_MAX_SECONDS == 900
        assert cfg.TRANSCRIPTION_BEAM == 0

    def test_models_dir_modo_repo(self, config_reloaded):
        """Modo repo: los pesos viven junto al resto del store del bridge."""
        cfg = config_reloaded()
        assert cfg.TRANSCRIPTION_MODELS_DIR == os.path.join(
            cfg.BRIDGE_SRC_DIR, "store", "models"
        )

    def test_models_dir_modo_plugin(self, config_reloaded):
        """Modo plugin: bajo ~/.whatsapp-mcp/ (sobrevive updates del plugin)."""
        cfg = config_reloaded(WHATSAPP_PLUGIN_MODE="1")
        esperado = os.path.expanduser(os.path.join("~", ".whatsapp-mcp", "models"))
        assert cfg.TRANSCRIPTION_MODELS_DIR == esperado

    def test_overrides_por_env(self, config_reloaded, tmp_path):
        cfg = config_reloaded(
            WHATSAPP_TRANSCRIPTION_MODEL="small",
            WHATSAPP_TRANSCRIPTION_MODELS_DIR=str(tmp_path / "custom-models"),
            WHATSAPP_TRANSCRIPTION_DEVICE="cuda",
            WHATSAPP_TRANSCRIPTION_COMPUTE="float16",
            WHATSAPP_TRANSCRIPTION_MAX_SECONDS="120",
            WHATSAPP_TRANSCRIPTION_BEAM="7",
        )
        assert cfg.TRANSCRIPTION_MODEL == "small"
        assert cfg.TRANSCRIPTION_MODELS_DIR == str(tmp_path / "custom-models")
        assert cfg.TRANSCRIPTION_DEVICE == "cuda"
        assert cfg.TRANSCRIPTION_COMPUTE == "float16"
        assert cfg.TRANSCRIPTION_MAX_SECONDS == 120
        assert cfg.TRANSCRIPTION_BEAM == 7

    def test_entero_invalido_cae_al_default(self, config_reloaded):
        """Un valor basura en un env var numérico no debe tumbar el server al importar."""
        cfg = config_reloaded(WHATSAPP_TRANSCRIPTION_MAX_SECONDS="banana")
        assert cfg.TRANSCRIPTION_MAX_SECONDS == 900


class TestToolTranscribeAudioMessage:
    @pytest.mark.anyio
    async def test_registrada_como_escritura_idempotente(self):
        """Existe como tool #67 con el MISMO perfil que download_media: no muta WhatsApp
        pero escribe el media descargado en disco vía bridge (readOnly sería mentirle
        al cliente MCP), y repetirla es idempotente."""
        tools = await mcp.list_tools()
        tool = next((t for t in tools if t.name == "transcribe_audio_message"), None)
        assert tool is not None, "la tool transcribe_audio_message no está registrada"
        assert tool.annotations.readOnlyHint is False
        assert tool.annotations.destructiveHint is False
        assert tool.annotations.idempotentHint is True
        assert tool.annotations.openWorldHint is True

    @pytest.mark.anyio
    async def test_download_falla_error_claro_sin_transcribir(self, monkeypatch):
        monkeypatch.setattr(bridge_mod, "download_media", lambda mid, jid: None)
        llamadas = []
        monkeypatch.setattr(
            trans_mod,
            "transcribe_file",
            lambda *a, **k: llamadas.append((a, k)) or {"success": True, "text": "x"},
        )
        result = await mcp.call_tool(
            "transcribe_audio_message",
            {"message_id": "MSG1", "chat_jid": "111@s.whatsapp.net"},
        )
        contents = result[0] if isinstance(result, tuple) else result
        textos = " ".join(c.text for c in contents if isinstance(c, TextContent))
        assert "download" in textos.lower(), f"error de descarga poco claro: {textos}"
        assert llamadas == [], "si la descarga falla no debe intentarse transcribir"

    @pytest.mark.anyio
    async def test_happy_path_devuelve_transcripcion_y_file_path(self, monkeypatch):
        monkeypatch.setattr(
            bridge_mod, "download_media", lambda mid, jid: "/tmp/store/chat/nota.ogg"
        )
        capturado = {}

        def fake_transcribe(path, language=None, model=None):
            capturado.update(path=path, language=language, model=model)
            return {
                "success": True,
                "text": "hola mundo",
                "language": "es",
                "language_probability": 0.99,
                "audio_seconds": 2.0,
                "elapsed_seconds": 0.1,
                "model": "small",
                "model_source": "param",
                "model_reason": "model requested explicitly: small",
            }

        monkeypatch.setattr(trans_mod, "transcribe_file", fake_transcribe)
        result = await mcp.call_tool(
            "transcribe_audio_message",
            {
                "message_id": "MSG1",
                "chat_jid": "111@s.whatsapp.net",
                "language": "es",
                "model": "small",
            },
        )
        contents = result[0] if isinstance(result, tuple) else result
        textos = " ".join(c.text for c in contents if isinstance(c, TextContent))
        assert capturado == {"path": "/tmp/store/chat/nota.ogg", "language": "es", "model": "small"}
        assert "hola mundo" in textos
        # igual que download_media, el resultado expone la ruta local del media
        assert "file_path" in textos and "/tmp/store/chat/nota.ogg" in textos


@pytest.fixture
def anyio_backend():
    return "asyncio"
