// Command ssm is the server-space-manager daemon.
//
// It periodically scans:
//   - host filesystem paths configured under host_paths
//   - filesystem capacity (statfs) for configured mount points
//   - Docker containers (log sizes, bind mounts) via the Docker socket
//   - Docker volumes and images via the Docker disk-usage API
//
// Each scan is persisted to SQLite for trend analysis and exposed via a small
// HTTP API and an embedded dashboard. Threshold breaches are forwarded to
// Telegram (when configured). A local LLM (vLLM / OpenAI-compatible endpoint)
// answers natural-language questions about the snapshot at /api/ask.
//
// The daemon is read-only by default: it never deletes, prunes, or rotates
// anything. Operators act on its recommendations themselves.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/ricgrangeia/server-space-manager-ai/internal/aireview"
	"github.com/ricgrangeia/server-space-manager-ai/internal/alert"
	"github.com/ricgrangeia/server-space-manager-ai/internal/api"
	"github.com/ricgrangeia/server-space-manager-ai/internal/config"
	"github.com/ricgrangeia/server-space-manager-ai/internal/llm"
	"github.com/ricgrangeia/server-space-manager-ai/internal/scanner"
	"github.com/ricgrangeia/server-space-manager-ai/internal/store"
	"github.com/ricgrangeia/server-space-manager-ai/internal/version"
)

func main() {
	cfgPath := flag.String("config", envOr("SSM_CONFIG", "/etc/ssm/config.yaml"), "path to config.yaml")
	dbPath := flag.String("db", envOr("SSM_DB", "/var/lib/ssm/ssm.db"), "path to SQLite database file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		log.Println(version.String())
		return
	}

	log.Printf("server-space-manager %s starting", version.String())

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		log.Fatalf("db dir: %v", err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	host := scanner.NewHost(cfg)

	var dock *scanner.DockerScanner
	if cfg.Docker.Enabled {
		dock, err = scanner.NewDocker(cfg.Docker)
		if err != nil {
			log.Printf("docker scanner disabled: %v", err)
		}
	}

	var llmClient *llm.Client
	if cfg.LLM.Enabled {
		llmClient = llm.New(cfg.LLM)
	}

	var notifier alert.Notifier = noopNotifier{}
	if cfg.Telegram.Enabled && cfg.Telegram.BotToken != "" && cfg.Telegram.ChatID != "" {
		notifier = alert.NewTelegram(cfg.Telegram.BotToken, cfg.Telegram.ChatID)
		log.Printf("telegram alerts enabled for chat %s", cfg.Telegram.ChatID)
	}

	rules := alert.DefaultRules()
	if cfg.Alerts.FSWarnPct > 0 {
		rules.FSWarnPct = cfg.Alerts.FSWarnPct
	}
	if cfg.Alerts.FSCritPct > 0 {
		rules.FSCritPct = cfg.Alerts.FSCritPct
	}
	if cfg.Alerts.BigItemMB > 0 {
		rules.BigItemMB = cfg.Alerts.BigItemMB
	}
	if cfg.Alerts.GrowthPct > 0 {
		rules.GrowthPct = cfg.Alerts.GrowthPct
	}

	ctx, cancel := signalContext()
	defer cancel()

	reviewer := aireview.New(llmClient, st, notifier)

	// Build the Server with manual-trigger callbacks wired to the same
	// functions the cron scheduler invokes. We construct the *Server later
	// (after triggers point at it for SetSnapshot) by deferring the call to
	// runScan through a closure that captures srv.
	// notifyManual wraps a manual-trigger job with "started" / "finished"
	// Telegram pings. Cron-driven jobs stay silent so the bot doesn't ping
	// every hour — the existing rule alerts and AI verdicts already cover
	// the noteworthy events. We use a fresh dedupe key per run (timestamp)
	// so the start message is never suppressed by the Telegram dedupe
	// window in alert.Telegram.
	notifyManual := func(label string, fn func(context.Context) string) func(context.Context) {
		return func(c context.Context) {
			startedKey := fmt.Sprintf("manual:%s:start:%d", label, time.Now().UnixNano())
			_ = notifier.Send(c, startedKey, "▶️ *"+label+"* started (manual trigger)")
			t0 := time.Now()
			summary := fn(c)
			dur := time.Since(t0).Round(time.Second)
			doneKey := fmt.Sprintf("manual:%s:done:%d", label, time.Now().UnixNano())
			msg := fmt.Sprintf("✅ *%s* finished in %s", label, dur)
			if summary != "" {
				msg += " — " + summary
			}
			_ = notifier.Send(c, doneKey, msg)
		}
	}

	var srv *api.Server
	triggers := api.Triggers{
		Scan: notifyManual("scan", func(c context.Context) string {
			runScan(c, cfg, st, host, dock, srv, notifier, rules)
			return buildScanReport(srv.LastSnapshot())
		}),
		AIReview: notifyManual("AI review", func(c context.Context) string {
			if reviewer.LLM == nil {
				return "skipped (LLM disabled)"
			}
			findings, err := reviewer.RunReview(c)
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("%d findings", len(findings))
		}),
		Digest: notifyManual("digest", func(c context.Context) string {
			if reviewer.LLM == nil {
				return "skipped (LLM disabled)"
			}
			if _, err := reviewer.RunDigest(c); err != nil {
				return "error: " + err.Error()
			}
			return "sent"
		}),
	}
	srv = api.New(cfg.HTTP.Listen, cfg.HTTP.Password, llmClient, st, triggers)
	if cfg.HTTP.Password == "" {
		log.Printf("WARNING: http.password is empty — dashboard is unauthenticated")
	}

	scheduler := buildScheduler(ctx, cfg, st, host, dock, srv, notifier, rules, reviewer)
	scheduler.Start()
	defer func() { <-scheduler.Stop().Done() }()

	// Restore the most-recent scan from SQLite so the dashboard isn't empty
	// for the first few minutes after a redeploy. Without this, until the
	// startup scan completes the UI shows zero containers / zero filesystems
	// even though the history is intact, which looks indistinguishable from
	// data loss.
	if samples, taken, err := st.LatestScan(); err != nil {
		log.Printf("restore last scan: %v", err)
	} else if len(samples) > 0 {
		srv.SetSnapshot(api.Snapshot{
			TakenAt:     taken,
			Samples:     samples,
			SampleCount: len(samples),
		})
		log.Printf("restored last scan from %s (%d samples)", taken.Format(time.RFC3339), len(samples))
	}

	// Start the HTTP server BEFORE the first scan. The startup scan can take
	// a while on first run (large bind mounts, lots of containers); we don't
	// want the dashboard unreachable while it churns. The summary endpoint
	// will return an empty snapshot until the first scan completes.
	log.Printf("HTTP listening on %s", cfg.HTTP.Listen)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Fatalf("http: %v", err)
		}
	}()

	// Telegram "online" ping. Useful after a redeploy: the bot confirms the
	// new build is running, with the version baked into the binary. The
	// dedupe key includes the boot time so reboots are never suppressed by
	// the notifier's 30-minute dedupe window.
	bootKey := fmt.Sprintf("boot:%d", time.Now().UnixNano())
	_ = notifier.Send(ctx, bootKey, fmt.Sprintf(
		"🟢 *ssm online* — %s\nlisten %s · scan %s · ai_review %s · digest %s",
		version.String(), cfg.HTTP.Listen,
		cfg.Schedules.Scan, cfg.Schedules.AIReview, cfg.Schedules.Digest))

	// Kick off the first scan in the background so it doesn't block startup.
	// Subsequent scans are driven by the cron scheduler.
	go runScan(ctx, cfg, st, host, dock, srv, notifier, rules)

	<-ctx.Done()
	log.Printf("shutting down")
}

