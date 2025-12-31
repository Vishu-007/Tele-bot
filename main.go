package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
)

const messagesCollection = "telegram_messages"

type TelegramMessage struct {
	ChannelID        int64     `firestore:"channel_id"`
	ChannelName      string    `firestore:"channel_name"`
	MessageID        int       `firestore:"message_id"`
	MessageText      string    `firestore:"message_text"`
	MessageTimestamp time.Time `firestore:"message_timestamp"`

	ReceivedAt time.Time `firestore:"received_at"`

	IsProcessed bool       `firestore:"is_processed"`
	IsRelevant  *bool      `firestore:"is_relevant"`
	ProcessedAt *time.Time `firestore:"processed_at"`

	JobFingerprint *string `firestore:"job_fingerprint"`
	IsForwarded    bool    `firestore:"is_forwarded"`
}

type TelegramUpdate struct {
	UpdateID    int                 `json:"update_id"`
	Message     *TelegramMessageRaw `json:"message,omitempty"`
	ChannelPost *TelegramMessageRaw `json:"channel_post,omitempty"`
}

type Photo struct {
	FileID string `json:"file_id"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
}
type TelegramMessageRaw struct {
	MessageID int    `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text,omitempty"`
	Caption   string `json:"caption,omitempty"`
	Chat      Chat   `json:"chat"`

	Photo    []Photo   `json:"photo,omitempty"`
	Document *Document `json:"document,omitempty"`
}

type Chat struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

var (
	urlRegex     = regexp.MustCompile(`https?://\S+`)
	emailRegex   = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	phoneRegex   = regexp.MustCompile(`\+?\d[\d\s\-]{7,}\d`)
	nonTextRegex = regexp.MustCompile(`[^a-z0-9\s]`)
	spaceRegex   = regexp.MustCompile(`\s+`)
)

var internshipKeywords = []string{
	"intern", "internship", "trainee", "apprentice",
}

var studentKeywords = []string{
	"final year", "final-year", "student", "students only",
	"currently pursuing", "pursuing degree", "campus hiring",
	"on campus", "college student",
}

var experienceRejectPatterns = []string{
	"2+ year", "3+ year", "4+ year",
	"minimum 2 year", "minimum 3 year",
	"2 years experience", "3 years experience",
	"experienced candidate", "experienced only",
}

var explicitNon2025Patterns = []string{
	"2024 batch", "2023 batch", "2022 batch", "2021 batch",
	"2022-2024", "2021-2023",
}

func getFirestoreClient(ctx context.Context) (*firestore.Client, error) {
	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		return nil, fmt.Errorf("GOOGLE_CLOUD_PROJECT not set")
	}
	return firestore.NewClient(ctx, projectID)
}
func storeMessage(ctx context.Context, client *firestore.Client, docID string, msg TelegramMessage) error {
	_, err := client.Collection(messagesCollection).
		Doc(docID).
		Set(ctx, msg)

	return err
}

func fetchUnprocessed(ctx context.Context, client *firestore.Client, limit int) ([]*firestore.DocumentSnapshot, error) {
	iter := client.Collection(messagesCollection).
		Where("is_processed", "==", false).
		Limit(limit).
		Documents(ctx)

	return iter.GetAll()
}

func updateProcessingResult(
	ctx context.Context,
	client *firestore.Client,
	docID string,
	isRelevant bool,
	fingerprint string,
) error {

	now := time.Now()

	updates := []firestore.Update{
		{Path: "is_processed", Value: true},
		{Path: "is_relevant", Value: isRelevant},
		{Path: "processed_at", Value: now},
		{Path: "job_fingerprint", Value: fingerprint},
	}

	_, err := client.Collection(messagesCollection).
		Doc(docID).
		Update(ctx, updates)

	return err
}

func markForwarded(ctx context.Context, client *firestore.Client, docID string) error {
	_, err := client.Collection(messagesCollection).
		Doc(docID).
		Update(ctx, []firestore.Update{
			{Path: "is_forwarded", Value: true},
		})

	return err
}

func isFingerprintForwarded(ctx context.Context, client *firestore.Client, fingerprint string) (bool, error) {
	iter := client.Collection(messagesCollection).
		Where("job_fingerprint", "==", fingerprint).
		Where("is_forwarded", "==", true).
		Limit(1).
		Documents(ctx)

	docs, err := iter.GetAll()
	if err != nil {
		return false, err
	}

	return len(docs) > 0, nil
}

func normalizeText(text string) string {
	t := strings.ToLower(text)

	t = urlRegex.ReplaceAllString(t, "")
	t = emailRegex.ReplaceAllString(t, "")
	t = phoneRegex.ReplaceAllString(t, "")
	t = nonTextRegex.ReplaceAllString(t, " ")
	t = spaceRegex.ReplaceAllString(t, " ")

	return strings.TrimSpace(t)
}

func computeFingerprint(messageText string) string {
	normalized := normalizeText(messageText)

	if len(normalized) > 400 {
		normalized = normalized[:400]
	}

	hash := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(hash[:])
}

