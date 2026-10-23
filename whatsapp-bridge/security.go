package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow/types"
)

const (
	defaultListenAddr     = "127.0.0.1:8080"
	maxRequestBodyBytes   = 256 << 10 // text and paths; JSON escaping can triple non-ASCII text
	maxMessageBytes       = 64 << 10
	defaultMaxMediaMB     = 100
	defaultSendRatePerMin = 10
	tokenFileName         = "api_token"
	minTokenLength        = 32
)

// Config holds the bridge's security-relevant settings. Everything comes from
// environment variables so the bridge and the MCP server can share them.
type Config struct {
	StoreDir          string // session keys, message DB, API token
	MediaDir          string // downloaded media
	OutboxDir         string // always-allowed source directory for outgoing files
	ListenAddr        string
	APIToken          string
	AllowedMediaDirs  []string
	AllowedRecipients map[string]bool // empty means any valid individual or group recipient
	ReadOnly          bool
	MaxMediaBytes     int64
	SendRatePerMinute int
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func loadConfig() (*Config, error) {
	storeDir := strings.TrimSpace(os.Getenv("WHATSAPP_STORE_DIR"))
	if storeDir == "" {
		storeDir = "store"
	}
	storeDir = filepath.Clean(storeDir)

	cfg := &Config{
		StoreDir:          storeDir,
		MediaDir:          filepath.Join(storeDir, "media"),
		OutboxDir:         filepath.Join(storeDir, "outbox"),
		ListenAddr:        defaultListenAddr,
		ReadOnly:          envBool("WHATSAPP_READ_ONLY"),
		MaxMediaBytes:     defaultMaxMediaMB << 20,
		SendRatePerMinute: defaultSendRatePerMin,
		AllowedRecipients: map[string]bool{},
	}

	if addr := strings.TrimSpace(os.Getenv("WHATSAPP_BRIDGE_ADDR")); addr != "" {
		cfg.ListenAddr = addr
	}
	host, _, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid WHATSAPP_BRIDGE_ADDR %q: %w", cfg.ListenAddr, err)
	}
	if !isLoopbackHostname(host) {
		return nil, fmt.Errorf("WHATSAPP_BRIDGE_ADDR must use a loopback address such as 127.0.0.1 (got %q)", host)
	}

	if v := strings.TrimSpace(os.Getenv("WHATSAPP_MAX_MEDIA_MB")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 4096 {
			return nil, fmt.Errorf("WHATSAPP_MAX_MEDIA_MB must be between 1 and 4096")
		}
		cfg.MaxMediaBytes = int64(n) << 20
	}

	if v := strings.TrimSpace(os.Getenv("WHATSAPP_SEND_RATE_PER_MINUTE")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("WHATSAPP_SEND_RATE_PER_MINUTE must be a non-negative integer (0 disables the limit)")
		}
		cfg.SendRatePerMinute = n
	}

	cfg.AllowedMediaDirs = []string{cfg.OutboxDir}
	for _, dir := range filepath.SplitList(os.Getenv("WHATSAPP_MEDIA_ALLOWED_DIRS")) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("WHATSAPP_MEDIA_ALLOWED_DIRS entries must be absolute paths (got %q)", dir)
		}
		cfg.AllowedMediaDirs = append(cfg.AllowedMediaDirs, filepath.Clean(dir))
	}

	for _, entry := range strings.Split(os.Getenv("WHATSAPP_ALLOWED_RECIPIENTS"), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		jid, err := parseRecipient(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid WHATSAPP_ALLOWED_RECIPIENTS entry %q: %w", entry, err)
		}
		cfg.AllowedRecipients[jid.String()] = true
	}

	return cfg, nil
}

// ---------------------------------------------------------------------------
// Filesystem permissions
// ---------------------------------------------------------------------------

