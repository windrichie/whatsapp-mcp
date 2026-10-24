package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

var (
	ownPN  = types.NewJID("6500000000", types.DefaultUserServer)
	ownLID = types.NewJID("555555555555555", types.HiddenUserServer)
)

type draftHarness struct {
	*drafter
	runs   atomic.Int32
	mu     sync.Mutex
	sent   []string
	chats  []types.JID // chats the draft command was run for
	result draftResult
	runErr error
	gate   chan struct{} // when set, run waits for it
}

func newDraftHarness(t *testing.T, delay time.Duration) *draftHarness {
	h := &draftHarness{result: draftResult{Reply: "Hi Alice, yes we are open!", Note: "check the date"}}
	resolver := testResolver()
	h.drafter = &drafter{
		delay:   delay,
		limiter: newRateLimiter(100, time.Hour),
		only:    map[string]bool{},
		skip:    map[string]bool{},
		isSelf: func(jid types.JID) bool {
			jid = jid.ToNonAD()
			return jid == ownPN || jid == ownLID
		},
		phoneOf:  resolver.phoneJID,
		describe: resolver.name,
		run: func(ctx context.Context, chat types.JID) (draftResult, error) {
			h.runs.Add(1)
			h.mu.Lock()
			h.chats = append(h.chats, chat)
			h.mu.Unlock()
			if h.gate != nil {
				<-h.gate
			}
			return h.result, h.runErr
		},
		send: func(_ context.Context, text string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.sent = append(h.sent, text)
			return nil
		},
		logf:         t.Logf,
		pending:      map[types.JID]*pendingDraft{},
		lastOutgoing: map[types.JID]time.Time{},
		busy:         make(chan struct{}, 1),
	}
	return h
}

func (h *draftHarness) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sent...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBurstOfMessagesGivesOneDraft(t *testing.T) {
	h := newDraftHarness(t, 60*time.Millisecond)
	for _, text := range []string{"hi", "are you open", "on sunday?"} {
		h.Incoming(aliceLID, text, time.Now())
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "draft", func() bool { return len(h.messages()) == 2 })
	time.Sleep(100 * time.Millisecond)
	if n := h.runs.Load(); n != 1 {
		t.Fatalf("draft command ran %d times, want 1", n)
	}
	msgs := h.messages()
	if !strings.Contains(msgs[0], "Alice Tan") || !strings.Contains(msgs[0], "“on sunday?”") || !strings.Contains(msgs[0], "⚠️ check the date") {
		t.Errorf("unexpected header: %q", msgs[0])
	}
	if msgs[1] != "Hi Alice, yes we are open!" {
		t.Errorf("draft message = %q, want the reply alone", msgs[1])
	}
}

func TestOwnerReplyCancelsDraft(t *testing.T) {
	h := newDraftHarness(t, 50*time.Millisecond)
	h.Incoming(aliceLID, "hello?", time.Now())
	h.Outgoing(alicePN) // she answered from her phone; same person, phone-number JID
	time.Sleep(150 * time.Millisecond)
	if h.runs.Load() != 0 || len(h.messages()) != 0 {
		t.Fatal("a draft was made although the owner had already replied")
	}
}

func TestDraftDroppedIfOwnerRepliesWhileDrafting(t *testing.T) {
	h := newDraftHarness(t, 10*time.Millisecond)
	h.gate = make(chan struct{})
	h.Incoming(aliceLID, "hello?", time.Now())
	waitFor(t, "draft command to start", func() bool { return h.runs.Load() == 1 })
	h.Outgoing(aliceLID)
	close(h.gate)
	time.Sleep(50 * time.Millisecond)
	if len(h.messages()) != 0 {
		t.Fatal("draft sent although the owner replied while it was being written")
	}
}

