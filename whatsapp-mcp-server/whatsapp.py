import ipaddress
import os
import re
import sqlite3
import tempfile
import unicodedata
from contextlib import closing
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import urlparse

import requests

import audio

# The bridge keeps its database, API token, outbox and downloads in one private directory.
# Set WHATSAPP_STORE_DIR to the same absolute path for both processes if you move it.
_DEFAULT_STORE_DIR = Path(__file__).resolve().parent.parent / "whatsapp-bridge" / "store"
STORE_DIR = Path(os.environ.get("WHATSAPP_STORE_DIR") or _DEFAULT_STORE_DIR)
MESSAGES_DB_PATH = STORE_DIR / "messages.db"
OUTBOX_DIR = STORE_DIR / "outbox"
MEDIA_DIR = STORE_DIR / "media"
TOKEN_PATH = STORE_DIR / "api_token"
WHATSAPP_API_BASE_URL = os.environ.get("WHATSAPP_BRIDGE_URL", "http://127.0.0.1:8080/api")

MAX_LIMIT = 100
MAX_CONTEXT = 20
MAX_QUERY_LENGTH = 200
MAX_MEDIA_BYTES = int(os.environ.get("WHATSAPP_MAX_MEDIA_MB") or 100) << 20
REQUEST_TIMEOUT = (5, 60)
MEDIA_REQUEST_TIMEOUT = (5, 600)

# Never route requests (and the API token) through proxies or pick up .netrc credentials.
_session = requests.Session()
_session.trust_env = False

# Zero-width (non-)joiners are needed by emoji sequences and several scripts; every other
# format character (bidi overrides, zero-width spaces, Unicode "tag" characters, ...) is removed.
_JOINERS = {"\u200c", "\u200d"}
# Invisible letters often used to make blank-looking names or hide text.
_INVISIBLE_LETTERS = set("\u034f\u115f\u1160\u3164\uffa0")


class BridgeError(Exception):
    """The WhatsApp bridge could not be reached or rejected the request."""


# ---------------------------------------------------------------------------
# Untrusted text
# ---------------------------------------------------------------------------

def clean_text(value: Optional[str]) -> Optional[str]:
    """Strip control, format and invisible characters from third-party text.

    Message bodies and names are written by other WhatsApp users. Results are returned as
    JSON fields, so newlines cannot forge extra "messages". Removing escape sequences, bidi
    overrides, tag characters and zero-width characters means a model cannot be handed
    instructions that the person reviewing the tool output cannot see. Runs of joiners or
    variation selectors (used to smuggle hidden data) are collapsed to one, which keeps
    multi-part emoji (family, rainbow flag, heart on fire) intact.
    """
    if value is None:
        return None
    out = []
    prev = ""
    for ch in str(value):
        cp = ord(ch)
        if ch in "\u2028\u2029":
            ch = "\n"
        category = unicodedata.category(ch)
        if ch not in "\n\t" and (
            category in ("Cc", "Cs")
            or (category == "Cf" and ch not in _JOINERS)
            or ch in _INVISIBLE_LETTERS
            or 0xE0100 <= cp <= 0xE01EF
        ):
            continue
        kind = "joiner" if ch in _JOINERS else "selector" if 0xFE00 <= cp <= 0xFE0F else ""
        if kind and kind == prev:
            continue
        out.append(ch)
        prev = kind
    return "".join(out)


def clean_line(value: Optional[str]) -> Optional[str]:
    """Like clean_text, for single-line fields such as chat and sender names."""
    text = clean_text(value)
    if text is None:
        return None
    return " ".join(text.split())


def _like_pattern(term: str) -> str:
    """Build a LIKE pattern that matches term literally (use with ESCAPE '\\')."""
    escaped = term.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
    return f"%{escaped}%"


def _clamp(value: Any, low: int, high: int, default: int) -> int:
    try:
        number = int(value)
    except (TypeError, ValueError):
        return default
    return max(low, min(high, number))


def _check_query(query: Optional[str]) -> Optional[str]:
    if query is None:
        return None
    if len(query) > MAX_QUERY_LENGTH:
        raise ValueError(f"Search query must be at most {MAX_QUERY_LENGTH} characters")
    return query


