"""Tests de whatsapp_mcp.bridge: contrato de errores HTTP, supervisor plug-and-play
(ensure_bridge / acquire_login_qr) y descarga verificada del binario del release.

Ningun test abre sockets, procesos ni toca el store real: _SESSION.post/get,
requests.get y los colaboradores del supervisor se monkeypatchean sobre el modulo.
Las duraciones se controlan con qr_wait_s/timeout_s chicos porque acquire_login_qr y
ensure_bridge hacen `import time as _time` local (no se parchea time).
"""

import hashlib
import json
import os

import pytest
import requests

import whatsapp_mcp.bridge as bridge_mod


class _RespuestaFalsa:
    """Doble minimo de requests.Response: status_code, text/content y json()."""

    def __init__(self, status_code=200, json_data=None, text="", content=b"", json_exc=None):
        self.status_code = status_code
        self.text = text
        self.content = content
        self._json_data = json_data
        self._json_exc = json_exc

    def json(self):
        if self._json_exc is not None:
            raise self._json_exc
        return self._json_data


class TestContratoErroresHTTP:
    """Los comentarios de bridge.py declaran los prefijos de error como contrato
    observable (razon explicita para NO migrar send_message/get_status a _bridge_post):
    no-200, RequestException y JSON invalido deben producir mensajes distinguibles."""

    @pytest.fixture(autouse=True)
    def _token_falso(self, monkeypatch):
        """Evita leer el .bridge_token del store real del bridge."""
        monkeypatch.setattr(bridge_mod, "_bridge_token", lambda: "token-de-test")

    # --- send_message ---

    def test_send_message_no_200_prefija_error_http(self, monkeypatch):
        """Una respuesta no-200 debe reportar 'Error: HTTP <code> - <body>' para que
        el LLM distinga un fallo del bridge de un rechazo de WhatsApp."""
        monkeypatch.setattr(
            bridge_mod._SESSION, "post",
            lambda url, json=None, headers=None, timeout=None: _RespuestaFalsa(
                status_code=500, text="panico interno"
            ),
        )
        ok, msg = bridge_mod.send_message("549111", "hola")
        assert not ok
        assert msg == "Error: HTTP 500 - panico interno", f"contrato de error roto: {msg}"

    def test_send_message_request_exception_prefija_request_error(self, monkeypatch):
        """Bridge caido (connection refused y similares): prefijo 'Request error:'."""
        def _post_explota(url, **kw):
            raise requests.ConnectionError("bridge caido")

        monkeypatch.setattr(bridge_mod._SESSION, "post", _post_explota)
        ok, msg = bridge_mod.send_message("549111", "hola")
        assert not ok
        assert msg.startswith("Request error:"), f"prefijo equivocado: {msg}"
        assert "bridge caido" in msg, "debe propagar la causa original"

    def test_send_message_200_json_invalido_prefija_error_parsing(self, monkeypatch):
        """200 con body no-JSON debe caer en la rama 'Error parsing response' e incluir
        el body para diagnostico (se simula con json.JSONDecodeError puro, que es lo
        que el except de bridge.py declara capturar)."""
        # La excepcion se construye FUERA de la lambda: su parametro `json=` (el
        # payload del POST) sombrea al modulo json dentro del cuerpo.
        resp = _RespuestaFalsa(
            status_code=200, text="<html>proxy</html>",
            json_exc=json.JSONDecodeError("Expecting value", "<html>proxy</html>", 0),
        )
        monkeypatch.setattr(
            bridge_mod._SESSION, "post",
            lambda url, json=None, headers=None, timeout=None: resp,
        )
        ok, msg = bridge_mod.send_message("549111", "hola")
        assert not ok
        assert msg.startswith("Error parsing response"), f"prefijo equivocado: {msg}"
        assert "<html>proxy</html>" in msg, "debe incluir el body recibido"

    def test_send_message_json_invalido_de_requests_real_reporta_error_parsing(self, monkeypatch):
        """Contrato correcto con una Response REAL: desde requests>=2.27, response.json()
        lanza requests.exceptions.JSONDecodeError, que hereda de RequestException Y de
        json.JSONDecodeError. Un 200 con body no-JSON es un problema de PARSING (proxy,
        HTML de error, body truncado), no de red: el prefijo observable debe ser
        'Error parsing response' e incluir el body para diagnostico."""
        monkeypatch.setattr(
            bridge_mod._SESSION, "post",
            lambda url, json=None, headers=None, timeout=None: _RespuestaFalsa(
                status_code=200, text="no-json",
                json_exc=requests.exceptions.JSONDecodeError("Expecting value", "no-json", 0),
            ),
        )
        ok, msg = bridge_mod.send_message("549111", "hola")
        assert not ok
        assert msg.startswith("Error parsing response"), (
            f"un 200 con body no-JSON es error de parsing, no de red: {msg}"
        )
        assert "no-json" in msg, "debe incluir el body recibido"

    def test_send_message_sin_destinatario_no_toca_la_red(self, monkeypatch):
        """La validacion de input corta ANTES de cualquier request."""
        def _no_debe_llamarse(*a, **kw):
            raise AssertionError("no debe haber red sin destinatario")

        monkeypatch.setattr(bridge_mod._SESSION, "post", _no_debe_llamarse)
        ok, msg = bridge_mod.send_message("", "hola")
        assert not ok
        assert msg == "Recipient must be provided", f"validacion de input rota: {msg}"

    # --- get_status ---

    def test_get_status_no_200_reporta_http_y_codigo(self, monkeypatch):
        """get_status es la tool de diagnostico: en no-200 su message es
        'HTTP <code> - <body>' (sin prefijo 'Error: ', a diferencia de send_message)."""
        monkeypatch.setattr(
            bridge_mod._SESSION, "get",
            lambda url, headers=None, timeout=None: _RespuestaFalsa(
                status_code=503, text="warming up"
            ),
        )
        data = bridge_mod.get_status()
        assert data["success"] is False
        assert data["message"] == "HTTP 503 - warming up", f"contrato roto: {data}"

    def test_get_status_request_exception_prefija_request_error(self, monkeypatch):
        """Bridge inalcanzable: dict con success False y prefijo 'Request error:'."""
        def _get_explota(url, **kw):
            raise requests.ConnectionError("refused")

        monkeypatch.setattr(bridge_mod._SESSION, "get", _get_explota)
        data = bridge_mod.get_status()
        assert data["success"] is False
        assert data["message"].startswith("Request error:"), f"prefijo equivocado: {data}"

    def test_get_status_200_json_invalido_reporta_error_parsing(self, monkeypatch):
        """200 con body corrupto: 'Error parsing response' (rama json.JSONDecodeError)."""
        monkeypatch.setattr(
            bridge_mod._SESSION, "get",
            lambda url, headers=None, timeout=None: _RespuestaFalsa(
                status_code=200, text="basura",
                json_exc=json.JSONDecodeError("Expecting value", "basura", 0),
            ),
        )
        data = bridge_mod.get_status()
        assert data["success"] is False
        assert data["message"] == "Error parsing response", f"contrato roto: {data}"

    def test_get_status_json_invalido_de_requests_real_reporta_error_parsing(self, monkeypatch):
        """Mismo contrato con una Response REAL (requests>=2.27): su JSONDecodeError
        hereda de RequestException, pero un 200 con body corrupto debe reportarse como
        'Error parsing response', no como 'Request error:' (get_status es la tool de
        diagnostico y el prefijo distingue bridge caido de bridge respondiendo basura)."""
        monkeypatch.setattr(
            bridge_mod._SESSION, "get",
            lambda url, headers=None, timeout=None: _RespuestaFalsa(
                status_code=200, text="basura",
                json_exc=requests.exceptions.JSONDecodeError("Expecting value", "basura", 0),
            ),
        )
        data = bridge_mod.get_status()
        assert data["success"] is False
        assert data["message"] == "Error parsing response", (
            f"un 200 con body no-JSON es error de parsing, no de red: {data}"
        )


