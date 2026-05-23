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

// maxDedupeEntries caps the dedupe map size so long-running deployments
// don't accumulate one entry per unique key forever. When the map exceeds
// this threshold we sweep out entries older than DedupeWindow. The cap is
// generous — a typical host fires far fewer distinct alert keys per day.
const maxDedupeEntries = 2048

// Telegram delivers alerts via the Bot API. Dedupes identical messages
// within DedupeWindow so a single recurring event doesn't spam the chat.
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

// sweepLocked drops dedupe entries older than DedupeWindow. Caller must hold
// t.mu. Called opportunistically from Send when the map outgrows the cap —
// no background goroutine, which keeps the type simple to construct/test.
func (t *Telegram) sweepLocked(now time.Time) {
	cutoff := now.Add(-t.DedupeWindow)
	for k, ts := range t.last {
		if ts.Before(cutoff) {
			delete(t.last, k)
		}
	}
}

func (t *Telegram) Enabled() bool { return t.BotToken != "" && t.ChatID != "" }

func (t *Telegram) Send(ctx context.Context, key, text string) error {
	if !t.Enabled() {
		return errors.New("telegram not configured")
	}
	now := time.Now()
	t.mu.Lock()
	if last, ok := t.last[key]; ok && now.Sub(last) < t.DedupeWindow {
		t.mu.Unlock()
		return nil
	}
	t.last[key] = now
	if len(t.last) > maxDedupeEntries {
		t.sweepLocked(now)
	}
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
