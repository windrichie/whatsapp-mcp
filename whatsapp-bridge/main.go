package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"bytes"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// Initialize message store
func NewMessageStore(storeDir string) (*MessageStore, error) {
	// Create a private directory for the database if it doesn't exist
	if err := ensurePrivateDir(storeDir); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Message history is sensitive: keep the database readable by the owner only
	dbPath := filepath.Join(storeDir, "messages.db")
	if err := ensurePrivateDB(dbPath); err != nil {
		return nil, fmt.Errorf("failed to secure message database: %v", err)
	}

	// Open SQLite database for messages
	db, err := sql.Open("sqlite3", sqliteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			direct_path TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	// Databases created by older versions lack the direct_path column
	if err := addColumnIfMissing(db, "messages", "direct_path", "TEXT"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to migrate messages table: %v", err)
	}

	// The contacts table mirrors the address book and LID mappings for the MCP server
	// (see SyncContacts). Older versions let history sync move a chat's
	// last_message_time backwards, so repair it from the stored messages.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS contacts (
			jid TEXT PRIMARY KEY,
			phone TEXT,
			name TEXT
		);
		CREATE INDEX IF NOT EXISTS contacts_phone ON contacts (phone);
		CREATE INDEX IF NOT EXISTS messages_chat_time ON messages (chat_jid, timestamp);

		UPDATE chats SET last_message_time = (
			SELECT m.timestamp FROM messages m WHERE m.chat_jid = chats.jid
			ORDER BY julianday(m.timestamp) DESC LIMIT 1
		)
		WHERE EXISTS (
			SELECT 1 FROM messages m WHERE m.chat_jid = chats.jid AND (
				chats.last_message_time IS NULL OR
				julianday(m.timestamp) > julianday(chats.last_message_time)
			)
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to migrate chats and contacts: %v", err)
	}

	return &MessageStore{db: db}, nil
}

