"""Summarise the owner's WhatsApp history so Claude can learn her voice and her repeated replies.

The learn-templates and learn-voice skills run this script and read its Markdown output. The
script does the heavy reading in plain Python, so no model tokens are spent scanning thousands
of messages. It only reads messages.db (through gw_store); the only files it ever writes are
the three data-file skeletons made by `init`. Standard library only.

  python3 -I gw_learn.py [--store-dir DIR] init
  python3 -I gw_learn.py find-templates [--min-count N] [--limit N] [--max-messages N]
  python3 -I gw_learn.py voice-sample [--n N] [--seed N]

Customer text in the output is context for Claude only. It must never be copied into the
data files.
"""
import argparse
import contextlib
import difflib
import math
import os
import random
import re
import statistics
import sys
import unicodedata
from collections import Counter, defaultdict
from dataclasses import dataclass, field
from pathlib import Path
from typing import Dict, List, Optional, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))  # python3 -I leaves this directory off sys.path

import gw_store  # noqa: E402

SKELETONS = {
    gw_store.VOICE_FILE: """# Voice
Notes language: English

## Languages and mixing
## Tone
## Length
## Greetings and sign-offs
## Emoji and punctuation
## Words and phrases she uses
## Things she avoids
## Example replies
""",
    gw_store.TEMPLATES_FILE: "# Templates\n",
    gw_store.FACTS_FILE: "# Facts\n",
}

MIN_TEMPLATE_CHARS = 25  # shorter normalised replies ("ok kak") are not templates
NEAR_DUPLICATE_MIN_CHARS = 40
NEAR_DUPLICATE_RATIO = 0.85
NEAR_DUPLICATE_LENGTH_RATIO = 0.8
COMPARE_CHARS = 1200  # difflib is slow on long strings; compare the start only
MAX_POSTING = 300  # words used by more clusters than this are too common to find candidates
OUTPUT_CAP = 40_000
CUSTOMER_PREVIEW = 120
REPLY_PREVIEW = 1500
VOICE_MAX_REPLY_CHARS = 600
VOICE_TEMPLATE_COUNT = 3  # a reply sent this often is a template, not voice
PAIRS_PER_CHAT = 3

_SPACE = re.compile(r"\s+")
_DIGITS = re.compile(r"\d+")
_TOKEN = re.compile(r"\w{3,}")
_WORD = re.compile(r"[^\W\d_]{3,}")
_NOT_EMOJI = {0x00A9, 0x00AE, 0x2122, 0xFE0F, 0x200D} | set(range(0x1F3FB, 0x1F400))


class UsageError(Exception):
    """A problem the owner can fix; printed as one line."""


# ---------------------------------------------------------------------------
# Text helpers
# ---------------------------------------------------------------------------

def normalise(text: str) -> str:
    """Lowercase, collapse whitespace, replace digit runs with '#'."""
    return _DIGITS.sub("#", _SPACE.sub(" ", text.lower()).strip())


def is_emoji(ch: str) -> bool:
    cp = ord(ch)
    if cp in _NOT_EMOJI:
        return False
    return unicodedata.category(ch) == "So" or 0x1F000 <= cp <= 0x1FAFF or 0x2600 <= cp <= 0x27BF


def emoji_in(text: str) -> List[str]:
    return [ch for ch in text if is_emoji(ch)]


def preview(text: str, limit: int) -> str:
    flat = _SPACE.sub(" ", text).strip()
    return flat if len(flat) <= limit else flat[: limit - 1].rstrip() + "…"


def day(timestamp: str) -> str:
    return timestamp[:10]


def count(n: int) -> str:
    return f"{n:,}".replace(",", " ")


# ---------------------------------------------------------------------------
# init
# ---------------------------------------------------------------------------

def cmd_init(_args) -> str:
    base = gw_store.data_dir()
    lines = ["# Data files", f"Folder: {base}", ""]
    for name, skeleton in SKELETONS.items():
        path = base / name
        try:
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        except FileExistsError:
            status = "already existed (kept as it is)"
        else:
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                f.write(skeleton)
            status = "created"
        os.chmod(path, 0o600)
        lines.append(f"- {path}: {status}")
    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------------------
