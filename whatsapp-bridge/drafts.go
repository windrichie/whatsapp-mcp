package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Draft replies. When a customer writes in a one-to-one chat, the bridge waits until
// they stop typing, asks an external command for a draft reply and sends the draft to
// the account's own "Message yourself" chat. The owner copies it into the customer's
// chat and sends it themselves. This is the only code that sends while
// WHATSAPP_READ_ONLY is set, and it can only ever reach the account itself: the AI
// that writes the draft has no way to send anything.

const (
	defaultDraftDelay    = 90 * time.Second
	defaultDraftsPerHour = 30
	draftTimeout         = 3 * time.Minute
	maxDraftAge          = 24 * time.Hour // older messages (after downtime) get no draft
	maxDraftOutputBytes  = 64 << 10
	maxDraftRunes        = 4000
)

var errDraftRateLimited = errors.New("draft rate limit reached; try again later")

// draftResult is what the draft command prints as JSON: the suggested reply (empty
// when the customer needs no reply) and a short note for the owner, such as what to
// check before sending.
type draftResult struct {
	Reply string `json:"reply"`
	Note  string `json:"note"`
}

type pendingDraft struct {
	chat     types.JID
	timer    *time.Timer
	gen      uint64
	lastText string
}

// drafter turns bursts of customer messages into draft replies.
type drafter struct {
	delay   time.Duration
	limiter *rateLimiter
	only    map[string]bool // empty: every individual chat
	skip    map[string]bool

	isSelf   func(types.JID) bool
	phoneOf  func(context.Context, types.JID) types.JID
	describe func(context.Context, types.JID) string
	run      func(context.Context, types.JID) (draftResult, error)
	send     func(context.Context, string) error // always to the account's own chat
	logf     func(string, ...any)

	mu           sync.Mutex
	pending      map[types.JID]*pendingDraft
	lastOutgoing map[types.JID]time.Time
	busy         chan struct{} // one draft at a time
}

func newDrafter(cfg *Config, client *whatsmeow.Client, run func(context.Context, types.JID) (draftResult, error)) *drafter {
	resolver := newContactResolver(client)
	return &drafter{
		delay:   cfg.DraftDelay,
		limiter: newRateLimiter(cfg.DraftsPerHour, time.Hour),
		only:    cfg.DraftChats,
		skip:    cfg.DraftSkipChats,
		isSelf: func(jid types.JID) bool {
			jid = jid.ToNonAD()
			own := client.Store.ID
			return (own != nil && jid == own.ToNonAD()) || (!client.Store.LID.IsEmpty() && jid == client.Store.LID.ToNonAD())
		},
		phoneOf: resolver.phoneJID,
		describe: func(ctx context.Context, chat types.JID) string {
			name := resolver.name(ctx, chat)
			if pn := resolver.phoneJID(ctx, chat); !pn.IsEmpty() && pn.User != name {
				return fmt.Sprintf("%s (+%s)", name, pn.User)
			}
			if isNumber(name) {
				return "+" + name
			}
			return name
		},
		run: run,
		send: func(ctx context.Context, text string) error {
			own := client.Store.ID
			if own == nil {
				return errors.New("not logged in")
			}
			if ok, msg := sendWhatsAppMessage(client, own.ToNonAD(), text, nil); !ok {
				return errors.New(msg)
			}
			return nil
		},
		logf:         func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
		pending:      map[types.JID]*pendingDraft{},
		lastOutgoing: map[types.JID]time.Time{},
		busy:         make(chan struct{}, 1),
	}
}

// key identifies a person across their LID and phone-number JIDs, so a reply sent in
// either form cancels the pending draft.
func (d *drafter) key(ctx context.Context, chat types.JID) types.JID {
	chat = chat.ToNonAD()
	if pn := d.phoneOf(ctx, chat); !pn.IsEmpty() {
		return pn
	}
	return chat
}

func (d *drafter) listed(ctx context.Context, set map[string]bool, chat types.JID) bool {
	if set[chat.String()] {
		return true
	}
	pn := d.phoneOf(ctx, chat)
	return !pn.IsEmpty() && set[pn.String()]
}

// eligible reports whether messages in chat get drafts: one-to-one chats only, never
// the account's own chat, and only chats the filters allow.
func (d *drafter) eligible(ctx context.Context, chat types.JID) bool {
	chat = chat.ToNonAD()
	if !isIndividualServer(chat.Server) || d.isSelf(chat) || d.listed(ctx, d.skip, chat) {
		return false
	}
	return len(d.only) == 0 || d.listed(ctx, d.only, chat)
}

// Incoming records a customer message and restarts the quiet-period timer, so a burst
// of messages produces a single draft.
func (d *drafter) Incoming(chat types.JID, text string, sent time.Time) {
	ctx := context.Background()
	if time.Since(sent) > maxDraftAge || !d.eligible(ctx, chat) {
		return
	}
	key := d.key(ctx, chat)
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.pending[key]
	if p == nil {
		p = &pendingDraft{}
		d.pending[key] = p
	} else {
		p.timer.Stop()
	}
	p.gen++
	gen := p.gen
	p.chat, p.lastText = chat.ToNonAD(), text
	p.timer = time.AfterFunc(d.delay, func() { d.fire(key, gen) })
}

