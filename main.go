package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const maxQueueMessages = 1024

type config struct {
	listenAddr  string
	stateDir    string
	authToken   string
	displayName string
}

type bridge struct {
	cfg    config
	client *whatsmeow.Client

	mu              sync.Mutex
	qrEvent         string
	qrCode          string
	qrTimeout       time.Duration
	lastError       string
	lastPairingCode string
	queue           []bridgeMessage
	nextCursor      int64
}

type bridgeMessage struct {
	Seq     int64  `json:"-"`
	ID      string `json:"id"`
	From    string `json:"from"`
	Text    string `json:"text"`
	ChatID  string `json:"chat_id"`
	IsGroup bool   `json:"is_group"`
	GroupID string `json:"group_id,omitempty"`
}

type messageRef struct {
	ID          string `json:"id"`
	ChatJID     string `json:"chat_jid"`
	SenderJID   string `json:"sender_jid,omitempty"`
	FromMe      bool   `json:"from_me"`
	TimestampMS int64  `json:"timestamp_ms"`
}

type pollRequest struct {
	AccountID string `json:"account_id"`
	Cursor    string `json:"cursor"`
}

type sendRequest struct {
	AccountID string `json:"account_id"`
	To        string `json:"to"`
	Text      string `json:"text"`
}

type pairCodeRequest struct {
	PhoneNumber string `json:"phone_number"`
	DisplayName string `json:"display_name"`
}

type actionRequest struct {
	AccountID string `json:"account_id"`
	To        string `json:"to"`
	MessageID string `json:"message_id"`
	Text      string `json:"text,omitempty"`
	Emoji     string `json:"emoji,omitempty"`
}

func main() {
	cfg := loadConfig()

	b, err := newBridge(cfg)
	if err != nil {
		log.Fatalf("failed to initialize bridge: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", b.withAuth(b.handleHealth))
	mux.HandleFunc("/qr", b.withAuth(b.handleQR))
	mux.HandleFunc("/pair-code", b.withAuth(b.handlePairCode))
	mux.HandleFunc("/poll", b.withAuth(b.handlePoll))
	mux.HandleFunc("/send", b.withAuth(b.handleSend))
	mux.HandleFunc("/edit", b.withAuth(b.handleEdit))
	mux.HandleFunc("/delete", b.withAuth(b.handleDelete))
	mux.HandleFunc("/reaction", b.withAuth(b.handleReaction))
	mux.HandleFunc("/read", b.withAuth(b.handleRead))

	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("whatsmeow bridge listening on %s", cfg.listenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
}

func loadConfig() config {
	listenAddr := strings.TrimSpace(os.Getenv("NULLCLAW_WHATSMEOW_BRIDGE_LISTEN"))
	if listenAddr == "" {
		listenAddr = "127.0.0.1:3301"
	}

	stateDir := strings.TrimSpace(os.Getenv("NULLCLAW_WHATSMEOW_BRIDGE_STATE_DIR"))
	if stateDir == "" {
		stateDir = "./state"
	}

	displayName := strings.TrimSpace(os.Getenv("NULLCLAW_WHATSMEOW_BRIDGE_DISPLAY_NAME"))
	if displayName == "" {
		displayName = "Chrome (Linux)"
	}

	return config{
		listenAddr:  listenAddr,
		stateDir:    stateDir,
		authToken:   strings.TrimSpace(os.Getenv("NULLCLAW_WHATSMEOW_BRIDGE_TOKEN")),
		displayName: displayName,
	}
}

func newBridge(cfg config) (*bridge, error) {
	if err := os.MkdirAll(cfg.stateDir, 0o755); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(cfg.stateDir, "whatsmeow.db")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	container := sqlstore.NewWithDB(db, "sqlite3", nil)
	if err := container.Upgrade(context.Background()); err != nil {
		return nil, err
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		return nil, err
	}

	client := whatsmeow.NewClient(deviceStore, nil)
	b := &bridge{
		cfg:     cfg,
		client:  client,
		qrEvent: "idle",
	}
	client.AddEventHandler(b.handleEvent)

	if deviceStore.ID == nil {
		qrChan, err := client.GetQRChannel(context.Background())
		if err != nil {
			return nil, err
		}
		go b.consumeQRChannel(qrChan)
	}

	if err := client.Connect(); err != nil {
		return nil, err
	}

	return b, nil
}

func (b *bridge) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b.cfg.authToken != "" {
			authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
			expected := "Bearer " + b.cfg.authToken
			if authHeader != expected {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next(w, r)
	}
}

func (b *bridge) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	b.mu.Lock()
	response := map[string]any{
		"ok":                     true,
		"connected":              b.client.IsConnected(),
		"logged_in":              b.client.IsLoggedIn(),
		"qr_event":               b.qrEvent,
		"qr_available":           b.qrCode != "",
		"pairing_code_available": b.lastPairingCode != "",
		"last_error":             b.lastError,
	}
	b.mu.Unlock()

	writeJSON(w, http.StatusOK, response)
}

func (b *bridge) handleQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	b.mu.Lock()
	response := map[string]any{
		"event":           b.qrEvent,
		"code":            b.qrCode,
		"timeout_seconds": int64(b.qrTimeout / time.Second),
	}
	b.mu.Unlock()

	writeJSON(w, http.StatusOK, response)
}