// ensurePrivateDir creates dir (and parents) and restricts it to the current user.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// ensurePrivateDB creates the SQLite file with owner-only permissions before SQLite
// opens it, and tightens files left by older versions. SQLite gives new -wal, -shm
// and -journal files the same mode as the database file.
func ensurePrivateDB(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func sqliteDSN(path string) string {
	return "file:" + filepath.ToSlash(path) + "?_foreign_keys=on"
}

// writePrivateFile atomically writes data to path with mode 0600.
func writePrivateFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ---------------------------------------------------------------------------
// API authentication
// ---------------------------------------------------------------------------

// loadToken returns the shared secret the MCP server must present. It comes from
// WHATSAPP_BRIDGE_TOKEN, or is freshly generated on every start and written to
// <store>/api_token, so a token captured earlier (e.g. by another local user
// squatting the port while the bridge was down) stops working on restart.
func loadToken(storeDir string) (string, error) {
	if t := strings.TrimSpace(os.Getenv("WHATSAPP_BRIDGE_TOKEN")); t != "" {
		if len(t) < minTokenLength {
			return "", fmt.Errorf("WHATSAPP_BRIDGE_TOKEN must be at least %d characters", minTokenLength)
		}
		return t, nil
	}

	path := filepath.Join(storeDir, tokenFileName)
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate API token: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := writePrivateFile(path, []byte(token+"\n")); err != nil {
		return "", fmt.Errorf("failed to save API token: %w", err)
	}
	return token, nil
}

func validBearer(header, token string) bool {
	const prefix = "bearer "
	if len(header) <= len(prefix) || strings.ToLower(header[:len(prefix)]) != prefix {
		return false
	}
	// Hash both sides so the comparison is constant-time regardless of length.
	got := sha256.Sum256([]byte(header[len(prefix):]))
	want := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func isLoopbackHostname(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackHostHeader rejects DNS-rebinding requests, whose Host header carries the
// attacker's domain name rather than a loopback address.
func isLoopbackHostHeader(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return isLoopbackHostname(host)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"success": false, "message": message})
}

// protect wraps an API handler with the checks every request must pass:
// loopback Host, no browser Origin, POST, bearer token, JSON content type and a body size cap.
func (s *apiServer) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHostHeader(r.Host) {
			writeError(w, http.StatusForbidden, "forbidden host")
			return
		}
		// Browsers attach Origin to cross-site POSTs; the MCP server never does.
		if r.Header.Get("Origin") != "" {
			writeError(w, http.StatusForbidden, "browser requests are not allowed")
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !validBearer(r.Header.Get("Authorization"), s.cfg.APIToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="whatsapp-bridge"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid API token")
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next(w, r)
	}
}

func decodeJSONBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after JSON object")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Recipients
// ---------------------------------------------------------------------------

var (
	phoneRe = regexp.MustCompile(`^[0-9]{6,15}$`)
	lidRe   = regexp.MustCompile(`^[0-9]{1,20}$`)
	groupRe = regexp.MustCompile(`^[0-9]{1,20}(-[0-9]{1,20})?$`)
)

// parseRecipient accepts a phone number or a JID for an individual or group chat.
// Status updates (status@broadcast), broadcast lists and newsletters are rejected so
// a misled agent cannot publish to all of the user's contacts.
func parseRecipient(recipient string) (types.JID, error) {
	r := strings.TrimSpace(recipient)
	if r == "" {
		return types.EmptyJID, errors.New("recipient is required")
	}
	if !strings.Contains(r, "@") {
		r = strings.TrimPrefix(r, "+")
		if !phoneRe.MatchString(r) {
			return types.EmptyJID, errors.New("recipient must be a phone number with country code (digits only) or a JID")
		}
		return types.NewJID(r, types.DefaultUserServer), nil
	}

	jid, err := types.ParseJID(r)
	if err != nil {
		return types.EmptyJID, fmt.Errorf("invalid JID: %w", err)
	}
	jid = jid.ToNonAD()
	switch jid.Server {
	case types.DefaultUserServer:
		if phoneRe.MatchString(jid.User) {
			return jid, nil
		}
	case types.HiddenUserServer:
		if lidRe.MatchString(jid.User) {
			return jid, nil
		}
	case types.GroupServer:
		if groupRe.MatchString(jid.User) {
			return jid, nil
		}
	default:
		return types.EmptyJID, fmt.Errorf("recipient server %q is not allowed; only individual chats (@s.whatsapp.net, @lid) and groups (@g.us) are supported", jid.Server)
	}
	return types.EmptyJID, errors.New("recipient JID has an invalid user part")
}

// rateLimiter is a sliding-window limiter that caps how fast a runaway or
// prompt-injected agent can send messages.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	events []time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window}
}

