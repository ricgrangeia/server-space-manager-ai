// Package api exposes the HTTP surface of the server-space-manager: a small
// JSON API used by the embedded dashboard, plus an /api/ask endpoint that
// forwards a structured snapshot of the current scan to the configured LLM.
//
// The dashboard itself is a single static HTML file (dashboard.html) embedded
// at compile time, so the binary has no external runtime asset dependency.
package api

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ricgrangeia/server-space-manager-ai/internal/llm"
	"github.com/ricgrangeia/server-space-manager-ai/internal/store"
	"github.com/ricgrangeia/server-space-manager-ai/internal/version"
)

//go:embed dashboard.html
var dashboardHTML []byte

// Snapshot is the most recent scan result, kept in memory by the Server so that
// the dashboard can render without re-querying SQLite on every request.
type Snapshot struct {
	TakenAt     time.Time       `json:"taken_at"`
	Samples     []store.Sample  `json:"-"`
	SampleCount int             `json:"sample_count"`
	Alerts      []string        `json:"alerts"`
}

// Triggers are the on-demand jobs the dashboard can fire. Each callback is
// expected to be safe to invoke concurrently with the scheduled cron jobs;
// the implementation lives in cmd/ssm (so the HTTP layer doesn't have to
// import the scanner/aireview packages directly).
type Triggers struct {
	Scan     func(context.Context)
	AIReview func(context.Context)
	Digest   func(context.Context)
}

// Server hosts the HTTP API and dashboard. It is safe for concurrent use; the
// scanner goroutine calls SetSnapshot whenever a new scan completes.
type Server struct {
	listen   string
	password string // shared password; empty disables auth (LAN-trusted mode)
	llm      *llm.Client
	store    *store.Store // used by /api/ask to enrich the prompt with history
	trig     Triggers

	mu       sync.RWMutex
	snap     Snapshot
	// running guards each trigger kind so a button mash doesn't queue
	// several scans on top of each other.
	running  map[string]bool
	runningM sync.Mutex
}

// New builds a Server. The store enables /api/ask to feed the model recent
// growth trends and prior decisions in addition to the live snapshot.
// password may be empty to disable auth entirely (only safe on a fully
// trusted network). The LLM client may be nil; /api/ask will return an error
// in that case rather than panicking. Triggers wire the dashboard's manual
// "run now" buttons to the corresponding job; nil fields disable the button.
func New(listen, password string, llmClient *llm.Client, st *store.Store, trig Triggers) *Server {
	return &Server{
		listen:   listen,
		password: password,
		llm:      llmClient,
		store:    st,
		trig:     trig,
		running:  map[string]bool{},
	}
}

// SetSnapshot atomically replaces the in-memory snapshot. Called by the
// scheduler after each completed scan.
func (s *Server) SetSnapshot(snap Snapshot) {
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
}

// Routes returns an http.Handler with all endpoints wired up. When a password
// is configured, every route except /login and /healthz is gated by the
// cookie-based auth middleware.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.auth(s.handleDashboard))
	mux.HandleFunc("/api/summary", s.auth(s.handleSummary))
	mux.HandleFunc("/api/ask", s.auth(s.handleAsk))
	mux.HandleFunc("/api/trigger/scan", s.auth(s.triggerHandler("scan", s.trig.Scan)))
	mux.HandleFunc("/api/trigger/ai-review", s.auth(s.triggerHandler("ai_review", s.trig.AIReview)))
	mux.HandleFunc("/api/trigger/digest", s.auth(s.triggerHandler("digest", s.trig.Digest)))
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/healthz", s.handleHealth)
	return mux
}

const authCookieName = "ssm_auth"

// auth wraps a handler with the shared-password gate. If no password is
// configured, the wrapper is a no-op (useful for fully trusted networks).
// Browser requests are redirected to /login on failure; API requests receive
// a 401 JSON response so the dashboard's fetch() calls can react cleanly.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.password == "" || s.validCookie(r) {
			next(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

func (s *Server) validCookie(r *http.Request) bool {
	c, err := r.Cookie(authCookieName)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.password)) == 1
}

// handleLogin renders the password form (GET) or validates the submission
// (POST). On success it sets an HttpOnly cookie containing the password and
// redirects to the dashboard.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.password == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		got := r.FormValue("password")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.password)) != 1 {
			renderLogin(w, "Incorrect password.")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     authCookieName,
			Value:    s.password,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   60 * 60 * 24 * 30, // 30 days
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	renderLogin(w, "")
}