func (b *bridge) handlePairCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req pairCodeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	phone := normalizeDigits(req.PhoneNumber)
	if phone == "" {
		writeError(w, http.StatusBadRequest, "phone_number is required")
		return
	}

	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = b.cfg.displayName
	}

	code, err := b.client.PairPhone(r.Context(), phone, true, whatsmeow.PairClientChrome, displayName)
	if err != nil {
		b.setLastError("pair_phone_failed:" + err.Error())
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	b.mu.Lock()
	b.lastPairingCode = code
	b.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"pairing_code": code,
	})
}

func (b *bridge) handlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req pollRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cursor := int64(0)
	if strings.TrimSpace(req.Cursor) != "" {
		parsed, err := strconv.ParseInt(strings.TrimSpace(req.Cursor), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "cursor must be an integer string")
			return
		}
		cursor = parsed
	}

	b.mu.Lock()
	messages := make([]bridgeMessage, 0, len(b.queue))
	for _, item := range b.queue {
		if item.Seq > cursor {
			messages = append(messages, item)
		}
	}
	nextCursor := strconv.FormatInt(b.nextCursor, 10)
	b.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"next_cursor": nextCursor,
		"messages":    messages,
	})
}

func (b *bridge) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req sendRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	target, err := parseTargetJID(req.To)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	text := req.Text

	resp, err := b.client.SendMessage(r.Context(), target, &waProto.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		b.setLastError("send_failed:" + err.Error())
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"accepted":   true,
		"message_id": encodeMessageRef(messageRef{ID: resp.ID, ChatJID: target.String(), FromMe: true, TimestampMS: resp.Timestamp.UnixMilli()}),
	})
}

func (b *bridge) handleEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req actionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ref, chat, _, err := parseActionRef(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := b.client.SendMessage(r.Context(), chat, b.client.BuildEdit(chat, ref.ID, &waProto.Message{
		Conversation: proto.String(req.Text),
	}))
	if err != nil {
		b.setLastError("edit_failed:" + err.Error())
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"accepted":   true,
		"message_id": encodeMessageRef(messageRef{ID: resp.ID, ChatJID: chat.String(), FromMe: true, TimestampMS: resp.Timestamp.UnixMilli()}),
	})
}

func (b *bridge) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req actionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ref, chat, sender, err := parseActionRef(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	revokeSender := types.EmptyJID
	if !ref.FromMe {
		revokeSender = sender
	}

	resp, err := b.client.SendMessage(r.Context(), chat, b.client.BuildRevoke(chat, revokeSender, ref.ID))
	if err != nil {
		b.setLastError("delete_failed:" + err.Error())
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"accepted":   true,
		"message_id": encodeMessageRef(messageRef{ID: resp.ID, ChatJID: chat.String(), FromMe: true, TimestampMS: resp.Timestamp.UnixMilli()}),
	})
}

func (b *bridge) handleReaction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req actionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ref, chat, sender, err := parseActionRef(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := b.client.SendMessage(r.Context(), chat, b.client.BuildReaction(chat, sender, ref.ID, req.Emoji))
	if err != nil {
		b.setLastError("reaction_failed:" + err.Error())
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"accepted":   true,
		"message_id": encodeMessageRef(messageRef{ID: resp.ID, ChatJID: chat.String(), FromMe: true, TimestampMS: resp.Timestamp.UnixMilli()}),
	})
}

func (b *bridge) handleRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req actionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ref, chat, sender, err := parseActionRef(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	timestamp := time.UnixMilli(ref.TimestampMS)
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	if err := b.client.MarkRead(r.Context(), []types.MessageID{ref.ID}, timestamp, chat, sender); err != nil {
		b.setLastError("mark_read_failed:" + err.Error())
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"accepted": true,
	})
}

func (b *bridge) handleEvent(evt any) {
	switch e := evt.(type) {
	case *events.Message:
		b.handleInboundMessage(e)
	case *events.Connected:
		b.mu.Lock()
		b.qrEvent = "success"
		b.qrCode = ""
		b.lastError = ""
		b.mu.Unlock()
	case *events.Disconnected:
		b.setLastError("disconnected")
	case *events.LoggedOut:
		b.mu.Lock()
		b.qrEvent = "logged_out"
		b.qrCode = ""
		b.lastError = "logged_out"
		b.mu.Unlock()
	}
}