// Outgoing records that the owner wrote in the chat, which cancels its draft.
func (d *drafter) Outgoing(chat types.JID) {
	key := d.key(context.Background(), chat)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastOutgoing[key] = time.Now()
	if p := d.pending[key]; p != nil {
		p.timer.Stop()
		delete(d.pending, key)
	}
}

func (d *drafter) repliedSince(key types.JID, t time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.lastOutgoing[key].Before(t)
}

func (d *drafter) fire(key types.JID, gen uint64) {
	d.mu.Lock()
	p := d.pending[key]
	if p == nil || p.gen != gen {
		d.mu.Unlock()
		return
	}
	delete(d.pending, key)
	chat, lastText := p.chat, p.lastText
	d.mu.Unlock()

	sent, err := d.Draft(context.Background(), chat, lastText, false)
	switch {
	case err != nil:
		d.logf("Draft for %s failed: %s", sanitizeForLog(chat.String()), oneLine(err.Error(), 300))
	case sent:
		d.logf("Draft for %s sent to your own chat", sanitizeForLog(chat.String()))
	default:
		d.logf("No draft for %s (no reply needed, or already answered)", sanitizeForLog(chat.String()))
	}
}

// Draft asks the command for a reply and sends it to the account's own chat as two
// messages: a header saying who it is for, then the reply alone so it copies cleanly.
// Automatic drafts are dropped if the owner answered while the draft was being written.
func (d *drafter) Draft(ctx context.Context, chat types.JID, lastText string, manual bool) (bool, error) {
	d.busy <- struct{}{}
	defer func() { <-d.busy }()
	if !d.limiter.Allow(time.Now()) {
		return false, errDraftRateLimited
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, draftTimeout)
	defer cancel()
	res, err := d.run(ctx, chat)
	if err != nil {
		return false, err
	}
	reply := cleanDraftText(res.Reply)
	if reply == "" || (!manual && d.repliedSince(d.key(ctx, chat), started)) {
		return false, nil
	}
	if err := d.send(ctx, d.header(ctx, chat, lastText, res.Note)); err != nil {
		return false, err
	}
	return true, d.send(ctx, reply)
}

func (d *drafter) header(ctx context.Context, chat types.JID, lastText, note string) string {
	var b strings.Builder
	b.WriteString("✍️ Draft reply for " + oneLine(d.describe(ctx, chat), 80))
	if quote := oneLine(lastText, 120); quote != "" {
		b.WriteString("\n↪ “" + quote + "”")
	}
	if note := oneLine(note, 300); note != "" {
		b.WriteString("\n⚠️ " + note)
	}
	b.WriteString("\nCopy the next message and paste it into the chat.")
	return b.String()
}

// cleanDraftText removes control and bidi characters (keeping line breaks) and caps
// the length of a draft.
func cleanDraftText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		return r
	}, s)
	return truncateRunes(strings.TrimSpace(s), maxDraftRunes)
}

// oneLine flattens text into a single line of at most n runes.
func oneLine(s string, n int) string {
	return truncateRunes(sanitizeForLog(strings.Join(strings.Fields(s), " ")), n)
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// cappedBuffer keeps at most limit bytes and remembers whether more was written.
type cappedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); len(p) > room {
		b.overflow = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// commandDraftRunner runs the configured draft command with the chat JID as its only
// argument and reads a draftResult from its standard output. Message text is never
// passed on the command line; the command reads the chat from the store itself.
func commandDraftRunner(command, storeDir string) func(context.Context, types.JID) (draftResult, error) {
	return func(ctx context.Context, chat types.JID) (draftResult, error) {
		cmd := exec.CommandContext(ctx, command, chat.String())
		cmd.Env = append(os.Environ(), "WHATSAPP_STORE_DIR="+storeDir, "GHOSTWRITER_CHAT_JID="+chat.String())
		configureProcessGroup(cmd)
		// Don't wait forever for output pipes that a leftover child keeps open
		cmd.WaitDelay = 5 * time.Second
		stdout := &cappedBuffer{limit: maxDraftOutputBytes}
		stderr := &cappedBuffer{limit: 4 << 10}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return draftResult{}, fmt.Errorf("draft command timed out after %s", draftTimeout)
			}
			return draftResult{}, fmt.Errorf("draft command failed (%v): %s", err, oneLine(stderr.String(), 300))
		}
		if stdout.overflow {
			return draftResult{}, errors.New("draft command printed too much output")
		}
		var res draftResult
		dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&res); err != nil {
			return draftResult{}, fmt.Errorf("draft command printed invalid JSON: %v", err)
		}
		if dec.More() {
			return draftResult{}, errors.New("draft command printed more than one JSON object")
		}
		return res, nil
	}
}
