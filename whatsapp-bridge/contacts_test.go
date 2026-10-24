package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type fakeContacts map[types.JID]types.ContactInfo

func (f fakeContacts) GetContact(_ context.Context, user types.JID) (types.ContactInfo, error) {
	return f[user], nil
}

func (f fakeContacts) GetAllContacts(context.Context) (map[types.JID]types.ContactInfo, error) {
	return f, nil
}

// fakeLIDs maps LID -> phone-number JID.
type fakeLIDs map[types.JID]types.JID

func (f fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	return f[lid], nil
}

func (f fakeLIDs) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	for lid, p := range f {
		if p == pn {
			return lid, nil
		}
	}
	return types.EmptyJID, nil
}

func (f fakeLIDs) GetManyLIDsForPNs(ctx context.Context, pns []types.JID) (map[types.JID]types.JID, error) {
	out := map[types.JID]types.JID{}
	for _, pn := range pns {
		if lid, _ := f.GetLIDForPN(ctx, pn); !lid.IsEmpty() {
			out[pn] = lid
		}
	}
	return out, nil
}

var (
	alicePN    = types.NewJID("6591234567", types.DefaultUserServer)
	aliceLID   = types.NewJID("111111111111111", types.HiddenUserServer)
	strangerPN = types.NewJID("6599990000", types.DefaultUserServer)
	strangerLD = types.NewJID("222222222222222", types.HiddenUserServer)
	unknownLID = types.NewJID("333333333333333", types.HiddenUserServer)
	bobPN      = types.NewJID("447700900123", types.DefaultUserServer)
	bobLID     = types.NewJID("444444444444444", types.HiddenUserServer)
)

func testResolver() contactResolver {
	return contactResolver{
		contacts: fakeContacts{
			alicePN: {Found: true, FullName: "Alice Tan", PushName: "Alice"},
			bobPN:   {Found: true, FullName: "Bob"},
			// Only a sender-chosen push name: must not be used as a name
			strangerPN: {Found: true, PushName: "Mom"},
		},
		lids: fakeLIDs{aliceLID: alicePN, strangerLD: strangerPN, bobLID: bobPN},
	}
}

func TestContactResolverNamesLIDChatsFromAddressBook(t *testing.T) {
	r, ctx := testResolver(), context.Background()
	cases := map[types.JID]string{
		aliceLID:   "Alice Tan",  // LID -> phone -> address book
		alicePN:    "Alice Tan",  // phone -> address book
		strangerLD: "6599990000", // no saved name: phone number, never the push name
		unknownLID: "333333333333333",
	}
	for jid, want := range cases {
		if got := r.name(ctx, jid); got != want {
			t.Errorf("name(%s) = %q, want %q", jid, got, want)
		}
	}
	if got := r.phoneJID(ctx, aliceLID); got != alicePN {
		t.Errorf("phoneJID(aliceLID) = %s", got)
	}
}

func TestSyncContactsFillsTableAndRenamesChats(t *testing.T) {
	cfg := testConfig(t)
	store := testStore(t, cfg)
	now := time.Now()
	for jid, name := range map[string]string{
		aliceLID.String():         "111111111111111", // placeholder from an older version
		strangerLD.String():       "222222222222222",
		unknownLID.String():       "333333333333333",
		"120363025246125486@g.us": "Family",
	} {
		if err := store.StoreChat(jid, name, now); err != nil {
			t.Fatal(err)
		}
	}

	renamed, err := store.SyncContacts(context.Background(), testResolver())
	if err != nil {
		t.Fatal(err)
	}
	if renamed != 2 {
		t.Errorf("renamed = %d, want 2", renamed)
	}
	names := map[string]string{}
	rows, _ := store.db.Query("SELECT jid, name FROM chats")
	for rows.Next() {
		var jid, name string
		rows.Scan(&jid, &name)
		names[jid] = name
	}
	rows.Close()
	want := map[string]string{
		aliceLID.String():         "Alice Tan",
		strangerLD.String():       "6599990000",
		unknownLID.String():       "333333333333333",
		"120363025246125486@g.us": "Family",
	}
	for jid, name := range want {
		if names[jid] != name {
			t.Errorf("chat %s name = %q, want %q", jid, names[jid], name)
		}
	}

	contacts := map[string][2]sql.NullString{}
	rows, _ = store.db.Query("SELECT jid, phone, name FROM contacts")
	for rows.Next() {
		var jid string
		var phone, name sql.NullString
		rows.Scan(&jid, &phone, &name)
		contacts[jid] = [2]sql.NullString{phone, name}
	}
	rows.Close()
	check := func(jid types.JID, phone, name string) {
		t.Helper()
		got, ok := contacts[jid.String()]
		if !ok || got[0].String != phone || got[1].String != name {
			t.Errorf("contacts[%s] = %v (present %v), want phone %q name %q", jid, got, ok, phone, name)
		}
	}
	check(alicePN, "6591234567", "Alice Tan")
	check(aliceLID, "6591234567", "Alice Tan")
	check(bobLID, "447700900123", "Bob") // no chat yet, but a group sender can still be named
	check(strangerLD, "6599990000", "")  // push name is not stored
	if _, ok := contacts[unknownLID.String()]; ok {
		t.Error("a LID with no phone and no name should not get a contacts row")
	}

	// A second sync replaces the table instead of piling up rows
	if _, err := store.SyncContacts(context.Background(), testResolver()); err != nil {
		t.Fatal(err)
	}
	var count int
	store.db.QueryRow("SELECT count(*) FROM contacts").Scan(&count)
	if count != len(contacts) {
		t.Errorf("contacts rows after resync = %d, want %d", count, len(contacts))
	}
}

