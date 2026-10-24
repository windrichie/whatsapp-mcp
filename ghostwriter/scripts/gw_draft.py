"""Draft a WhatsApp reply for one customer chat, in the owner's voice.

The bridge runs `bin/draft <chat_jid>` after a customer stops typing. This script reads the chat
and the owner's past replies from the bridge's database (read-only), asks `claude -p` for a draft
and prints one JSON object, {"reply": ..., "note": ...}. It never sends anything: the bridge sends
the draft to the owner's own chat. Message text comes from other people, so it is only ever data:
the model gets no tools, and every untrusted string is cleaned before it goes into the prompt.

Exit 0: stdout is exactly one JSON object ("reply" is empty when no reply is needed).
Exit 1: stdout is empty; stderr has one line that names the failure, never message text.

Standard library only, and no syntax newer than Python 3.9 (the bridge may run an older python3).
"""
import argparse
import json
import math
import os
import re
import shutil
import sqlite3
import subprocess
import sys
from contextlib import closing
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, NamedTuple, Optional, Sequence

# The wrapper runs `python3 -I`, which leaves this script's directory off sys.path.
sys.path.insert(0, str(Path(__file__).resolve().parent))
import gw_store  # noqa: E402

SKILL_FILE = Path(__file__).resolve().parent.parent / "skills" / "draft-reply" / "SKILL.md"

DEFAULT_MODEL = "sonnet"
DEFAULT_TIMEOUT = 150.0  # seconds; the bridge kills the command after 180
MAX_TIMEOUT = 170.0

MESSAGE_LIMIT = 30  # messages of the chat shown to the model
MESSAGE_CHARS = 1000
EXAMPLE_POOL = 3000  # past reply pairs searched for examples
EXAMPLES_BEST = 6
EXAMPLES_MIN = 4
EXAMPLE_CHARS = 500
FILE_CHARS = 12000
NAME_CHARS = 80
REPLY_CHARS = 4000  # the bridge cuts replies at 4000 characters
NOTE_CHARS = 500
ERROR_CHARS = 300

NONE_YET = "(none yet)"
TRUNCATED = "\n[…truncated]"
NO_MESSAGES_NOTE = "no messages in this chat"
ALREADY_ANSWERED_NOTE = "already answered"
CHECK_NOTE = "Complete the [CHECK: …] placeholder before you send this."

SCHEMA = json.dumps({
    "type": "object",
    "properties": {"reply": {"type": "string"}, "note": {"type": "string"}},
    "required": ["reply", "note"],
    "additionalProperties": False,
})

TASK = (
    "Draft the owner's next reply to the customer's unanswered message(s), which are the Customer lines "
    "after the owner's last reply. Return JSON with \"reply\" and \"note\"."
)
EXAMPLES_INTRO = "Past replies by the owner (for tone and wording only; facts in them may be out of date):"

# An opening or closing tag of the prompt layout. Text from outside must not be able to write one.
# Every part is linear: no two quantifiers can match the same characters, so a message such as
# "<" followed by 60000 spaces cannot make the match slow. ZWJ and ZWNJ (kept by clean_text) are
# allowed where a space is, so that "<" + ZWJ + "/task>" is also caught.
_BLANK = r"[\s‌‍]*"
_OUR_TAG = re.compile(
    rf"<({_BLANK}(?:/{_BLANK})?(?:voice|templates|facts|examples|conversation|task)\b[^>]*)(>?)", re.IGNORECASE
)
_TIMESTAMP = re.compile(r"^(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2})")
# gw_store's own pattern ends in `$`, which also accepts a trailing newline.
_INDIVIDUAL_JID = re.compile(r"[0-9]{1,20}@(?:s\.whatsapp\.net|lid)")
_MODEL_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,99}$")
# Errors from sqlite3 can quote a stored value, so only these are safe to show.
_SAFE_SQLITE_ERROR = re.compile(
    r"database is locked|unable to open database file|disk I/O error|database disk image is malformed"
    r"|no such (?:table|column): \w+"
)