// buildScanReport formats a short human-readable summary of a snapshot,
// suitable for a Telegram message body. Markdown-flavoured (Telegram
// `parse_mode=Markdown`). Sections are skipped if empty so the report
// stays compact on quiet hosts.
func buildScanReport(snap api.Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d samples, %d alerts", snap.SampleCount, len(snap.Alerts))

	// Filesystems with their % full, sorted by % desc.
	type fsRow struct {
		path string
		pct  float64
	}
	var fss []fsRow
	for _, s := range snap.Samples {
		if s.Kind != "fs" {
			continue
		}
		var extra struct {
			Total int64 `json:"total"`
			Avail int64 `json:"avail"`
		}
		if err := json.Unmarshal([]byte(s.Extra), &extra); err == nil && extra.Total > 0 {
			used := extra.Total - extra.Avail
			fss = append(fss, fsRow{s.Label, 100 * float64(used) / float64(extra.Total)})
		}
	}
	sort.Slice(fss, func(i, j int) bool { return fss[i].pct > fss[j].pct })
	if len(fss) > 0 {
		b.WriteString("\n\n*Filesystems:*")
		for i, f := range fss {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&b, "\n• `%s` — %.0f%%", f.path, f.pct)
		}
	}

	addTop := func(title, kind string, n int) {
		var picked []store.Sample
		for _, s := range snap.Samples {
			if s.Kind == kind {
				picked = append(picked, s)
			}
		}
		sort.Slice(picked, func(i, j int) bool { return picked[i].Bytes > picked[j].Bytes })
		if len(picked) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n\n*%s:*", title)
		for i, s := range picked {
			if i >= n {
				break
			}
			fmt.Fprintf(&b, "\n• `%s` — %s", s.Label, humanBytes(s.Bytes))
		}
	}
	addTop("Top container logs", "container_log", 3)
	addTop("Top volumes", "volume", 3)

	// Up to 3 active alerts, truncated so we don't blow the Telegram message limit.
	if len(snap.Alerts) > 0 {
		b.WriteString("\n\n*Alerts:*")
		for i, a := range snap.Alerts {
			if i >= 3 {
				fmt.Fprintf(&b, "\n• (+%d more)", len(snap.Alerts)-i)
				break
			}
			b.WriteString("\n• ")
			b.WriteString(a)
		}
	}
	return b.String()
}

