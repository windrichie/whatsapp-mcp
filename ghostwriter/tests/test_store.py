import sqlite3
import stat

import pytest

import gw_store
from conftest import OWN_JID, OWN_LID

ALICE = "111111111111111@lid"
BOB = "6591234567@s.whatsapp.net"
GROUP = "120363025246125486@g.us"


def test_owner_replies_pair_customer_messages_with_her_answers(store):
    store.conversation(ALICE, [("customer", "hi, open sunday?"), ("me", "Hi kak! Closed on Sunday 🙏"), ("me", "Mon-Sat 10-6")])
    store.conversation(BOB, [("me", "promo starts today"), ("customer", "price?"), ("me", "IDR 250k")], start_hour=11)
    store.conversation(GROUP, [("customer", "group q"), ("me", "group answer")], start_hour=12)
    store.conversation(OWN_JID, [("customer", "draft header"), ("me", "draft body")], start_hour=13)

    with gw_store.connect() as conn:
        pairs = gw_store.owner_replies(conn)
        mine = gw_store.owner_messages(conn)
    assert [(p.chat_jid, p.customer_text, p.reply_text) for p in pairs] == [
        (BOB, "price?", "IDR 250k"),
        (ALICE, "hi, open sunday?", "Hi kak! Closed on Sunday 🙏"),
    ]
    assert [m.content for m in mine] == ["IDR 250k", "promo starts today", "Mon-Sat 10-6", "Hi kak! Closed on Sunday 🙏"]


def test_chat_messages_and_label(store):
    store.chat(ALICE, "Alice Tan")
    store.contact(ALICE, "6591234567", "Alice Tan")
    store.conversation(ALICE, [("customer", f"m{i}") for i in range(5)])
    with gw_store.connect() as conn:
        assert [m.content for m in gw_store.chat_messages(conn, ALICE, limit=3)] == ["m2", "m3", "m4"]
        assert gw_store.chat_label(conn, ALICE) == ("Alice Tan", "6591234567")
        assert gw_store.chat_label(conn, "999@lid") == ("999", None)
        assert gw_store.own_jids(conn) == {OWN_JID, OWN_LID}


def test_untrusted_text_is_cleaned(store):
    store.message(ALICE, "hello‮​\x1b[31m‍👍", "2025-01-02 09:00:00+08:00")
    with gw_store.connect() as conn:
        assert gw_store.chat_messages(conn, ALICE)[0].content == "hello[31m‍👍"


def test_database_is_read_only_and_data_dir_is_private(store):
    with gw_store.connect() as conn:
        with pytest.raises(sqlite3.OperationalError):
            conn.execute("DELETE FROM messages")
    home = gw_store.data_dir()
    assert stat.S_IMODE(home.stat().st_mode) == 0o700


def test_chat_jid_validation():
    assert gw_store.validate_chat_jid(ALICE) == ALICE
    for bad in [GROUP, "status@broadcast", "../x@lid", "6591234567", "", "1@lid; rm -rf /"]:
        with pytest.raises(ValueError):
            gw_store.validate_chat_jid(bad)


def test_missing_store_is_a_clear_error(tmp_path, monkeypatch):
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(tmp_path / "nope"))
    with pytest.raises(gw_store.StoreError, match="No message database"):
        gw_store.connect()
    monkeypatch.delenv("WHATSAPP_STORE_DIR")
    with pytest.raises(gw_store.StoreError, match="WHATSAPP_STORE_DIR"):
        gw_store.store_dir()