class TestEnsureBridge:
    """Supervisor: adoptar un bridge sano, o lanzar uno y esperar a que sirva."""

    def test_adopta_bridge_sano_sin_spawn(self, monkeypatch):
        """Invariante anti-duplicacion: dos bridges = dos conexiones WhatsApp = riesgo
        de ban. Si /api/status responde, se adopta ese bridge y spawn_bridge NO corre."""
        spawns = []
        monkeypatch.setattr(bridge_mod, "spawn_bridge", lambda: spawns.append(1) or (True, ""))
        monkeypatch.setattr(
            bridge_mod, "get_status", lambda: {"success": True, "connected": True}
        )
        res = bridge_mod.ensure_bridge(timeout_s=0.5)
        assert res["ok"] is True and res["spawned"] is False, res
        assert not spawns, "spawn_bridge no debe llamarse con un bridge sano corriendo"

    def test_sin_bridge_spawnea_y_pollea_hasta_sano(self, monkeypatch):
        """Sin bridge respondiendo: spawn + poll de /api/status hasta que sirva."""
        estados = iter([{"success": False}, {"success": True}, {"success": True}])
        monkeypatch.setattr(bridge_mod, "get_status", lambda: next(estados))
        spawns = []
        monkeypatch.setattr(bridge_mod, "spawn_bridge", lambda: spawns.append(1) or (True, ""))
        res = bridge_mod.ensure_bridge(timeout_s=5.0)
        assert res["ok"] is True and res["spawned"] is True, res
        assert spawns == [1], "debio spawnear exactamente una vez"

    def test_spawn_ok_pero_nunca_sano_da_error_accionable(self, monkeypatch):
        """Si el binario arranca pero la API nunca responde, el mensaje debe apuntar
        al bridge.log (unico lugar con la causa real: puerto tomado, store roto...)."""
        monkeypatch.setattr(bridge_mod, "get_status", lambda: {"success": False})
        monkeypatch.setattr(bridge_mod, "spawn_bridge", lambda: (True, "bridge spawned"))
        res = bridge_mod.ensure_bridge(timeout_s=0.6)
        assert res["ok"] is False and res["spawned"] is True, res
        assert "no respondio" in res["message"], f"mensaje no accionable: {res['message']}"
        assert "bridge.log" in res["message"], "debe apuntar al log del bridge"

    def test_spawn_falla_propaga_el_motivo(self, monkeypatch):
        """Si ni siquiera se pudo lanzar el binario, el motivo del spawn se propaga
        tal cual (es el error accionable de ensure_bridge_binary)."""
        monkeypatch.setattr(bridge_mod, "get_status", lambda: {"success": False})
        monkeypatch.setattr(
            bridge_mod, "spawn_bridge", lambda: (False, "sin binario del bridge")
        )
        res = bridge_mod.ensure_bridge(timeout_s=0.5)
        assert res["ok"] is False and res["spawned"] is False, res
        assert res["message"] == "sin binario del bridge"


