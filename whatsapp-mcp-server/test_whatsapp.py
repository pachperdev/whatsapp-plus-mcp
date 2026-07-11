"""Tests de whatsapp_mcp.db: funciones puras de resolución de identidad y acceso a la
base (con fixtures SQLite).

No tocan el bridge ni la DB real: los paths se apuntan a temporales y donde hace
falta el índice de contactos se monkeypatchea _get_contact_index.
"""

import sqlite3

from whatsapp_mcp import db


def _make_messages_db(path: str) -> None:
    """Crea una messages.db mínima con el esquema que consultan las lecturas."""
    conn = sqlite3.connect(path)
    conn.executescript(
        """
        CREATE TABLE chats (
            jid TEXT PRIMARY KEY,
            name TEXT,
            last_message_time TEXT
        );
        CREATE TABLE messages (
            id TEXT,
            chat_jid TEXT,
            sender TEXT,
            content TEXT,
            timestamp TEXT,
            is_from_me INTEGER,
            media_type TEXT,
            filename TEXT,
            url TEXT,
            direct_path TEXT,
            media_key BLOB,
            file_sha256 BLOB,
            file_enc_sha256 BLOB,
            file_length INTEGER,
            PRIMARY KEY (id, chat_jid)
        );
        INSERT INTO chats (jid, name, last_message_time) VALUES
            ('111@s.whatsapp.net', 'Ana', '2026-07-01 10:00:00'),
            ('222@s.whatsapp.net', 'Beto', '2026-07-02 11:00:00');
        """
    )
    conn.commit()
    conn.close()


class TestNormalizePhone:
    def test_quita_sufijo(self):
        assert db._normalize_phone("5491122334455@s.whatsapp.net") == "5491122334455"

    def test_quita_device_id(self):
        assert db._normalize_phone("5491122334455:12@s.whatsapp.net") == "5491122334455"

    def test_lid(self):
        assert db._normalize_phone("12345@lid") == "12345"

    def test_vacio(self):
        assert db._normalize_phone("") == ""

    def test_solo_numero(self):
        assert db._normalize_phone("5491122334455") == "5491122334455"


class TestCanonicalChatKey:
    def test_grupo_sin_cambios(self):
        assert db._canonical_chat_key("123-456@g.us") == "123-456@g.us"

    def test_vacio(self):
        assert db._canonical_chat_key("") == ""

    def test_lid_colapsa_a_numero(self, monkeypatch):
        monkeypatch.setattr(
            db, "_get_contact_index", lambda refresh=False: ({}, {"999": "5491122334455"})
        )
        assert db._canonical_chat_key("999@lid") == "5491122334455"

    def test_numero_directo(self, monkeypatch):
        monkeypatch.setattr(db, "_get_contact_index", lambda refresh=False: ({}, {}))
        assert db._canonical_chat_key("5491122334455@s.whatsapp.net") == "5491122334455"


class TestSiblingChatJids:
    def test_grupo_solo_su_jid(self):
        assert db._sibling_chat_jids("123-456@g.us") == ["123-456@g.us"]

    def test_lid_agrega_numero(self, monkeypatch):
        monkeypatch.setattr(
            db, "_get_contact_index", lambda refresh=False: ({}, {"999": "5491122334455"})
        )
        siblings = set(db._sibling_chat_jids("999@lid"))
        assert "999@lid" in siblings
        assert "5491122334455@s.whatsapp.net" in siblings

    def test_numero_agrega_lid(self, monkeypatch):
        monkeypatch.setattr(
            db, "_get_contact_index", lambda refresh=False: ({}, {"999": "5491122334455"})
        )
        siblings = set(db._sibling_chat_jids("5491122334455@s.whatsapp.net"))
        assert "5491122334455@s.whatsapp.net" in siblings
        assert "999@lid" in siblings


class TestListChatsFixture:
    def test_lista_los_chats(self, tmp_path, monkeypatch):
        dbfile = tmp_path / "messages.db"
        _make_messages_db(str(dbfile))
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(dbfile))
        monkeypatch.setattr(db, "_get_contact_index", lambda refresh=False: ({}, {}))

        chats = db.list_chats()
        jids = {c.jid for c in chats}
        assert jids == {"111@s.whatsapp.net", "222@s.whatsapp.net"}


class TestReadOnlyNoCreaDB:
    """Regresión del fix mode=ro: un lector NUNCA debe crear la DB del bridge.

    Antes, sqlite3.connect(path) en modo rwc creaba un archivo vacío si faltaba;
    con mode=ro falla limpio y devuelve [] sin tocar el disco.
    """

    def test_list_messages_no_crea_db(self, tmp_path, monkeypatch):
        missing = tmp_path / "no_existe.db"
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(missing))
        monkeypatch.setattr(db, "_get_contact_index", lambda refresh=False: ({}, {}))

        assert db.list_messages() == []
        assert not missing.exists()

    def test_list_chats_no_crea_db(self, tmp_path, monkeypatch):
        missing = tmp_path / "no_existe.db"
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(missing))
        monkeypatch.setattr(db, "_get_contact_index", lambda refresh=False: ({}, {}))

        assert db.list_chats() == []
        assert not missing.exists()
