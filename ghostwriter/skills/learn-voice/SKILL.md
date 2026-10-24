---
name: learn-voice
description: Learn how the shop owner writes to customers (languages she mixes, tone, length, greetings, emoji, favourite words) from her own past replies, and save a short voice.md style guide after she confirms it. Use when she says "learn how I write", "learn my voice", "update my voice", or "my drafts don't sound like me".
allowed-tools: Bash(python3 -I ${CLAUDE_PLUGIN_ROOT}/scripts/gw_learn.py *)
---

# Learn her voice

You help a small-shop owner teach the draft writer how she writes. The result is one file, `voice.md`, in her ghostwriter data folder. The draft writer pastes it into every draft prompt, so keep it short.

Talk to the owner in plain, friendly language. She is not a developer.

## Rules that always apply

- The owner confirms the file before you save it. Show it, apply her corrections, then write.
- Never overwrite her manual edits. On a re-run, show what would change and keep her edits unless she agrees to replace them.
- Example replies come from her own replies only. Never copy customer text into the file. Never put customer names, phone numbers, street addresses or order numbers in it. Pick replies that have none, or leave those parts out.
- Do not read the WhatsApp history yourself with other tools. Use the script. It does the scanning, so you only see statistics and a small sample.
- Keep the file under 12 000 characters.

## Step 1: Set up

Run:

```
python3 -I ${CLAUDE_PLUGIN_ROOT}/scripts/gw_learn.py init
```

- If it says `WHATSAPP_STORE_DIR is not set`, ask her where the bridge's `store` folder is. The usual place is `whatsapp-bridge/store` inside the whatsapp-mcp folder. Then run the same command again with `--store-dir <that folder>` before `init`, and use `--store-dir` the same way for every script command below.
- `init` prints the data folder and the file paths. Tell her which folder it is. If she set `GHOSTWRITER_HOME` for the bridge, it must be the same folder. If it is not, stop and help her fix that first.
- `init` never overwrites. An existing `voice.md` stays as it is.

## Step 2: Read her writing

1. Run `python3 -I ${CLAUDE_PLUGIN_ROOT}/scripts/gw_learn.py voice-sample`. Add `--n 100` for a larger sample if she writes in several styles.
2. Read the Stats section and the sample replies. The sample leaves out replies she sends often, because those belong in templates.
3. Read the current `voice.md`. If it only has the empty headings, it is a first run.

## Step 3: Ask one question

Ask which language she wants the draft notes in. The notes are the short line that comes with each draft, for example "check the date before you send". Suggest the language she writes most. Put the answer in the `Notes language:` line.

## Step 4: Write the guide

Use exactly this layout. Keep the first two lines as they are. Write each section as 1 to 4 short lines or bullets, based on the statistics and the sample.

```
# Voice
Notes language: <language>

## Languages and mixing
## Tone
## Length
## Greetings and sign-offs
## Emoji and punctuation
## Words and phrases she uses
## Things she avoids
## Example replies
- "<one of her real replies>"
```

- Languages and mixing: which languages she uses and how she mixes them, for example Indonesian with English product words.
- Tone: warm, formal, joking, brisk. Say how she addresses customers, for example "kak".
- Length: the usual length in sentences or words, from the median in the Stats.
- Greetings and sign-offs: how she opens and closes, from the top first and last words and the sample.
- Emoji and punctuation: which emoji she uses and how often, whether she uses "!" and "?" and "..." and so on.
- Words and phrases she uses: words from the top-words list and the sample that carry her style.
- Things she avoids: only what the data shows, such as "no emoji in price messages". Do not guess. If there is nothing clear, write "Nothing clear yet."
- Example replies: 6 to 10 short replies copied from the sample, with the exact wording and emoji she used. One reply per line, in quotes. Choose replies that show different situations. Remove or avoid anything that names a customer or contains a number that belongs to a customer.

## Step 5: Confirm and save

1. Show her the whole guide. Say which replies you used as examples, and ask her to correct anything that does not sound like her.
2. Apply her corrections.
3. If `voice.md` already has content from before, show what changes section by section. Keep every line she edited unless she says to replace it. Save only the sections she approves.
4. Write `voice.md` only after a clear yes. Tell her the folder and that she can open the file and edit it any time.
5. Tell her to run this skill again after a few months, or when her drafts stop sounding like her.