func TestRealNamesSurviveAnEmptyContactStore(t *testing.T) {
	cfg := testConfig(t)
	store := testStore(t, cfg)
	store.StoreChat(aliceLID.String(), "Alice Tan", time.Now())
	chatName := func() string {
		var name string
		store.db.QueryRow("SELECT name FROM chats WHERE jid = ?", aliceLID.String()).Scan(&name)
		return name
	}

	// Right after linking again: no contacts and no LID mappings yet
	empty := contactResolver{contacts: fakeContacts{}, lids: fakeLIDs{}}
	if renamed, err := store.SyncContacts(context.Background(), empty); err != nil || renamed != 0 {
		t.Errorf("SyncContacts with an empty store renamed %d chats (%v)", renamed, err)
	}
	// A live message in the meantime names the chat by its number
	store.StoreChat(aliceLID.String(), empty.name(context.Background(), aliceLID), time.Now())
	if got := chatName(); got != "Alice Tan" {
		t.Errorf("real name replaced by a number: %q", got)
	}

	// Once the address book arrives, a better name still replaces the old one
	renamedContacts := testResolver()
	renamedContacts.contacts.(fakeContacts)[alicePN] = types.ContactInfo{Found: true, FullName: "Alice T."}
	if _, err := store.SyncContacts(context.Background(), renamedContacts); err != nil {
		t.Fatal(err)
	}
	if got := chatName(); got != "Alice T." {
		t.Errorf("address-book rename not applied: %q", got)
	}
}

func chatTime(t *testing.T, store *MessageStore, jid string) time.Time {
	t.Helper()
	var ts time.Time
	if err := store.db.QueryRow("SELECT last_message_time FROM chats WHERE jid = ?", jid).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestStoreChatKeepsNewestTime(t *testing.T) {
	cfg := testConfig(t)
	store := testStore(t, cfg)
	jid := aliceLID.String()
	newer := time.Date(2026, 10, 24, 20, 26, 2, 0, time.FixedZone("SGT", 8*3600))
	older := newer.Add(-5 * time.Hour)

	store.StoreChat(jid, "Alice Tan", newer)
	store.StoreChat(jid, "", older) // history sync arriving after a live message
	if got := chatTime(t, store, jid); !got.Equal(newer) {
		t.Errorf("older history time replaced the newer one: %v", got)
	}
	var name string
	store.db.QueryRow("SELECT name FROM chats WHERE jid = ?", jid).Scan(&name)
	if name != "Alice Tan" {
		t.Errorf("empty name replaced the stored one: %q", name)
	}

	// Compare instants, not strings: 13:00 UTC is later than 20:26 SGT (12:26 UTC)
	later := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
	store.StoreChat(jid, "Alice Tan", later)
	if got := chatTime(t, store, jid); !got.Equal(later) {
		t.Errorf("newer time in another zone was ignored: %v", got)
	}
}

func TestMigrationRepairsStaleChatTimes(t *testing.T) {
	cfg := testConfig(t)
	store := testStore(t, cfg)
	jid := aliceLID.String()
	stale := time.Date(2026, 10, 24, 15, 47, 54, 0, time.FixedZone("SGT", 8*3600))
	newest := stale.Add(4 * time.Hour)
	store.StoreChat(jid, "Alice Tan", newest)
	store.StoreMessage("M1", jid, "111111111111111", "hi", newest, false, MediaInfo{})
	// Simulate the old INSERT OR REPLACE moving the time backwards
	store.db.Exec("UPDATE chats SET last_message_time = ? WHERE jid = ?", stale, jid)
	store.Close()

	reopened, err := NewMessageStore(cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := chatTime(t, reopened, jid); !got.Equal(newest) {
		t.Errorf("stale chat time not repaired: %v", got)
	}
	var index string
	if err := reopened.db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'messages_chat_time'").Scan(&index); err != nil {
		t.Errorf("messages_chat_time index missing: %v", err)
	}
}

func TestLatestHistoryTimestamp(t *testing.T) {
	msg := func(ts uint64) *waHistorySync.HistorySyncMsg {
		return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{MessageTimestamp: proto.Uint64(ts)}}
	}
	// Oldest first, with a nil entry and a message without a timestamp
	msgs := []*waHistorySync.HistorySyncMsg{msg(100), nil, msg(300), {Message: &waWeb.WebMessageInfo{}}, msg(200)}
	if got := latestHistoryTimestamp(msgs); got.Unix() != 300 {
		t.Errorf("latestHistoryTimestamp = %d, want 300", got.Unix())
	}
	if got := latestHistoryTimestamp(nil); !got.IsZero() {
		t.Errorf("latestHistoryTimestamp(nil) = %v", got)
	}
}