func TestOnlyOneToOneCustomerChatsGetDrafts(t *testing.T) {
	h := newDraftHarness(t, time.Hour)
	for _, chat := range []types.JID{
		types.NewJID("120363025246125486", types.GroupServer),
		types.NewJID("status", types.BroadcastServer),
		types.NewJID("120363111111111111", types.NewsletterServer),
		ownPN, ownLID, // "Message yourself", where the drafts themselves arrive
	} {
		h.Incoming(chat, "hi", time.Now())
	}
	h.Incoming(aliceLID, "an old message", time.Now().Add(-25*time.Hour))
	if len(h.pending) != 0 {
		t.Fatalf("pending drafts for chats that should be ignored: %v", h.pending)
	}

	// Filters accept phone numbers for chats keyed by LID
	h.skip = map[string]bool{alicePN.String(): true}
	h.Incoming(aliceLID, "hi", time.Now())
	h.only = map[string]bool{bobPN.String(): true}
	h.skip = map[string]bool{}
	h.Incoming(strangerLD, "hi", time.Now())
	h.Incoming(bobLID, "hi", time.Now())
	if len(h.pending) != 1 || h.pending[bobPN] == nil {
		t.Fatalf("filters not applied: %v", h.pending)
	}
}

func TestDraftTextIsCleanedAndEmptyMeansNoReply(t *testing.T) {
	h := newDraftHarness(t, time.Hour)
	h.result = draftResult{Reply: "  \n\n", Note: "spam"}
	if sent, err := h.Draft(context.Background(), aliceLID, "", true); sent || err != nil {
		t.Fatalf("empty reply: sent=%v err=%v", sent, err)
	}
	h.result = draftResult{Reply: "Line one\nLine two‮\x1b[31m" + strings.Repeat("x", maxDraftRunes), Note: "multi\nline"}
	if sent, err := h.Draft(context.Background(), aliceLID, "", true); !sent || err != nil {
		t.Fatalf("Draft: sent=%v err=%v", sent, err)
	}
	msgs := h.messages()
	if strings.ContainsAny(msgs[1], "‮\x1b") || !strings.HasPrefix(msgs[1], "Line one\nLine two") {
		t.Errorf("draft not cleaned: %q", msgs[1][:40])
	}
	if n := len([]rune(msgs[1])); n != maxDraftRunes {
		t.Errorf("draft length = %d runes, want %d", n, maxDraftRunes)
	}
	if !strings.Contains(msgs[0], "⚠️ multi line") || strings.Contains(msgs[0], "↪") {
		t.Errorf("unexpected header for a manual draft: %q", msgs[0])
	}
}

func TestDraftRateLimitAndErrors(t *testing.T) {
	h := newDraftHarness(t, time.Hour)
	h.limiter = newRateLimiter(1, time.Hour)
	if _, err := h.Draft(context.Background(), aliceLID, "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Draft(context.Background(), aliceLID, "", true); !errors.Is(err, errDraftRateLimited) {
		t.Errorf("second draft: %v, want rate limit", err)
	}
	h.limiter = newRateLimiter(10, time.Hour)
	h.runErr = errors.New("claude failed")
	if sent, err := h.Draft(context.Background(), aliceLID, "", true); sent || err == nil {
		t.Errorf("failing command: sent=%v err=%v", sent, err)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "draft")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommandDraftRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	ctx := context.Background()
	store := t.TempDir()

	ok := writeScript(t, `printf '{"reply":"for %s in %s","note":""}' "$1" "$WHATSAPP_STORE_DIR"`)
	res, err := commandDraftRunner(ok, store)(ctx, aliceLID)
	if err != nil || res.Reply != "for "+aliceLID.String()+" in "+store {
		t.Errorf("runner = %+v, %v", res, err)
	}

	for name, body := range map[string]string{
		"invalid json":  `echo not json`,
		"unknown field": `echo '{"reply":"x","note":"","send_to":"someone"}'`,
		"failure":       `echo boom >&2; exit 3`,
		"too much":      `head -c 70000 /dev/zero | tr '\0' 'a'`,
		"two objects":   `echo '{"reply":"a","note":""} {"reply":"b","note":""}'`,
	} {
		if _, err := commandDraftRunner(writeScript(t, body), store)(ctx, aliceLID); err == nil {
			t.Errorf("%s: no error", name)
		}
	}

	// A hung command (and its children) is killed at the deadline
	slow := writeScript(t, `sleep 30 & wait`)
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := commandDraftRunner(slow, store)(tctx, aliceLID); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("slow command: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %s", elapsed)
	}
}