// triggerHandler returns an HTTP handler that runs `fn` in the background
// when called. Each (kind) is guarded against concurrent runs so repeated
// clicks don't pile up jobs. Responds immediately:
//   - 202 {"status":"started"} when the job kicks off
//   - 409 {"status":"busy"}    when the same kind is already running
//   - 503 when the trigger isn't wired (e.g. LLM disabled)
func (s *Server) triggerHandler(kind string, fn func(context.Context)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if fn == nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]string{"error": kind + " not available"})
			return
		}
		s.runningM.Lock()
		if s.running[kind] {
			s.runningM.Unlock()
			writeJSON(w, http.StatusConflict, map[string]string{"status": "busy"})
			return
		}
		s.running[kind] = true
		s.runningM.Unlock()

		go func() {
			defer func() {
				s.runningM.Lock()
				delete(s.running, kind)
				s.runningM.Unlock()
			}()
			fn(context.Background())
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "kind": kind})
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: authCookieName, Value: "", Path: "/", MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func renderLogin(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := `<!doctype html><html><head><meta charset="utf-8"><title>Login — Server Space Manager</title>
<style>
body{font-family:-apple-system,Segoe UI,Roboto,sans-serif;background:#0e1116;color:#e6edf3;
     display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
form{background:#161b22;border:1px solid #30363d;padding:24px;border-radius:8px;min-width:280px}
h1{margin:0 0 16px;font-size:18px}
input{width:100%;padding:8px;background:#0d1117;border:1px solid #30363d;border-radius:6px;
      color:#e6edf3;font-size:14px;box-sizing:border-box}
button{margin-top:12px;width:100%;padding:9px;background:#238636;border:0;border-radius:6px;
       color:#fff;font-size:14px;cursor:pointer}
button:hover{background:#2ea043}
.err{color:#f85149;font-size:13px;margin-top:8px}
</style></head><body>
<form method="POST" action="/login">
  <h1>Server Space Manager</h1>
  <input type="password" name="password" autofocus placeholder="Password" />
  <button type="submit">Sign in</button>
  ` + errBlock(errMsg) + `
</form></body></html>`
	_, _ = w.Write([]byte(page))
}

func errBlock(msg string) string {
	if msg == "" {
		return ""
	}
	return `<div class="err">` + msg + `</div>`
}

// ListenAndServe starts the HTTP server. Blocks until the listener errors.
func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:              s.listen,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(dashboardHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": version.String(),
	})
}

// summaryItem is a flat, dashboard-friendly view of a sample.
type summaryItem struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Bytes int64  `json:"bytes"`
}

type fsItem struct {
	Path    string  `json:"path"`
	Used    int64   `json:"used"`
	Total   int64   `json:"total"`
	UsedPct float64 `json:"used_pct"`
}

type summaryResp struct {
	TakenAt     time.Time     `json:"taken_at"`
	SampleCount int           `json:"sample_count"`
	Alerts      []string      `json:"alerts"`
	Filesystems []fsItem      `json:"filesystems"`
	TopLogs     []summaryItem `json:"top_logs"`
	TopVolumes  []summaryItem `json:"top_volumes"`
	TopPaths    []summaryItem `json:"top_paths"`
	TopImages   []summaryItem `json:"top_images"`
}

func (s *Server) handleSummary(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	snap := s.snap
	s.mu.RUnlock()

	resp := summaryResp{
		TakenAt:     snap.TakenAt,
		SampleCount: snap.SampleCount,
		Alerts:      snap.Alerts,
		TopLogs:     topN(snap.Samples, "container_log", 10),
		TopVolumes:  topN(snap.Samples, "volume", 10),
		TopPaths:    topN(snap.Samples, "host_path", 10),
		TopImages:   topN(snap.Samples, "image", 10),
	}
	for _, x := range snap.Samples {
		if x.Kind != "fs" {
			continue
		}
		var extra struct {
			Total int64 `json:"total"`
			Avail int64 `json:"avail"`
		}
		_ = json.Unmarshal([]byte(x.Extra), &extra)
		used := x.Bytes
		pct := 0.0
		if extra.Total > 0 {
			pct = 100 * float64(used) / float64(extra.Total)
		}
		resp.Filesystems = append(resp.Filesystems, fsItem{
			Path: x.Label, Used: used, Total: extra.Total, UsedPct: pct,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// askReq is the JSON payload accepted by /api/ask.
type askReq struct {
	Question string `json:"question"`
}

// handleAsk forwards the question, along with a compact summary of the
// most recent scan, to the configured LLM. The model is given a strict
// read-only role: it may suggest cleanups but never execute them.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.llm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "llm disabled"})
		return
	}
	var req askReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing question"})
		return
	}
	s.mu.RLock()
	snap := s.snap
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	system := "You are a careful Linux/Docker storage advisor. " +
		"You receive: the current disk-usage snapshot, growth trends over the " +
		"last 7 days, and any decisions previously made by the system " +
		"(static threshold alerts and prior AI reviews). " +
		"Use this history to reason — for example, if you've previously " +
		"flagged an item and it has continued to grow, say so. " +
		"Never invent paths, containers, or volumes that are not present in " +
		"the data shown to you. Suggest concrete actions ranked by risk " +
		"(safest first). Be concise."
	user := buildPrompt(snap, s.store, req.Question)

	answer, err := s.llm.Chat(ctx, system, user)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"answer": answer})
}