# find-templates
# ---------------------------------------------------------------------------

@dataclass
class Group:
    """All of the owner's messages with the same normalised text."""
    norm: str
    messages: list = field(default_factory=list)  # newest first, as owner_messages returns them

    @property
    def newest(self) -> str:
        return self.messages[0].timestamp


@dataclass
class Cluster:
    rep: str  # normalised text of the first (most used) group
    groups: List[Group] = field(default_factory=list)

    @property
    def total(self) -> int:
        return sum(len(g.messages) for g in self.groups)

    def messages(self):
        for g in self.groups:
            yield from g.messages


def _group(messages) -> List[Group]:
    by_norm: Dict[str, Group] = {}
    for msg in messages:
        norm = normalise(msg.content)
        if len(norm) < MIN_TEMPLATE_CHARS or not any(ch.isalpha() for ch in norm):
            continue
        by_norm.setdefault(norm, Group(norm)).messages.append(msg)
    groups = list(by_norm.values())
    groups.sort(key=lambda g: g.newest, reverse=True)
    groups.sort(key=lambda g: len(g.messages), reverse=True)  # stable: ties stay newest first
    return groups


def _similar(a: str, b: str) -> bool:
    short, long_ = sorted((len(a), len(b)))
    if short / long_ < NEAR_DUPLICATE_LENGTH_RATIO:
        return False
    sm = difflib.SequenceMatcher(None, a[:COMPARE_CHARS], b[:COMPARE_CHARS], autojunk=False)
    return (
        sm.real_quick_ratio() >= NEAR_DUPLICATE_RATIO
        and sm.quick_ratio() >= NEAR_DUPLICATE_RATIO
        and sm.ratio() >= NEAR_DUPLICATE_RATIO
    )


def _cluster(groups: Sequence[Group]) -> List[Cluster]:
    """Greedy near-duplicate merge. A word index picks the few clusters worth comparing."""
    clusters: List[Cluster] = []
    index: Dict[str, List[int]] = defaultdict(list)
    for group in groups:
        if len(group.norm) >= NEAR_DUPLICATE_MIN_CHARS:
            words = set(_TOKEN.findall(group.norm))
            rare = [w for w in words if len(index.get(w, ())) <= MAX_POSTING]
            votes: Counter = Counter()
            for w in rare:
                votes.update(index.get(w, ()))
            need = max(1, math.ceil(len(rare) / 2))
            for ci in sorted(ci for ci, v in votes.items() if v >= need):
                if _similar(clusters[ci].rep, group.norm):
                    clusters[ci].groups.append(group)
                    break
            else:
                for w in words:
                    index[w].append(len(clusters))
                clusters.append(Cluster(group.norm, [group]))
        else:
            clusters.append(Cluster(group.norm, [group]))
    return clusters


def _blockquote(text: str) -> str:
    return "\n".join("> " + line if line.strip() else ">" for line in text.strip().splitlines())


def cmd_find_templates(args) -> str:
    with _connect() as conn:
        messages = gw_store.owner_messages(conn, limit=args.max_messages)
        if not messages:
            return "No messages from you were found in one-to-one chats, so there is nothing to learn from yet.\n"
        clusters = [c for c in _cluster(_group(messages)) if c.total >= args.min_count]
        clusters.sort(key=lambda c: c.total, reverse=True)  # stable: ties keep the greedy order
        found = len(clusters)
        clusters = clusters[: args.limit]
        replies_by_norm: Dict[str, list] = defaultdict(list)
        wanted = {g.norm for c in clusters for g in c.groups}
        if wanted:
            for pair in gw_store.owner_replies(conn, limit=args.max_messages):
                norm = normalise(pair.reply_text)
                if norm in wanted:
                    replies_by_norm[norm].append(pair.customer_text)

    shown = f"{found}" if found <= args.limit else f"top {args.limit} of {found}"
    head = f"# Repeated replies ({shown} found from {count(len(messages))} of your messages)\n"
    if not clusters:
        return head + f"\nNo reply was sent at least {args.min_count} times.\n"
    out, size = [head], len(head)
    left_out = 0
    for i, cluster in enumerate(clusters, 1):
        block = _render_cluster(i, cluster, replies_by_norm)
        if size + len(block) > OUTPUT_CAP:
            left_out = len(clusters) - i + 1
            break
        out.append(block)
        size += len(block)
    if left_out:
        out.append(f"\n({left_out} more repeated replies left out to keep this summary short. Raise --min-count to see fewer, stronger ones.)\n")
    out.append(
        "\nNote: the customer messages above are only context. Do not copy customer names or "
        "phone numbers into any file.\n"
    )
    return "\n".join(out)