class TestAcquireLoginQr:
    """Maquina de estados del login autogestionado: sesion valida, zombie, modo QR,
    canal agotado y presupuesto de reciclajes."""

    def test_sesion_valida_no_genera_qr_ni_recicla(self, monkeypatch):
        """logged_in && connected: exito inmediato SIN tocar /api/qr ni reciclar
        (generar un QR con sesion valida desvincularia al usuario)."""
        monkeypatch.setattr(
            bridge_mod, "ensure_bridge",
            lambda: {
                "ok": True, "spawned": False,
                "status": {"success": True, "logged_in": True, "connected": True},
            },
        )
        tocados = []
        monkeypatch.setattr(bridge_mod, "get_qr", lambda: tocados.append("get_qr") or {})
        monkeypatch.setattr(
            bridge_mod, "shutdown_bridge", lambda: tocados.append("shutdown") or {}
        )
        monkeypatch.setattr(
            bridge_mod, "get_status",
            lambda: {"success": True, "logged_in": True, "connected": True},
        )
        res = bridge_mod.acquire_login_qr(max_recycles=0, qr_wait_s=0.2)
        assert res["ok"] is True and res["logged_in"] is True, res
        assert tocados == [], f"sesion valida: no debe generar QR ni reciclar: {tocados}"

    def test_zombie_recicla_y_entrega_qr(self, monkeypatch):
        """logged_in && needs_qr = sesion zombie (device invalidado remoto): ese
        proceso jamas emitira QR, hay que reciclarlo (shutdown + respawn) y el bridge
        nuevo entra en modo QR."""
        estados = iter([
            {"ok": True, "spawned": False,
             "status": {"success": True, "logged_in": True, "needs_qr": True}},
            {"ok": True, "spawned": True,
             "status": {"success": True, "logged_in": False}},
        ])
        monkeypatch.setattr(bridge_mod, "ensure_bridge", lambda: next(estados))
        monkeypatch.setattr(bridge_mod, "get_status", lambda: {"success": False})
        apagados = []
        monkeypatch.setattr(
            bridge_mod, "shutdown_bridge", lambda: apagados.append(1) or {"success": True}
        )
        monkeypatch.setattr(
            bridge_mod, "get_qr",
            lambda: {"qr_status": "active", "code": "c1", "png_base64": "cDE="},
        )
        res = bridge_mod.acquire_login_qr(max_recycles=1, qr_wait_s=0.2, recycle_wait_s=0)
        assert res["ok"] is True and res["logged_in"] is False, res
        assert res["qr"]["png_base64"] == "cDE=", "debio entregar el QR del bridge nuevo"
        assert apagados == [1], "el zombie debio reciclarse exactamente una vez"

    def test_modo_qr_espera_active_y_devuelve_png(self, monkeypatch):
        """Bridge sin sesion (!logged_in): se espera el estado 'active' del canal QR
        y se devuelve el codigo + png para el visor."""
        monkeypatch.setattr(
            bridge_mod, "ensure_bridge",
            lambda: {
                "ok": True, "spawned": True,
                "status": {"success": True, "logged_in": False},
            },
        )
        monkeypatch.setattr(bridge_mod, "get_status", lambda: {"success": False})
        monkeypatch.setattr(bridge_mod, "shutdown_bridge", lambda: {"success": True})
        monkeypatch.setattr(
            bridge_mod, "get_qr",
            lambda: {"qr_status": "active", "code": "c1", "png_base64": "cDE="},
        )
        res = bridge_mod.acquire_login_qr(max_recycles=0, qr_wait_s=0.2)
        assert res["ok"] is True and res["logged_in"] is False, res
        assert res["qr"]["code"] == "c1" and res["qr"]["png_base64"] == "cDE=", res

    def test_qr_timeout_recicla_para_canal_fresco(self, monkeypatch):
        """Canal QR agotado ('timeout'): se recicla el bridge para abrir un canal
        fresco y se entrega el QR del segundo intento."""
        monkeypatch.setattr(
            bridge_mod, "ensure_bridge",
            lambda: {
                "ok": True, "spawned": True,
                "status": {"success": True, "logged_in": False},
            },
        )
        monkeypatch.setattr(bridge_mod, "get_status", lambda: {"success": False})
        apagados = []
        monkeypatch.setattr(
            bridge_mod, "shutdown_bridge", lambda: apagados.append(1) or {"success": True}
        )
        qrs = iter([{"qr_status": "timeout"}])
        monkeypatch.setattr(
            bridge_mod, "get_qr",
            lambda: next(qrs, {"qr_status": "active", "code": "c2", "png_base64": "cDI="}),
        )
        res = bridge_mod.acquire_login_qr(max_recycles=1, qr_wait_s=0.2, recycle_wait_s=0)
        assert res["ok"] is True and res["qr"]["code"] == "c2", res
        assert apagados == [1], "el canal agotado debio forzar exactamente un reciclaje"

    def test_max_recycles_agotados_devuelve_ok_false(self, monkeypatch):
        """Si tras agotar el presupuesto de reciclajes sigue sin haber QR, el
        resultado es ok=False con mensaje accionable (nunca un loop infinito)."""
        monkeypatch.setattr(
            bridge_mod, "ensure_bridge",
            lambda: {
                "ok": True, "spawned": True,
                "status": {"success": True, "logged_in": False},
            },
        )
        monkeypatch.setattr(bridge_mod, "get_status", lambda: {"success": False})
        apagados = []
        monkeypatch.setattr(
            bridge_mod, "shutdown_bridge", lambda: apagados.append(1) or {"success": True}
        )
        monkeypatch.setattr(bridge_mod, "get_qr", lambda: {"qr_status": "timeout"})
        res = bridge_mod.acquire_login_qr(max_recycles=0, qr_wait_s=0.2, recycle_wait_s=0)
        assert res["ok"] is False, res
        assert "QR" in res["message"], f"mensaje no accionable: {res['message']}"
        assert apagados == [1], "max_recycles=0 permite exactamente un reciclaje final"

    def test_bridge_no_disponible_falla_rapido(self, monkeypatch):
        """Si ensure_bridge no consigue un bridge, acquire falla de inmediato con el
        motivo original (sin esperar QRs que jamas llegaran)."""
        monkeypatch.setattr(
            bridge_mod, "ensure_bridge", lambda: {"ok": False, "message": "sin binario"}
        )
        monkeypatch.setattr(bridge_mod, "get_status", lambda: {"success": False})
        res = bridge_mod.acquire_login_qr(max_recycles=0, qr_wait_s=0.2)
        assert res["ok"] is False and res["message"] == "sin binario", res