// humanBytes formats a byte count with two-letter units (KB, MB, GB…).
// Kept here rather than imported so cmd/ssm has no cross-package dep
// just for a one-line helper.
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

// runScan executes one disk-usage pass: walk host paths, query Docker,
// persist samples to SQLite, prune old rows, evaluate static rules, and
// publish the result to the dashboard.
//
// It is called both at startup and from the cron "scan" job.
func runScan(
	ctx context.Context,
	cfg *config.Config,
	st *store.Store,
	host *scanner.HostScanner,
	dock *scanner.DockerScanner,
	srv *api.Server,
	notifier alert.Notifier,
	rules alert.Rules,
) {
	now := time.Now().UTC()
	samples := host.Scan(now)
	if dock != nil {
		samples = append(samples, dock.Scan(ctx, now)...)
	}
	if err := st.Insert(samples); err != nil {
		log.Printf("store insert: %v", err)
	}
	if cfg.RetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -cfg.RetentionDays)
		if removed, err := st.Prune(cutoff); err == nil && removed > 0 {
			log.Printf("pruned %d old samples", removed)
		}
	}
	fired := alert.Evaluate(ctx, notifier, rules, samples)

	// Persist rule-engine findings so /api/ask can include them as "prior
	// decisions" in the prompt context.
	if len(fired) > 0 {
		records := make([]store.Finding, 0, len(fired))
		for _, msg := range fired {
			records = append(records, store.Finding{
				Kind: "rule", Key: "rule", Severity: "warn",
				Reason: msg, Source: "rules", CreatedAt: now,
			})
		}
		if err := st.InsertFindings(records); err != nil {
			log.Printf("persist findings: %v", err)
		}
	}

	srv.SetSnapshot(api.Snapshot{
		TakenAt:     now,
		Samples:     samples,
		SampleCount: len(samples),
		Alerts:      fired,
	})
	log.Printf("scan complete: %d samples, %d alerts", len(samples), len(fired))
}

// buildScheduler wires the configured cron expressions to the corresponding
// jobs. Empty expressions disable the job. The scheduler is responsible only
// for *when* — the work itself lives in runScan / Reviewer.RunReview /
// Reviewer.RunDigest, so each can also be triggered manually via the API.
func buildScheduler(
	ctx context.Context,
	cfg *config.Config,
	st *store.Store,
	host *scanner.HostScanner,
	dock *scanner.DockerScanner,
	srv *api.Server,
	notifier alert.Notifier,
	rules alert.Rules,
	reviewer *aireview.Reviewer,
) *cron.Cron {
	c := cron.New()

	if expr := cfg.Schedules.Scan; expr != "" {
		if _, err := c.AddFunc(expr, func() {
			runScan(ctx, cfg, st, host, dock, srv, notifier, rules)
		}); err != nil {
			log.Fatalf("schedule scan: %v", err)
		}
		log.Printf("scheduled scan: %s", expr)
	}

	if expr := cfg.Schedules.AIReview; expr != "" && reviewer.LLM != nil {
		if _, err := c.AddFunc(expr, func() {
			findings, err := reviewer.RunReview(ctx)
			if err != nil {
				log.Printf("ai review: %v", err)
				return
			}
			log.Printf("ai review fired %d findings", len(findings))
		}); err != nil {
			log.Fatalf("schedule ai_review: %v", err)
		}
		log.Printf("scheduled ai_review: %s", expr)
	}

	if expr := cfg.Schedules.Digest; expr != "" && reviewer.LLM != nil {
		if _, err := c.AddFunc(expr, func() {
			if _, err := reviewer.RunDigest(ctx); err != nil {
				log.Printf("digest: %v", err)
			}
		}); err != nil {
			log.Fatalf("schedule digest: %v", err)
		}
		log.Printf("scheduled digest: %s", expr)
	}

	return c
}

// noopNotifier is used when Telegram is disabled, so the rule engine can call
// Send unconditionally without a nil check.
type noopNotifier struct{}

func (noopNotifier) Send(_ context.Context, _, _ string) error { return nil }

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