func (b *bridge) consumeQRChannel(ch <-chan whatsmeow.QRChannelItem) {
	for item := range ch {
		b.mu.Lock()
		b.qrEvent = item.Event
		b.qrTimeout = item.Timeout
		if item.Event == "code" {
			b.qrCode = item.Code
		}
		if item.Error != nil {
			b.lastError = item.Error.Error()
		}
		b.mu.Unlock()
	}
}

func (b *bridge) handleInboundMessage(evt *events.Message) {
	if evt == nil || evt.Info.IsFromMe || evt.Message == nil {
		return
	}

	text := extractText(evt.Message)
	if strings.TrimSpace(text) == "" {
		return
	}

	chat := evt.Info.Chat.String()
	sender := evt.Info.Sender.String()
	if sender == "" {
		sender = chat
	}

	msg := bridgeMessage{
		ID:      encodeMessageRef(messageRef{ID: evt.Info.ID, ChatJID: chat, SenderJID: sender, FromMe: evt.Info.IsFromMe, TimestampMS: evt.Info.Timestamp.UnixMilli()}),
		From:    sender,
		Text:    text,
		ChatID:  chat,
		IsGroup: evt.Info.IsGroup,
	}
	if evt.Info.IsGroup {
		msg.GroupID = chat
	}

	b.mu.Lock()
	b.nextCursor++
	msg.Seq = b.nextCursor
	b.queue = append(b.queue, msg)
	if len(b.queue) > maxQueueMessages {
		b.queue = append([]bridgeMessage(nil), b.queue[len(b.queue)-maxQueueMessages:]...)
	}
	b.mu.Unlock()
}

func (b *bridge) setLastError(message string) {
	b.mu.Lock()
	b.lastError = message
	b.mu.Unlock()
}

func parseActionRef(req actionRequest) (messageRef, types.JID, types.JID, error) {
	if strings.TrimSpace(req.MessageID) == "" {
		return messageRef{}, types.EmptyJID, types.EmptyJID, errors.New("message_id is required")
	}

	ref, err := decodeMessageRef(req.MessageID)
	if err != nil {
		return messageRef{}, types.EmptyJID, types.EmptyJID, err
	}

	chat, err := parseTargetJID(coalesce(req.To, ref.ChatJID))
	if err != nil {
		return messageRef{}, types.EmptyJID, types.EmptyJID, err
	}

	sender := types.EmptyJID
	if ref.SenderJID != "" {
		sender, err = parseTargetJID(ref.SenderJID)
		if err != nil {
			return messageRef{}, types.EmptyJID, types.EmptyJID, err
		}
	}
	if sender.IsEmpty() {
		sender = chat
	}

	return ref, chat, sender, nil
}

func parseTargetJID(raw string) (types.JID, error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return types.EmptyJID, errors.New("target is required")
	}
	if strings.Contains(target, "@") {
		return types.ParseJID(target)
	}

	digits := normalizeDigits(target)
	if digits == "" {
		return types.EmptyJID, errors.New("target must be a JID or phone number")
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}

func normalizeDigits(input string) string {
	var out strings.Builder
	for _, r := range input {
		if r >= '0' && r <= '9' {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func extractText(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}
	if msg.GetConversation() != "" {
		return msg.GetConversation()
	}
	if msg.GetExtendedTextMessage() != nil && msg.GetExtendedTextMessage().GetText() != "" {
		return msg.GetExtendedTextMessage().GetText()
	}
	if msg.GetImageMessage() != nil && msg.GetImageMessage().GetCaption() != "" {
		return msg.GetImageMessage().GetCaption()
	}
	if msg.GetVideoMessage() != nil && msg.GetVideoMessage().GetCaption() != "" {
		return msg.GetVideoMessage().GetCaption()
	}
	if msg.GetEphemeralMessage() != nil {
		return extractText(msg.GetEphemeralMessage().GetMessage())
	}
	if msg.GetViewOnceMessage() != nil {
		return extractText(msg.GetViewOnceMessage().GetMessage())
	}
	if msg.GetViewOnceMessageV2() != nil {
		return extractText(msg.GetViewOnceMessageV2().GetMessage())
	}
	if msg.GetEditedMessage() != nil && msg.GetEditedMessage().GetMessage() != nil && msg.GetEditedMessage().GetMessage().GetProtocolMessage() != nil {
		return extractText(msg.GetEditedMessage().GetMessage().GetProtocolMessage().GetEditedMessage())
	}
	return ""
}

func encodeMessageRef(ref messageRef) string {
	raw, _ := json.Marshal(ref)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeMessageRef(encoded string) (messageRef, error) {
	var ref messageRef
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return ref, errors.New("message_id is not valid base64url")
	}
	if err := json.Unmarshal(data, &ref); err != nil {
		return ref, errors.New("message_id is not valid JSON")
	}
	if ref.ID == "" || ref.ChatJID == "" {
		return ref, errors.New("message_id is incomplete")
	}
	return ref, nil
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": message,
	})
}

func coalesce(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