func (l *rateLimiter) Allow(now time.Time) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	kept := l.events[:0]
	for _, t := range l.events {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.events = kept
	if len(l.events) >= l.limit {
		return false
	}
	l.events = append(l.events, now)
	return true
}

// ---------------------------------------------------------------------------
// Outgoing media: which local files may be sent
// ---------------------------------------------------------------------------

// matchingAncestor returns the ancestor of path that is the same directory as dir.
// Comparing file identity instead of strings keeps this correct on case-insensitive
// filesystems and when dir is reached through a symlink.
func matchingAncestor(path, dir string) (string, bool) {
	dirInfo, err := os.Stat(dir)
	if err != nil || !dirInfo.IsDir() {
		return "", false
	}
	cur := filepath.Dir(path)
	for {
		if fi, err := os.Stat(cur); err == nil && os.SameFile(fi, dirInfo) {
			return cur, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		cur = parent
	}
}

// resolveSendablePath decides whether a local file may be uploaded to WhatsApp.
// The file must live under an allowed directory, must not be hidden or inside a hidden
// directory (.ssh, .aws, .env, ...), and must not come from the bridge's private store.
func (c *Config) resolveSendablePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("media_path is required")
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("media_path must be an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", errors.New("media file not found or not accessible")
	}

	allowedRoot := ""
	for _, dir := range c.AllowedMediaDirs {
		if root, ok := matchingAncestor(resolved, dir); ok {
			allowedRoot = root
			break
		}
	}
	if allowedRoot == "" {
		return "", fmt.Errorf("media_path is outside the allowed media directories; copy the file into %s or add its directory to WHATSAPP_MEDIA_ALLOWED_DIRS", c.OutboxDir)
	}

	rel, err := filepath.Rel(allowedRoot, resolved)
	if err != nil {
		return "", errors.New("media_path is outside the allowed media directories")
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(part, ".") {
			return "", errors.New("refusing to send hidden files or files inside hidden directories")
		}
	}

	if _, inStore := matchingAncestor(resolved, c.StoreDir); inStore {
		_, inOutbox := matchingAncestor(resolved, c.OutboxDir)
		_, inMedia := matchingAncestor(resolved, c.MediaDir)
		if !inOutbox && !inMedia {
			return "", errors.New("refusing to send files from the bridge's private store directory")
		}
	}
	return resolved, nil
}