def _iso(timestamp: Any) -> Optional[str]:
    if timestamp is None:
        return None
    try:
        return datetime.fromisoformat(str(timestamp)).isoformat()
    except ValueError:
        return str(timestamp)


def _jid_user(jid: str) -> str:
    return jid.split("@", 1)[0]


# ---------------------------------------------------------------------------
# Database access (read-only)
# ---------------------------------------------------------------------------

def _connect() -> sqlite3.Connection:
    """Open the bridge's message database read-only; only the bridge writes to it."""
    uri = MESSAGES_DB_PATH.resolve().as_uri() + "?mode=ro"
    try:
        return sqlite3.connect(uri, uri=True, timeout=5)
    except sqlite3.Error as e:
        raise RuntimeError(f"Cannot open the WhatsApp message database at {MESSAGES_DB_PATH}. Is the bridge running? ({e})") from e


def _sender_name(cursor: sqlite3.Cursor, sender: str, cache: Dict[str, str]) -> str:
    """Resolve a sender to a chat name by exact JID match only."""
    if sender in cache:
        return cache[sender]
    candidates = [sender] if "@" in sender else [f"{sender}@s.whatsapp.net", f"{sender}@lid", sender]
    name = None
    for jid in candidates:
        row = cursor.execute("SELECT name FROM chats WHERE jid = ? LIMIT 1", (jid,)).fetchone()
        if row and row[0]:
            name = row[0]
            break
    cache[sender] = name or sender
    return cache[sender]


_MESSAGE_COLUMNS = """
    messages.timestamp, messages.sender, chats.name, messages.content,
    messages.is_from_me, messages.chat_jid, messages.id, messages.media_type
"""


def _message_dict(row: Tuple, cursor: sqlite3.Cursor, names: Dict[str, str], is_context: bool = False) -> Dict[str, Any]:
    timestamp, sender, chat_name, content, is_from_me, chat_jid, msg_id, media_type = row
    message = {
        "id": clean_line(msg_id),
        "chat_jid": chat_jid,
        "chat_name": clean_line(chat_name),
        "timestamp": _iso(timestamp),
        "from_me": bool(is_from_me),
        "sender": clean_line(sender),
        "sender_name": "Me" if is_from_me else clean_line(_sender_name(cursor, sender or "", names)),
        "content": clean_text(content),
    }
    if media_type:
        message["media_type"] = media_type
    if is_context:
        message["context"] = True
    return message


def _chat_dict(row: Tuple) -> Dict[str, Any]:
    jid, name, last_message_time, last_message, last_sender, last_is_from_me = row
    chat = {
        "jid": jid,
        "name": clean_line(name),
        "is_group": jid.endswith("@g.us"),
        "last_message_time": _iso(last_message_time),
    }
    if last_message is not None or last_sender is not None:
        chat["last_message"] = clean_text(last_message)
        chat["last_sender"] = clean_line(last_sender)
        chat["last_is_from_me"] = bool(last_is_from_me)
    return chat


def _context_rows(cursor: sqlite3.Cursor, chat_jid: str, timestamp: Any, before: int, after: int) -> Tuple[List[Tuple], List[Tuple]]:
    before_rows = cursor.execute(f"""
        SELECT {_MESSAGE_COLUMNS}
        FROM messages JOIN chats ON messages.chat_jid = chats.jid
        WHERE messages.chat_jid = ? AND messages.timestamp < ?
        ORDER BY messages.timestamp DESC
        LIMIT ?
    """, (chat_jid, timestamp, before)).fetchall()
    after_rows = cursor.execute(f"""
        SELECT {_MESSAGE_COLUMNS}
        FROM messages JOIN chats ON messages.chat_jid = chats.jid
        WHERE messages.chat_jid = ? AND messages.timestamp > ?
        ORDER BY messages.timestamp ASC
        LIMIT ?
    """, (chat_jid, timestamp, after)).fetchall()
    return list(reversed(before_rows)), after_rows


