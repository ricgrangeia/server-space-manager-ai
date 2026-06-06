package alert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// CommandHandlers wires Telegram slash-commands to the same code paths the
// dashboard's manual triggers use. Each handler returns a short string that
// is sent back to the chat as the command's reply. Empty return means
// "started, will report when done" (the caller should send its own follow-
// up via the notifier).
//
// All handlers are invoked with a context that is cancelled when the bot
// shuts down, so long-running jobs (a 3-minute scan) can be aborted on
// SIGTERM cleanly.
type CommandHandlers struct {
	OnScan   func(ctx context.Context) string
	OnReview func(ctx context.Context) string
	OnDigest func(ctx context.Context) string
	OnStatus func(ctx context.Context) string
}

// CommandBot polls Telegram for incoming messages and dispatches recognised
// commands to the configured handlers. Only messages from AllowedChatID are
// acted on; everything else is silently dropped (we never reply to
// strangers — that would leak the bot's existence).
//
// Implementation note: we use the long-polling getUpdates endpoint with a
// 30-second wait. Telegram blocks the request until a message arrives or
// the timeout elapses, so the polling loop costs ~one HTTP request every
// 30s while idle and reacts within a second when a real command arrives.
type CommandBot struct {
	BotToken      string
	AllowedChatID string
	Handlers      CommandHandlers
	Sender        *Telegram // reuses the existing send pipeline for replies

	http    *http.Client
	offset  int64
}

// NewCommandBot constructs a bot that authenticates against the given chat.
// Sender is the existing Telegram notifier — reused so command replies go
// through the same dedupe + transport as alerts.
func NewCommandBot(token, chatID string, handlers CommandHandlers, sender *Telegram) *CommandBot {
	return &CommandBot{
		BotToken:      token,
		AllowedChatID: chatID,
		Handlers:      handlers,
		Sender:        sender,
		// Long-poll timeout is 30s; client timeout must be longer than
		// that or the request will be killed before Telegram replies.
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// telegram update structures — only the fields we use.
type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

type tgUpdatesResp struct {
	OK     bool       `json:"ok"`
	Result []tgUpdate `json:"result"`
	Desc   string     `json:"description"`
}

// Run blocks until ctx is cancelled, polling Telegram for new messages and
// dispatching them. Errors are logged and retried with a short backoff so
// transient network blips don't kill the loop.
func (b *CommandBot) Run(ctx context.Context) {
	if b.BotToken == "" || b.AllowedChatID == "" {
		log.Printf("telegram commands: disabled (missing token or chat_id)")
		return
	}
	// Register the command list with Telegram so users see autocompletion
	// suggestions when they tap "/" — no more remembering commands.
	if err := b.registerCommands(ctx); err != nil {
		log.Printf("telegram commands: setMyCommands failed: %v", err)
	}
	log.Printf("telegram commands: listening for /scan /digest /review /status /help from chat %s",
		b.AllowedChatID)

	// On startup, fetch with offset=-1 to skip any backlog from while
	// the daemon was down. Otherwise a redeploy could trigger a flood of
	// stale commands that the user already saw fail to respond.
	b.offset = -1

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		updates, err := b.poll(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("telegram commands: poll error: %v", err)
			// Backoff so a misconfigured token doesn't hammer the API.
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			b.handle(ctx, u)
			if u.UpdateID >= b.offset {
				b.offset = u.UpdateID + 1
			}
		}
	}
}

func (b *CommandBot) poll(ctx context.Context) ([]tgUpdate, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?timeout=30&offset=%d",
		b.BotToken, b.offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out tgUpdatesResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("telegram: %s", out.Desc)
	}
	// After the startup skip-backlog request, switch back to "from where
	// we left off" semantics.
	if b.offset == -1 && len(out.Result) > 0 {
		b.offset = out.Result[len(out.Result)-1].UpdateID + 1
	}
	return out.Result, nil
}