class DraftError(Exception):
    """A draft could not be made. The message never contains message text."""


class Answer(NamedTuple):
    reply: str
    note: str
    cost: Optional[float]  # USD, as reported by claude
    model: str


@dataclass(frozen=True)
class Turn:
    """One message of the chat as the model sees it. `text` is already clean and neutral."""
    time: str  # "YYYY-MM-DD HH:MM"
    from_me: bool
    media: str  # "image", "audio", ... or ""
    text: str

    @property
    def shown(self) -> str:
        return f"[{self.media}] {self.text}".strip() if self.media else self.text


@dataclass(frozen=True)
class Context:
    name: str
    phone: Optional[str]
    messages: List[Turn]  # the newest MESSAGE_LIMIT messages that have something to show
    pending: List[Turn]  # customer messages after the owner's last message
    examples: List[gw_store.ReplyPair]  # best match last
    voice: str
    templates: str
    facts: str


# --------------------------------------------------------------------------- untrusted text

def neutralise(text: str) -> str:
    """Make every opening or closing prompt tag in `text` harmless (< and > become ‹ and ›)."""
    return _OUR_TAG.sub(lambda m: "‹" + m.group(1) + ("›" if m.group(2) else ""), text)


def _clip(text: str, limit: int) -> str:
    """At most `limit` characters; a cut ends with an ellipsis."""
    return text if len(text) <= limit else text[:limit - 1].rstrip() + "…"


def _untrusted(text: Optional[str], limit: int) -> str:
    """Text from outside, ready for the prompt. The cut comes first, so the work stays bounded."""
    return neutralise(gw_store.clean_text(_clip(text or "", limit))).strip()


def _indent_lines(text: str) -> str:
    """Indent every line after the first, so that a line break inside a message can never
    look like the start of a new turn ("[time] Owner: ...")."""
    return text.replace("\n", "\n    ")


def _one_line(text: str, limit: int = ERROR_CHARS) -> str:
    return _clip(" ".join(str(text).split()), limit)


# --------------------------------------------------------------------------- context

def _format_time(stamp: str) -> str:
    match = _TIMESTAMP.match(stamp)
    return f"{match.group(1)} {match.group(2)}" if match else _untrusted(stamp, 16)


def _turn(msg: gw_store.Message) -> Turn:
    return Turn(
        time=_format_time(msg.timestamp),
        from_me=msg.from_me,
        media=re.sub(r"[^A-Za-z0-9_-]", "", msg.media_type)[:20],
        text=_untrusted(msg.content, MESSAGE_CHARS),
    )


def _words(text: str) -> set:
    return set(re.findall(r"\w{3,}", text.lower()))


def pick_examples(pairs: Sequence[gw_store.ReplyPair], pending_text: str) -> List[gw_store.ReplyPair]:
    """Past replies that answered a similar question, best match last, at least EXAMPLES_MIN of them.

    `pairs` is newest first. A pair scores by the share of its customer message's words that the
    pending messages also use. Pairs with a score are taken best first (a tie keeps the newer one),
    then the newest pairs fill up to the minimum. A reply text is used once.
    """
    wanted = _words(pending_text)
    scored = []
    for pair in pairs:
        words = _words(pair.customer_text)
        score = len(wanted & words) / len(words) if words else 0.0
        if score > 0:
            scored.append((pair, score))
    scored.sort(key=lambda item: -item[1])  # a stable sort keeps newer pairs first within a score

    chosen: List[gw_store.ReplyPair] = []
    seen = set()

    def take(pair: gw_store.ReplyPair) -> None:
        key = " ".join(pair.reply_text.lower().split())
        if key not in seen:
            seen.add(key)
            chosen.append(pair)

    for pair, _ in scored:
        if len(chosen) >= EXAMPLES_BEST:
            break
        take(pair)
    for pair in pairs:
        if len(chosen) >= EXAMPLES_MIN:
            break
        take(pair)
    return chosen[::-1]