func TestDraftConfig(t *testing.T) {
	t.Setenv("WHATSAPP_STORE_DIR", t.TempDir())
	t.Setenv("WHATSAPP_DRAFTS", "1")
	exe := writeScript(t, "true")
	notExe := filepath.Join(t.TempDir(), "draft")
	os.WriteFile(notExe, []byte("x"), 0o600)

	for name, env := range map[string]map[string]string{
		"no command":       {"WHATSAPP_DRAFT_COMMAND": ""},
		"relative command": {"WHATSAPP_DRAFT_COMMAND": "bin/draft"},
		"not executable":   {"WHATSAPP_DRAFT_COMMAND": notExe},
		"group in filter":  {"WHATSAPP_DRAFT_COMMAND": exe, "WHATSAPP_DRAFT_CHATS": "120363025246125486@g.us"},
		"delay too short":  {"WHATSAPP_DRAFT_COMMAND": exe, "WHATSAPP_DRAFT_DELAY_SECONDS": "1"},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := loadConfig(); err == nil {
				t.Error("loadConfig accepted an invalid draft setting")
			}
		})
	}

	t.Setenv("WHATSAPP_DRAFT_COMMAND", exe)
	t.Setenv("WHATSAPP_DRAFT_CHATS", "+6591234567, "+aliceLID.String())
	t.Setenv("WHATSAPP_DRAFT_DELAY_SECONDS", "30")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Drafts || cfg.DraftDelay != 30*time.Second || !cfg.DraftChats[alicePN.String()] || !cfg.DraftChats[aliceLID.String()] {
		t.Errorf("draft config not loaded: %+v", cfg)
	}
}

func TestAPIDraft(t *testing.T) {
	cfg := testConfig(t)
	store := testStore(t, cfg)
	if rec, _ := doAPI(t, newAPIHandler(nil, store, cfg, nil), apiCase{path: "/api/draft", body: `{"chat_jid":"` + aliceLID.String() + `"}`}); rec.Code != 409 {
		t.Errorf("drafts disabled: status %d", rec.Code)
	}

	h := newDraftHarness(t, time.Hour)
	api := newAPIHandler(nil, store, cfg, h.drafter)
	cases := map[string]int{
		`{"chat_jid":"` + aliceLID.String() + `"}`: 200,
		`{"chat_jid":"6591234567"}`:                200,
		`{"chat_jid":"120363025246125486@g.us"}`:   400,
		`{"chat_jid":"` + ownPN.String() + `"}`:    400,
		`{"chat_jid":"status@broadcast"}`:          400,
		`{"chat_jid":"x","extra":1}`:               400,
	}
	for body, want := range cases {
		if rec, _ := doAPI(t, api, apiCase{path: "/api/draft", body: body}); rec.Code != want {
			t.Errorf("%s: status %d, want %d (%s)", body, rec.Code, want, rec.Body.String())
		}
	}
	if got := len(h.messages()); got != 4 {
		t.Errorf("sent %d messages, want 4 (two drafts)", got)
	}

	h.limiter = newRateLimiter(1, time.Hour)
	h.limiter.Allow(time.Now())
	if rec, _ := doAPI(t, api, apiCase{path: "/api/draft", body: `{"chat_jid":"` + aliceLID.String() + `"}`}); rec.Code != 429 {
		t.Errorf("rate limited: status %d", rec.Code)
	}
}

func TestAPIDraftByPhoneNumberUsesTheLIDChat(t *testing.T) {
	cfg := testConfig(t)
	store := testStore(t, cfg)
	store.StoreChat(aliceLID.String(), "Alice Tan", time.Now()) // stored under the LID only
	store.StoreChat(bobPN.String(), "Bob", time.Now())          // an older chat stored under the phone number
	h := newDraftHarness(t, time.Hour)
	s := &apiServer{store: store, cfg: cfg, limiter: newRateLimiter(1000, time.Minute), lids: testResolver().lids, drafts: h.drafter}
	api := s.routes()
	for _, phone := range []string{alicePN.User, bobPN.User, strangerPN.User} {
		if rec, _ := doAPI(t, api, apiCase{path: "/api/draft", body: `{"chat_jid":"` + phone + `"}`}); rec.Code != 200 {
			t.Fatalf("%s: status %d (%s)", phone, rec.Code, rec.Body.String())
		}
	}
	want := []types.JID{aliceLID, bobPN, strangerLD} // LID when the phone-number chat doesn't exist
	if len(h.chats) != len(want) {
		t.Fatalf("draft command ran for %v, want %v", h.chats, want)
	}
	for i := range want {
		if h.chats[i] != want[i] {
			t.Errorf("draft %d ran for %s, want %s", i, h.chats[i], want[i])
		}
	}
}
