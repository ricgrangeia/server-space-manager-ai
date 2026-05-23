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
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
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
	var srv *api.Server
	triggers := api.Triggers{
		Scan: func(c context.Context) { runScan(c, cfg, st, host, dock, srv, notifier, rules) },
		AIReview: func(c context.Context) {
			if reviewer.LLM == nil {
				log.Printf("manual ai_review skipped: llm disabled")
				return
			}
			findings, err := reviewer.RunReview(c)
			if err != nil {
				log.Printf("manual ai_review: %v", err)
				return
			}
			log.Printf("manual ai_review fired %d findings", len(findings))
		},
		Digest: func(c context.Context) {
			if reviewer.LLM == nil {
				log.Printf("manual digest skipped: llm disabled")
				return
			}
			if _, err := reviewer.RunDigest(c); err != nil {
				log.Printf("manual digest: %v", err)
			}
		},
	}
	srv = api.New(cfg.HTTP.Listen, cfg.HTTP.Password, llmClient, st, triggers)
	if cfg.HTTP.Password == "" {
		log.Printf("WARNING: http.password is empty — dashboard is unauthenticated")
	}

	scheduler := buildScheduler(ctx, cfg, st, host, dock, srv, notifier, rules, reviewer)
	scheduler.Start()
	defer func() { <-scheduler.Stop().Done() }()

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

	// Kick off the first scan in the background so it doesn't block startup.
	// Subsequent scans are driven by the cron scheduler.
	go runScan(ctx, cfg, st, host, dock, srv, notifier, rules)

	<-ctx.Done()
	log.Printf("shutting down")
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