def _read_data_file(name: str) -> str:
    """One of the owner's files (voice, templates, facts), cleaned and capped; NONE_YET if missing."""
    try:
        with open(gw_store.data_dir() / name, "rb") as f:
            raw = f.read(FILE_CHARS * 4 + 4)  # at most 4 bytes per character
    except OSError:
        return NONE_YET
    text = raw.decode("utf-8", errors="replace").strip()
    cut = len(text) > FILE_CHARS
    text = _untrusted(text[:FILE_CHARS], FILE_CHARS)
    if not text:
        return NONE_YET
    return text + TRUNCATED if cut else text


def build_context(conn: sqlite3.Connection, chat_jid: str) -> Context:
    turns = [_turn(m) for m in gw_store.chat_messages(conn, chat_jid, limit=MESSAGE_LIMIT)]
    messages = [t for t in turns if t.shown]
    pending: List[Turn] = []
    for turn in reversed(messages):
        if turn.from_me:
            break
        pending.append(turn)
    pending.reverse()
    name, phone = gw_store.chat_label(conn, chat_jid)
    examples: List[gw_store.ReplyPair] = []
    if pending:
        pairs = gw_store.owner_replies(conn, limit=EXAMPLE_POOL, exclude_chats=[chat_jid])
        examples = pick_examples(pairs, "\n".join(t.text for t in pending))
    return Context(
        name=name, phone=phone, messages=messages, pending=pending, examples=examples,
        voice=_read_data_file(gw_store.VOICE_FILE),
        templates=_read_data_file(gw_store.TEMPLATES_FILE),
        facts=_read_data_file(gw_store.FACTS_FILE),
    )


def _skip_note(ctx: Context) -> Optional[str]:
    """Why no draft is needed, or None when a customer is waiting for an answer."""
    if not ctx.messages:
        return NO_MESSAGES_NOTE
    if not ctx.pending:
        return ALREADY_ANSWERED_NOTE
    return None


# --------------------------------------------------------------------------- prompt

def render_prompt(ctx: Context) -> str:
    name = _one_line(_untrusted(ctx.name, NAME_CHARS), NAME_CHARS).replace('"', "&quot;") or "unknown"
    digits = re.sub(r"\D", "", ctx.phone or "")[:20]
    phone = f"+{digits}" if digits else "unknown"

    pairs = [
        "Customer: " + _indent_lines(_untrusted(p.customer_text, EXAMPLE_CHARS)) + "\n"
        "Owner: " + _indent_lines(_untrusted(p.reply_text, EXAMPLE_CHARS))
        for p in ctx.examples
    ]
    lines = [f"[{t.time}] {'Owner' if t.from_me else 'Customer'}: {_indent_lines(t.shown)}" for t in ctx.messages]
    return "\n".join([
        "<voice>", ctx.voice, "</voice>",
        "<templates>", ctx.templates, "</templates>",
        "<facts>", ctx.facts, "</facts>",
        "<examples>", EXAMPLES_INTRO, "\n\n".join(pairs) if pairs else NONE_YET, "</examples>",
        f'<conversation customer="{name}" phone="{phone}">', *lines, "</conversation>",
        "<task>", TASK, "</task>",
    ]) + "\n"


def load_system_prompt() -> str:
    """The body of the draft-reply skill (its frontmatter removed): the drafting rules."""
    try:
        text = SKILL_FILE.read_text(encoding="utf-8")
    except OSError as e:
        raise DraftError(f"cannot read {SKILL_FILE}: {e.strerror or type(e).__name__}") from e
    if text.startswith("---"):
        end = re.search(r"^---[ \t]*$", text[3:], re.MULTILINE)
        if not end:
            raise DraftError(f"unterminated frontmatter in {SKILL_FILE}")
        text = text[3 + end.end():]
    text = text.strip()
    if not text:
        raise DraftError(f"{SKILL_FILE} has no rules")
    return text


# --------------------------------------------------------------------------- model call

def _claude_binary() -> str:
    explicit = os.environ.get("CLAUDE_BIN", "").strip()
    return explicit or shutil.which("claude") or str(Path.home() / ".local" / "bin" / "claude")


