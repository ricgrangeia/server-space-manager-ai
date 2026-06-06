// Package aireview wraps the LLM in two scheduled roles:
//
//   - Reviewer: asks the model to look at recent growth trends, decide which
//     items deserve an alert, and explain why. Output is constrained to a
//     JSON array; any item whose key is not present in the snapshot we sent
//     is dropped (defence against hallucinated container/volume names).
//
//   - Digest: asks the model to write a short, human-readable summary of
//     the last day/week of disk usage. The result goes straight to the
//     notifier (Telegram, typically) as a single message.
//
// Both jobs are driven externally by the cron scheduler in cmd/ssm; this
// package only knows how to *run one pass*, not when to run it.
package aireview

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ricgrangeia/server-space-manager-ai/internal/alert"
	"github.com/ricgrangeia/server-space-manager-ai/internal/llm"
	"github.com/ricgrangeia/server-space-manager-ai/internal/store"
)

// Reviewer runs the periodic AI passes.
type Reviewer struct {
	LLM      *llm.Client
	Store    *store.Store
	Notifier alert.Notifier
	// LookbackDays controls the trend window passed to the model.
	LookbackDays int
}

// New builds a Reviewer with sensible defaults.
func New(l *llm.Client, s *store.Store, n alert.Notifier) *Reviewer {
	return &Reviewer{LLM: l, Store: s, Notifier: n, LookbackDays: 7}
}

// Finding is the structured per-item verdict we accept back from the model.
type Finding struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"` // info | warn | crit
	Reason   string `json:"reason"`
}

// RunReview executes one AI-judged anomaly pass:
//  1. pull growth-since-N-days for the kinds we care about
//  2. ask the model to nominate items worth alerting on
//  3. validate the response (drop hallucinations)
//  4. push survivors through the notifier
//
// It is safe to call from a cron callback — the function never panics on
// LLM or store errors, it just logs and returns.
func (r *Reviewer) RunReview(ctx context.Context) ([]Finding, error) {
	if r.LLM == nil {
		return nil, fmt.Errorf("llm not configured")
	}
	since := time.Now().Add(-time.Duration(r.LookbackDays) * 24 * time.Hour)

	type kindRows struct {
		kind string
		rows []store.GrowthRow
	}
	var batches []kindRows
	var anomalies []store.AnomalyRow
	for _, k := range []string{"container_log", "volume", "host_path", "bind_mount", "image"} {
		rows, err := r.Store.GrowthSince(k, since, 15)
		if err == nil && len(rows) > 0 {
			batches = append(batches, kindRows{k, rows})
		}
		// Anomaly detection: items whose last-24h growth is >=3x their own
		// average over the prior 7 days, and at least 50 MB in absolute terms.
		// Skipped silently while the store is still warming up.
		if a, err := r.Store.BaselineAnomalies(k, 7, 3.0, 50*1024*1024, 10); err == nil {
			anomalies = append(anomalies, a...)
		}
	}
	if len(batches) == 0 && len(anomalies) == 0 {
		return nil, nil
	}

	// Build an allowlist of (kind,key) so we can reject anything the model invents.
	allow := map[string]struct{}{}
	var prompt strings.Builder
	prompt.WriteString("You are reviewing disk-usage trends. Below are top growers per kind over the lookback window, ")
	prompt.WriteString("followed by items whose last-24h growth is far above their own historical average.\n")
	fmt.Fprintf(&prompt, "Lookback: %d days\n\n", r.LookbackDays)
	for _, b := range batches {
		fmt.Fprintf(&prompt, "## %s\n", b.kind)
		for _, row := range b.rows {
			allow[b.kind+"|"+row.Key] = struct{}{}
			fmt.Fprintf(&prompt, "- key=%q label=%q size=%s delta=%s\n",
				row.Key, row.Label, human(row.Bytes), human(row.DeltaBytes))
		}
		prompt.WriteString("\n")
	}
	if len(anomalies) > 0 {
		prompt.WriteString("## Anomalies (last 24h vs own 7-day baseline)\n")
		for _, a := range anomalies {
			allow[a.Kind+"|"+a.Key] = struct{}{}
			ratio := fmt.Sprintf("%.1fx", a.Ratio)
			if a.Ratio >= 999 {
				ratio = "new-growth"
			}
			fmt.Fprintf(&prompt, "- kind=%s key=%q label=%q size=%s last24h=+%s baseline=%s/day ratio=%s\n",
				a.Kind, a.Key, a.Label, human(a.Bytes), human(a.Last24hDelta),
				human(int64(a.BaselineDaily)), ratio)
		}
		prompt.WriteString("\n")
	}
	prompt.WriteString(`Respond with ONLY a JSON array of findings. Each finding must use this shape:
{"key":"<exact key from above>","kind":"<exact kind>","severity":"info|warn|crit","reason":"<one short sentence>"}
Rules:
- Do not invent keys or kinds. Only reference items listed above.
- Pick at most 10 items. Skip noise; favor sustained growth or anomalies.
- Items in the "Anomalies" section deserve attention even if they aren't top growers overall — they just broke their own pattern.
- "crit" is reserved for things likely to cause an incident within days.`)

	system := "You are a careful Linux/Docker storage advisor. Output must be valid JSON only — no prose, no code fences."
	raw, err := r.LLM.Chat(ctx, system, prompt.String())
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}

	findings, err := parseFindings(raw)
	if err != nil {
		return nil, fmt.Errorf("parse llm output: %w (raw=%q)", err, truncate(raw, 200))
	}

	// Drop hallucinations and clamp severity values.
	out := findings[:0]
	for _, f := range findings {
		if _, ok := allow[f.Kind+"|"+f.Key]; !ok {
			continue
		}
		if f.Severity != "info" && f.Severity != "warn" && f.Severity != "crit" {
			f.Severity = "warn"
		}
		out = append(out, f)
	}

	// Persist findings so /api/ask can reason from prior verdicts.
	now := time.Now().UTC()
	persisted := make([]store.Finding, 0, len(out))
	for _, f := range out {
		persisted = append(persisted, store.Finding{
			Kind: f.Kind, Key: f.Key, Severity: f.Severity,
			Reason: f.Reason, Source: "ai_review", CreatedAt: now,
		})
	}
	if err := r.Store.InsertFindings(persisted); err != nil {
		// Non-fatal: keep going so the notifier still fires.
		fmt.Printf("ai review: persist findings: %v\n", err)
	}

	// Send notifications for warn/crit only (info stays in the dashboard).
	for _, f := range out {
		if f.Severity == "info" {
			continue
		}
		icon := "⚠️"
		if f.Severity == "crit" {
			icon = "🚨"
		}
		msg := fmt.Sprintf("%s *AI review* (%s) `%s`: %s", icon, f.Kind, f.Key, f.Reason)
		_ = r.Notifier.Send(ctx, "ai-review:"+f.Kind+":"+f.Key, msg)
	}
	return out, nil
}

