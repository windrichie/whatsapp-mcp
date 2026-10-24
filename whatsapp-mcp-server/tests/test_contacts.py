"""People addressed by LID (...@lid) as well as by phone number, and chat recency."""
import pytest

import whatsapp
from conftest import Store

ALICE_PHONE = "6591234567"
ALICE_LID = "111111111111111@lid"
BOB_PHONE = "447700900123"
BOB_LID = "444444444444444@lid"
STRANGER_LID = "222222222222222@lid"
GROUP = "120363025246125486@g.us"


@pytest.fixture
def people(store):
    """What the bridge writes after SyncContacts: chats keyed by LID, names from the address book."""
    store.chat(ALICE_LID, "Alice Tan", "2025-01-02 09:00:00+00:00")
    store.chat(STRANGER_LID, "6599990000", "2025-01-01 08:00:00+00:00")
    store.chat(GROUP, "Family", "2025-01-03 10:00:00+00:00")
    store.contact(f"{ALICE_PHONE}@s.whatsapp.net", ALICE_PHONE, "Alice Tan")
    store.contact(ALICE_LID, ALICE_PHONE, "Alice Tan")
    store.contact(BOB_LID, BOB_PHONE, "Bob")  # in the address book, but no direct chat
    store.contact(STRANGER_LID, "6599990000", None)
    store.message("A1", ALICE_LID, "111111111111111", "hi from alice", "2025-01-02 09:00:00+00:00")
    store.message("G1", GROUP, "444444444444444", "hi from bob", "2025-01-03 10:00:00+00:00")
    store.message("G2", GROUP, "222222222222222", "hi from a stranger", "2025-01-03 09:00:00+00:00")
    store.message("G3", GROUP, "333333333333333", "hi from nobody known", "2025-01-03 08:00:00+00:00")
    return store


def test_lid_chats_show_names_and_phone_numbers(people):
    chats = {c["jid"]: c for c in whatsapp.list_chats()}
    assert chats[ALICE_LID]["name"] == "Alice Tan"
    assert chats[ALICE_LID]["phone_number"] == ALICE_PHONE
    assert "phone_number" not in chats[GROUP]

    found = whatsapp.search_contacts("alice")
    assert found == [{"phone_number": ALICE_PHONE, "name": "Alice Tan", "jid": ALICE_LID}]
    assert [c["jid"] for c in whatsapp.search_contacts("659123")] == [ALICE_LID]
    assert [c["jid"] for c in whatsapp.list_chats(query="659123")] == [ALICE_LID]

    people.chat("0@s.whatsapp.net", "WhatsApp")  # service account, not a phone number
    assert whatsapp.search_contacts("whatsapp")[0]["phone_number"] is None


def test_group_senders_are_named_from_the_address_book(people):
    names = {m["id"]: m["sender_name"] for m in whatsapp.list_messages(chat_jid=GROUP, include_context=False)}
    assert names == {"G1": "Bob", "G2": "6599990000", "G3": "333333333333333"}


def test_unknown_group_senders_are_not_shown_as_the_group(people):
    # Older bridges stored the group's own ID as the sender of synced history
    people.message("G4", GROUP, GROUP.split("@")[0], "from history", "2025-01-03 07:00:00+00:00")
    people.message("G5", GROUP, "", "sender not in the sync", "2025-01-03 06:00:00+00:00")
    messages = {m["id"]: m for m in whatsapp.list_messages(chat_jid=GROUP, include_context=False)}
    for msg_id in ("G4", "G5"):
        assert messages[msg_id]["sender"] is None and messages[msg_id]["sender_name"] is None


def test_phone_number_lookups_find_lid_chats(people):
    chat = whatsapp.get_direct_chat_by_contact("+65 9123 4567")
    assert chat["jid"] == ALICE_LID and chat["last_message"] == "hi from alice"

    messages = whatsapp.list_messages(sender_phone_number=ALICE_PHONE, include_context=False)
    assert [m["id"] for m in messages] == ["A1"]

    bob_chats = whatsapp.get_contact_chats(f"{BOB_PHONE}@s.whatsapp.net")
    assert [c["jid"] for c in bob_chats] == [GROUP]

    last = whatsapp.get_last_interaction(f"{BOB_PHONE}@s.whatsapp.net")
    assert last["id"] == "G1"


def test_allowlisted_phone_number_covers_the_same_persons_lid(people, bridge, monkeypatch):
    monkeypatch.delenv("WHATSAPP_ALLOW_ANY_RECIPIENT")
    monkeypatch.setenv("WHATSAPP_ALLOWED_RECIPIENTS", f"+{ALICE_PHONE}, {BOB_LID}")
    for recipient in [ALICE_LID, ALICE_PHONE, BOB_PHONE]:
        ok, message = whatsapp.send_message(recipient, "hi")
        assert ok, (recipient, message)
    for recipient in [STRANGER_LID, "333333333333333@lid", "6599990000"]:
        ok, message = whatsapp.send_message(recipient, "hi")
        assert not ok and "WHATSAPP_ALLOWED_RECIPIENTS" in message
    assert [r["body"]["recipient"] for r in bridge.requests] == [ALICE_LID, ALICE_PHONE, BOB_PHONE]


def test_last_message_is_the_newest_stored_message(store):
    # The chat's last activity was a reaction, which the bridge does not store
    store.chat(ALICE_LID, "Alice Tan", "2025-01-05 12:00:00+00:00")
    store.chat(STRANGER_LID, "6599990000", "2025-01-04 12:00:00+00:00")
    store.message("A1", ALICE_LID, "111111111111111", "older", "2025-01-02 09:00:00+00:00")
    store.message("A2", ALICE_LID, "111111111111111", "newest", "2025-01-03 09:00:00+00:00")

    chats = whatsapp.list_chats()
    assert [c["jid"] for c in chats] == [ALICE_LID, STRANGER_LID]
    assert chats[0]["last_message"] == "newest"
    assert "last_message" not in chats[1]
    assert whatsapp.get_chat(ALICE_LID)["last_message"] == "newest"


def test_database_from_an_older_bridge_still_works(tmp_path, monkeypatch):
    old = Store(tmp_path / "old-store", with_contacts=False)
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", old.db_path)
    old.chat(ALICE_LID, "111111111111111")
    old.message("A1", ALICE_LID, "111111111111111", "hi")
    assert whatsapp.list_chats()[0]["last_message"] == "hi"
    assert whatsapp.search_contacts("1111")[0]["phone_number"] is None
    assert whatsapp.list_messages(sender_phone_number=ALICE_PHONE) == []
