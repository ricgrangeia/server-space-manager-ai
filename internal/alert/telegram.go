package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Telegram delivers alerts via the Bot API. Dedupes identical messages within DedupeWindow.
type Telegram struct {
	BotToken     string
	ChatID       string
	DedupeWindow time.Duration

	http *http.Client
	mu   sync.Mutex
	last map[string]time.Time
}

func NewTelegram(botToken, chatID string) *Telegram {
	return &Telegram{
		BotToken:     botToken,
		ChatID:       chatID,
		DedupeWindow: 30 * time.Minute,
		http:         &http.Client{Timeout: 15 * time.Second},
		last:         map[string]time.Time{},
	}
}

func (t *Telegram) Enabled() bool { return t.BotToken != "" && t.ChatID != "" }

func (t *Telegram) Send(ctx context.Context, key, text string) error {
	if !t.Enabled() {
		return errors.New("telegram not configured")
	}
	t.mu.Lock()
	if last, ok := t.last[key]; ok && time.Since(last) < t.DedupeWindow {
		t.mu.Unlock()
		return nil
	}
	t.last[key] = time.Now()
	t.mu.Unlock()

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.BotToken)
	payload, _ := json.Marshal(map[string]any{
		"chat_id":    t.ChatID,
		"text":       text,
		"parse_mode": "Markdown",
	})
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram: status %d", resp.StatusCode)
	}
	return nil
}