// registerCommands publishes the supported command list to Telegram via
// setMyCommands. The list then appears as autocomplete suggestions when the
// user taps the "/" button in the chat, plus a hamburger menu next to the
// text input — operators don't need to remember the names.
//
// Telegram caches this list per-bot, so we only need to call it on startup;
// subsequent calls just overwrite the previous registration, which is fine.
func (b *CommandBot) registerCommands(ctx context.Context) error {
	body := map[string]any{
		"commands": []map[string]string{
			{"command": "scan", "description": "Run a disk scan now"},
			{"command": "digest", "description": "Generate an AI digest now"},
			{"command": "review", "description": "Run the AI anomaly review"},
			{"command": "status", "description": "Current snapshot summary"},
			{"command": "help", "description": "Show this command list"},
		},
	}
	return b.tgPOST(ctx, "setMyCommands", body)
}

// tgPOST is a small helper for non-getUpdates Bot API calls (setMyCommands,
// etc.) that take a JSON body and return the standard {ok, description}
// envelope.
func (b *CommandBot) tgPOST(ctx context.Context, method string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/%s", b.BotToken, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	var env struct {
		OK   bool   `json:"ok"`
		Desc string `json:"description"`
	}
	if err := json.Unmarshal(rb, &env); err != nil {
		return err
	}
	if !env.OK {
		return fmt.Errorf("%s: %s", method, env.Desc)
	}
	return nil
}

// handle dispatches one update. Unauthorised chats are silently ignored;
// unrecognised commands get a short hint so the operator knows the bot
// received the message but didn't know what to do with it.
func (b *CommandBot) handle(ctx context.Context, u tgUpdate) {
	if u.Message == nil {
		return
	}
	chatID := fmt.Sprintf("%d", u.Message.Chat.ID)
	if chatID != b.AllowedChatID {
		// Don't reply — strangers should not learn the bot is alive.
		return
	}
	text := strings.TrimSpace(u.Message.Text)
	if text == "" {
		return
	}

	// Take the first whitespace-separated token, strip "@botname" suffix
	// (groups attach it when the bot is one of several).
	cmd := strings.ToLower(strings.SplitN(text, " ", 2)[0])
	if at := strings.Index(cmd, "@"); at != -1 {
		cmd = cmd[:at]
	}

	switch cmd {
	case "/scan":
		b.runAsync(ctx, "scan", b.Handlers.OnScan)
	case "/digest":
		b.runAsync(ctx, "digest", b.Handlers.OnDigest)
	case "/review", "/ai_review", "/ai-review":
		b.runAsync(ctx, "AI review", b.Handlers.OnReview)
	case "/status":
		b.runSync(ctx, "status", b.Handlers.OnStatus)
	case "/help", "/start":
		b.reply(ctx, "*Commands*\n"+
			"/scan — run a disk scan now\n"+
			"/digest — generate an AI digest now\n"+
			"/review — run the AI anomaly review\n"+
			"/status — current snapshot summary\n"+
			"/help — this list")
	default:
		b.reply(ctx, "Unknown command. Try /help.")
	}
}

// runAsync acknowledges immediately and runs the handler in a goroutine.
// Suited for long jobs (scan ~3 min) so the Telegram reply isn't held open.
func (b *CommandBot) runAsync(ctx context.Context, label string, fn func(context.Context) string) {
	if fn == nil {
		b.reply(ctx, label+" not configured")
		return
	}
	b.reply(ctx, "▶️ "+label+" started")
	go func() {
		// Detached context so a SIGTERM during the job doesn't cancel
		// the user-initiated run mid-flight; the parent ctx is for the
		// polling loop, not for the work the command kicks off.
		summary := fn(context.Background())
		if summary != "" {
			b.reply(context.Background(), "✅ "+label+" — "+summary)
		}
	}()
}

// runSync invokes the handler inline and replies with its result. For quick
// handlers (e.g. /status) where the user expects an immediate answer.
func (b *CommandBot) runSync(ctx context.Context, label string, fn func(context.Context) string) {
	if fn == nil {
		b.reply(ctx, label+" not configured")
		return
	}
	out := fn(ctx)
	if out == "" {
		out = "(no data)"
	}
	b.reply(ctx, out)
}

// reply sends a message back to the authorised chat. Uses a unique dedupe
// key so the existing Telegram dedupe window doesn't suppress identical
// replies to repeated commands.
func (b *CommandBot) reply(ctx context.Context, text string) {
	if b.Sender == nil {
		return
	}
	key := fmt.Sprintf("cmd-reply:%d", time.Now().UnixNano())
	_ = b.Sender.Send(ctx, key, text)
}
