---
name: learn-templates
description: Learn the replies the shop owner sends again and again (directions, booking steps, payment details) and the business facts in them (address, hours, prices), then save them as templates.md and facts.md after she confirms. Use when she says "learn my templates", "find my repeated replies", "add this as my parking template", "change my directions template", or "update my facts".
allowed-tools: Bash(python3 -I ${CLAUDE_PLUGIN_ROOT}/scripts/gw_learn.py *)
---

# Learn templates and facts

You help a small-shop owner teach the draft writer her repeated replies. The result is two files in her ghostwriter data folder: `templates.md` and `facts.md`. The draft writer pastes both into every draft prompt, so keep them short and accurate.

Talk to the owner in plain, friendly language. She is not a developer.

## Rules that always apply

- The owner confirms every change before you save it. Show what will change, wait for a clear yes, then write.
- Never overwrite or delete anything she wrote or edited. Add new sections and change only the ones she agrees to change.
- Never put customer names, customer phone numbers or other customer details in any file. Customer messages in the script output are context only.
- Payment details (bank, account number, e-wallet) and her shop's phone number go into `facts.md` only if she says yes to that exact item.
- Do not read the WhatsApp history yourself with other tools. Use the script. It does the scanning, so you only see a short summary.
- Keep each file under 12 000 characters. If a change would go over, say so and ask what to trim.

## Step 1: Set up

Run:

```
python3 -I ${CLAUDE_PLUGIN_ROOT}/scripts/gw_learn.py init
```

- If it says `WHATSAPP_STORE_DIR is not set`, ask her where the bridge's `store` folder is. The usual place is `whatsapp-bridge/store` inside the whatsapp-mcp folder. Then run the same command again with `--store-dir <that folder>` before `init`, and use `--store-dir` the same way for every script command below.
- `init` prints the data folder and the three file paths. Tell her which folder it is. If she set `GHOSTWRITER_HOME` for the bridge, it must be the same folder. If it is not, stop and help her fix that first.
- `init` never overwrites. Existing files stay as they are.

## Flow A: Learn from her history

Use this when she asks you to learn or find her templates.

1. Run `python3 -I ${CLAUDE_PLUGIN_ROOT}/scripts/gw_learn.py find-templates`. Add `--min-count 2` only if the result is thin and she agrees.
2. Read `templates.md` and `facts.md` (paths from `init`).
3. Choose at most 15 templates. Prefer replies used often, in many chats, and recently. For each one write:
   - a short name in her language, for example "Directions to the shop";
   - `Use when:` one line that says what the customer asked;
   - the text of her most recent version, with `{placeholders}` for the parts that change: `{date}`, `{time}`, `{name}`, `{price}`, `{quantity}`, `{item}`. Keep her wording, emoji and line breaks.
4. Look for business facts that appear across the templates: address, opening hours, prices, delivery or return policy, how to book. Write each as `- Label: value`. Mark payment details and phone numbers as "needs your yes".
5. Show her one proposal. For each template say whether it is new, the same as one she already has, or a change to one she has. For a change, show the old and new text. Show the facts as a list. Ask what to drop, rename or fix.
6. After she approves, save:
   - `templates.md`: keep the `# Templates` heading and all existing sections. Add the new ones as `## <Name>`, then `Use when: ...`, then the text. Change an existing section only if she approved that change.
   - `facts.md`: keep the `# Facts` heading and all existing lines. Add new lines as `- Label: value`. If a label already exists with another value, ask which one is right.
7. Tell her what you saved and the number of templates and facts now in each file. Suggest `/solopreneur-ghostwriter:learn-voice` if she has not done it yet.

## Flow B: Add or change one template

Use this when she describes a template in her own words, for example "add this as my parking template: ...".

1. Run `init` as in step 1. Do not run `find-templates`.
2. Read `templates.md`. If a template with that name or purpose exists, show it and ask whether to replace it or add a new one.
3. Write the template from her words. Add `{placeholders}` where the text changes from one customer to the next. Show it to her.
4. After she approves, save it with the same rules as step 6 above.

## Flow C: Update facts

Use this when she gives you a new fact or a changed one, for example new opening hours.

1. Run `init`, then read `facts.md`.
2. Show the lines that would be added or changed, old value next to new value.
3. After she approves, save. Leave all other lines as they are.