def _model_name() -> str:
    model = os.environ.get("GHOSTWRITER_MODEL", "").strip() or DEFAULT_MODEL
    if not _MODEL_NAME.match(model):  # also keeps a value such as "--flag" out of the command line
        raise DraftError("GHOSTWRITER_MODEL is not a valid model name")
    return model


def timeout_seconds() -> float:
    try:
        value = float(os.environ.get("GHOSTWRITER_CLAUDE_TIMEOUT", ""))
    except ValueError:
        return DEFAULT_TIMEOUT
    return min(value, MAX_TIMEOUT) if value > 0 else DEFAULT_TIMEOUT


def _field(value: Any) -> str:
    """A short, safe rendering of a field of claude's output for an error line."""
    return re.sub(r"[^\w.-]", "", str(value))[:40] or "none"


def parse_claude_output(stdout: str, exit_code: int, model: str) -> Answer:
    """The draft from `claude -p --output-format json`; raises DraftError."""
    try:
        data = json.loads(stdout)
    except ValueError:
        data = None
    if isinstance(data, list):  # some versions print every event; the result is the last one
        data = next((item for item in reversed(data) if isinstance(item, dict) and item.get("type") == "result"), None)
    if not isinstance(data, dict):
        raise DraftError(f"claude printed no result object (exit={exit_code})")
    if exit_code != 0 or data.get("is_error") or data.get("subtype") != "success":
        raise DraftError(
            f"claude failed (exit={exit_code}, subtype={_field(data.get('subtype'))}, "
            f"api_error_status={_field(data.get('api_error_status'))})"
        )
    out = data.get("structured_output")
    if not (isinstance(out, dict) and isinstance(out.get("reply"), str) and isinstance(out.get("note"), str)):
        raise DraftError("claude result has no structured_output with reply and note")
    cost = data.get("total_cost_usd")
    usable = isinstance(cost, (int, float)) and not isinstance(cost, bool) and math.isfinite(cost)
    return Answer(out["reply"], out["note"], float(cost) if usable else None, model)


def call_model(system_prompt: str, user_message: str) -> Answer:
    """Run claude with no tools and return its draft. The model gets nothing but the prompt."""
    model = _model_name()
    argv = [
        _claude_binary(), "-p", "--restricted", "--strict-mcp-config", "--tools", "",
        "--system-prompt", system_prompt, "--model", model, "--no-session-persistence",
        "--output-format", "json", "--json-schema", SCHEMA,
    ]
    # claude needs neither the store path nor the chat; everything else (login, PATH, HOME) stays.
    env = {k: v for k, v in os.environ.items() if k not in ("WHATSAPP_STORE_DIR", "GHOSTWRITER_CHAT_JID")}
    timeout = timeout_seconds()
    # Same process group as this script, so that when the bridge kills us it kills claude too.
    proc = subprocess.Popen(
        argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        encoding="utf-8", errors="replace", cwd=str(gw_store.data_dir()), env=env,
    )
    try:
        stdout, _ = proc.communicate(user_message, timeout=timeout)
    except subprocess.TimeoutExpired:
        proc.kill()
        try:
            proc.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            pass
        raise DraftError(f"claude timed out after {timeout:g}s") from None
    return parse_claude_output(stdout, proc.returncode, model)


# --------------------------------------------------------------------------- commands

def finish_draft(answer: Answer) -> Dict[str, str]:
    """Clean the model's answer: no hidden characters, a one-line note, and a note whenever a
    [CHECK: …] placeholder is in the reply, so that she cannot paste it unseen."""
    reply = _clip(gw_store.clean_text(answer.reply).strip(), REPLY_CHARS)
    note = _one_line(gw_store.clean_text(answer.note), NOTE_CHARS)
    if re.search(r"\[\s*check\b", reply, re.IGNORECASE) and not note:
        note = CHECK_NOTE
    return {"reply": reply, "note": note}