def _render_cluster(rank: int, cluster: Cluster, replies_by_norm: Dict[str, list]) -> str:
    msgs = sorted(cluster.messages(), key=lambda m: m.timestamp)
    chats = len({m.chat_jid for m in msgs})
    variants = len(cluster.groups)
    newest = max(msgs, key=lambda m: m.timestamp)
    questions: Counter = Counter()
    first_seen: Dict[str, str] = {}
    for g in cluster.groups:
        for text in replies_by_norm.get(g.norm, ()):
            key = normalise(text)
            if key:
                questions[key] += 1
                first_seen.setdefault(key, text)
    lines = [
        f"\n## {rank}. Used {cluster.total} times in {chats} chats "
        f"(first {day(msgs[0].timestamp)}, last {day(newest.timestamp)}) · {variants} variant{'s' if variants != 1 else ''}"
    ]
    if questions:
        lines.append("Customers asked:")
        for key, _ in questions.most_common(3):
            lines.append(f'- "{preview(first_seen[key], CUSTOMER_PREVIEW)}"')
    lines.append("Most recent version:")
    text = newest.content.strip()
    if len(text) > REPLY_PREVIEW:
        text = text[:REPLY_PREVIEW].rstrip() + " …"
    lines.append(_blockquote(text))
    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------------------
# voice-sample
# ---------------------------------------------------------------------------

