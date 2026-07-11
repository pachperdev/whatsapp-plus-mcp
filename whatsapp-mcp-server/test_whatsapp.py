"""Tests de las funciones puras de resolución de identidad de whatsapp.py.

No tocan la base ni el bridge: donde hace falta el índice de contactos se
monkeypatchea _get_contact_index.
"""

import whatsapp


class TestNormalizePhone:
    def test_quita_sufijo(self):
        assert whatsapp._normalize_phone("5491122334455@s.whatsapp.net") == "5491122334455"

    def test_quita_device_id(self):
        assert whatsapp._normalize_phone("5491122334455:12@s.whatsapp.net") == "5491122334455"

    def test_lid(self):
        assert whatsapp._normalize_phone("12345@lid") == "12345"

    def test_vacio(self):
        assert whatsapp._normalize_phone("") == ""

    def test_solo_numero(self):
        assert whatsapp._normalize_phone("5491122334455") == "5491122334455"


class TestCanonicalChatKey:
    def test_grupo_sin_cambios(self):
        assert whatsapp._canonical_chat_key("123-456@g.us") == "123-456@g.us"

    def test_vacio(self):
        assert whatsapp._canonical_chat_key("") == ""

    def test_lid_colapsa_a_numero(self, monkeypatch):
        monkeypatch.setattr(
            whatsapp, "_get_contact_index", lambda refresh=False: ({}, {"999": "5491122334455"})
        )
        assert whatsapp._canonical_chat_key("999@lid") == "5491122334455"

    def test_numero_directo(self, monkeypatch):
        monkeypatch.setattr(whatsapp, "_get_contact_index", lambda refresh=False: ({}, {}))
        assert whatsapp._canonical_chat_key("5491122334455@s.whatsapp.net") == "5491122334455"


class TestSiblingChatJids:
    def test_grupo_solo_su_jid(self):
        assert whatsapp._sibling_chat_jids("123-456@g.us") == ["123-456@g.us"]

    def test_lid_agrega_numero(self, monkeypatch):
        monkeypatch.setattr(
            whatsapp, "_get_contact_index", lambda refresh=False: ({}, {"999": "5491122334455"})
        )
        siblings = set(whatsapp._sibling_chat_jids("999@lid"))
        assert "999@lid" in siblings
        assert "5491122334455@s.whatsapp.net" in siblings

    def test_numero_agrega_lid(self, monkeypatch):
        monkeypatch.setattr(
            whatsapp, "_get_contact_index", lambda refresh=False: ({}, {"999": "5491122334455"})
        )
        siblings = set(whatsapp._sibling_chat_jids("5491122334455@s.whatsapp.net"))
        assert "5491122334455@s.whatsapp.net" in siblings
        assert "999@lid" in siblings
