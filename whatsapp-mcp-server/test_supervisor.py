"""Supervisor plug-and-play: auto-build del bridge cuando falta el binario.

En modo plugin el binario no viene en el repo; el supervisor debe compilarlo con Go
la primera vez (a BRIDGE_BIN_PATH) y dar un error accionable si no hay toolchain.
"""
import os

import whatsapp_mcp.bridge as bridge_mod


def test_binario_existente_no_compila(tmp_path, monkeypatch):
    binario = tmp_path / "whatsapp-bridge"
    binario.write_bytes(b"#!bin")
    llamado = []
    monkeypatch.setattr(bridge_mod, "_run_go_build", lambda *a: llamado.append(a) or (True, ""))
    ok, msg = bridge_mod.ensure_bridge_binary(str(binario))
    assert ok and not llamado, f"no debia compilar: {msg}"


def test_sin_binario_y_sin_go_da_error_accionable(tmp_path, monkeypatch):
    monkeypatch.setattr(bridge_mod, "_download_release_binary", lambda p: (False, "sin release"))
    monkeypatch.setattr(bridge_mod.shutil, "which", lambda _: None)
    ok, msg = bridge_mod.ensure_bridge_binary(str(tmp_path / "nope"))
    assert not ok
    assert "go" in msg.lower() and "go.dev" in msg, msg


def test_sin_binario_con_go_compila(tmp_path, monkeypatch):
    destino = tmp_path / "bin" / "whatsapp-bridge"
    monkeypatch.setattr(bridge_mod, "_download_release_binary", lambda p: (False, "sin release"))
    monkeypatch.setattr(bridge_mod.shutil, "which", lambda _: "/usr/local/bin/go")
    builds = []

    def fake_build(src_dir, out_path):
        builds.append((src_dir, out_path))
        os.makedirs(os.path.dirname(out_path), exist_ok=True)
        open(out_path, "wb").write(b"bin")
        return True, ""

    monkeypatch.setattr(bridge_mod, "_run_go_build", fake_build)
    ok, msg = bridge_mod.ensure_bridge_binary(str(destino))
    assert ok, msg
    assert builds and builds[0][1] == str(destino)
    assert os.path.isfile(destino)


# --- Cascada de binarios precompilados (GitHub Releases) ---

def test_nombre_de_asset_por_plataforma(monkeypatch):
    casos = [
        (("Darwin", "arm64"), "whatsapp-bridge-darwin-arm64"),
        (("Darwin", "x86_64"), "whatsapp-bridge-darwin-amd64"),
        (("Linux", "x86_64"), "whatsapp-bridge-linux-amd64"),
        (("Linux", "aarch64"), "whatsapp-bridge-linux-arm64"),
        (("Windows", "AMD64"), "whatsapp-bridge-windows-amd64.exe"),
    ]
    for (sistema, maquina), esperado in casos:
        monkeypatch.setattr(bridge_mod.platform, "system", lambda s=sistema: s)
        monkeypatch.setattr(bridge_mod.platform, "machine", lambda m=maquina: m)
        assert bridge_mod._platform_asset_name() == esperado, (sistema, maquina)


def test_verificacion_checksum():
    datos = b"binario-de-prueba"
    import hashlib
    h = hashlib.sha256(datos).hexdigest()
    checksums = f"{h}  whatsapp-bridge-darwin-arm64\notrohash  otro-archivo\n"
    assert bridge_mod._sha256_matches(datos, "whatsapp-bridge-darwin-arm64", checksums)
    assert not bridge_mod._sha256_matches(b"alterado", "whatsapp-bridge-darwin-arm64", checksums)
    assert not bridge_mod._sha256_matches(datos, "asset-inexistente", checksums)


def test_cascada_descarga_ok_no_compila(tmp_path, monkeypatch):
    destino = tmp_path / "bin" / "whatsapp-bridge"

    def fake_download(out_path):
        os.makedirs(os.path.dirname(out_path), exist_ok=True)
        open(out_path, "wb").write(b"bin-descargado")
        return True, "descargado"

    monkeypatch.setattr(bridge_mod, "_download_release_binary", fake_download)
    compilaciones = []
    monkeypatch.setattr(bridge_mod, "_run_go_build", lambda *a: compilaciones.append(a) or (True, ""))
    ok, msg = bridge_mod.ensure_bridge_binary(str(destino))
    assert ok and not compilaciones, msg
    assert os.path.isfile(destino)


