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
		// Scan deliberately bypasses notifyManual: runScan already sends
		// the end-of-scan Telegram report (so cron and manual paths look
		// identical). We just prepend a one-line "manual trigger" ping so
		// the operator sees their click was acknowledged.
		Scan: func(c context.Context) {
			startKey := fmt.Sprintf("manual:scan:start:%d", time.Now().UnixNano())
			_ = notifier.Send(c, startKey, "▶️ *scan* started (manual trigger)")
			runScan(c, cfg, st, host, dock, srv, notifier, rules)
		},
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

	// Diagnostic: report the SQLite file size at startup. Lets us tell at a
	// glance whether the persistent volume is doing its job — a freshly-
	// initialised DB is ~28 KB; an established one with weeks of scans is
	// MB-sized. Sudden drops here mean the named volume isn't persisting.
	if fi, err := os.Stat(*dbPath); err == nil {
		log.Printf("DB %s: %d bytes (modified %s)", *dbPath, fi.Size(), fi.ModTime().Format(time.RFC3339))
	} else {
		log.Printf("DB %s: not present yet (%v)", *dbPath, err)
	}

	// Restore the most-recent scan from SQLite so the dashboard isn't empty
	// for the first few minutes after a redeploy. Without this, until the
	// startup scan completes the UI shows zero containers / zero filesystems
	// even though the history is intact, which looks indistinguishable from
	// data loss.
	samples, taken, err := st.LatestScan()
	switch {
	case err != nil:
		log.Printf("restore last scan: error %v", err)
	case len(samples) == 0:
		log.Printf("restore last scan: DB has no prior samples (first run or volume reset)")
	default:
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

// buildScanReport formats the rich end-of-scan summary pushed to Telegram.
// Markdown-flavoured (Telegram `parse_mode=Markdown`). Sections are skipped
// when empty so the report stays compact on quiet hosts.
//
// Layout (top to bottom):
//
//	headline ........... sample count + alert count
//	*Disk* ............. filesystems with usage bar + GB used / total
//	*Top container logs* ... 5 biggest
//	*Top volumes* .......... 5 biggest in-use
//	*Top bind mounts* ...... 5 biggest non-trivial
//	*Biggest files* ........ 5 biggest individual files
//	*Anomalies* ............ recent 24h growth vs 7-day baseline
//	*Orphan volumes* ....... rollup count + total bytes + top 3 names
//	*Alerts* ............... rule fires from this scan (first 5)
func buildScanReport(snap api.Snapshot, st *store.Store) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d samples, %d alerts", snap.SampleCount, len(snap.Alerts))

	// --- Disk overview: every filesystem with bar + bytes + 24h delta ----
	// The "delta" lets the operator see at a glance whether a filesystem
	// is filling up, draining, or steady — which is what "track if it's
	// growing" boils down to. We pull from store.GrowthSince once and
	// look up each filesystem's delta locally.
	deltas := map[string]int64{}
	if st != nil {
		if rows, err := st.GrowthSince("fs", time.Now().Add(-24*time.Hour), 100); err == nil {
			for _, r := range rows {
				deltas[r.Key] = r.DeltaBytes
			}
		}
	}

	type fsRow struct {
		path                    string
		used, free, total, pct  float64
		delta24h                int64
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
			used := float64(extra.Total - extra.Avail)
			fss = append(fss, fsRow{
				path:     s.Label,
				used:     used,
				free:     float64(extra.Avail),
				total:    float64(extra.Total),
				pct:      100 * used / float64(extra.Total),
				delta24h: deltas[s.Key],
			})
		}
	}
	sort.Slice(fss, func(i, j int) bool { return fss[i].pct > fss[j].pct })
	if len(fss) > 0 {
		b.WriteString("\n\n*Disk:*")
		for _, f := range fss {
			// Two lines per filesystem: header with bar + %, detail line
			// with absolute numbers and the 24h delta. Two lines is
			// readable in Telegram and easier to scan than a single very
			// long line.
			fmt.Fprintf(&b, "\n• `%s`  %s  %.0f%%", f.path, usageBar(f.pct), f.pct)
			fmt.Fprintf(&b, "\n    used %s · free %s · total %s · %s/24h",
				humanBytes(int64(f.used)),
				humanBytes(int64(f.free)),
				humanBytes(int64(f.total)),
				signedHumanBytes(f.delta24h))
		}
	}

	// --- Generic top-N helper for kinds where size is what matters --------
	addTop := func(title, kind string, n int, minBytes int64) {
		var picked []store.Sample
		for _, s := range snap.Samples {
			if s.Kind == kind && s.Bytes >= minBytes {
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
	addTop("Top container logs", "container_log", 5, 0)
	addTop("Top volumes", "volume", 5, 0)
	addTop("Top bind mounts", "bind_mount", 5, 100*1024*1024)
	addTop("Biggest files", "big_file", 5, 0)

	// --- Anomalies: last-24h growth vs 7-day per-item baseline ----------
	if st != nil {
		var anoms []store.AnomalyRow
		for _, k := range []string{"container_log", "volume", "host_path", "bind_mount", "image"} {
			rows, err := st.BaselineAnomalies(k, 7, 3.0, 50*1024*1024, 5)
			if err == nil {
				anoms = append(anoms, rows...)
			}
		}
		sort.Slice(anoms, func(i, j int) bool { return anoms[i].Ratio > anoms[j].Ratio })
		if len(anoms) > 0 {
			b.WriteString("\n\n*Anomalies (24h vs baseline):*")
			for i, a := range anoms {
				if i >= 5 {
					break
				}
				ratio := fmt.Sprintf("%.1fx", a.Ratio)
				if a.Ratio >= 999 {
					ratio = "new"
				}
				fmt.Fprintf(&b, "\n• `%s` +%s (%s)",
					a.Label, humanBytes(a.Last24hDelta), ratio)
			}
		}
	}

	// --- Orphan volumes: rollup count + bytes + top 3 ---------------------
	var orphanCount int
	var orphanBytes int64
	type orphanRow struct {
		label string
		bytes int64
	}
	var orphans []orphanRow
	for _, s := range snap.Samples {
		if s.Kind == "orphan_volume" {
			orphanCount++
			orphanBytes += s.Bytes
			orphans = append(orphans, orphanRow{s.Label, s.Bytes})
		}
	}
	if orphanCount > 0 {
		sort.Slice(orphans, func(i, j int) bool { return orphans[i].bytes > orphans[j].bytes })
		fmt.Fprintf(&b, "\n\n*Orphan volumes:* %d, %s total",
			orphanCount, humanBytes(orphanBytes))
		for i, o := range orphans {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&b, "\n• `%s` (%s)", o.label, humanBytes(o.bytes))
		}
	}

	// --- Active alerts from this scan ------------------------------------
	// The orphan rollup string is already rendered in its own section
	// above; drop it here so the report doesn't duplicate that content.
	var alerts []string
	for _, a := range snap.Alerts {
		if strings.Contains(a, "orphan volumes") {
			continue
		}
		alerts = append(alerts, a)
	}
	if len(alerts) > 0 {
		b.WriteString("\n\n*Alerts:*")
		for i, a := range alerts {
			if i >= 5 {
				fmt.Fprintf(&b, "\n• (+%d more)", len(alerts)-i)
				break
			}
			b.WriteString("\n• ")
			b.WriteString(a)
		}
	}
	return b.String()
}

// signedHumanBytes formats a (possibly negative) byte delta with an explicit
// arrow + sign so growth direction is obvious at a glance:
//
//	+125 MB   filesystem grew by 125 MB
//	-2.0 GB   filesystem freed 2 GB
//	±0 B      no measurable change
//	(no data) value is unknown / first-ever scan
//
// Operators tracking "is the disk filling?" want this number to lean toward
// the lower the better; the arrow makes that scannable even on small screens.
func signedHumanBytes(b int64) string {
	switch {
	case b > 0:
		return "↑ +" + humanBytes(b)
	case b < 0:
		return "↓ -" + humanBytes(-b)
	default:
		return "±0 B"
	}
}

// usageBar returns a 10-segment unicode bar for a percentage. Used in the
// Telegram disk-overview lines so each filesystem's headroom is visible at
// a glance, without needing a chart.
func usageBar(pct float64) string {
	const segs = 10
	filled := int(pct * segs / 100)
	if filled < 0 {
		filled = 0
	}
	if filled > segs {
		filled = segs
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", segs-filled)
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

	snap := api.Snapshot{
		TakenAt:     now,
		Samples:     samples,
		SampleCount: len(samples),
		Alerts:      fired,
	}
	srv.SetSnapshot(snap)
	log.Printf("scan complete: %d samples, %d alerts", len(samples), len(fired))

	// End-of-scan Telegram report. Same code path for cron and manual
	// triggers — manual just adds a "started" ping before runScan, so the
	// finished message lives in one place. Gated by telegram.scan_reports
	// so operators can mute hourly pings if they want only alerts.
	if cfg.Telegram.ScanReportsEnabled() {
		key := "scan:report:" + now.UTC().Format(time.RFC3339)
		_ = notifier.Send(ctx, key, "📊 *scan complete* — "+buildScanReport(snap, st))
	}
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
