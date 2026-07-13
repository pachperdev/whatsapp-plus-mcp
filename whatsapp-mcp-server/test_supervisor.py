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