def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1
) -> List[Dict[str, Any]]:
    """Get messages matching the specified criteria with optional context."""
    limit = _clamp(limit, 1, MAX_LIMIT, 20)
    page = _clamp(page, 0, 10_000, 0)
    context_before = _clamp(context_before, 0, MAX_CONTEXT, 1)
    context_after = _clamp(context_after, 0, MAX_CONTEXT, 1)
    query = _check_query(query)

    where_clauses = []
    params: List[Any] = []
    if after:
        try:
            where_clauses.append("messages.timestamp > ?")
            params.append(datetime.fromisoformat(after).isoformat(sep=" "))
        except ValueError:
            raise ValueError(f"Invalid date format for 'after': {after}. Please use ISO-8601 format.")
    if before:
        try:
            where_clauses.append("messages.timestamp < ?")
            params.append(datetime.fromisoformat(before).isoformat(sep=" "))
        except ValueError:
            raise ValueError(f"Invalid date format for 'before': {before}. Please use ISO-8601 format.")
    if sender_phone_number:
        where_clauses.append("messages.sender = ?")
        params.append(_jid_user(sender_phone_number))
    if chat_jid:
        where_clauses.append("messages.chat_jid = ?")
        params.append(chat_jid)
    if query:
        where_clauses.append("LOWER(messages.content) LIKE LOWER(?) ESCAPE '\\'")
        params.append(_like_pattern(query))

    sql = f"SELECT {_MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid"
    if where_clauses:
        sql += " WHERE " + " AND ".join(where_clauses)
    sql += " ORDER BY messages.timestamp DESC LIMIT ? OFFSET ?"
    params.extend([limit, page * limit])

    with closing(_connect()) as conn:
        cursor = conn.cursor()
        names: Dict[str, str] = {}
        rows = cursor.execute(sql, tuple(params)).fetchall()
        if not include_context:
            return [_message_dict(row, cursor, names) for row in rows]

        results: List[Dict[str, Any]] = []
        seen = set()

        def add(row: Tuple, is_context: bool) -> None:
            key = (row[5], row[6])
            if key not in seen:
                seen.add(key)
                results.append(_message_dict(row, cursor, names, is_context))

        for row in rows:
            before_rows, after_rows = _context_rows(cursor, row[5], row[0], context_before, context_after)
            for context_row in before_rows:
                add(context_row, True)
            add(row, False)
            for context_row in after_rows:
                add(context_row, True)
        return results


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5,
    chat_jid: Optional[str] = None
) -> Dict[str, Any]:
    """Get context around a specific message."""
    before = _clamp(before, 0, MAX_CONTEXT, 5)
    after = _clamp(after, 0, MAX_CONTEXT, 5)

    sql = f"SELECT {_MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid WHERE messages.id = ?"
    params: List[Any] = [message_id]
    if chat_jid:
        sql += " AND messages.chat_jid = ?"
        params.append(chat_jid)

    with closing(_connect()) as conn:
        cursor = conn.cursor()
        rows = cursor.execute(sql, tuple(params)).fetchall()
        if not rows:
            raise ValueError(f"Message with ID {message_id} not found")
        # Message IDs are chosen by the sending client, so the same ID can exist in several chats.
        if len(rows) > 1:
            raise ValueError("Message ID exists in more than one chat; pass chat_jid as well")
        row = rows[0]
        names: Dict[str, str] = {}
        before_rows, after_rows = _context_rows(cursor, row[5], row[0], before, after)
        return {
            "message": _message_dict(row, cursor, names),
            "before": [_message_dict(r, cursor, names) for r in before_rows],
            "after": [_message_dict(r, cursor, names) for r in after_rows],
        }