// readMediaFile reads a regular file of at most maxBytes. Devices, FIFOs and
// directories are rejected before opening so they cannot block or exhaust memory.
func readMediaFile(path string, maxBytes int64) ([]byte, error) {
	before, err := os.Stat(path)
	if err != nil {
		return nil, errors.New("media file not found or not accessible")
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("media_path must be a regular file")
	}
	if before.Size() > maxBytes {
		return nil, fmt.Errorf("media file is larger than the %d MB limit", maxBytes>>20)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("media file not found or not accessible")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.New("media file changed while it was being opened")
	}

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read media file: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("media file is larger than the %d MB limit", maxBytes>>20)
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// Incoming media: where downloads are written and where they are fetched from
// ---------------------------------------------------------------------------

var (
	safeNameRe = regexp.MustCompile(`^[A-Za-z0-9_@-][A-Za-z0-9_.@-]{0,127}$`)
	safeExtRe  = regexp.MustCompile(`^[A-Za-z0-9]{1,10}$`)
	// Direct paths are appended to WhatsApp's media hostnames, so they must start
	// with a single slash and must not contain characters that could change the host.
	directPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~%/?=&+-]+$`)
)

// safePathComponent maps sender-controlled strings (chat JIDs, message IDs) to one
// harmless path component. Anything unusual is replaced by a hash.
func safePathComponent(s string) string {
	s = strings.ReplaceAll(s, ":", "_")
	if safeNameRe.MatchString(s) && !strings.Contains(s, "..") {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return "x" + hex.EncodeToString(sum[:12])
}

// Extensions kept for downloaded attachments. Anything else is saved as .bin, so a
// sender cannot drop file types that the OS acts on when a folder is merely viewed
// (.url, .scf, .library-ms, .lnk, .desktop, ...).
var allowedMediaExtensions = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true, "heic": true,
	"mp4": true, "mov": true, "3gp": true, "mkv": true, "webm": true, "avi": true,
	"ogg": true, "opus": true, "mp3": true, "m4a": true, "aac": true, "amr": true, "wav": true,
	"pdf": true, "txt": true, "csv": true, "rtf": true, "md": true, "json": true,
	"doc": true, "docx": true, "xls": true, "xlsx": true, "ppt": true, "pptx": true,
	"odt": true, "ods": true, "odp": true, "pages": true, "numbers": true, "key": true,
	"zip": true, "7z": true, "rar": true, "gz": true, "tar": true,
	"epub": true, "vcf": true, "ics": true,
}

func mediaExtension(mediaType, filename string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	if safeExtRe.MatchString(ext) && allowedMediaExtensions[ext] {
		return "." + ext
	}
	switch mediaType {
	case "image":
		return ".jpg"
	case "video":
		return ".mp4"
	case "audio":
		return ".ogg"
	}
	return ".bin"
}

// mediaFilePath builds the local path for a downloaded attachment. The sender's
// filename only contributes an allowlisted extension, never directory components.
// A hash of the exact chat and message ID keeps IDs that differ only in letter case
// apart on case-insensitive filesystems (macOS, Windows).
func (c *Config) mediaFilePath(chatJID, messageID, mediaType, filename string) (string, error) {
	sum := sha256.Sum256([]byte(chatJID + "\x00" + messageID))
	name := safePathComponent(mediaType) + "_" + safePathComponent(messageID) + "_" +
		hex.EncodeToString(sum[:4]) + mediaExtension(mediaType, filename)
	p := filepath.Join(c.MediaDir, safePathComponent(chatJID), name)
	rel, err := filepath.Rel(c.MediaDir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("unsafe media path")
	}
	return p, nil
}

// fileMatchesSHA256 reports whether the file at path has the expected SHA-256.
func fileMatchesSHA256(path string, want []byte) bool {
	if len(want) != sha256.Size {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(h.Sum(nil), want) == 1
}

var errMediaTooLarge = errors.New("media download exceeds the size limit")

// mediaOverhead covers AES-CBC padding and the HMAC appended to encrypted media.
const mediaOverhead = 64 << 10

// limitedBodyTransport caps every media HTTP response body. The sender declares the
// file length in the message, so it cannot be trusted to bound the download.
type limitedBodyTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t *limitedBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.limit {
		resp.Body.Close()
		return nil, errMediaTooLarge
	}
	resp.Body = &limitedReadCloser{rc: resp.Body, remaining: t.limit}
	return resp, nil
}

type limitedReadCloser struct {
	rc        io.ReadCloser
	remaining int64
}

func (l *limitedReadCloser) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		var probe [1]byte
		if n, err := l.rc.Read(probe[:]); n > 0 {
			return 0, errMediaTooLarge
		} else {
			return 0, err
		}
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.rc.Read(p)
	l.remaining -= int64(n)
	return n, err
}

func (l *limitedReadCloser) Close() error {
	return l.rc.Close()
}

func newMediaHTTPClient(maxMediaBytes int64) *http.Client {
	return &http.Client{
		Transport: &limitedBodyTransport{
			base:  http.DefaultTransport.(*http.Transport).Clone(),
			limit: maxMediaBytes + mediaOverhead,
		},
	}
}

func validDirectPath(p string) bool {
	return len(p) <= 2048 && directPathRe.MatchString(p) && !strings.HasPrefix(p, "//")
}

// directPathFromURL recovers a direct path for messages stored before the bridge
// recorded one. Only HTTPS URLs on WhatsApp's own media hosts are accepted.
func directPathFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "whatsapp.net" && !strings.HasSuffix(host, ".whatsapp.net") {
		return ""
	}
	p := u.EscapedPath()
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	if !validDirectPath(p) {
		return ""
	}
	return p
}

// sanitizeForLog strips control and bidi characters so sender-controlled text
// (chat names, filenames) cannot inject terminal escape sequences into logs.
func sanitizeForLog(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return '?'
		}
		return r
	}, s)
}