def log_draft(chat_jid: str, draft: Dict[str, str], answer: Answer) -> None:
    entry = {
        "time": datetime.now().astimezone().isoformat(timespec="seconds"),
        "chat_jid": chat_jid, "reply": draft["reply"], "note": draft["note"],
        "model": answer.model, "cost_usd": answer.cost,
    }
    # os.open, not open(): the file is created with mode 0600, and a symlink in its place is refused.
    fd = os.open(gw_store.data_dir() / gw_store.DRAFT_LOG, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        os.fchmod(fd, 0o600)  # an older file keeps its mode otherwise
        os.write(fd, (json.dumps(entry, ensure_ascii=False) + "\n").encode("utf-8", "replace"))
    finally:
        os.close(fd)


def _write_stdout(text: str) -> None:
    """Print as UTF-8, whatever the locale: stdout can use another encoding."""
    sys.stdout.buffer.write(text.encode("utf-8", "replace"))
    sys.stdout.flush()


def _emit(draft: Dict[str, str]) -> None:
    _write_stdout(json.dumps(draft, ensure_ascii=False) + "\n")


def _load_context(chat_jid: str) -> Context:
    if not (gw_store.is_individual(chat_jid) and _INDIVIDUAL_JID.fullmatch(chat_jid)):
        raise DraftError("not a one-to-one chat JID")
    with closing(gw_store.connect()) as conn:
        # sqlite3 raises, and quotes the text in its message, for a stored value that is not UTF-8.
        conn.text_factory = lambda raw: raw.decode("utf-8", "replace")
        return build_context(conn, chat_jid)


def cmd_draft(chat_jid: str) -> None:
    ctx = _load_context(chat_jid)
    skip = _skip_note(ctx)
    if skip is not None:
        _emit({"reply": "", "note": skip})
        return
    answer = call_model(load_system_prompt(), render_prompt(ctx))
    draft = finish_draft(answer)
    try:
        log_draft(chat_jid, draft, answer)
    except OSError as e:  # the draft is still worth sending
        print(f"gw_draft: warning: cannot write {gw_store.DRAFT_LOG}: {e.strerror or type(e).__name__}", file=sys.stderr)
    _emit(draft)


def cmd_context(chat_jid: str) -> None:
    ctx = _load_context(chat_jid)
    status = _skip_note(ctx) or f"{len(ctx.pending)} unanswered customer message(s)"
    _write_stdout(f"System prompt: {SKILL_FILE}\nStatus: {status}\n\n{render_prompt(ctx)}")


def _failure_line(error: Exception) -> str:
    """One line for stderr. It shows only what cannot hold message text, which is why most exceptions
    are named without their message (sqlite3 errors, for one, can quote a stored value)."""
    detail = ""
    if isinstance(error, (DraftError, gw_store.StoreError)):
        detail = str(error)
    elif isinstance(error, OSError):
        detail = ": ".join(part for part in (error.strerror, error.filename) if part)
    elif isinstance(error, sqlite3.Error) and _SAFE_SQLITE_ERROR.fullmatch(str(error)):
        detail = str(error)
    return f"gw_draft: {type(error).__name__}" + (f": {_one_line(detail)}" if detail else "")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="gw_draft.py",
        description="Draft a WhatsApp reply for one customer chat, in the owner's voice.",
    )
    sub = parser.add_subparsers(dest="command", required=True)
    draft = sub.add_parser("draft", help="print the draft as JSON: {\"reply\": ..., \"note\": ...} (calls claude)")
    draft.add_argument("chat_jid", help="the customer's chat, for example 6591234567@s.whatsapp.net")
    draft.set_defaults(func=cmd_draft)
    context = sub.add_parser("context", help="print the prompt a draft would use; does not call claude")
    context.add_argument("chat_jid")
    context.set_defaults(func=cmd_context)
    return parser


def main(argv: Optional[Sequence[str]] = None) -> int:
    args = build_parser().parse_args(argv)  # a bad command line exits 2 here; the wrapper checks its own
    try:
        args.func(args.chat_jid)
    except Exception as e:  # one line, no message text, nothing on stdout
        print(_failure_line(e), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