_CHAT_COLUMNS_WITH_LAST = "c.jid, c.name, c.last_message_time, m.content, m.sender, m.is_from_me"
_CHAT_COLUMNS_ONLY = "c.jid, c.name, c.last_message_time, NULL, NULL, NULL"
_LAST_MESSAGE_JOIN = "LEFT JOIN messages m ON c.jid = m.chat_jid AND c.last_message_time = m.timestamp"


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Dict[str, Any]]:
    """Get chats matching the specified criteria."""
    limit = _clamp(limit, 1, MAX_LIMIT, 20)
    page = _clamp(page, 0, 10_000, 0)
    query = _check_query(query)
    if sort_by not in ("last_active", "name"):
        raise ValueError("sort_by must be 'last_active' or 'name'")

    if include_last_message:
        sql = f"SELECT {_CHAT_COLUMNS_WITH_LAST} FROM chats c {_LAST_MESSAGE_JOIN}"
    else:
        sql = f"SELECT {_CHAT_COLUMNS_ONLY} FROM chats c"
    params: List[Any] = []
    if query:
        sql += " WHERE (LOWER(c.name) LIKE LOWER(?) ESCAPE '\\' OR c.jid LIKE ? ESCAPE '\\')"
        params.extend([_like_pattern(query), _like_pattern(query)])
    sql += " ORDER BY " + ("c.last_message_time DESC" if sort_by == "last_active" else "c.name")
    sql += " LIMIT ? OFFSET ?"
    params.extend([limit, page * limit])

    with closing(_connect()) as conn:
        return [_chat_dict(row) for row in conn.execute(sql, tuple(params)).fetchall()]


def search_contacts(query: str) -> List[Dict[str, Any]]:
    """Search contacts by name or phone number."""
    query = _check_query(query) or ""
    pattern = _like_pattern(query)
    with closing(_connect()) as conn:
        rows = conn.execute("""
            SELECT DISTINCT jid, name
            FROM chats
            WHERE
                (LOWER(name) LIKE LOWER(?) ESCAPE '\\' OR LOWER(jid) LIKE LOWER(?) ESCAPE '\\')
                AND jid NOT LIKE '%@g.us'
            ORDER BY name, jid
            LIMIT 50
        """, (pattern, pattern)).fetchall()
    return [{"phone_number": _jid_user(jid), "name": clean_line(name), "jid": jid} for jid, name in rows]


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Dict[str, Any]]:
    """Get all chats involving the contact."""
    limit = _clamp(limit, 1, MAX_LIMIT, 20)
    page = _clamp(page, 0, 10_000, 0)
    with closing(_connect()) as conn:
        rows = conn.execute(f"""
            SELECT {_CHAT_COLUMNS_WITH_LAST}
            FROM chats c {_LAST_MESSAGE_JOIN}
            WHERE c.jid = ?
               OR EXISTS (SELECT 1 FROM messages s WHERE s.chat_jid = c.jid AND s.sender IN (?, ?))
            ORDER BY c.last_message_time DESC
            LIMIT ? OFFSET ?
        """, (jid, jid, _jid_user(jid), limit, page * limit)).fetchall()
    return [_chat_dict(row) for row in rows]


def get_last_interaction(jid: str) -> Optional[Dict[str, Any]]:
    """Get most recent message involving the contact."""
    with closing(_connect()) as conn:
        cursor = conn.cursor()
        row = cursor.execute(f"""
            SELECT {_MESSAGE_COLUMNS}
            FROM messages JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.sender IN (?, ?) OR chats.jid = ?
            ORDER BY messages.timestamp DESC
            LIMIT 1
        """, (jid, _jid_user(jid), jid)).fetchone()
        return _message_dict(row, cursor, {}) if row else None


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Dict[str, Any]]:
    """Get chat metadata by JID."""
    if include_last_message:
        sql = f"SELECT {_CHAT_COLUMNS_WITH_LAST} FROM chats c {_LAST_MESSAGE_JOIN} WHERE c.jid = ?"
    else:
        sql = f"SELECT {_CHAT_COLUMNS_ONLY} FROM chats c WHERE c.jid = ?"
    with closing(_connect()) as conn:
        row = conn.execute(sql, (chat_jid,)).fetchone()
    return _chat_dict(row) if row else None


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Dict[str, Any]]:
    """Get the direct chat for an exact phone number (country code, digits only)."""
    digits = re.sub(r"\D", "", sender_phone_number or "")
    if not 6 <= len(digits) <= 15:
        raise ValueError("Phone number must contain 6 to 15 digits including the country code")
    with closing(_connect()) as conn:
        row = conn.execute(
            f"SELECT {_CHAT_COLUMNS_WITH_LAST} FROM chats c {_LAST_MESSAGE_JOIN} WHERE c.jid = ? LIMIT 1",
            (f"{digits}@s.whatsapp.net",),
        ).fetchone()
    return _chat_dict(row) if row else None