def test_cascada_descarga_falla_cae_a_go_build(tmp_path, monkeypatch):
    destino = tmp_path / "whatsapp-bridge"
    monkeypatch.setattr(bridge_mod, "_download_release_binary", lambda p: (False, "sin release"))
    monkeypatch.setattr(bridge_mod.shutil, "which", lambda _: "/usr/local/bin/go")

    def fake_build(src_dir, out_path):
        open(out_path, "wb").write(b"bin-compilado")
        return True, ""

    monkeypatch.setattr(bridge_mod, "_run_go_build", fake_build)
    ok, msg = bridge_mod.ensure_bridge_binary(str(destino))
    assert ok, msg
    assert open(destino, "rb").read() == b"bin-compilado"


def test_cascada_sin_release_ni_go_error_combinado(tmp_path, monkeypatch):
    monkeypatch.setattr(bridge_mod, "_download_release_binary", lambda p: (False, "release: 404"))
    monkeypatch.setattr(bridge_mod.shutil, "which", lambda _: None)
    ok, msg = bridge_mod.ensure_bridge_binary(str(tmp_path / "nope"))
    assert not ok
    assert "release: 404" in msg and "go.dev" in msg, msg


# --- Watcher de rotación del QR (refresco proactivo de la Vista Previa) ---

def test_watcher_refresca_en_cada_rotacion_y_termina_al_login(monkeypatch):
    secuencia = [
        {"qr_status": "active", "code": "c1", "png_base64": "cDE="},
        {"qr_status": "active", "code": "c1", "png_base64": "cDE="},  # sin cambio
        {"qr_status": "active", "code": "c2", "png_base64": "cDI="},  # rotó
        {"qr_status": "logged_in"},                                     # escaneado
    ]
    it = iter(secuencia)
    monkeypatch.setattr(bridge_mod, "get_qr", lambda: next(it, {"qr_status": "logged_in"}))
    refrescos = []
    arranco = bridge_mod.start_qr_preview_watcher(
        lambda png: refrescos.append(png), initial_code="c1", poll_interval_s=0.01
    )
    assert arranco
    import time as _t
    for _ in range(200):
        if not bridge_mod._qr_watcher_running:
            break
        _t.sleep(0.01)
    assert not bridge_mod._qr_watcher_running, "el watcher no terminó"
    assert refrescos == [b"p2"], f"debio refrescar solo con la rotacion: {refrescos}"


def test_watcher_unico_a_la_vez(monkeypatch):
    monkeypatch.setattr(bridge_mod, "get_qr", lambda: {"qr_status": "active", "code": "x", "png_base64": "eA=="})
    a = bridge_mod.start_qr_preview_watcher(lambda p: None, initial_code="x", poll_interval_s=0.05, max_seconds=0.3)
    b = bridge_mod.start_qr_preview_watcher(lambda p: None, initial_code="x", poll_interval_s=0.05, max_seconds=0.3)
    assert a and not b, "no debe haber dos watchers simultaneos"
    import time as _t
    for _ in range(100):
        if not bridge_mod._qr_watcher_running:
            break
        _t.sleep(0.01)


def test_watcher_sobrevive_timeout_transitorio_entre_rotaciones(monkeypatch):
    # Entre la expiracion del codigo N y la emision del N+1 el bridge reporta "timeout"
    # un instante; el watcher NO debe morir ahi (solo el timeout persistente es terminal).
    secuencia = [
        {"qr_status": "active", "code": "c1", "png_base64": "cDE="},
        {"qr_status": "timeout"},                                      # gap transitorio
        {"qr_status": "active", "code": "c2", "png_base64": "cDI="},  # roto
        {"qr_status": "logged_in"},
    ]
    it = iter(secuencia)
    monkeypatch.setattr(bridge_mod, "get_qr", lambda: next(it, {"qr_status": "logged_in"}))
    refrescos = []
    assert bridge_mod.start_qr_preview_watcher(
        lambda png: refrescos.append(png), initial_code="c1", poll_interval_s=0.01
    )
    import time as _t
    for _ in range(300):
        if not bridge_mod._qr_watcher_running:
            break
        _t.sleep(0.01)
    assert refrescos == [b"p2"], f"debio refrescar c2 tras el gap: {refrescos}"


def test_watcher_termina_con_timeout_persistente(monkeypatch):
    monkeypatch.setattr(bridge_mod, "get_qr", lambda: {"qr_status": "timeout"})
    assert bridge_mod.start_qr_preview_watcher(
        lambda png: None, initial_code="x", poll_interval_s=0.01, max_seconds=30
    )
    import time as _t
    for _ in range(300):
        if not bridge_mod._qr_watcher_running:
            break
        _t.sleep(0.01)
    assert not bridge_mod._qr_watcher_running, "debio rendirse con timeout persistente"
