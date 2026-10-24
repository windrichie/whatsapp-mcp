import json
import sqlite3
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

SERVER_DIR = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(SERVER_DIR))

import whatsapp  # noqa: E402

TOKEN = "0123456789abcdef0123456789abcdef0123456789abcdef"

# Same schema the Go bridge creates.
SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, direct_path TEXT, media_key BLOB, file_sha256 BLOB,
    file_enc_sha256 BLOB, file_length INTEGER,
    PRIMARY KEY (id, chat_jid), FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);
"""


class Store:
    def __init__(self, root: Path):
        self.root = root
        self.outbox = root / "outbox"
        self.media = root / "media"
        self.db_path = root / "messages.db"
        for d in (root, self.outbox, self.media):
            d.mkdir(mode=0o700, exist_ok=True)
        (root / "api_token").write_text(TOKEN + "\n")
        with sqlite3.connect(self.db_path) as conn:
            conn.executescript(SCHEMA)

    def chat(self, jid, name, last="2025-01-01 10:00:00+00:00"):
        with sqlite3.connect(self.db_path) as conn:
            conn.execute("INSERT INTO chats VALUES (?, ?, ?)", (jid, name, last))

    def message(self, msg_id, chat_jid, sender, content, ts="2025-01-01 10:00:00+00:00", from_me=False, media_type=""):
        with sqlite3.connect(self.db_path) as conn:
            conn.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type) VALUES (?, ?, ?, ?, ?, ?, ?)",
                (msg_id, chat_jid, sender, content, ts, from_me, media_type),
            )


@pytest.fixture
def store(tmp_path, monkeypatch):
    s = Store(tmp_path / "store")
    monkeypatch.setattr(whatsapp, "STORE_DIR", s.root)
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", s.db_path)
    monkeypatch.setattr(whatsapp, "OUTBOX_DIR", s.outbox)
    monkeypatch.setattr(whatsapp, "MEDIA_DIR", s.media)
    monkeypatch.setattr(whatsapp, "TOKEN_PATH", s.root / "api_token")
    monkeypatch.delenv("WHATSAPP_MEDIA_ALLOWED_DIRS", raising=False)
    monkeypatch.delenv("WHATSAPP_BRIDGE_TOKEN", raising=False)
    monkeypatch.delenv("WHATSAPP_ALLOWED_RECIPIENTS", raising=False)
    # Most tests send to arbitrary numbers; the default (refuse) has its own test.
    monkeypatch.setenv("WHATSAPP_ALLOW_ANY_RECIPIENT", "1")
    return s


class FakeBridge:
    """Records requests the MCP server sends to the bridge."""

    def __init__(self):
        self.requests = []
        bridge = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                media = body.get("media_path")
                bridge.requests.append({
                    "path": self.path,
                    "auth": self.headers.get("Authorization"),
                    "body": body,
                    "media_existed": bool(media) and Path(media).is_file(),
                })
                ok = self.headers.get("Authorization") == f"Bearer {TOKEN}"
                payload = json.dumps({"success": ok, "message": "sent" if ok else "missing or invalid API token"}).encode()
                self.send_response(200 if ok else 401)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}/api"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()


@pytest.fixture
def bridge(monkeypatch):
    b = FakeBridge()
    monkeypatch.setattr(whatsapp, "WHATSAPP_API_BASE_URL", b.url)
    yield b
    b.close()
