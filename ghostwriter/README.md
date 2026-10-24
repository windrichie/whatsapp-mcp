# Solopreneur Ghostwriter

Ghostwriter drafts replies to your WhatsApp customers in your own voice. When a customer writes and then goes quiet, it writes a draft and sends it to your own "Message yourself" chat. You read it, fix anything that is wrong, then copy it into the customer's chat and send it yourself. Ghostwriter never sends anything to a customer.

This folder is a Claude Code plugin named `solopreneur-ghostwriter`. It works with the WhatsApp bridge in this repo. It has three parts:

- Two learning skills, `learn-templates` and `learn-voice`. You run them once, with Claude Code. They read your past messages and save what they learn after you confirm it.
- A draft runner, `bin/draft`. The bridge calls it when a customer writes.
- A skill for the runner, `draft-reply`, which holds the drafting rules.

## How it stays safe

- The bridge stays read-only for the AI. Keep `WHATSAPP_READ_ONLY=1`. The bridge has one narrow exception: it can send a draft to your own chat and nowhere else. It checks this itself, so nothing the AI writes can change who receives a message.
- The AI that writes drafts has no tools. It cannot read files, browse, or send.
- Customer messages are treated as untrusted text. A customer who writes "ignore your rules" gets no special treatment.
- Your data files are private. The data folder is readable by your user only (mode 0700) and each file in it too (0600).
- The learning scripts only read the message database. They never write to it.
- Names and phone numbers of your customers are not saved in `voice.md`, `templates.md` or `facts.md`.

The bridge is an unofficial WhatsApp client. WhatsApp can ban accounts that use unofficial clients, and this setup does not remove that risk. It keeps the footprint small: the bridge reads messages and sends a few messages to yourself. It does not send to customers. For the full security model, read [SECURITY_AUDIT.md](../SECURITY_AUDIT.md).

## Setup

### 1. Run the bridge

Link your WhatsApp account and start the bridge as described in the [main README](../README.md). Check that your messages appear in the MCP tools before you go on.

### 2. Teach it how you write

Start Claude Code with this plugin. Use the full path of the `ghostwriter` folder in this repo:

```bash
claude --plugin-dir /path/to/whatsapp-mcp/ghostwriter
```

Then run the two skills, one after the other:

```
/solopreneur-ghostwriter:learn-templates
/solopreneur-ghostwriter:learn-voice
```

You can also type what you want, for example "learn my templates" or "add this as my parking template". Each skill shows you what it plans to save. Nothing is saved until you say yes.

If the skill asks where the bridge's `store` folder is, give it the path of `whatsapp-bridge/store` in this repo. To skip the question, set `WHATSAPP_STORE_DIR` to that path before you start `claude`.

Use the same `GHOSTWRITER_HOME` here as you give the bridge (see step 4), or leave it unset in both places.

Run both skills again after a few months, or when drafts stop sounding like you.

### 3. Turn on drafts in the bridge

Set these in the environment where the bridge starts, then restart the bridge:

| Variable | Value |
|---|---|
| `WHATSAPP_DRAFTS` | `1` to turn drafts on. |
| `WHATSAPP_DRAFT_COMMAND` | The absolute path of the runner: `/path/to/whatsapp-mcp/ghostwriter/bin/draft`. The file must be executable. |
| `WHATSAPP_DRAFT_DELAY_SECONDS` | How long the bridge waits after the last customer message before it drafts. Default 90. Allowed range: 5 to 3600 seconds. |
| `WHATSAPP_DRAFTS_PER_HOUR` | The most drafts in one hour. Default 30. |
| `WHATSAPP_DRAFT_CHATS` | A comma-separated list of chats that get drafts. If you leave it empty, every one-to-one chat gets drafts. Start with one chat that you use for tests. |
| `WHATSAPP_DRAFT_SKIP_CHATS` | Chats that never get drafts, for example family and friends. |
| `WHATSAPP_READ_ONLY` | Keep this at `1`. |

For the chat lists, write a phone number with country code and no `+`, for example `6591234567`, or a `...@lid` address. Group chats are not allowed. The bridge will not start if an entry is not valid.

### 4. Settings for the runner

These are optional. Set them in the same place as the bridge variables.

| Variable | What it does |
|---|---|
| `GHOSTWRITER_MODEL` | The Claude model used for drafts. Default `sonnet`. |
| `CLAUDE_BIN` | The path of the `claude` program. Set it if the bridge cannot find `claude`. |
| `GHOSTWRITER_PYTHON` | The path of a Python 3.11 or newer. Set it if the bridge starts from `launchd` or another place with a short `PATH`. |
| `GHOSTWRITER_HOME` | The folder for your voice, templates, facts and draft log. Default: a `ghostwriter` folder inside the bridge's `store` folder. |
| `GHOSTWRITER_CLAUDE_TIMEOUT` | How many seconds the runner waits for the model. |

## Ask for a draft by hand

You can ask for a draft for one chat at any time. The bridge writes a new token to `store/api_token` each time it starts. Replace the chat address with the customer's:

```bash
curl -s -X POST http://127.0.0.1:8080/api/draft \
  -H "Authorization: Bearer $(cat /path/to/whatsapp-mcp/whatsapp-bridge/store/api_token)" \
  -H "Content-Type: application/json" \
  -d '{"chat_jid": "6591234567@s.whatsapp.net"}'
```

The answer is "draft sent to your own chat" or "no reply needed". It returns an error if drafts are not turned on, if the chat is your own chat, or if you reached the hourly limit.

## Files and privacy

The data folder is `GHOSTWRITER_HOME`. If you did not set it, it is `ghostwriter` inside the bridge's `store` folder.

| File | What it holds |
|---|---|
| `voice.md` | How you write, with a few of your own replies as examples. |
| `templates.md` | Replies you send again and again, with `{placeholders}` for the parts that change. |
| `facts.md` | Facts drafts can use: address, hours, prices, policies. Bank details are in it only if you said yes. |
| `drafts.jsonl` | A log with one line for each draft the runner made. |

You can open and edit the three Markdown files in any text editor. The skills keep your edits when you run them again. The runner reads at most 12 000 characters from each file, so keep them short.

Everything stays on your computer, with one exception: for each draft, the runner sends the file contents and the recent messages of that chat to Claude. Do not put anything in these files that you would not want Claude to see.

## Cost

Ghostwriter makes one model call for each burst of customer messages, after the delay. It makes no calls when the shop is quiet. The two learning skills use a small summary of your history, not the full history, so they cost little.

## Troubleshooting

**No drafts arrive.**
1. Check that `WHATSAPP_DRAFTS=1` is set and that you restarted the bridge.
2. Check `WHATSAPP_DRAFT_CHATS` and `WHATSAPP_DRAFT_SKIP_CHATS`. The customer's chat must be allowed and not skipped.
3. Wait for the delay. The default is 90 seconds after the last message.
4. Look in the bridge log for `Draft for ... failed`. The text after it gives the cause. A common cause is that `claude` or `python3` is not found. Set `CLAUDE_BIN` or `GHOSTWRITER_PYTHON`.
5. Try the `curl` command above. It shows the error directly.

**Drafts do not sound like you.** Run `/solopreneur-ghostwriter:learn-voice` again. Check `voice.md` and fix anything that is wrong.

**A draft uses the wrong price or time.** Open `facts.md` and `templates.md` and correct the lines. Run `/solopreneur-ghostwriter:learn-templates` again to add what is missing.

**You do not get a notification for drafts.** WhatsApp is quiet for messages that you send to yourself. Pin the "Message yourself" chat, and turn on notifications for it on your phone.