class TestDownloadReleaseBinary:
    """Descarga verificada del binario precompilado: SHA256 obligatorio e instalacion
    atomica (tmp + os.replace) para jamas dejar un binario corrupto en bin_path."""

    def _montar_red(self, monkeypatch, checksums_txt, binario):
        """Simula la API de GitHub Releases completa (release + checksums + asset)."""
        asset = bridge_mod._platform_asset_name()
        respuestas = {
            "https://api.fake/sums": _RespuestaFalsa(200, text=checksums_txt),
            "https://api.fake/bin": _RespuestaFalsa(200, content=binario),
        }

        def fake_get(url, headers=None, timeout=None):
            if url.endswith("/releases/latest"):
                return _RespuestaFalsa(200, json_data={"assets": [
                    {"name": asset, "url": "https://api.fake/bin"},
                    {"name": "checksums.txt", "url": "https://api.fake/sums"},
                ]})
            if url in respuestas:
                return respuestas[url]
            raise AssertionError(f"URL inesperada: {url}")

        monkeypatch.setattr(bridge_mod, "_gh_token", lambda: "")  # sin subprocess `gh`
        monkeypatch.setattr(bridge_mod.requests, "get", fake_get)
        return asset

    def test_checksum_no_coincide_descarta_la_descarga(self, tmp_path, monkeypatch):
        """Un binario cuyo SHA256 no coincide con checksums.txt (descarga corrupta o
        manipulada) NUNCA debe instalarse: ni el destino ni el .tmp deben existir."""
        destino = str(tmp_path / "bin" / "whatsapp-bridge")
        asset = self._montar_red(
            monkeypatch,
            checksums_txt=f"{'0' * 64}  {bridge_mod._platform_asset_name()}\n",
            binario=b"payload-que-no-matchea",
        )
        ok, msg = bridge_mod._download_release_binary(destino)
        assert not ok, "checksum invalido debio abortar la instalacion"
        assert "checksum" in msg.lower() and asset in msg, f"motivo poco claro: {msg}"
        assert not os.path.exists(destino), "no debe instalar un binario sin verificar"
        assert not os.path.exists(destino + ".tmp"), "ni dejar temporales huerfanos"

    def test_instalacion_atomica_via_tmp_y_replace(self, tmp_path, monkeypatch):
        """La instalacion escribe a <bin>.tmp y publica con os.replace (atomico):
        un fallo a mitad de descarga jamas deja un binario corrupto en bin_path."""
        binario = b"binario-verificado"
        digest = hashlib.sha256(binario).hexdigest()
        asset = self._montar_red(
            monkeypatch,
            checksums_txt=f"{digest}  {bridge_mod._platform_asset_name()}\n",
            binario=binario,
        )
        destino = str(tmp_path / "bin" / "whatsapp-bridge")
        real_replace = os.replace
        reemplazos = []

        def espia_replace(src, dst):
            reemplazos.append((src, dst))
            return real_replace(src, dst)

        monkeypatch.setattr(bridge_mod.os, "replace", espia_replace)
        ok, msg = bridge_mod._download_release_binary(destino)
        assert ok, f"la descarga verificada debio instalarse: {msg}"
        assert asset in msg and "SHA256" in msg, f"mensaje sin trazabilidad: {msg}"
        assert reemplazos == [(destino + ".tmp", destino)], (
            f"la publicacion debe ser tmp -> os.replace: {reemplazos}"
        )
        with open(destino, "rb") as f:
            assert f.read() == binario, "el contenido instalado debe ser el verificado"
        assert os.stat(destino).st_mode & 0o111, "el binario debe quedar ejecutable"
        assert not os.path.exists(destino + ".tmp"), "el .tmp no debe sobrevivir"

    def test_release_http_error_no_instala(self, tmp_path, monkeypatch):
        """API de releases caida o release inexistente: fallo limpio con el codigo
        HTTP para que la cascada caiga al go build local."""
        monkeypatch.setattr(bridge_mod, "_gh_token", lambda: "")
        monkeypatch.setattr(
            bridge_mod.requests, "get",
            lambda url, headers=None, timeout=None: _RespuestaFalsa(404, text="not found"),
        )
        destino = str(tmp_path / "whatsapp-bridge")
        ok, msg = bridge_mod._download_release_binary(destino)
        assert not ok and msg == "release: HTTP 404", f"motivo poco claro: {msg}"
        assert not os.path.exists(destino)

    def test_release_sin_assets_esperados_no_instala(self, tmp_path, monkeypatch):
        """Release sin el asset de la plataforma o sin checksums.txt: sin verificacion
        posible, no se instala nada."""
        monkeypatch.setattr(bridge_mod, "_gh_token", lambda: "")
        monkeypatch.setattr(
            bridge_mod.requests, "get",
            lambda url, headers=None, timeout=None: _RespuestaFalsa(
                200, json_data={"assets": []}
            ),
        )
        destino = str(tmp_path / "whatsapp-bridge")
        ok, msg = bridge_mod._download_release_binary(destino)
        assert not ok and "release sin asset" in msg, f"motivo poco claro: {msg}"
        assert not os.path.exists(destino)
