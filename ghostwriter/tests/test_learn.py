"""Tests for gw_learn.py (the learn-templates and learn-voice helper)."""
import re
import stat
import time
from pathlib import Path

import pytest

import gw_learn
from conftest import OWN_JID

SKILLS = Path(__file__).resolve().parent.parent / "skills"
CUSTOMER = "62811111111@s.whatsapp.net"

DIRECTIONS = "Halo kak! Untuk fitting bisa datang ke Jl. Melati No. 12 lantai 2, tanggal {d}/10 jam 10 ya, kami buka sampai jam 5 sore"
PAYMENT = "Pembayaran bisa transfer ke BCA 1234567 a.n. Toko Bunga, kirim bukti transfer ya kak, terima kasih"


WORDS = "alpha bravo charlie delta echo foxtrot golf hotel".split()


def chat_jid(i: int) -> str:
    return f"62812{i:07d}@s.whatsapp.net"


def ts(day: int, minute: int = 0) -> str:
    return f"2025-02-{day:02d} 10:{minute:02d}:00+08:00"


def run(capsys, *argv):
    code = gw_learn.main(list(argv))
    out = capsys.readouterr()
    return code, out.out, out.err


def test_init_creates_private_files_and_never_overwrites(store, tmp_path, capsys):
    code, out, _ = run(capsys, "init")
    home = tmp_path / "ghostwriter-home"
    assert code == 0
    for name in ("voice.md", "templates.md", "facts.md"):
        path = home / name
        assert path.is_file() and str(path) in out and "created" in out
        assert stat.S_IMODE(path.stat().st_mode) == 0o600
    assert stat.S_IMODE(home.stat().st_mode) == 0o700
    assert "Notes language:" in (home / "voice.md").read_text()

    (home / "templates.md").write_text("# Templates\n\n## Mine\nUse when: x\nhello\n")
    code, out, _ = run(capsys, "init")
    assert code == 0 and "already existed" in out and "created" not in out
    assert "## Mine" in (home / "templates.md").read_text()


def test_find_templates_groups_variants(store, capsys):
    for i, day in enumerate((3, 5, 9, 12)):
        jid = chat_jid(i)
        text = DIRECTIONS.format(d=day)
        if i == 2:
            text = text.replace("fitting", "fiting")
        store.message(jid, "kak mau fitting hari sabtu bisa?", ts(day, 0))
        store.message(jid, text, ts(day, 1), from_me=True)
    for i in range(10):
        store.message(chat_jid(20 + i), "ok kak", ts(15, i), from_me=True)
    # copies in a group chat and in her own chat must not count
    store.message("120363025246125486@g.us", DIRECTIONS.format(d=20), ts(20), from_me=True)
    store.message(OWN_JID, DIRECTIONS.format(d=21), ts(21), from_me=True)

    code, out, _ = run(capsys, "find-templates")
    assert code == 0
    assert "Used 4 times in 4 chats" in out
    assert "last 2025-02-12" in out and "2 variants" in out
    assert out.count("\n## ") == 1
    assert "ok kak" not in out.split("Most recent")[0].replace("Customers asked", "")
    assert '- "kak mau fitting hari sabtu bisa?"' in out
    assert "> Halo kak! Untuk fitting bisa datang" in out


def test_find_templates_min_count_and_limit(store, capsys):
    texts = [
        "Ongkos kirim ke Jakarta sekitar dua puluh ribu rupiah ya kak",
        "Kami buka setiap hari dari jam sembilan pagi sampai jam lima sore",
        "Mohon maaf stok warna merah sedang kosong, bisa pesan dulu",
    ]
    for n, (t, uses) in enumerate(zip(texts, (5, 4, 2))):
        for i in range(uses):
            store.message(chat_jid(i), t, ts(1 + n, i), from_me=True)
    code, out, _ = run(capsys, "find-templates", "--min-count", "4")
    assert code == 0 and "Used 5 times" in out and "Used 4 times" in out and "Used 2 times" not in out

    code, out, _ = run(capsys, "find-templates", "--min-count", "4", "--limit", "1")
    assert "Used 5 times" in out and "Used 4 times" not in out and "top 1 of 2" in out

    code, out, _ = run(capsys, "find-templates", "--min-count", "9")
    assert code == 0 and "No reply was sent at least 9 times" in out


def test_find_templates_with_no_messages(store, capsys):
    code, out, _ = run(capsys, "find-templates")
    assert code == 0 and "nothing to learn" in out