func TestHistorySender(t *testing.T) {
	group := types.NewJID("120363025246125486", types.GroupServer)
	cases := []struct {
		chat types.JID
		msg  *waWeb.WebMessageInfo
		want string
	}{
		{group, &waWeb.WebMessageInfo{Participant: proto.String(aliceLID.String())}, aliceLID.User},
		{group, &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{Participant: proto.String(bobPN.String())}}, bobPN.User},
		{group, &waWeb.WebMessageInfo{}, ""}, // unknown, never the group's own ID
		{aliceLID, &waWeb.WebMessageInfo{}, aliceLID.User},
	}
	for _, c := range cases {
		if got := historySender(c.chat, c.msg); got != c.want {
			t.Errorf("historySender(%s, %v) = %q, want %q", c.chat, c.msg, got, c.want)
		}
	}
}

func TestOnlyHistorySyncRepairsGroupIDSenders(t *testing.T) {
	cfg := testConfig(t)
	store := testStore(t, cfg)
	group := "120363025246125486@g.us"
	now := time.Now()
	store.StoreChat(group, "Family", now)
	sender := func(id string) string {
		var s string
		store.db.QueryRow("SELECT sender FROM messages WHERE id = ?", id).Scan(&s)
		return s
	}

	// Rows written by older versions carry the group's ID as the sender
	store.db.Exec("INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('H1', ?, '120363025246125486', 'old', ?, 0), ('H2', ?, '120363025246125486', 'old', ?, 0)",
		group, now, group, now)

	// A live message reusing the ID must not take over the archived message
	store.StoreMessage("H1", group, "999999999999999", "overwritten", now, false, MediaInfo{})
	if got := sender("H1"); got != "120363025246125486" {
		t.Errorf("live message repaired/overwrote a broken row: sender %q", got)
	}
	// History sync, from the user's own phone, may repair it
	store.StoreHistoryMessage("H2", group, aliceLID.User, "old", now, false, MediaInfo{})
	if got := sender("H2"); got != aliceLID.User {
		t.Errorf("history sync did not repair the sender: %q", got)
	}
	// ...but not with an unknown sender, and not a row with a real sender
	store.StoreHistoryMessage("H1", group, "", "old", now, false, MediaInfo{})
	store.StoreHistoryMessage("H2", group, bobPN.User, "changed", now, false, MediaInfo{})
	if sender("H1") != "120363025246125486" || sender("H2") != aliceLID.User {
		t.Errorf("unexpected senders after repair attempts: H1=%q H2=%q", sender("H1"), sender("H2"))
	}
}

func TestAllowlistMatchesLIDAndPhone(t *testing.T) {
	cfg := testConfig(t)
	cfg.AllowAnyRecipient = false
	cfg.AllowedRecipients = map[string]bool{alicePN.String(): true, bobLID.String(): true}
	s := &apiServer{store: testStore(t, cfg), cfg: cfg, limiter: newRateLimiter(1000, time.Minute), lids: testResolver().lids.(fakeLIDs)}
	h := s.routes()

	cases := map[string]int{
		aliceLID.String():   503, // allowlisted by phone number; 503 = no WhatsApp connection in tests
		alicePN.User:        503,
		bobPN.User:          503, // allowlisted by LID
		strangerLD.String(): 403,
		unknownLID.String(): 403,
	}
	for recipient, want := range cases {
		rec, _ := doAPI(t, h, apiCase{body: `{"recipient":"` + recipient + `","message":"hi"}`})
		if rec.Code != want {
			t.Errorf("recipient %s: status %d, want %d (%s)", recipient, rec.Code, want, rec.Body.String())
		}
	}

	// Without a LID directory only exact matches are allowed
	s.lids = nil
	if rec, _ := doAPI(t, h, apiCase{body: `{"recipient":"` + aliceLID.String() + `","message":"hi"}`}); rec.Code != 403 {
		t.Errorf("no LID directory: status %d", rec.Code)
	}
}
