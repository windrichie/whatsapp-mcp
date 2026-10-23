package main

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef"

func testConfig(t *testing.T) *Config {
	t.Helper()
	store := t.TempDir()
	cfg := &Config{
		StoreDir:          store,
		MediaDir:          filepath.Join(store, "media"),
		OutboxDir:         filepath.Join(store, "outbox"),
		ListenAddr:        defaultListenAddr,
		APIToken:          testToken,
		MaxMediaBytes:     1 << 20,
		SendRatePerMinute: 1000,
		AllowedRecipients: map[string]bool{},
	}
	cfg.AllowedMediaDirs = []string{cfg.OutboxDir}
	for _, dir := range []string{cfg.StoreDir, cfg.MediaDir, cfg.OutboxDir} {
		if err := ensurePrivateDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func testStore(t *testing.T, cfg *Config) *MessageStore {
	t.Helper()
	store, err := NewMessageStore(cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func writeFile(t *testing.T, path string, data string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseRecipient(t *testing.T) {
	ok := map[string]string{
		"447700900123":                   "447700900123@s.whatsapp.net",
		"+447700900123":                  "447700900123@s.whatsapp.net",
		"447700900123@s.whatsapp.net":    "447700900123@s.whatsapp.net",
		"447700900123:12@s.whatsapp.net": "447700900123@s.whatsapp.net",
		"120363025246125486@g.us":        "120363025246125486@g.us",
		"447700900123-1612345678@g.us":   "447700900123-1612345678@g.us",
		"123456789012345@lid":            "123456789012345@lid",
	}
	for in, want := range ok {
		jid, err := parseRecipient(in)
		if err != nil || jid.String() != want {
			t.Errorf("parseRecipient(%q) = %v, %v; want %s", in, jid, err, want)
		}
	}
	bad := []string{
		"", "status@broadcast", "123456@broadcast", "120363@newsletter", "0@s.whatsapp.net",
		"abc", "44 7700 900123", "447700900123@evil.com", "../../x@g.us", "1234567890123456789@s.whatsapp.net",
	}
	for _, in := range bad {
		if jid, err := parseRecipient(in); err == nil {
			t.Errorf("parseRecipient(%q) = %v; want error", in, jid)
		}
	}
}

func TestLoadConfigRejectsUnsafeSettings(t *testing.T) {
	t.Setenv("WHATSAPP_STORE_DIR", t.TempDir())
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "192.168.1.10:8080", "[::]:8080", "example.com:8080"} {
		t.Setenv("WHATSAPP_BRIDGE_ADDR", addr)
		if _, err := loadConfig(); err == nil {
			t.Errorf("loadConfig accepted non-loopback address %q", addr)
		}
	}
	t.Setenv("WHATSAPP_BRIDGE_ADDR", "")

	t.Setenv("WHATSAPP_MEDIA_ALLOWED_DIRS", "relative/dir")
	if _, err := loadConfig(); err == nil {
		t.Error("loadConfig accepted a relative allowed media dir")
	}
	t.Setenv("WHATSAPP_MEDIA_ALLOWED_DIRS", "")

	t.Setenv("WHATSAPP_ALLOWED_RECIPIENTS", "447700900123, status@broadcast")
	if _, err := loadConfig(); err == nil {
		t.Error("loadConfig accepted status@broadcast as an allowed recipient")
	}
	t.Setenv("WHATSAPP_ALLOWED_RECIPIENTS", "+447700900123, 120363025246125486@g.us")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowedRecipients["447700900123@s.whatsapp.net"] || !cfg.AllowedRecipients["120363025246125486@g.us"] {
		t.Errorf("allowlist not normalised: %v", cfg.AllowedRecipients)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Errorf("default listen address = %s", cfg.ListenAddr)
	}
}

func TestTokenAndFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	assertMode(t, dir, 0o700)

	t.Setenv("WHATSAPP_BRIDGE_TOKEN", "")
	tok1, err := loadOrCreateToken(dir)
	if err != nil || len(tok1) != 64 {
		t.Fatalf("token = %q, %v", tok1, err)
	}
	assertMode(t, filepath.Join(dir, tokenFileName), 0o600)
	tok2, _ := loadOrCreateToken(dir)
	if tok1 != tok2 {
		t.Error("token changed between loads")
	}

	t.Setenv("WHATSAPP_BRIDGE_TOKEN", "short")
	if _, err := loadOrCreateToken(dir); err == nil {
		t.Error("accepted a short WHATSAPP_BRIDGE_TOKEN")
	}

	db := filepath.Join(dir, "legacy.db")
	writeFile(t, db, "")
	os.Chmod(db, 0o644)
	writeFile(t, db+"-wal", "")
	os.Chmod(db+"-wal", 0o644)
	if err := ensurePrivateDB(db); err != nil {
		t.Fatal(err)
	}
	assertMode(t, db, 0o600)
	assertMode(t, db+"-wal", 0o600)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func TestValidBearerAndHost(t *testing.T) {
	if !validBearer("Bearer "+testToken, testToken) || !validBearer("bearer "+testToken, testToken) {
		t.Error("valid token rejected")
	}
	for _, h := range []string{"", "Bearer", "Bearer ", "Basic " + testToken, "Bearer " + testToken + "x", testToken} {
		if validBearer(h, testToken) {
			t.Errorf("validBearer(%q) = true", h)
		}
	}
	for _, h := range []string{"127.0.0.1:8080", "localhost:8080", "LOCALHOST", "[::1]:8080", "127.0.0.2:8080"} {
		if !isLoopbackHostHeader(h) {
			t.Errorf("isLoopbackHostHeader(%q) = false", h)
		}
	}
	for _, h := range []string{"evil.example:8080", "evil.example", "192.168.1.10:8080", "localhost.evil.example", ""} {
		if isLoopbackHostHeader(h) {
			t.Errorf("isLoopbackHostHeader(%q) = true", h)
		}
	}
}

type apiCase struct {
	name   string
	method string
	host   string
	path   string
	body   string
	header map[string]string
	status int
}

func doAPI(t *testing.T, h http.Handler, c apiCase) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	method := c.method
	if method == "" {
		method = http.MethodPost
	}
	path := c.path
	if path == "" {
		path = "/api/send"
	}
	req := httptest.NewRequest(method, path, strings.NewReader(c.body))
	req.Host = "127.0.0.1:8080"
	if c.host != "" {
		req.Host = c.host
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	for k, v := range c.header {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestAPIRequestGuards(t *testing.T) {
	cfg := testConfig(t)
	h := newAPIHandler(nil, testStore(t, cfg), cfg)
	inOutbox := writeFile(t, filepath.Join(cfg.OutboxDir, "photo.jpg"), "jpeg")
	secret := filepath.Join(cfg.StoreDir, "whatsapp.db")
	valid := `{"recipient":"447700900123","message":"hi"}`

	cases := []apiCase{
		{name: "dns rebinding host", host: "evil.example:8080", body: valid, status: 403},
		{name: "browser origin", body: valid, header: map[string]string{"Origin": "https://evil.example"}, status: 403},
		{name: "GET", method: http.MethodGet, status: 405},
		{name: "no token", body: valid, header: map[string]string{"Authorization": ""}, status: 401},
		{name: "wrong token", body: valid, header: map[string]string{"Authorization": "Bearer nope"}, status: 401},
		{name: "csrf text/plain", body: valid, header: map[string]string{"Content-Type": "text/plain"}, status: 415},
		{name: "form post", body: valid, header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, status: 415},
		{name: "unknown field", body: `{"recipient":"447700900123","message":"hi","x":1}`, status: 400},
		{name: "oversized body", body: `{"recipient":"447700900123","message":"` + strings.Repeat("a", maxRequestBodyBytes) + `"}`, status: 400},
		{name: "status broadcast", body: `{"recipient":"status@broadcast","message":"hi"}`, status: 400},
		{name: "garbage recipient", body: `{"recipient":"hello","message":"hi"}`, status: 400},
		{name: "empty", body: `{"recipient":"447700900123"}`, status: 400},
		{name: "etc passwd", body: `{"recipient":"447700900123","media_path":"/etc/passwd"}`, status: 403},
		{name: "dev zero", body: `{"recipient":"447700900123","media_path":"/dev/zero"}`, status: 403},
		{name: "relative path", body: `{"recipient":"447700900123","media_path":"outbox/photo.jpg"}`, status: 403},
		{name: "session db", body: `{"recipient":"447700900123","media_path":"` + secret + `"}`, status: 403},
		{name: "traversal out of outbox", body: `{"recipient":"447700900123","media_path":"` + cfg.OutboxDir + `/../whatsapp.db"}`, status: 403},
		// Valid requests pass every check and stop only because there is no WhatsApp connection.
		{name: "valid text", body: valid, status: 503},
		{name: "valid media", body: `{"recipient":"447700900123","media_path":"` + inOutbox + `"}`, status: 503},
		{name: "download traversal id", path: "/api/download", body: `{"message_id":"../../x","chat_jid":"x@s.whatsapp.net"}`, status: 500},
	}
	ensurePrivateDB(secret)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, out := doAPI(t, h, c)
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.status, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content type = %q", ct)
			}
			if out["success"] == true {
				t.Error("success = true")
			}
		})
	}
}

func TestAPIAllowlistReadOnlyAndRateLimit(t *testing.T) {
	cfg := testConfig(t)
	cfg.AllowedRecipients = map[string]bool{"447700900123@s.whatsapp.net": true}
	cfg.SendRatePerMinute = 2
	h := newAPIHandler(nil, testStore(t, cfg), cfg)

	if rec, _ := doAPI(t, h, apiCase{body: `{"recipient":"447700900999","message":"hi"}`}); rec.Code != 403 {
		t.Errorf("non-allowlisted recipient: status %d", rec.Code)
	}
	for i := 0; i < 2; i++ {
		if rec, _ := doAPI(t, h, apiCase{body: `{"recipient":"+447700900123","message":"hi"}`}); rec.Code != 503 {
			t.Errorf("allowlisted recipient #%d: status %d", i, rec.Code)
		}
	}
	if rec, _ := doAPI(t, h, apiCase{body: `{"recipient":"447700900123","message":"hi"}`}); rec.Code != 429 {
		t.Errorf("rate limit: status %d", rec.Code)
	}

	cfg.ReadOnly = true
	if rec, _ := doAPI(t, h, apiCase{body: `{"recipient":"447700900123","message":"hi"}`}); rec.Code != 403 {
		t.Errorf("read-only: status %d", rec.Code)
	}
}

func TestResolveSendablePath(t *testing.T) {
	cfg := testConfig(t)
	extra := t.TempDir()
	cfg.AllowedMediaDirs = append(cfg.AllowedMediaDirs, extra)
	outside := writeFile(t, filepath.Join(t.TempDir(), "secret.txt"), "secret")

	ok := []string{
		writeFile(t, filepath.Join(cfg.OutboxDir, "a.jpg"), "x"),
		writeFile(t, filepath.Join(cfg.OutboxDir, "sub", "b.pdf"), "x"),
		writeFile(t, filepath.Join(extra, "c.png"), "x"),
	}
	for _, p := range ok {
		if _, err := cfg.resolveSendablePath(p); err != nil {
			t.Errorf("resolveSendablePath(%q) = %v", p, err)
		}
	}

	link := filepath.Join(cfg.OutboxDir, "link.txt")
	os.Symlink(outside, link)
	dirLink := filepath.Join(cfg.OutboxDir, "dirlink")
	os.Symlink(filepath.Dir(outside), dirLink)
	tokenPath := writeFile(t, filepath.Join(cfg.StoreDir, tokenFileName), testToken)

	// Allow a parent of the store, as a user allowing their home directory would.
	cfgWide := *cfg
	cfgWide.AllowedMediaDirs = []string{filepath.Dir(cfg.StoreDir)}

	bad := map[string]*Config{
		outside:                              cfg,
		link:                                 cfg,
		filepath.Join(dirLink, "secret.txt"): cfg,
		writeFile(t, filepath.Join(cfg.OutboxDir, ".env"), "K=V"):       cfg,
		writeFile(t, filepath.Join(extra, ".ssh", "id_ed25519"), "key"): cfg,
		filepath.Join(cfg.OutboxDir, "missing.jpg"):                     cfg,
		"relative.jpg": cfg,
		"":             cfg,
		tokenPath:      &cfgWide,
		filepath.Join(cfg.StoreDir, "outbox", "..", tokenFileName): &cfgWide,
	}
	for p, c := range bad {
		if got, err := c.resolveSendablePath(p); err == nil {
			t.Errorf("resolveSendablePath(%q) = %q; want error", p, got)
		}
	}
}

func TestReadMediaFile(t *testing.T) {
	dir := t.TempDir()
	small := writeFile(t, filepath.Join(dir, "small.bin"), "hello")
	if data, err := readMediaFile(small, 10); err != nil || string(data) != "hello" {
		t.Errorf("readMediaFile(small) = %q, %v", data, err)
	}
	big := writeFile(t, filepath.Join(dir, "big.bin"), strings.Repeat("x", 11))
	if _, err := readMediaFile(big, 10); err == nil {
		t.Error("oversized file accepted")
	}
	if _, err := readMediaFile(dir, 10); err == nil {
		t.Error("directory accepted")
	}
	if runtime.GOOS != "windows" {
		fifo := filepath.Join(dir, "fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err == nil {
			done := make(chan error, 1)
			go func() { _, err := readMediaFile(fifo, 10); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Error("FIFO accepted")
				}
			case <-time.After(2 * time.Second):
				t.Error("readMediaFile blocked on a FIFO")
			}
		}
	}
}

func TestMediaFilePathStaysInsideMediaDir(t *testing.T) {
	cfg := testConfig(t)
	evilNames := []string{
		"../../../../../../etc/passwd",
		"../../.bashrc",
		`..\..\Windows\evil.dll`,
		"/absolute/path.sh",
		"normal.pdf",
		"x.pdf/../../../y",
		"Library/LaunchAgents/evil.plist",
		"",
	}
	evilIDs := []string{"3EB0C767D26A1D2D", "../../x", "..", ".", "a/b", `a\b`, "", strings.Repeat("A", 500), "CON"}
	evilChats := []string{"447700900123@s.whatsapp.net", "../../..", "120363025246125486@g.us", "x/../../y", ""}
	for _, chat := range evilChats {
		for _, id := range evilIDs {
			for _, name := range evilNames {
				p, err := cfg.mediaFilePath(chat, id, "document", name)
				if err != nil {
					continue
				}
				rel, err := filepath.Rel(cfg.MediaDir, p)
				if err != nil || strings.HasPrefix(rel, "..") || strings.Count(rel, string(filepath.Separator)) != 1 {
					t.Errorf("mediaFilePath(%q, %q, %q) = %q escapes %q", chat, id, name, p, cfg.MediaDir)
				}
			}
		}
	}
	p, _ := cfg.mediaFilePath("447700900123@s.whatsapp.net", "3EB0C767D26A1D2D", "document", "report.PDF")
	if want := filepath.Join(cfg.MediaDir, "447700900123@s.whatsapp.net", "document_3EB0C767D26A1D2D.pdf"); p != want {
		t.Errorf("readable path = %q, want %q", p, want)
	}
}

func TestDownloadIgnoresSenderFilename(t *testing.T) {
	cfg := testConfig(t)
	store := testStore(t, cfg)
	chat := "447700900123@s.whatsapp.net"
	store.StoreChat(chat, "Mallory", time.Now())
	evil := "../../../../../../../../../../etc/passwd"
	media := MediaInfo{Type: "document", Filename: evil, URL: "https://mmg.whatsapp.net/v/t62/x.enc?oh=1",
		MediaKey: []byte{1}, FileSHA256: []byte{1}, FileEncSHA256: []byte{1}, FileLength: 1}
	if err := store.StoreMessage("EVIL1", chat, "447700900123", "", time.Now(), false, media); err != nil {
		t.Fatal(err)
	}
	ok, _, _, path, err := downloadMedia(nil, store, cfg, "EVIL1", chat)
	if ok || path == "/etc/passwd" {
		t.Fatalf("download returned ok=%v path=%q", ok, path)
	}
	if err == nil || !strings.Contains(err.Error(), "failed to download") {
		t.Fatalf("expected the (nil) client to be reached with a safe path, got %v", err)
	}
}

func TestStoreMessageRejectsCrossSenderOverwrite(t *testing.T) {
	cfg := testConfig(t)
	store := testStore(t, cfg)
	chat := "120363025246125486@g.us"
	store.StoreChat(chat, "Group", time.Now())
	store.StoreMessage("ID1", chat, "447700900001", "original from Alice", time.Now(), false, MediaInfo{})
	store.StoreMessage("ID1", chat, "447700900666", "forged by Mallory", time.Now(), false, MediaInfo{})

	var sender, content string
	store.db.QueryRow("SELECT sender, content FROM messages WHERE id = 'ID1'").Scan(&sender, &content)
	if sender != "447700900001" || content != "original from Alice" {
		t.Fatalf("message overwritten by another sender: %s %q", sender, content)
	}

	store.StoreMessage("ID1", chat, "447700900001", "history sync copy", time.Now(), false, MediaInfo{})
	store.db.QueryRow("SELECT content FROM messages WHERE id = 'ID1'").Scan(&content)
	if content != "history sync copy" {
		t.Errorf("same-sender update was not applied: %q", content)
	}
}

func TestMigrationAddsDirectPath(t *testing.T) {
	cfg := testConfig(t)
	store := testStore(t, cfg)
	if _, err := store.db.Exec("ALTER TABLE messages DROP COLUMN direct_path"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE messages SET direct_path = 'x' WHERE 0"); err == nil {
		t.Fatal("column was not dropped")
	}
	store.Close()
	store = testStore(t, cfg)
	if _, err := store.db.Exec("UPDATE messages SET direct_path = 'x' WHERE 0"); err != nil {
		t.Fatalf("direct_path column missing after migration: %v", err)
	}
}

func TestDirectPathValidation(t *testing.T) {
	good := map[string]string{
		"https://mmg.whatsapp.net/v/t62.7118-24/1234_n.enc?ccb=11-4&oh=01_Q5Aa&oe=6727C2C1&_nc_sid=5e03e0": "/v/t62.7118-24/1234_n.enc?ccb=11-4&oh=01_Q5Aa&oe=6727C2C1&_nc_sid=5e03e0",
		"https://media-lhr8-1.cdn.whatsapp.net/v/x.enc":                                                    "/v/x.enc",
	}
	for in, want := range good {
		if got := directPathFromURL(in); got != want {
			t.Errorf("directPathFromURL(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{
		"http://mmg.whatsapp.net/v/x.enc", "https://evil.example/v/x.enc", "https://whatsapp.net.evil.example/x",
		"https://mmg.whatsapp.net@evil.example/x", "https://mmg.whatsapp.net:8443/x", "@evil.example/x",
		"https://169.254.169.254/latest/meta-data", "file:///etc/passwd", "",
	} {
		if got := directPathFromURL(in); got != "" {
			t.Errorf("directPathFromURL(%q) = %q; want rejection", in, got)
		}
	}
	for _, p := range []string{"//evil.example/x", "@evil.example/x", "/x@evil.example", "/x\\y", "/x y", "v/x", ""} {
		if validDirectPath(p) {
			t.Errorf("validDirectPath(%q) = true", p)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(3, time.Minute)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !l.Allow(now) {
			t.Fatal("blocked too early")
		}
	}
	if l.Allow(now.Add(30 * time.Second)) {
		t.Error("allowed past the limit")
	}
	if !l.Allow(now.Add(61 * time.Second)) {
		t.Error("window did not slide")
	}
	if !newRateLimiter(0, time.Minute).Allow(now) {
		t.Error("limit 0 should disable limiting")
	}
}

func TestSanitizeForLog(t *testing.T) {
	in := "Mallory\x1b]52;c;cHduZWQ=\x07\n‮evil"
	out := sanitizeForLog(in)
	if strings.ContainsAny(out, "\x1b\x07\n‮") {
		t.Errorf("control characters survived: %q", out)
	}
}

func TestAnalyzeOggOpusDuration(t *testing.T) {
	data, err := os.ReadFile("testdata/tone_2_5s.ogg")
	if err != nil {
		t.Fatal(err)
	}
	seconds, waveform, err := analyzeOggOpus(data)
	if err != nil || seconds != 3 || len(waveform) != 64 {
		t.Fatalf("analyzeOggOpus = %d s, %d bytes, %v; want 3 s, 64 bytes", seconds, len(waveform), err)
	}
}

func TestAnalyzeOggOpusMalformedInput(t *testing.T) {
	real, _ := os.ReadFile("testdata/tone_2_5s.ogg")
	inputs := [][]byte{
		[]byte("OggS"),
		append([]byte("OggS"), make([]byte, 24)...),
		append(append([]byte("OggS"), make([]byte, 22)...), 1, 255, 0),
		append(append(append([]byte("OggS"), make([]byte, 22)...), 1, 20), []byte("OpusHead")...),
	}
	for n := 1; n < len(real); n += 7 {
		inputs = append(inputs, real[:n])
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		buf := append([]byte(nil), real...)
		for j := 0; j < 8; j++ {
			buf[rng.Intn(len(buf))] = byte(rng.Intn(256))
		}
		inputs = append(inputs, buf)
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("analyzeOggOpus panicked on %d-byte input: %v", len(in), r)
				}
			}()
			analyzeOggOpus(in)
		}()
	}
}

func FuzzAnalyzeOggOpus(f *testing.F) {
	real, _ := os.ReadFile("testdata/tone_2_5s.ogg")
	f.Add(real)
	f.Add(append(append([]byte("OggS"), make([]byte, 22)...), 1, 255, 0))
	f.Fuzz(func(t *testing.T, data []byte) {
		analyzeOggOpus(data)
	})
}

func TestNoBrowserCanReachAPI(t *testing.T) {
	// A browser page can only send "simple" cross-origin requests without a preflight:
	// no Authorization header and a text/plain, form or multipart body. All are refused.
	cfg := testConfig(t)
	srv := httptest.NewServer(newAPIHandler(nil, testStore(t, cfg), cfg))
	defer srv.Close()
	body := bytes.NewBufferString(`{"recipient":"447700900123","message":"hi"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/send", body)
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("simple cross-origin request: status %d, want 403", resp.StatusCode)
	}
}