// RunDigest produces a single human-readable summary and ships it to the
// notifier as one message. Recommended schedule: every 4-6 hours. The model
// receives a structured snapshot containing:
//
//   - Filesystem usage (used / free / total / 24h delta) — the disk-overview
//     bit the operator needs to answer "how full is the box right now?"
//     without opening the dashboard.
//   - Top growers per kind over the last 24 hours.
//
// We prepend a non-LLM "Disk:" header so the operator always sees raw
// numbers, even if the model omits or paraphrases them.
func (r *Reviewer) RunDigest(ctx context.Context) (string, error) {
	if r.LLM == nil {
		return "", fmt.Errorf("llm not configured")
	}
	since := time.Now().Add(-24 * time.Hour)

	// 24h FS deltas keyed by path so we can show direction-of-fill per
	// filesystem in both the prompt and the deterministic header.
	deltas := map[string]int64{}
	if rows, err := r.Store.GrowthSince("fs", since, 100); err == nil {
		for _, row := range rows {
			deltas[row.Key] = row.DeltaBytes
		}
	}

	// Build the deterministic "Disk:" header from the most recent FS samples.
	// LatestScan returns *all* kinds of samples from the most recent scan;
	// we filter to "fs" here.
	type fsLine struct {
		path     string
		used     int64
		avail    int64
		total    int64
		delta24h int64
	}
	var fss []fsLine
	if samples, _, err := r.Store.LatestScan(); err == nil {
		for _, s := range samples {
			if s.Kind != "fs" {
				continue
			}
			var extra struct {
				Total int64 `json:"total"`
				Avail int64 `json:"avail"`
			}
			if err := json.Unmarshal([]byte(s.Extra), &extra); err == nil && extra.Total > 0 {
				fss = append(fss, fsLine{
					path:     s.Label,
					used:     extra.Total - extra.Avail,
					avail:    extra.Avail,
					total:    extra.Total,
					delta24h: deltas[s.Key],
				})
			}
		}
	}

	var header strings.Builder
	if len(fss) > 0 {
		header.WriteString("*Disk:*")
		for _, f := range fss {
			pct := 100 * float64(f.used) / float64(f.total)
			header.WriteString(fmt.Sprintf("\n• `%s` %.0f%% — used %s · free %s · total %s · %s/24h",
				f.path, pct, human(f.used), human(f.avail), human(f.total),
				signedHuman(f.delta24h)))
		}
		header.WriteString("\n\n")
	}

	// LLM-written narrative built from a single global "top growers" list.
	// Earlier versions grouped by kind, which let the model reorder
	// across groups and produce a narrative where a 100 MB grower sat
	// above a 5 GB one. Now we merge every kind into one list, sort by
	// 24h delta descending, and instruct the model to preserve that
	// order — operators want to see the biggest change first.
	type grower struct {
		kind        string
		label       string
		bytes       int64
		deltaBytes  int64
	}
	var growers []grower
	for _, k := range []string{"container_log", "volume", "host_path", "bind_mount", "image"} {
		rows, err := r.Store.GrowthSince(k, since, 10)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if row.DeltaBytes <= 0 {
				continue
			}
			growers = append(growers, grower{
				kind: k, label: row.Label, bytes: row.Bytes, deltaBytes: row.DeltaBytes,
			})
		}
	}
	sort.Slice(growers, func(i, j int) bool { return growers[i].deltaBytes > growers[j].deltaBytes })
	if len(growers) > 12 {
		growers = growers[:12]
	}

	var prompt strings.Builder
	prompt.WriteString("Write a short ops digest (5-8 bullet points, plain text).\n")
	prompt.WriteString("Use the data below. Don't restate the disk overview.\n")
	prompt.WriteString("IMPORTANT: keep the bullets in the SAME ORDER as the list — biggest change first.\n\n")
	prompt.WriteString("## 24h top growers (already sorted by delta, biggest first)\n")
	wrote := false
	for _, g := range growers {
		fmt.Fprintf(&prompt, "- [%s] %s — now %s (+%s)\n", g.kind, g.label, human(g.bytes), human(g.deltaBytes))
		wrote = true
	}
	prompt.WriteString("\nKeep it under 600 characters. No code fences.")

	// If neither header nor narrative would have content, skip the digest.
	if !wrote && len(fss) == 0 {
		return "", nil
	}

	var answer string
	if wrote {
		got, err := r.LLM.Chat(ctx,
			"You are writing a concise ops digest. Be factual, no hype.",
			prompt.String())
		if err != nil {
			return "", err
		}
		answer = got
	}

	msg := "📊 *Disk digest*\n" + header.String() + answer
	_ = r.Notifier.Send(ctx, "ai-digest:"+time.Now().Format("2006-01-02T15"), msg)
	return answer, nil
}

// signedHuman mirrors cmd/ssm.signedHumanBytes for the digest. Kept local so
// the aireview package has no upward dependency on the main binary.
func signedHuman(b int64) string {
	switch {
	case b > 0:
		return "↑ +" + human(b)
	case b < 0:
		return "↓ -" + human(-b)
	default:
		return "±0 B"
	}
}

// parseFindings is lenient: it accepts either a bare JSON array or an object
// wrapper {"findings":[...]}, because models occasionally add the wrapper.
func parseFindings(raw string) ([]Finding, error) {
	raw = strings.TrimSpace(raw)
	// Strip accidental code fences.
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	if strings.HasPrefix(raw, "[") {
		var arr []Finding
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var wrap struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil, err
	}
	return wrap.Findings, nil
}

func human(b int64) string {
	const u = 1024
	if b > -u && b < u {
		return fmt.Sprintf("%d B", b)
	}
	neg := b < 0
	if neg {
		b = -b
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%.1f %cB", sign, float64(b)/float64(div), "KMGTPE"[exp])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
