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
    monkeypatch.setattr(bridge_mod.shutil, "which", lambda _: None)
    ok, msg = bridge_mod.ensure_bridge_binary(str(tmp_path / "nope"))
    assert not ok
    assert "go" in msg.lower() and "go.dev" in msg, msg


def test_sin_binario_con_go_compila(tmp_path, monkeypatch):
    destino = tmp_path / "bin" / "whatsapp-bridge"
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
