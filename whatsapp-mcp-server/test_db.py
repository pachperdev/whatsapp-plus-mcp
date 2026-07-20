"""Tests de whatsapp_mcp.db: nucleo de identidad lid<->numero y lecturas SQLite.

Complementa test_whatsapp.py con los huecos de mayor valor: resolve_contact_name,
_load_contact_index + cache TTL, list_messages (validacion ISO, expansion de siblings,
dedup de contexto), search_contacts (unificacion canonica) y get_direct_chat_by_contact
(regresion del LIKE '%phone%'). Ningun test toca el store real del bridge: todos los
paths apuntan a DBs temporales (tmp_path) y el cache global se resetea entre tests.
"""

import sqlite3

import pytest

from whatsapp_mcp import db


@pytest.fixture(autouse=True)
def _cache_de_contactos_limpio(monkeypatch):
    """db.py cachea el indice de contactos en globals (_CONTACT_INDEX, TTL 5 min);
    sin este reset, el indice que carga un test contamina lo que ve el siguiente."""
    monkeypatch.setattr(db, "_CONTACT_INDEX", None)
    monkeypatch.setattr(db, "_CONTACT_INDEX_TS", 0.0)


def _make_messages_db(path: str, chats=(), messages=()) -> None:
    """Crea una messages.db con el esquema real del bridge (patron de test_whatsapp.py),
    parametrizando filas de chats y messages para cada escenario."""
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
        """
    )
    conn.executemany(
        "INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)", list(chats)
    )
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type)"
        " VALUES (?, ?, ?, ?, ?, ?, ?)",
        list(messages),
    )
    conn.commit()
    conn.close()


def _make_whatsapp_db(path: str, contacts=(), lid_map=(), *, contacts_table=True,
                      lid_table=True) -> None:
    """Crea una whatsapp.db (libreta de whatsmeow) con las tablas que lee
    _load_contact_index; los flags permiten simular esquemas sin alguna tabla."""
    conn = sqlite3.connect(path)
    if contacts_table:
        conn.execute(
            "CREATE TABLE whatsmeow_contacts (their_jid TEXT, first_name TEXT,"
            " full_name TEXT, push_name TEXT, business_name TEXT)"
        )
        conn.executemany(
            "INSERT INTO whatsmeow_contacts VALUES (?, ?, ?, ?, ?)", list(contacts)
        )
    if lid_table:
        conn.execute("CREATE TABLE whatsmeow_lid_map (lid TEXT, pn TEXT)")
        conn.executemany("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", list(lid_map))
    conn.commit()
    conn.close()


class TestResolveContactName:
    """Nucleo de identidad: WhatsApp reporta al mismo contacto como <lid>@lid o como
    <numero>@s.whatsapp.net; el nombre real vive indexado por numero de telefono."""

    def _indice(self, monkeypatch, names=None, lid_map=None):
        monkeypatch.setattr(
            db, "_get_contact_index",
            lambda refresh=False: (names or {}, lid_map or {}),
        )

    def test_lid_mapeado_resuelve_nombre(self, monkeypatch):
        """Un @lid con entrada en whatsmeow_lid_map debe cruzar lid -> numero -> nombre."""
        self._indice(monkeypatch, names={"549111": "Ana"}, lid_map={"777": "549111"})
        assert db.resolve_contact_name("777@lid") == "Ana", "debio resolver via lid_map"

    def test_lid_no_mapeado_devuelve_none(self, monkeypatch):
        """Sin mapeo lid->numero no hay forma de llegar al nombre: None, no inventar."""
        self._indice(monkeypatch, names={"549111": "Ana"})
        assert db.resolve_contact_name("777@lid") is None, "lid sin mapa no debe resolver"

    def test_lid_crudo_sin_sufijo_hace_doble_resolucion(self, monkeypatch):
        """El sender crudo a veces es un lid SIN sufijo @lid (db.py:113-114): tambien
        debe mapearse a su numero real, por consistencia con list_chats."""
        self._indice(monkeypatch, names={"549111": "Ana"}, lid_map={"777": "549111"})
        assert db.resolve_contact_name("777") == "Ana", "el lid pelado debio mapearse"

    def test_grupo_devuelve_none(self, monkeypatch):
        """Los grupos usan su propio nombre (tabla chats), nunca la libreta: el
        shortcut @g.us corta ANTES de tocar el indice (names trae una trampa)."""
        self._indice(monkeypatch, names={"123-456": "Trampa"})
        assert db.resolve_contact_name("123-456@g.us") is None, "grupo no usa la libreta"

    def test_jid_vacio_devuelve_none(self):
        """Entrada vacia no debe explotar ni consultar nada: None directo."""
        assert db.resolve_contact_name("") is None

    def test_device_id_se_normaliza(self, monkeypatch):
        """Un JID con device id (numero:12@...) debe resolver igual que sin el."""
        self._indice(monkeypatch, names={"549111": "Ana"})
        assert db.resolve_contact_name("549111:12@s.whatsapp.net") == "Ana", (
            "el sufijo :device debio normalizarse antes del lookup"
        )


class TestLoadContactIndex:
    """Carga del indice desde whatsapp.db (whatsmeow_contacts + whatsmeow_lid_map)."""

    _CONTACTOS = [
        # (their_jid, first_name, full_name, push_name, business_name)
        ("1@s.whatsapp.net", "Anita", "Ana Perez", "aniuska", "Ana SAS"),
        ("2@s.whatsapp.net", "Beto", "", "betico", ""),
        ("3@s.whatsapp.net", "", "", "carlitos", "Carlos SAS"),
        ("4@s.whatsapp.net", "", "", "", "Delta SAS"),
    ]

    def test_preferencia_de_nombre(self, tmp_path, monkeypatch):
        """Orden de preferencia: full_name > first_name > push_name > business_name
        (lo guardado en TU agenda manda sobre el push name que eligio el otro)."""
        wa = tmp_path / "whatsapp.db"
        _make_whatsapp_db(str(wa), contacts=self._CONTACTOS)
        monkeypatch.setattr(db, "WHATSAPP_DB_PATH", str(wa))
        names, _, _ = db._load_contact_index()
        assert names == {
            "1": "Ana Perez",   # full_name gana a todo
            "2": "Beto",        # first_name gana a push
            "3": "carlitos",    # push gana a business
            "4": "Delta SAS",   # business como ultimo recurso
        }, f"preferencia de nombre rota: {names}"

    def test_saved_solo_con_nombres_de_agenda(self, tmp_path, monkeypatch):
        """saved distingue contactos GUARDADOS (first/full de tu agenda) de conocidos
        capturados por push/business name al chatear; es la base de saved_only."""
        wa = tmp_path / "whatsapp.db"
        _make_whatsapp_db(str(wa), contacts=self._CONTACTOS)
        monkeypatch.setattr(db, "WHATSAPP_DB_PATH", str(wa))
        _, _, saved = db._load_contact_index()
        assert saved == {"1", "2"}, (
            f"saved debe tener solo agenda (first/full), nunca push/business: {saved}"
        )

    def test_lid_map_se_normaliza(self, tmp_path, monkeypatch):
        """whatsmeow puede guardar lid/pn como JIDs completos: el indice debe quedar
        normalizado a numeros pelados para que los lookups crucen."""
        wa = tmp_path / "whatsapp.db"
        _make_whatsapp_db(str(wa), lid_map=[("777@lid", "549111@s.whatsapp.net")])
        monkeypatch.setattr(db, "WHATSAPP_DB_PATH", str(wa))
        _, lid_to_pn, _ = db._load_contact_index()
        assert lid_to_pn == {"777": "549111"}, f"lid_map sin normalizar: {lid_to_pn}"

    def test_db_ausente_devuelve_indices_vacios(self, tmp_path, monkeypatch):
        """Sin whatsapp.db (bridge nunca logueado) debe degradar a indices vacios sin
        explotar, y el modo ro NUNCA debe crear el archivo."""
        missing = tmp_path / "no_existe.db"
        monkeypatch.setattr(db, "WHATSAPP_DB_PATH", str(missing))
        names, lid_to_pn, saved = db._load_contact_index()
        assert (names, lid_to_pn, saved) == ({}, {}, set()), "sin DB debe dar vacios"
        assert not missing.exists(), "el lector ro no debe crear la DB"

    def test_sin_tabla_contacts_degrada_silencioso(self, tmp_path, monkeypatch):
        """Una whatsapp.db sin whatsmeow_contacts (esquema viejo/parcial) no debe
        romper: names vacio pero el lid_map se carga igual."""
        wa = tmp_path / "whatsapp.db"
        _make_whatsapp_db(
            str(wa), lid_map=[("777", "549111")], contacts_table=False
        )
        monkeypatch.setattr(db, "WHATSAPP_DB_PATH", str(wa))
        names, lid_to_pn, saved = db._load_contact_index()
        assert names == {} and saved == set(), "sin tabla contacts: names/saved vacios"
        assert lid_to_pn == {"777": "549111"}, "el lid_map debio cargarse igual"

    def test_sin_tabla_lid_map_degrada_silencioso(self, tmp_path, monkeypatch):
        """Sin whatsmeow_lid_map los nombres siguen disponibles; solo se pierde el
        cruce lid->numero (degradacion parcial, no fallo total)."""
        wa = tmp_path / "whatsapp.db"
        _make_whatsapp_db(str(wa), contacts=self._CONTACTOS, lid_table=False)
        monkeypatch.setattr(db, "WHATSAPP_DB_PATH", str(wa))
        names, lid_to_pn, _ = db._load_contact_index()
        assert names, "los nombres debieron cargarse aunque falte el lid_map"
        assert lid_to_pn == {}, "sin tabla lid_map el cruce queda vacio"


class TestContactIndexCache:
    """Cache TTL del indice: el server MCP es de larga vida y no debe releer la DB
    en cada resolucion de nombre, pero refresh=True debe forzar la recarga."""

    def test_dos_llamadas_dentro_del_ttl_cargan_una_vez(self, monkeypatch):
        """Dentro del TTL (5 min) la segunda llamada debe servirse del cache."""
        cargas = []
        monkeypatch.setattr(
            db, "_load_contact_index",
            lambda: cargas.append(1) or ({"1": "Ana"}, {}, set()),
        )
        primera = db._get_contact_index()
        segunda = db._get_contact_index()
        assert len(cargas) == 1, f"dentro del TTL no debe releer la DB: {len(cargas)} cargas"
        assert primera == segunda == ({"1": "Ana"}, {}), "ambas llamadas ven el mismo indice"

    def test_refresh_true_fuerza_recarga(self, monkeypatch):
        """refresh=True (tool refresh_contacts) salta el TTL: es la via para ver un
        contacto recien agregado/renombrado sin reiniciar el server."""
        cargas = []
        monkeypatch.setattr(
            db, "_load_contact_index",
            lambda: cargas.append(1) or ({}, {}, set()),
        )
        db._get_contact_index()
        db._get_contact_index(refresh=True)
        assert len(cargas) == 2, "refresh=True debio recargar el indice"


class TestListMessages:
    """Lecturas de mensajes: validacion de fechas, hilo unificado lid+numero y
    dedup del contexto."""

    def test_after_iso_invalido_lanza_valueerror(self, tmp_path, monkeypatch):
        """Una fecha invalida debe fallar con mensaje accionable para el LLM, no con
        un WHERE silenciosamente vacio."""
        dbfile = tmp_path / "messages.db"
        _make_messages_db(str(dbfile))
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(dbfile))
        monkeypatch.setattr(db, "_get_contact_index", lambda refresh=False: ({}, {}))
        with pytest.raises(ValueError) as exc:
            db.list_messages(after="ayer")
        assert "Invalid date format for 'after': ayer" in str(exc.value), str(exc.value)
        assert "ISO-8601" in str(exc.value), "el mensaje debe sugerir el formato correcto"

    def test_before_iso_invalido_lanza_valueerror(self, tmp_path, monkeypatch):
        """Mismo contrato para 'before': el mensaje nombra el parametro que fallo."""
        dbfile = tmp_path / "messages.db"
        _make_messages_db(str(dbfile))
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(dbfile))
        monkeypatch.setattr(db, "_get_contact_index", lambda refresh=False: ({}, {}))
        with pytest.raises(ValueError) as exc:
            db.list_messages(before="31/12/2025")
        assert "Invalid date format for 'before': 31/12/2025" in str(exc.value), str(exc.value)
        assert "ISO-8601" in str(exc.value), "el mensaje debe sugerir el formato correcto"

    def _db_con_hilo_partido(self, tmp_path):
        """Un mismo contacto con mensajes bajo su @lid (entrante en vivo) y bajo
        numero@s.whatsapp.net (saliente/history): el hilo real esta partido en dos."""
        dbfile = tmp_path / "messages.db"
        _make_messages_db(
            str(dbfile),
            chats=[
                ("777@lid", "777", "2026-07-02 10:00:00"),
                ("549111@s.whatsapp.net", "Ana", "2026-07-01 10:00:00"),
            ],
            messages=[
                ("m-lid", "777@lid", "777@lid", "entrante en vivo",
                 "2026-07-02 10:00:00", 0, None),
                ("m-pn", "549111@s.whatsapp.net", "me", "saliente history",
                 "2026-07-01 10:00:00", 1, None),
            ],
        )
        return dbfile

    def test_chat_jid_numero_expande_a_siblings_lid(self, tmp_path, monkeypatch):
        """Pedir el chat por numero debe traer TAMBIEN los mensajes bajo el @lid del
        mismo contacto (sin la expansion, media conversacion desaparecia)."""
        dbfile = self._db_con_hilo_partido(tmp_path)
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(dbfile))
        monkeypatch.setattr(
            db, "_get_contact_index", lambda refresh=False: ({}, {"777": "549111"})
        )
        result = db.list_messages(chat_jid="549111@s.whatsapp.net", include_context=False)
        ids = {m["message_id"] for m in result}
        assert ids == {"m-lid", "m-pn"}, f"el hilo debio unirse (lid+numero): {ids}"

    def test_chat_jid_lid_expande_a_siblings_numero(self, tmp_path, monkeypatch):
        """La expansion tambien funciona al reves: pedir por @lid trae los mensajes
        guardados bajo numero@s.whatsapp.net."""
        dbfile = self._db_con_hilo_partido(tmp_path)
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(dbfile))
        monkeypatch.setattr(
            db, "_get_contact_index", lambda refresh=False: ({}, {"777": "549111"})
        )
        result = db.list_messages(chat_jid="777@lid", include_context=False)
        ids = {m["message_id"] for m in result}
        assert ids == {"m-lid", "m-pn"}, f"el hilo debio unirse (numero+lid): {ids}"

    def test_contexto_deduplica_por_id_y_chat(self, tmp_path, monkeypatch):
        """Dos matches consecutivos comparten contexto: cada mensaje debe salir UNA
        sola vez (dedup por (id, chat_jid)) preservando el orden de anexado."""
        chat = "111@s.whatsapp.net"
        dbfile = tmp_path / "messages.db"
        _make_messages_db(
            str(dbfile),
            chats=[(chat, "Ana", "2026-07-01 10:02:00")],
            messages=[
                ("m1", chat, chat, "primero", "2026-07-01 10:00:00", 0, None),
                ("m2", chat, chat, "hola dos", "2026-07-01 10:01:00", 0, None),
                ("m3", chat, chat, "hola tres", "2026-07-01 10:02:00", 0, None),
            ],
        )
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(dbfile))
        monkeypatch.setattr(db, "_get_contact_index", lambda refresh=False: ({}, {}))
        result = db.list_messages(
            query="hola", include_context=True, context_before=1, context_after=1
        )
        ids = [m["message_id"] for m in result]
        assert len(ids) == len(set(ids)), f"contexto duplicado: {ids}"
        assert set(ids) == {"m1", "m2", "m3"}, f"contexto incompleto: {ids}"
        # Orden actual: matches DESC (m3, m2); m3 anexa su bloque [m2, m3] y m2 solo
        # aporta m1 (m2/m3 ya vistos). El dedup preserva el orden de anexado.
        assert ids == ["m2", "m3", "m1"], f"el dedup debio preservar el orden: {ids}"


class TestSearchContacts:
    """Busqueda unificada: libreta whatsmeow + chats conocidos, canonica por numero."""

    def _montar_dbs(self, tmp_path, monkeypatch, contacts=(), lid_map=(), chats=()):
        wa = tmp_path / "whatsapp.db"
        msgs = tmp_path / "messages.db"
        _make_whatsapp_db(str(wa), contacts=contacts, lid_map=lid_map)
        _make_messages_db(str(msgs), chats=chats)
        monkeypatch.setattr(db, "WHATSAPP_DB_PATH", str(wa))
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(msgs))

    def test_lid_y_numero_colapsan_en_un_contacto(self, tmp_path, monkeypatch):
        """El mismo humano aparece en la libreta como @lid (push name) y como numero
        (nombre de agenda): la busqueda debe devolver UN solo Contact canonico."""
        self._montar_dbs(
            tmp_path, monkeypatch,
            contacts=[
                ("777@lid", "", "", "Ana", ""),
                ("549111@s.whatsapp.net", "", "Ana Perez", "", ""),
            ],
            lid_map=[("777", "549111")],
        )
        results = db.search_contacts("ana")
        assert len(results) == 1, f"lid+numero debieron colapsar en uno: {results}"
        contacto = results[0]
        assert contacto.phone_number == "549111", "el canonico es el numero, no el lid"
        assert contacto.jid == "549111@s.whatsapp.net", "el jid debe ser el canonico"
        assert contacto.name == "Ana Perez", "debe preferir el nombre de agenda"

    def test_complementa_desde_tabla_chats(self, tmp_path, monkeypatch):
        """Un contacto que solo existe en messages.db (chats) debe aparecer igual:
        la tabla chats complementa lo que la libreta no tiene."""
        self._montar_dbs(
            tmp_path, monkeypatch,
            chats=[("333@s.whatsapp.net", "Carlos", "2026-07-01 10:00:00")],
        )
        results = db.search_contacts("carlos")
        assert len(results) == 1, f"debio complementar desde chats: {results}"
        assert results[0].phone_number == "333"
        assert results[0].name == "Carlos", "sin libreta, cae al nombre de chats"

    def test_tope_de_50_resultados(self, tmp_path, monkeypatch):
        """El tope de 50 evita volcar cientos de contactos en el contexto del LLM."""
        self._montar_dbs(
            tmp_path, monkeypatch,
            chats=[
                (f"{9000 + i}@s.whatsapp.net", f"Bulk {i:03d}", "2026-07-01 10:00:00")
                for i in range(60)
            ],
        )
        results = db.search_contacts("bulk")
        assert len(results) == 50, f"el tope de 50 no se respeto: {len(results)}"

    def test_libreta_rota_devuelve_resultados_parciales(self, tmp_path, monkeypatch):
        """Si whatsapp.db falta/falla, la busqueda debe degradar a lo que haya en
        messages.db (resultados parciales), nunca lanzar excepcion."""
        msgs = tmp_path / "messages.db"
        _make_messages_db(
            str(msgs), chats=[("333@s.whatsapp.net", "Carlos", "2026-07-01 10:00:00")]
        )
        monkeypatch.setattr(db, "WHATSAPP_DB_PATH", str(tmp_path / "no_existe.db"))
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(msgs))
        results = db.search_contacts("carlos")
        assert [c.name for c in results] == ["Carlos"], (
            f"con la libreta caida debio dar los parciales de chats: {results}"
        )


class TestGetDirectChatByContact:
    """Regresion documentada: el LIKE '%phone%' historico daba falsos positivos con
    numeros parecidos y no cubria los chats @lid. Hoy se buscan candidatos exactos
    (numero + lids mapeados) y se elige el mas reciente."""

    def test_elige_el_chat_mas_reciente_entre_candidatos(self, tmp_path, monkeypatch):
        """Con el hilo partido (numero + @lid del mismo contacto) debe ganar el chat
        con actividad mas reciente, sin importar bajo que JID viva."""
        dbfile = tmp_path / "messages.db"
        _make_messages_db(
            str(dbfile),
            chats=[
                ("549111@s.whatsapp.net", "Ana", "2026-07-01 10:00:00"),
                ("777@lid", "Ana", "2026-07-05 10:00:00"),
            ],
        )
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(dbfile))
        monkeypatch.setattr(
            db, "_get_contact_index", lambda refresh=False: ({}, {"777": "549111"})
        )
        chat = db.get_direct_chat_by_contact("549111")
        assert chat is not None, "debio encontrar el chat del contacto"
        assert chat.jid == "777@lid", f"debio ganar el candidato mas reciente: {chat.jid}"

    def test_numero_parecido_pero_distinto_no_matchea(self, tmp_path, monkeypatch):
        """El bug historico: LIKE '%549111%' matcheaba '15491112222'. La busqueda
        exacta por candidate_jids no debe devolver a otra persona."""
        dbfile = tmp_path / "messages.db"
        _make_messages_db(
            str(dbfile),
            chats=[("15491112222@s.whatsapp.net", "Otra Persona", "2026-07-01 10:00:00")],
        )
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(dbfile))
        monkeypatch.setattr(db, "_get_contact_index", lambda refresh=False: ({}, {}))
        assert db.get_direct_chat_by_contact("549111") is None, (
            "un numero que solo CONTIENE al buscado no debe matchear (regresion LIKE)"
        )

    def test_acepta_jid_completo_como_entrada(self, tmp_path, monkeypatch):
        """La tool recibe a veces el JID completo: debe normalizarlo a numero y
        construir los mismos candidatos."""
        dbfile = tmp_path / "messages.db"
        _make_messages_db(
            str(dbfile),
            chats=[("549111@s.whatsapp.net", "Ana", "2026-07-01 10:00:00")],
        )
        monkeypatch.setattr(db, "MESSAGES_DB_PATH", str(dbfile))
        monkeypatch.setattr(db, "_get_contact_index", lambda refresh=False: ({}, {}))
        chat = db.get_direct_chat_by_contact("549111@s.whatsapp.net")
        assert chat is not None and chat.jid == "549111@s.whatsapp.net", (
            "el JID completo debio normalizarse y encontrar el chat"
        )