def test_voice_sample_is_spread_and_deterministic(store, capsys):
    for c in range(8):
        for k in range(6):
            day = 1 + c * 3 + k % 3
            store.message(chat_jid(c), f"customer question {WORDS[c]} {WORDS[k]}", ts(day, 10 + k * 2))
            body = f"reply {WORDS[c]} {WORDS[k]} is varied text about this order"
            if k % 2:
                body += " 😊"
            store.message(chat_jid(c), body, ts(day, 11 + k * 2), from_me=True)
    for c in range(5):  # a template sent 5 times: not voice
        store.message(chat_jid(c), "Where is your shop please", ts(28, c))
        store.message(chat_jid(c), "Our shop is at the mall, third floor, next to the bank", ts(28, c + 10), from_me=True)

    code, out, _ = run(capsys, "voice-sample", "--n", "20")
    assert code == 0
    assert "## Stats" in out and "## Sample replies (20)" in out
    assert "Median length" in out and "With at least one emoji: 4" in out  # 24 of 53 messages carry emoji (45%)
    assert "third floor" not in out.split("## Sample replies")[1]
    per_chat = {}
    for customer, owner in re.findall(r"Customer: (.*)\nOwner: (.*)", out):
        per_chat.setdefault(customer.split(" ")[2], []).append(owner)
        assert customer.startswith("customer question")
    assert per_chat and max(len(v) for v in per_chat.values()) <= 3

    _, again, _ = run(capsys, "voice-sample", "--n", "20")
    assert again == out
    _, other, _ = run(capsys, "voice-sample", "--n", "20", "--seed", "7")
    assert other != out


def test_voice_sample_keeps_short_repeated_replies(store, capsys):
    for c in range(4):  # a short reply she repeats is how she talks, not a template
        store.message(chat_jid(c), "ada stok kak?", ts(27, c))
        store.message(chat_jid(c), "baik kak", ts(27, c + 10), from_me=True)
    _, out, _ = run(capsys, "voice-sample")
    assert "Owner: baik kak" in out


def test_voice_stats_numbers(store, capsys):
    for i in range(10):
        text = f"halo kak 😊 {i}" if i < 5 else f"baik kak {i} ya?"
        store.message(chat_jid(i), "pesan", ts(1, i))
        store.message(chat_jid(i), text, ts(1, i + 20), from_me=True)
    _, out, _ = run(capsys, "voice-sample", "--n", "5")
    assert "With at least one emoji: 50%" in out and "😊 (5)" in out
    assert "Ending in ?: 50%" in out
    assert "Median length: 13 characters, 4 words" in out
    assert "kak (10)" in out


def test_voice_sample_excludes_self_chat_and_groups(store, capsys):
    store.message(chat_jid(1), "a question", ts(1, 0))
    store.message(chat_jid(1), "A normal friendly reply from the shop owner", ts(1, 1), from_me=True)
    store.message("120363025246125486@g.us", "group question", ts(2, 0), sender="62899")
    store.message("120363025246125486@g.us", "Group reply that must stay out", ts(2, 1), from_me=True)
    store.message(OWN_JID, "note to self", ts(3, 0))
    store.message(OWN_JID, "Draft: self chat text that must stay out", ts(3, 1), from_me=True)
    _, out, _ = run(capsys, "voice-sample")
    assert "friendly reply" in out
    assert "must stay out" not in out
    assert "Messages analysed: 1" in out


def test_missing_store_dir_is_a_clear_error(monkeypatch, capsys):
    monkeypatch.delenv("WHATSAPP_STORE_DIR", raising=False)
    monkeypatch.delenv("GHOSTWRITER_HOME", raising=False)
    for cmd in ("find-templates", "voice-sample", "init"):
        code, out, err = run(capsys, cmd)
        assert code == 1 and out == ""
        assert "WHATSAPP_STORE_DIR" in err and err.count("\n") == 1


def test_missing_database_is_a_clear_error(store, capsys):
    store.db_path.unlink()
    code, _, err = run(capsys, "find-templates")
    assert code == 1 and "messages.db" in err


def test_skills_have_frontmatter():
    for name in ("learn-templates", "learn-voice"):
        text = (SKILLS / name / "SKILL.md").read_text()
        assert text.startswith("---\n")
        front = text.split("---\n")[1]
        fields = dict(line.split(":", 1) for line in front.splitlines() if re.match(r"^[a-z-]+:", line))
        assert fields["name"].strip() == name
        assert fields["description"].strip()


def test_find_templates_speed(store, capsys):
    import random
    rng = random.Random(1)
    words = "kak halo bisa harga ready stok ukuran warna kirim hari besok jam toko alamat transfer terima kasih pesanan".split()
    rows = []
    for i in range(3000):
        sentence = " ".join(rng.choice(words) for _ in range(rng.randint(6, 18)))
        rows.append((chat_jid(i % 150), f"{sentence} {i}", ts(1 + i % 27, i % 60)))
    for jid, text, stamp in rows:
        store.message(jid, text, stamp, from_me=True)
    started = time.process_time()
    code, _, _ = run(capsys, "find-templates")
    assert code == 0 and time.process_time() - started < 10


def test_store_dir_option_replaces_the_environment_variable(store, monkeypatch, capsys):
    store.message(chat_jid(1), "a question", ts(1, 0))
    store.message(chat_jid(1), "A normal friendly reply from the shop owner", ts(1, 1), from_me=True)
    monkeypatch.delenv("WHATSAPP_STORE_DIR")
    code, out, _ = run(capsys, "--store-dir", str(store.root), "voice-sample")
    assert code == 0 and "friendly reply" in out