func addColumnIfMissing(db *sql.DB, table, column, columnType string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, columnType))
	return err
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database. last_message_time only ever moves forward: history
// sync delivers older messages after newer live ones.
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			name = CASE
				WHEN excluded.name IS NULL OR excluded.name = '' THEN chats.name
				-- A bare number is a fallback (the contact store may still be empty after
				-- linking again); never let it replace a real name
				WHEN excluded.name NOT GLOB '*[^0-9]*' AND chats.name GLOB '*[^0-9]*' THEN chats.name
				ELSE excluded.name
			END,
			last_message_time = CASE
				WHEN chats.last_message_time IS NULL
					OR julianday(excluded.last_message_time) > julianday(chats.last_message_time)
				THEN excluded.last_message_time
				ELSE chats.last_message_time
			END`,
		jid, name, lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool, media MediaInfo) error {
	return store.storeMessage(id, chatJID, sender, content, timestamp, isFromMe, media, false)
}

// StoreHistoryMessage stores a message from history sync, which comes from the user's
// own phone. Unlike a live message, it may also repair rows where older versions
// recorded the group's own ID as the sender.
func (store *MessageStore) StoreHistoryMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool, media MediaInfo) error {
	return store.storeMessage(id, chatJID, sender, content, timestamp, isFromMe, media, true)
}

func (store *MessageStore) storeMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool, media MediaInfo, fromHistory bool) error {
	// Only store if there's actual content or media
	if content == "" && media.Type == "" {
		return nil
	}

	// Message IDs are chosen by the sending client, so a re-used ID must not let one
	// participant overwrite somebody else's message. Only the original sender's
	// duplicates (e.g. from history sync) may update an existing row.
	_, err := store.db.Exec(
		`INSERT INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, direct_path, media_key, file_sha256, file_enc_sha256, file_length)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, chat_jid) DO UPDATE SET
			sender = excluded.sender,
			is_from_me = excluded.is_from_me,
			content = excluded.content,
			timestamp = excluded.timestamp,
			media_type = excluded.media_type,
			filename = excluded.filename,
			url = excluded.url,
			direct_path = excluded.direct_path,
			media_key = excluded.media_key,
			file_sha256 = excluded.file_sha256,
			file_enc_sha256 = excluded.file_enc_sha256,
			file_length = excluded.file_length
		WHERE (messages.sender = excluded.sender AND messages.is_from_me = excluded.is_from_me)
			OR (? AND excluded.sender != '' AND messages.chat_jid LIKE '%@g.us'
				AND messages.sender = substr(messages.chat_jid, 1, instr(messages.chat_jid, '@') - 1))`,
		id, chatJID, sender, content, timestamp, isFromMe, media.Type, media.Filename, media.URL, media.DirectPath,
		media.MediaKey, media.FileSHA256, media.FileEncSHA256, media.FileLength, fromHistory,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// For now, we're ignoring non-text messages
	return ""
}

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
	// SourcePath is the user's original file when the MCP server converted it into
	// the outbox (voice messages). It must satisfy the same policy as MediaPath.
	SourcePath string `json:"source_path,omitempty"`
}

// outgoingMedia is a validated local file that is about to be uploaded
type outgoingMedia struct {
	Name string
	Data []byte
}

// Function to send a WhatsApp message. The recipient and media have already been
// validated by the API handler.
func sendWhatsAppMessage(client *whatsmeow.Client, recipientJID types.JID, message string, media *outgoingMedia) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	msg := &waProto.Message{}

	// Check if we have media to send
	if media != nil {
		mediaData := media.Data

		// Determine media type and mime type based on file extension
		fileExt := strings.ToLower(strings.TrimPrefix(filepath.Ext(media.Name), "."))
		var mediaType whatsmeow.MediaType
		var mimeType string

		// Handle different media types
		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types (for any other file type)
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/octet-stream"
		}

		// Upload media to WhatsApp servers. The response holds the media decryption
		// key, so it is never logged.
		resp, err := client.Upload(ctx, mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Handle ogg audio files
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			// Try to analyze the ogg file
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waProto.DocumentMessage{
				Title:         proto.String(media.Name),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Send message
	_, err := client.SendMessage(ctx, recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	return true, fmt.Sprintf("Message sent to %s", recipientJID)
}

// MediaInfo describes an attachment as received from WhatsApp
type MediaInfo struct {
	Type          string
	Filename      string
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	FileLength    uint64
}

var mimeExtensions = map[string]string{
	"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp", "image/gif": "gif",
	"video/mp4": "mp4", "video/3gpp": "3gp", "video/quicktime": "mov",
	"audio/ogg": "ogg", "audio/mpeg": "mp3", "audio/mp4": "m4a", "audio/aac": "aac", "audio/amr": "amr",
}

func extensionForMimetype(mimetype, fallback string) string {
	base := strings.TrimSpace(strings.SplitN(mimetype, ";", 2)[0])
	if ext, ok := mimeExtensions[strings.ToLower(base)]; ok {
		return ext
	}
	return fallback
}

// Extract media info from a message
func extractMediaInfo(msg *waProto.Message) MediaInfo {
	if msg == nil {
		return MediaInfo{}
	}
	stamp := time.Now().Format("20060102_150405")

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return MediaInfo{"image", "image_" + stamp + "." + extensionForMimetype(img.GetMimetype(), "jpg"),
			img.GetURL(), img.GetDirectPath(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()}
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return MediaInfo{"video", "video_" + stamp + "." + extensionForMimetype(vid.GetMimetype(), "mp4"),
			vid.GetURL(), vid.GetDirectPath(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()}
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return MediaInfo{"audio", "audio_" + stamp + "." + extensionForMimetype(aud.GetMimetype(), "ogg"),
			aud.GetURL(), aud.GetDirectPath(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()}
	}

	// Check for document message. The filename is chosen by the sender: it is stored
	// for display only and never used to build a local path.
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + stamp
		}
		return MediaInfo{"document", filename,
			doc.GetURL(), doc.GetDirectPath(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()}
	}

	return MediaInfo{}
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Save message to database
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	media := extractMediaInfo(msg.Message)

	// Skip if there's no content and no media
	if content == "" && media.Type == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(msg.Info.ID, chatJID, sender, content, msg.Info.Timestamp, msg.Info.IsFromMe, media)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception. Message text and sender-chosen names are never
		// written to the log: logs end up in files and terminals the user does not audit.
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}
		kind := "text"
		if media.Type != "" {
			kind = media.Type
		}
		fmt.Printf("[%s] %s %s message in %s\n", timestamp, direction, kind, sanitizeForLog(chatJID))
	}
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (MediaInfo, error) {
	var mediaType, filename, url, directPath sql.NullString
	var fileLength sql.NullInt64
	var info MediaInfo

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, direct_path, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &directPath, &info.MediaKey, &info.FileSHA256, &info.FileEncSHA256, &fileLength)
	if err != nil {
		return MediaInfo{}, err
	}

	info.Type, info.Filename, info.URL, info.DirectPath = mediaType.String, filename.String, url.String, directPath.String
	if fileLength.Valid && fileLength.Int64 > 0 {
		info.FileLength = uint64(fileLength.Int64)
	}
	return info, nil
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface. It
// deliberately has no URL: the URL in a message is chosen by the sender, while a
// direct path is always fetched from WhatsApp's own media hosts.
type MediaDownloader struct {
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// Function to download media from a message
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, cfg *Config, messageID, chatJID string) (bool, string, string, string, error) {
	// Get media info from the database
	info, err := messageStore.GetMediaInfo(messageID, chatJID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", "", "", fmt.Errorf("message not found")
	} else if err != nil {
		return false, "", "", "", fmt.Errorf("failed to look up message: %v", err)
	}

	// Check if this is a media message
	if info.Type == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	var waMediaType whatsmeow.MediaType
	switch info.Type {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", info.Type)
	}

	// The local path is derived from the chat and message IDs, never from the
	// sender's filename, so a crafted name like "../../.bashrc" cannot escape
	localPath, err := cfg.mediaFilePath(chatJID, messageID, info.Type, info.Filename)
	if err != nil {
		return false, "", "", "", err
	}
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Return an earlier download only if it is intact; otherwise download it again
	if fi, err := os.Lstat(localPath); err == nil {
		if !fi.Mode().IsRegular() {
			return false, "", "", "", fmt.Errorf("unexpected file type at media path")
		}
		if fileMatchesSHA256(localPath, info.FileSHA256) {
			return true, info.Type, info.Filename, absPath, nil
		}
	}

	directPath := info.DirectPath
	if !validDirectPath(directPath) {
		directPath = directPathFromURL(info.URL)
	}

	// If we don't have all the media info we need, we can't download
	if directPath == "" || len(info.MediaKey) == 0 || len(info.FileSHA256) == 0 || len(info.FileEncSHA256) == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}
	if info.FileLength > uint64(cfg.MaxMediaBytes) {
		return false, "", "", "", fmt.Errorf("media is larger than the %d MB limit", cfg.MaxMediaBytes>>20)
	}

	downloader := &MediaDownloader{
		DirectPath:    directPath,
		MediaKey:      info.MediaKey,
		FileLength:    info.FileLength,
		FileSHA256:    info.FileSHA256,
		FileEncSHA256: info.FileEncSHA256,
		MediaType:     waMediaType,
	}

	// Download the media using whatsmeow client
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	mediaData, err := client.Download(ctx, downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}
	if int64(len(mediaData)) > cfg.MaxMediaBytes {
		return false, "", "", "", fmt.Errorf("media is larger than the %d MB limit", cfg.MaxMediaBytes>>20)
	}

	// Save the downloaded media to a private file, writing to a temporary name first
	chatDir := filepath.Dir(localPath)
	if err := ensurePrivateDir(chatDir); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}
	tmp, err := os.CreateTemp(chatDir, ".download-*")
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(mediaData); err != nil {
		tmp.Close()
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}
	if err := os.Rename(tmp.Name(), localPath); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	fmt.Printf("Downloaded %s media (%d bytes)\n", info.Type, len(mediaData))
	return true, info.Type, info.Filename, absPath, nil
}

// apiServer exposes the WhatsApp client to the local MCP server
type apiServer struct {
	client  *whatsmeow.Client
	store   *MessageStore
	cfg     *Config
	limiter *rateLimiter
	lids    lidDirectory // nil when there is no client (tests)
}

func newAPIHandler(client *whatsmeow.Client, messageStore *MessageStore, cfg *Config) http.Handler {
	s := &apiServer{
		client:  client,
		store:   messageStore,
		cfg:     cfg,
		limiter: newRateLimiter(cfg.SendRatePerMinute, time.Minute),
	}
	if client != nil {
		s.lids = client.Store.LIDs
	}
	return s.routes()
}

func (s *apiServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/send", s.protect(s.handleSend))
	mux.HandleFunc("/api/download", s.protect(s.handleDownload))
	return mux
}

// Handler for sending messages
func (s *apiServer) handleSend(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ReadOnly {
		writeError(w, http.StatusForbidden, "sending is disabled (WHATSAPP_READ_ONLY is set)")
		return
	}
	// Without an allowlist, a prompt-injected agent could send to anyone, so that
	// needs an explicit opt-in.
	if len(s.cfg.AllowedRecipients) == 0 && !s.cfg.AllowAnyRecipient {
		writeError(w, http.StatusForbidden, "sending is disabled until WHATSAPP_ALLOWED_RECIPIENTS is set (or WHATSAPP_ALLOW_ANY_RECIPIENT=1)")
		return
	}

	// Parse the request body
	var req SendMessageRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request format")
		return
	}

	// Validate request
	recipientJID, err := parseRecipient(req.Recipient)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(s.cfg.AllowedRecipients) > 0 && !s.recipientAllowed(r.Context(), recipientJID) {
		writeError(w, http.StatusForbidden, "recipient is not listed in WHATSAPP_ALLOWED_RECIPIENTS")
		return
	}
	if req.Message == "" && req.MediaPath == "" {
		writeError(w, http.StatusBadRequest, "message or media path is required")
		return
	}
	if len(req.Message) > maxMessageBytes {
		writeError(w, http.StatusBadRequest, "message is too long")
		return
	}

	var mediaPath string
	if req.MediaPath != "" {
		mediaPath, err = s.cfg.resolveSendablePath(req.MediaPath)
		if err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
	}
	if req.SourcePath != "" {
		if req.MediaPath == "" {
			writeError(w, http.StatusBadRequest, "source_path requires media_path")
			return
		}
		source, err := s.cfg.resolveSendablePath(req.SourcePath)
		if err == nil {
			var f *os.File
			if f, _, err = openSendableFile(source); err == nil {
				f.Close()
			}
		}
		if err != nil {
			writeError(w, http.StatusForbidden, "source file: "+err.Error())
			return
		}
	}

	if !s.limiter.Allow(time.Now()) {
		writeError(w, http.StatusTooManyRequests, "send rate limit exceeded; try again later")
		return
	}

	var media *outgoingMedia
	if mediaPath != "" {
		data, err := readMediaFile(mediaPath, s.cfg.MaxMediaBytes)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		media = &outgoingMedia{Name: filepath.Base(mediaPath), Data: data}
	}

	// Send the message
	success, message := sendWhatsAppMessage(s.client, recipientJID, req.Message, media)
	kind := "text"
	if media != nil {
		kind = "media"
	}
	fmt.Printf("Send %s message to %s: success=%v\n", kind, sanitizeForLog(recipientJID.String()), success)

	status := http.StatusOK
	if !success {
		status = http.StatusBadGateway
		if !s.client.IsConnected() {
			status = http.StatusServiceUnavailable
		}
	}
	writeJSON(w, status, SendMessageResponse{Success: success, Message: message})
}

// recipientAllowed checks the allowlist. A person has both a phone-number JID and a
// LID, and chats now mostly use the LID, so an allowlisted phone number also allows
// that person's LID (and the other way round), using WhatsApp's own mapping.
func (s *apiServer) recipientAllowed(ctx context.Context, jid types.JID) bool {
	if s.cfg.AllowedRecipients[jid.String()] {
		return true
	}
	if s.lids == nil {
		return false
	}
	var other types.JID
	switch jid.Server {
	case types.HiddenUserServer:
		other, _ = s.lids.GetPNForLID(ctx, jid)
	case types.DefaultUserServer:
		other, _ = s.lids.GetLIDForPN(ctx, jid)
	}
	return !other.IsEmpty() && s.cfg.AllowedRecipients[other.ToNonAD().String()]
}

// Handler for downloading media
func (s *apiServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	// Parse the request body
	var req DownloadMediaRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request format")
		return
	}

	// Validate request
	if req.MessageID == "" || req.ChatJID == "" || len(req.MessageID) > 256 || len(req.ChatJID) > 256 {
		writeError(w, http.StatusBadRequest, "message ID and chat JID are required")
		return
	}

	// Download the media
	success, mediaType, filename, path, err := downloadMedia(s.client, s.store, s.cfg, req.MessageID, req.ChatJID)

	// Handle download result
	if !success || err != nil {
		errMsg := "unknown error"
		if err != nil {
			errMsg = err.Error()
		}
		writeJSON(w, http.StatusInternalServerError, DownloadMediaResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to download media: %s", errMsg),
		})
		return
	}

	// Send successful response
	writeJSON(w, http.StatusOK, DownloadMediaResponse{
		Success:  true,
		Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
		Filename: sanitizeForLog(filepath.Base(filename)),
		Path:     path,
	})
}

// Start a REST API server to expose the WhatsApp client functionality. It only
// listens on loopback and every request must carry the API token.
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, cfg *Config) (*http.Server, error) {
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, err
	}
	if addr, ok := ln.Addr().(*net.TCPAddr); !ok || !addr.IP.IsLoopback() {
		ln.Close()
		return nil, fmt.Errorf("refusing to serve the API on non-loopback address %s", ln.Addr())
	}

	server := &http.Server{
		Handler:           newAPIHandler(client, messageStore, cfg),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      10 * time.Minute, // uploads of large media happen inside the request
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	fmt.Printf("Starting REST API server on http://%s (token in %s)\n", ln.Addr(), filepath.Join(cfg.StoreDir, tokenFileName))

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
	return server, nil
}

func main() {
	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)
	ctx := context.Background()

	cfg, err := loadConfig()
	if err != nil {
		logger.Errorf("Invalid configuration: %v", err)
		os.Exit(1)
	}

	// The store holds the linked-device keys (anyone who reads whatsapp.db can act as
	// this device) and the full message history, so it is private to the current user
	for _, dir := range []string{cfg.StoreDir, cfg.MediaDir, cfg.OutboxDir} {
		if err := ensurePrivateDir(dir); err != nil {
			logger.Errorf("Failed to create directory %s: %v", dir, err)
			return
		}
	}

	cfg.APIToken, err = loadToken(cfg.StoreDir)
	if err != nil {
		logger.Errorf("Failed to load API token: %v", err)
		return
	}

	sessionDB := filepath.Join(cfg.StoreDir, "whatsapp.db")
	if err := ensurePrivateDB(sessionDB); err != nil {
		logger.Errorf("Failed to secure session database: %v", err)
		return
	}

	container, err := sqlstore.New(ctx, "sqlite3", sqliteDSN(sessionDB), dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}
	// Bound media downloads by bytes actually received, not the sender's declared size
	client.SetMediaHTTPClient(newMediaHTTPClient(cfg.MaxMediaBytes))

	// Initialize message store
	messageStore, err := NewMessageStore(cfg.StoreDir)
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger)

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")

		case *events.AppStateSyncComplete:
			// The address book arrives through app state sync
			syncContacts(client, messageStore, logger)

		case *events.Contact:
			if !v.FromFullSync {
				syncContacts(client, messageStore, logger)
			}

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
		}
	})

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Print QR code for pairing with phone
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp!")
	syncContacts(client, messageStore, logger)
	if cfg.ReadOnly {
		fmt.Println("Read-only mode: sending messages is disabled.")
	} else if len(cfg.AllowedRecipients) == 0 && !cfg.AllowAnyRecipient {
		fmt.Println("Sending is disabled: set WHATSAPP_ALLOWED_RECIPIENTS (or WHATSAPP_ALLOW_ANY_RECIPIENT=1) to enable it.")
	} else if len(cfg.AllowedRecipients) == 0 {
		fmt.Println("Warning: WHATSAPP_ALLOW_ANY_RECIPIENT is set, so any individual or group can be messaged.")
	}

	// Start REST API server
	server, err := startRESTServer(client, messageStore, cfg)
	if err != nil {
		logger.Errorf("Failed to start REST API server: %v", err)
		return
	}

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	server.Shutdown(shutdownCtx)
	cancel()
	// Disconnect client
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, logger waLog.Logger) string {
	// Individual chats are named from the local address book on every call, so a chat
	// stored before its contact or LID mapping arrived gets its real name later
	if jid.Server != types.GroupServer {
		return newContactResolver(client).name(context.Background(), jid)
	}

	// Group names need a network call, so reuse the stored name when there is one
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" {
		return existingName
	}

	// Need to determine chat name
	var name string

	// This is a group chat
	logger.Debugf("Getting name for group: %s", sanitizeForLog(chatJID))

	// Use conversation data if provided (from history sync)
	if conversation != nil {
		// Extract name from conversation if available
		// This uses type assertions to handle different possible types
		var displayName, convName *string
		// Try to extract the fields we care about regardless of the exact type
		v := reflect.ValueOf(conversation)
		if v.Kind() == reflect.Ptr && !v.IsNil() {
			v = v.Elem()

			// Try to find DisplayName field
			if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
				dn := displayNameField.Elem().String()
				displayName = &dn
			}

			// Try to find Name field
			if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
				n := nameField.Elem().String()
				convName = &n
			}
		}

		// Use the name we found
		if displayName != nil && *displayName != "" {
			name = *displayName
		} else if convName != nil && *convName != "" {
			name = *convName
		}
	}

	// If we didn't get a name, try group info
	if name == "" {
		groupInfo, err := client.GetGroupInfo(context.Background(), jid)
		if err == nil && groupInfo.Name != "" {
			name = groupInfo.Name
		} else {
			// Fallback name for groups
			name = fmt.Sprintf("Group %s", jid.User)
		}
	}

	logger.Debugf("Using group name: %s", sanitizeForLog(name))

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with the newest message timestamp; the order of messages in a
			// sync blob is not guaranteed
			timestamp := latestHistoryTimestamp(messages)
			if timestamp.IsZero() {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// Extract media info
				var media MediaInfo
				if msg.Message.Message != nil {
					media = extractMediaInfo(msg.Message.Message)
				}

				// Skip messages with no content and no media
				if content == "" && media.Type == "" {
					continue
				}

				// Determine sender
				isFromMe := msg.Message.GetKey().GetFromMe()
				sender := historySender(jid, msg.Message)
				if isFromMe {
					sender = client.Store.ID.User
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreHistoryMessage(msgID, chatJID, sender, content, timestamp, isFromMe, media)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)

	// History sync carries LID-to-phone mappings, which can name more chats
	syncContacts(client, messageStore, logger)
}

// historySender returns the user part of the sender of a message that someone else
// sent, as live messages store it. In groups the sender is in the message's participant
// field (older clients use the key's); when neither is set the sender is unknown (""),
// never the group's own ID.
func historySender(chat types.JID, msg *waWeb.WebMessageInfo) string {
	if isIndividualServer(chat.Server) || chat.Server == types.NewsletterServer {
		return chat.User
	}
	for _, participant := range []string{msg.GetParticipant(), msg.GetKey().GetParticipant()} {
		if participant == "" {
			continue
		}
		if jid, err := types.ParseJID(participant); err == nil {
			return jid.User
		}
	}
	return ""
}

// latestHistoryTimestamp returns the newest message time in a history sync conversation.
func latestHistoryTimestamp(messages []*waHistorySync.HistorySyncMsg) time.Time {
	var latest time.Time
	for _, msg := range messages {
		if msg == nil || msg.Message == nil {
			continue
		}
		if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
			if t := time.Unix(int64(ts), 0); t.After(latest) {
				latest = t
			}
		}
	}
	return latest
}

// syncContacts refreshes the contacts table and individual chat names.
func syncContacts(client *whatsmeow.Client, messageStore *MessageStore, logger waLog.Logger) {
	renamed, err := messageStore.SyncContacts(context.Background(), newContactResolver(client))
	if err != nil {
		logger.Warnf("Failed to sync contacts: %v", err)
	} else if renamed > 0 {
		fmt.Printf("Updated the names of %d chats from your contacts\n", renamed)
	}
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		fmt.Println("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		fmt.Println("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
		fmt.Println("Client is not logged in. Please scan the QR code first.")
		return
	}

	// Build and send a history sync request
	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		fmt.Println("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		fmt.Printf("Failed to request history sync: %v\n", err)
	} else {
		fmt.Println("History sync requested. Waiting for server response...")
	}
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position.
	// Opus granule positions always count 48 kHz samples, whatever the input rate.
	const sampleRate = 48000
	var lastGranule uint64
	var preSkip uint16 = 0
	var foundOpusHead bool

	// Scan through the file looking for Ogg pages
	for i := 0; i < len(data); {
		// Check if we have enough data to read Ogg page header
		if i+27 > len(data) {
			break
		}

		// Verify Ogg page signature
		if string(data[i:i+4]) != "OggS" {
			// Skip until next potential page
			i++
			continue
		}

		// Extract header fields
		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		// Extract segment table
		if i+27+numSegments > len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		// Calculate page size
		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		// Stop at a truncated page instead of reading past the end of the data
		if i+pageSize > len(data) {
			break
		}

		// Check if we're looking at an OpusHead packet (should be in first few pages)
		if !foundOpusHead && pageSeqNum <= 1 {
			// Look for "OpusHead" marker in this page
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			// OpusHead format: Magic(8) + Version(1) + Channels(1) + PreSkip(2) + InputSampleRate(4) + ...
			if headPos >= 0 && headPos+16 <= len(pageData) {
				preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
				foundOpusHead = true
			}
		}

		// Keep track of last valid granule position (-1 means no packet ends on this page)
		if granulePos != 0 && granulePos != math.MaxUint64 {
			lastGranule = granulePos
		}

		// Move to next page
		i += pageSize
	}

	if !foundOpusHead {
		fmt.Println("Warning: OpusHead not found, using default values")
	}

	// Calculate duration based on granule position
	if lastGranule > uint64(preSkip) {
		// Formula for duration: (lastGranule - preSkip) / sampleRate
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		fmt.Printf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)\n",
			durationSeconds, lastGranule)
	} else {
		// Fallback to rough estimation if granule position not found
		fmt.Println("Warning: No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	fmt.Printf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes\n",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// min returns the smaller of x or y
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Seed the random number generator for consistent results with the same duration
	rng := rand.New(rand.NewSource(int64(duration)))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rng.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