// buildPrompt constructs a token-budget-conscious snapshot to feed the LLM.
// Sections, in order:
//
//  1. The user's question.
//  2. Top-N current usage per kind (the live snapshot).
//  3. Active alerts from this scan.
//  4. 7-day growth trends per kind (from SQLite, top growers only).
//  5. Recent decisions: static-rule alerts and prior AI-review findings,
//     so the model can build on its own history rather than re-deriving
//     conclusions each time.
//
// We pre-aggregate (top-N per kind) rather than dumping every sample so the
// payload stays well under typical 8k context windows.
func buildPrompt(snap Snapshot, st *store.Store, question string) string {
	var b strings.Builder
	b.WriteString("# Snapshot taken: ")
	b.WriteString(snap.TakenAt.Format(time.RFC3339))
	b.WriteString("\n\n## Question\n")
	b.WriteString(question)
	b.WriteString("\n\n## Top items by kind (current)\n")

	for _, kind := range []string{"fs", "container_log", "volume", "orphan_volume", "image", "bind_mount", "host_path"} {
		items := topN(snap.Samples, kind, 10)
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n", kind)
		for _, it := range items {
			fmt.Fprintf(&b, "- %s — %s\n", it.Label, humanBytes(it.Bytes))
		}
	}
	if len(snap.Alerts) > 0 {
		b.WriteString("\n## Active alerts (this scan)\n")
		for _, a := range snap.Alerts {
			b.WriteString("- ")
			b.WriteString(a)
			b.WriteString("\n")
		}
	}

	// 7-day growth trends — only meaningful if the store has accumulated data.
	if st != nil {
		since := time.Now().AddDate(0, 0, -7)
		b.WriteString("\n## 7-day growth (top growers per kind)\n")
		any := false
		for _, kind := range []string{"container_log", "volume", "host_path", "bind_mount", "image"} {
			rows, err := st.GrowthSince(kind, since, 5)
			if err != nil || len(rows) == 0 {
				continue
			}
			any = true
			fmt.Fprintf(&b, "\n### %s\n", kind)
			for _, r := range rows {
				fmt.Fprintf(&b, "- %s — now %s (+%s)\n",
					r.Label, humanBytes(r.Bytes), humanBytes(r.DeltaBytes))
			}
		}
		if !any {
			b.WriteString("(no growth history yet — first scans still building baseline)\n")
		}

		// Last-24h anomalies vs 7-day per-item baseline. These are *unusual*
		// growths the static rules would not catch.
		b.WriteString("\n## Anomalies (last 24h vs 7-day baseline)\n")
		anyAnom := false
		for _, kind := range []string{"container_log", "volume", "host_path", "bind_mount", "image"} {
			rows, err := st.BaselineAnomalies(kind, 7, 3.0, 50*1024*1024, 5)
			if err != nil || len(rows) == 0 {
				continue
			}
			anyAnom = true
			fmt.Fprintf(&b, "\n### %s\n", kind)
			for _, r := range rows {
				ratio := fmt.Sprintf("%.1fx", r.Ratio)
				if r.Ratio >= 999 {
					ratio = "new-growth"
				}
				fmt.Fprintf(&b, "- %s — +%s in 24h (baseline %s/day, %s)\n",
					r.Label, humanBytes(r.Last24hDelta),
					humanBytes(int64(r.BaselineDaily)), ratio)
			}
		}
		if !anyAnom {
			b.WriteString("(none detected)\n")
		}

		// Prior decisions, most-recent-first. Caps at 30 to keep tokens reasonable.
		if findings, err := st.RecentFindings(since, 30); err == nil && len(findings) > 0 {
			b.WriteString("\n## Recent decisions (last 7 days)\n")
			for _, f := range findings {
				fmt.Fprintf(&b, "- [%s] %s/%s %s/%s — %s\n",
					f.CreatedAt.Format("2006-01-02 15:04"),
					f.Source, f.Severity, f.Kind, f.Key, f.Reason)
			}
		}
	}
	return b.String()
}

func topN(samples []store.Sample, kind string, n int) []summaryItem {
	var filtered []store.Sample
	for _, s := range samples {
		if s.Kind == kind {
			filtered = append(filtered, s)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Bytes > filtered[j].Bytes })
	if len(filtered) > n {
		filtered = filtered[:n]
	}
	out := make([]summaryItem, 0, len(filtered))
	for _, s := range filtered {
		out = append(out, summaryItem{Key: s.Key, Label: s.Label, Bytes: s.Bytes})
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func humanBytes(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