def _spread(items: list, n: int) -> list:
    """n evenly spaced items of a list that is already sorted."""
    if len(items) <= n:
        return items
    if n == 1:
        return [items[len(items) // 2]]
    return [items[round(i * (len(items) - 1) / (n - 1))] for i in range(n)]


def sample_pairs(pairs, template_norms: set, n: int, seed: int) -> list:
    usable = [p for p in pairs if len(p.reply_text) <= VOICE_MAX_REPLY_CHARS and normalise(p.reply_text) not in template_norms]
    rng = random.Random(seed)
    usable.sort(key=lambda p: (p.timestamp, p.chat_jid, p.reply_text))  # fixed order before shuffling
    rng.shuffle(usable)
    per_chat: Counter = Counter()
    kept = []
    for pair in usable:
        if per_chat[pair.chat_jid] < PAIRS_PER_CHAT:
            per_chat[pair.chat_jid] += 1
            kept.append(pair)
    kept.sort(key=lambda p: (p.timestamp, p.chat_jid))
    return _spread(kept, n)


def _top(counter: Counter, n: int) -> str:
    return ", ".join(f"{item} ({c})" for item, c in counter.most_common(n)) or "none"


def voice_stats(messages) -> List[str]:
    texts = [m.content.strip() for m in messages if m.content.strip()]
    if not texts:
        return ["- No messages to analyse."]
    emoji = Counter()
    with_emoji = multi_line = question = exclaim = 0
    first, last, words = Counter(), Counter(), Counter()
    for text in texts:
        found = emoji_in(text)
        if found:
            with_emoji += 1
            emoji.update(found)
        multi_line += "\n" in text
        question += text.endswith("?")
        exclaim += text.endswith("!")
        tokens = [t for t in (re.sub(r"[^\w]", "", w) for w in text.lower().split()) if t]
        if tokens:
            first[tokens[0]] += 1
            last[tokens[-1]] += 1
        words.update(_WORD.findall(text.lower()))
    total = len(texts)

    def pct(k: int) -> str:
        return f"{100 * k / total:.0f}%"

    return [
        f"- Messages analysed: {count(total)}",
        f"- Median length: {statistics.median(len(t) for t in texts):g} characters, "
        f"{statistics.median(len(t.split()) for t in texts):g} words",
        f"- With at least one emoji: {pct(with_emoji)}. Top emoji: {_top(emoji, 10)}",
        f"- With more than one line: {pct(multi_line)}",
        f"- Ending in ?: {pct(question)}. Ending in !: {pct(exclaim)}",
        f"- Top first words: {_top(first, 10)}",
        f"- Top last words: {_top(last, 10)}",
        f"- Top words of 3 letters or more: {_top(words, 25)}",
    ]


def cmd_voice_sample(args) -> str:
    with _connect() as conn:
        messages = gw_store.owner_messages(conn, limit=20_000)
        if not messages:
            return "No messages from you were found in one-to-one chats, so there is nothing to learn from yet.\n"
        pairs = gw_store.owner_replies(conn, limit=20_000)
    norm_counts = Counter(normalise(m.content) for m in messages)
    # Short repeated replies ("baik kak") are how she talks, so only longer ones count as templates.
    template_norms = {
        norm for norm, c in norm_counts.items() if c >= VOICE_TEMPLATE_COUNT and len(norm) >= MIN_TEMPLATE_CHARS
    }
    sample = sample_pairs(pairs, template_norms, args.n, args.seed)
    out = ["# Voice sample", "", "## Stats", *voice_stats(messages), "", f"## Sample replies ({len(sample)})"]
    out.append(
        "Customer text is context only. Do not copy it into voice.md. "
        "Long replies sent 3 or more times are left out: they are templates, not voice."
    )
    for pair in sample:
        out.append("")
        out.append(f"Customer: {preview(pair.customer_text, 200)}")
        out.append(f"Owner: {pair.reply_text.strip()}")
    return "\n".join(out) + "\n"


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

@contextlib.contextmanager
def _connect():
    """Open the database read-only; turn setup problems into one-line errors."""
    try:
        conn = gw_store.connect()
    except gw_store.StoreError as e:
        raise UsageError(_with_hint(str(e))) from e
    try:
        yield conn
    finally:
        conn.close()


def _with_hint(message: str) -> str:
    if "WHATSAPP_STORE_DIR" in message:
        return message + " (for example: export WHATSAPP_STORE_DIR=<repo>/whatsapp-bridge/store)"
    return message


def _positive(value: str) -> int:
    n = int(value)
    if n < 1:
        raise argparse.ArgumentTypeError("must be 1 or more")
    return n


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="gw_learn.py",
        description="Summarise the owner's WhatsApp history for the learn-templates and learn-voice skills (read-only).",
    )
    parser.add_argument(
        "--store-dir", metavar="DIR",
        help="the bridge's store folder (same as setting WHATSAPP_STORE_DIR); put it before the command",
    )
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("init", help="create voice.md, templates.md and facts.md if they are missing").set_defaults(run=cmd_init)
    ft = sub.add_parser("find-templates", help="list replies the owner sent many times")
    ft.add_argument("--min-count", type=_positive, default=3, help="fewest uses to report (default 3)")
    ft.add_argument("--limit", type=_positive, default=40, help="most templates to report (default 40)")
    ft.add_argument("--max-messages", type=_positive, default=20_000, help="newest messages to read (default 20000)")
    ft.set_defaults(run=cmd_find_templates)
    vs = sub.add_parser("voice-sample", help="writing statistics and sample replies")
    vs.add_argument("--n", type=_positive, default=60, help="sample size (default 60)")
    vs.add_argument("--seed", type=int, default=0, help="sampling seed (default 0)")
    vs.set_defaults(run=cmd_voice_sample)
    return parser


def main(argv: Optional[Sequence[str]] = None) -> int:
    args = build_parser().parse_args(argv)
    if args.store_dir:
        os.environ["WHATSAPP_STORE_DIR"] = args.store_dir
    try:
        output = args.run(args)
    except UsageError as e:
        print(f"gw_learn: {e}", file=sys.stderr)
        return 1
    except gw_store.StoreError as e:
        print(f"gw_learn: {_with_hint(str(e))}", file=sys.stderr)
        return 1
    sys.stdout.write(output)
    return 0


if __name__ == "__main__":
    sys.exit(main())