# ---------------------------------------------------------------------------
# Bridge API
# ---------------------------------------------------------------------------

def _bridge_base_url() -> str:
    """Only talk to the bridge over loopback: the API token must never leave the machine."""
    parsed = urlparse(WHATSAPP_API_BASE_URL)
    host = parsed.hostname or ""
    if parsed.scheme != "http" or parsed.username or parsed.password:
        raise BridgeError("WHATSAPP_BRIDGE_URL must be a plain http:// URL on a loopback address")
    if host != "localhost":
        try:
            if not ipaddress.ip_address(host).is_loopback:
                raise ValueError
        except ValueError:
            raise BridgeError("WHATSAPP_BRIDGE_URL must point to a loopback address such as 127.0.0.1")
    return WHATSAPP_API_BASE_URL.rstrip("/")


def _api_token() -> str:
    token = os.environ.get("WHATSAPP_BRIDGE_TOKEN", "").strip()
    if token:
        return token
    try:
        token = TOKEN_PATH.read_text().strip()
    except OSError:
        token = ""
    if not token:
        raise BridgeError(f"Bridge API token not found at {TOKEN_PATH}. Start the WhatsApp bridge first, or set WHATSAPP_BRIDGE_TOKEN.")
    return token


def _post(endpoint: str, payload: Dict[str, Any], timeout: Tuple[int, int] = REQUEST_TIMEOUT) -> Dict[str, Any]:
    url = f"{_bridge_base_url()}/{endpoint}"
    try:
        response = _session.post(
            url,
            json=payload,
            headers={"Authorization": f"Bearer {_api_token()}"},
            timeout=timeout,
            allow_redirects=False,
        )
    except requests.RequestException as e:
        raise BridgeError(f"Cannot reach the WhatsApp bridge at {url}: {e.__class__.__name__}") from e
    try:
        result = response.json()
    except ValueError:
        result = None
    if not isinstance(result, dict):
        raise BridgeError(f"Unexpected response from the WhatsApp bridge (HTTP {response.status_code})")
    return result


def _bridge_call(endpoint: str, payload: Dict[str, Any], timeout: Tuple[int, int] = REQUEST_TIMEOUT) -> Tuple[bool, str, Dict[str, Any]]:
    try:
        result = _post(endpoint, payload, timeout)
    except BridgeError as e:
        return False, str(e), {}
    return bool(result.get("success", False)), str(result.get("message", "Unknown response")), result


def _allowed_media_dirs() -> List[Path]:
    dirs = [OUTBOX_DIR]
    for entry in os.environ.get("WHATSAPP_MEDIA_ALLOWED_DIRS", "").split(os.pathsep):
        entry = entry.strip()
        if not entry:
            continue
        if not os.path.isabs(entry):
            raise ValueError(f"WHATSAPP_MEDIA_ALLOWED_DIRS entries must be absolute paths (got {entry!r})")
        dirs.append(Path(entry))
    return dirs


def _matching_ancestor(path: Path, directory: Path) -> Optional[Path]:
    """Return the ancestor of path that is the same directory as `directory` (by file identity)."""
    try:
        dir_stat = os.stat(directory)
    except OSError:
        return None
    for parent in path.parents:
        try:
            if os.path.samestat(os.stat(parent), dir_stat):
                return parent
        except OSError:
            continue
    return None


def validate_media_path(media_path: str) -> Path:
    """Check that a file may be sent. The bridge enforces the same rules."""
    if not media_path:
        raise ValueError("media_path is required")
    if not os.path.isabs(media_path):
        raise ValueError("media_path must be an absolute path")
    try:
        resolved = Path(media_path).resolve(strict=True)
    except (OSError, RuntimeError):
        raise ValueError("Media file not found or not accessible")

    root = None
    for directory in _allowed_media_dirs():
        root = _matching_ancestor(resolved, directory)
        if root is not None:
            break
    if root is None:
        raise ValueError(
            f"media_path is outside the allowed media directories; copy the file into {OUTBOX_DIR} "
            "or add its directory to WHATSAPP_MEDIA_ALLOWED_DIRS"
        )
    if any(part.startswith(".") for part in resolved.relative_to(root).parts):
        raise ValueError("Refusing to send hidden files or files inside hidden directories")
    if _matching_ancestor(resolved, STORE_DIR) and not (
        _matching_ancestor(resolved, OUTBOX_DIR) or _matching_ancestor(resolved, MEDIA_DIR)
    ):
        raise ValueError("Refusing to send files from the bridge's private store directory")
    if not resolved.is_file():
        raise ValueError("media_path must be a regular file")
    if resolved.stat().st_size > MAX_MEDIA_BYTES:
        raise ValueError(f"Media file is larger than the {MAX_MEDIA_BYTES >> 20} MB limit")
    return resolved