func containsAny(text string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

func excludes2025(text string) bool {
	// explicit exclusions
	if containsAny(text, explicitNon2025Patterns) {
		return true
	}

	// if range exists AND includes 2025 → accept
	if strings.Contains(text, "2025") {
		return false
	}

	return false
}

func isRelevant(messageText string) bool {
	text := strings.ToLower(messageText)

	if containsAny(text, internshipKeywords) {
		return false
	}

	if containsAny(text, studentKeywords) {
		return false
	}

	if containsAny(text, experienceRejectPatterns) {
		return false
	}

	if excludes2025(text) {
		return false
	}

	return true
}

func telegramWebhookHandler(w http.ResponseWriter, r *http.Request) {
	// Telegram sends POST only
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := context.Background()

	var update TelegramUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		// Malformed update → ACK and drop
		w.WriteHeader(http.StatusOK)
		return
	}

	// Handle both message & channel_post
	var msg *TelegramMessageRaw
	if update.Message != nil {
		msg = update.Message
	} else if update.ChannelPost != nil {
		msg = update.ChannelPost
	} else {
		// Not a message we care about
		w.WriteHeader(http.StatusOK)
		return
	}

	hasMedia := msg.Document != nil || len(msg.Photo) > 0
	hasText := msg.Text != "" || msg.Caption != ""

	if !hasText && !hasMedia {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Build Firestore document ID
	docID := fmt.Sprintf("%d_%d", msg.Chat.ID, msg.MessageID)

	client, err := getFirestoreClient(ctx)
	if err != nil {
		// 🔴 IMPORTANT: log, but ACK Telegram
		log.Println("Firestore client error:", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	defer client.Close()

	firestoreMsg := TelegramMessage{
		ChannelID:        msg.Chat.ID,
		ChannelName:      msg.Chat.Title,
		MessageID:        msg.MessageID,
		MessageText:      msg.Text,
		MessageTimestamp: time.Unix(msg.Date, 0),

		ReceivedAt:  time.Now(),
		IsProcessed: false,
		IsRelevant:  nil,
		ProcessedAt: nil,

		JobFingerprint: nil,
		IsForwarded:    false,
	}

	// Idempotent upsert
	if err := storeMessage(ctx, client, docID, firestoreMsg); err != nil {
		// 🔴 IMPORTANT: log, but ACK Telegram
		log.Println("Firestore write error:", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	// ✅ Final ACK — exactly once
	w.WriteHeader(http.StatusOK)
}

func telegramAPIURL(method string) string {
	return fmt.Sprintf(
		"https://api.telegram.org/bot%s/%s",
		os.Getenv("BOT_TOKEN"),
		method,
	)
}

func forwardMessage(chatID int64, fromChatID int64, messageID int) error {
	payload := map[string]interface{}{
		"chat_id":      chatID,
		"from_chat_id": fromChatID,
		"message_id":   messageID,
	}

	body, _ := json.Marshal(payload)

	resp, err := http.Post(
		telegramAPIURL("forwardMessage"),
		"application/json",
		bytes.NewBuffer(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram forward failed: %d", resp.StatusCode)
	}

	return nil
}

func sendTextMessage(chatID int64, text string) error {
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}

	body, _ := json.Marshal(payload)

	resp, err := http.Post(
		telegramAPIURL("sendMessage"),
		"application/json",
		bytes.NewBuffer(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram send failed: %d", resp.StatusCode)
	}

	return nil
}

func workerHandler(w http.ResponseWriter, r *http.Request) {
	// 1️⃣ Always handle GET separately (browser / health check)
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("worker alive"))
		return
	}

	// 2️⃣ Only allow POST for actual processing
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := context.Background()

	client, err := getFirestoreClient(ctx)
	if err != nil {
		http.Error(w, "firestore unavailable", http.StatusInternalServerError)
		return
	}
	defer client.Close()

	docs, err := fetchUnprocessed(ctx, client, 50)
	if err != nil {
		http.Error(w, "failed to fetch", http.StatusInternalServerError)
		return
	}

	for _, doc := range docs {
		processOne(ctx, client, doc)
	}

	// 3️⃣ Always write a response body
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("processed"))
}

func processOne(
	ctx context.Context,
	client *firestore.Client,
	doc *firestore.DocumentSnapshot,
) {

	var msg TelegramMessage
	if err := doc.DataTo(&msg); err != nil {
		return
	}

	// Compute fingerprint
	fingerprint := computeFingerprint(msg.MessageText)

	// Apply relevance rules
	relevant := isRelevant(msg.MessageText)

	// Update processing state
	updateProcessingResult(
		ctx,
		client,
		doc.Ref.ID,
		relevant,
		fingerprint,
	)

	if !relevant {
		return
	}

	// Deduplication check
	forwarded, err := isFingerprintForwarded(ctx, client, fingerprint)
	if err != nil || forwarded {
		return
	}

	// Send message to personal chat
	chatID := mustGetPersonalChatID()
	sendTextMessage(chatID, formatMessage(msg))

	// Mark forwarded
	markForwarded(ctx, client, doc.Ref.ID)
}

func formatMessage(msg TelegramMessage) string {
	return fmt.Sprintf(
		"📢 Job Post\n\nChannel: %s\n\n%s",
		msg.ChannelName,
		msg.MessageText,
	)
}

func mustGetPersonalChatID() int64 {
	idStr := os.Getenv("PERSONAL_CHAT_ID")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	return id
}

func main() {
	// Cloud Run provides PORT env variable
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // local fallback
	}

	mux := http.NewServeMux()

	// Webhook endpoint (Telegram calls this)
	mux.HandleFunc("/webhook", telegramWebhookHandler)

	// Worker endpoint (Cloud Scheduler / manual trigger)
	mux.HandleFunc("/worker", workerHandler)

	log.Printf("Starting server on port %s", port)

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
