"""Shared fixtures for the ghostwriter scripts.

`store` builds a throwaway bridge store with the bridge's exact schema. Every ghostwriter test
uses it; extend the helpers here rather than building databases by hand in a test file.
"""
import sqlite3
import sys
from pathlib import Path

import pytest

SCRIPTS_DIR = Path(__file__).resolve().parent.parent / "scripts"
sys.path.insert(0, str(SCRIPTS_DIR))

OWN_PHONE = "6500000000"
OWN_JID = f"{OWN_PHONE}@s.whatsapp.net"
OWN_LID = "555555555555555@lid"

# Same schema the bridge creates (whatsapp-bridge/main.go, NewMessageStore).
SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, direct_path TEXT, media_key BLOB, file_sha256 BLOB,
    file_enc_sha256 BLOB, file_length INTEGER,
    PRIMARY KEY (id, chat_jid), FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE contacts (jid TEXT PRIMARY KEY, phone TEXT, name TEXT);
CREATE INDEX contacts_phone ON contacts (phone);
CREATE INDEX messages_chat_time ON messages (chat_jid, timestamp);
"""


class Store:
    """A fake bridge store. Timestamps use the bridge's format: '2025-01-02 09:00:00+08:00'."""

    def __init__(self, root: Path):
        self.root = root
        self.db_path = root / "messages.db"
        root.mkdir(mode=0o700, parents=True, exist_ok=True)
        self._seq = 0
        with self._conn() as conn:
            conn.executescript(SCHEMA)
            conn.executemany("INSERT INTO meta VALUES (?, ?)", [("own_jid", OWN_JID), ("own_lid", OWN_LID)])

    def _conn(self):
        return sqlite3.connect(self.db_path)

    def chat(self, jid, name, last="2025-01-01 10:00:00+08:00"):
        with self._conn() as conn:
            conn.execute("INSERT OR REPLACE INTO chats VALUES (?, ?, ?)", (jid, name, last))

    def contact(self, jid, phone=None, name=None):
        with self._conn() as conn:
            conn.execute("INSERT OR REPLACE INTO contacts VALUES (?, ?, ?)", (jid, phone, name))

    def message(self, chat_jid, content, ts, from_me=False, sender=None, media_type="", msg_id=None):
        """Add a message; the chat row is created if missing. Returns the message id."""
        self._seq += 1
        msg_id = msg_id or f"M{self._seq}"
        sender = sender if sender is not None else (OWN_PHONE if from_me else chat_jid.split("@", 1)[0])
        with self._conn() as conn:
            conn.execute("INSERT OR IGNORE INTO chats VALUES (?, ?, ?)", (chat_jid, chat_jid.split("@", 1)[0], ts))
            conn.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type) VALUES (?, ?, ?, ?, ?, ?, ?)",
                (msg_id, chat_jid, sender, content, ts, from_me, media_type),
            )
        return msg_id

    def conversation(self, chat_jid, turns, start_hour=9):
        """Add alternating turns: [("customer", text) | ("me", text), ...], one minute apart."""
        for i, (who, text) in enumerate(turns):
            ts = f"2025-01-02 {start_hour:02d}:{i:02d}:00+08:00"
            self.message(chat_jid, text, ts, from_me=(who == "me"))


@pytest.fixture
def store(tmp_path, monkeypatch):
    s = Store(tmp_path / "store")
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(s.root))
    monkeypatch.setenv("GHOSTWRITER_HOME", str(tmp_path / "ghostwriter-home"))
    return s