def _normalize_recipient(recipient: str) -> str:
    """Canonical JID for allowlist comparison (mirrors the bridge's parsing)."""
    recipient = recipient.strip()
    if "@" not in recipient:
        return f"{recipient.lstrip('+')}@s.whatsapp.net"
    user, server = recipient.split("@", 1)
    return f"{user.split(':', 1)[0].split('.', 1)[0]}@{server.lower()}"


def _allowed_recipients() -> set:
    entries = os.environ.get("WHATSAPP_ALLOWED_RECIPIENTS", "").split(",")
    return {_normalize_recipient(e) for e in entries if e.strip()}


def _check_recipient(recipient: str) -> Optional[str]:
    if not recipient or not recipient.strip():
        return "Recipient must be provided"
    if len(recipient) > 64:
        return "Recipient is too long"
    allowed = _allowed_recipients()
    if allowed and _normalize_recipient(recipient) not in allowed:
        return "Recipient is not listed in WHATSAPP_ALLOWED_RECIPIENTS"
    return None


def send_message(recipient: str, message: str) -> Tuple[bool, str]:
    error = _check_recipient(recipient)
    if error:
        return False, error
    if not message:
        return False, "Message must not be empty"
    success, status, _ = _bridge_call("send", {"recipient": recipient.strip(), "message": message})
    return success, status


def send_file(recipient: str, media_path: str) -> Tuple[bool, str]:
    error = _check_recipient(recipient)
    if error:
        return False, error
    try:
        path = validate_media_path(media_path)
    except ValueError as e:
        return False, str(e)
    success, status, _ = _bridge_call(
        "send", {"recipient": recipient.strip(), "media_path": str(path)}, MEDIA_REQUEST_TIMEOUT
    )
    return success, status


def send_audio_message(recipient: str, media_path: str) -> Tuple[bool, str]:
    error = _check_recipient(recipient)
    if error:
        return False, error
    try:
        path = validate_media_path(media_path)
    except ValueError as e:
        return False, str(e)

    if path.suffix.lower() == ".ogg":
        success, status, _ = _bridge_call(
            "send", {"recipient": recipient.strip(), "media_path": str(path)}, MEDIA_REQUEST_TIMEOUT
        )
        return success, status

    # Convert into the outbox (which the bridge always accepts) and delete afterwards.
    try:
        fd, converted = tempfile.mkstemp(prefix="voice-", suffix=".ogg", dir=OUTBOX_DIR)
        os.close(fd)
    except OSError as e:
        return False, f"Cannot create a temporary file in {OUTBOX_DIR}: {e}"
    try:
        try:
            audio.convert_to_opus_ogg(str(path), converted)
        except Exception as e:
            return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {e}"
        # The bridge also checks the original file against its own media policy.
        success, status, _ = _bridge_call(
            "send",
            {"recipient": recipient.strip(), "media_path": converted, "source_path": str(path)},
            MEDIA_REQUEST_TIMEOUT,
        )
        return success, status
    finally:
        try:
            os.unlink(converted)
        except OSError:
            pass


def download_media(message_id: str, chat_jid: str) -> Tuple[Optional[str], str]:
    """Download media from a message.

    Returns:
        A tuple of (local file path or None, status message)
    """
    if not message_id or not chat_jid:
        return None, "message_id and chat_jid are required"
    success, status, result = _bridge_call(
        "download", {"message_id": message_id, "chat_jid": chat_jid}, MEDIA_REQUEST_TIMEOUT
    )
    if success and result.get("path"):
        return str(result["path"]), status
    return None, status
