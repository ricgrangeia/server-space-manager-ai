package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ricgrangeia/server-space-manager-ai/internal/store"
)

// Rules are simple thresholds evaluated after each scan. Tune via config later.
type Rules struct {
	FSWarnPct  float64 // e.g. 80 -> warn at 80% full
	FSCritPct  float64 // e.g. 90 -> critical at 90% full
	BigItemMB  int64   // log/volume size that triggers a warn (MB)
	GrowthPct  float64 // bytes growth pct vs prior sample that triggers a warn
}

func DefaultRules() Rules {
	return Rules{FSWarnPct: 80, FSCritPct: 90, BigItemMB: 5_000, GrowthPct: 25}
}

type Notifier interface {
	Send(ctx context.Context, key, text string) error
}

// Evaluate inspects a batch of samples and fires alerts. Caller passes a
// Notifier (e.g. Telegram). Returns the list of alert messages that were
// fired (for the UI).
//
// Per-item findings (filesystems near full, single large logs/volumes/bind
// mounts) are emitted as individual alerts. Orphan volumes are deliberately
// rolled up into one summary message per scan — hosts often accumulate many
// anonymous volumes, and one ping per orphan would drown the Telegram chat.
func Evaluate(ctx context.Context, n Notifier, r Rules, samples []store.Sample) []string {
	var fired []string

	// Orphan rollup state: count, total bytes, and the few biggest entries
	// so the summary message can name something concrete.
	var orphanCount int
	var orphanBytes int64
	type orphanItem struct {
		label string
		bytes int64
	}
	var biggest []orphanItem

	for _, s := range samples {
		switch s.Kind {
		case "fs":
			pct := fsPercent(s)
			if pct >= r.FSCritPct {
				msg := fmt.Sprintf("🚨 *CRITICAL* `%s` is %.1f%% full", s.Label, pct)
				_ = n.Send(ctx, "fs-crit:"+s.Key, msg)
				fired = append(fired, msg)
			} else if pct >= r.FSWarnPct {
				msg := fmt.Sprintf("⚠️ `%s` is %.1f%% full", s.Label, pct)
				_ = n.Send(ctx, "fs-warn:"+s.Key, msg)
				fired = append(fired, msg)
			}
		case "container_log", "volume", "bind_mount":
			if s.Bytes >= r.BigItemMB*1024*1024 {
				msg := fmt.Sprintf("⚠️ %s `%s` reached %s",
					s.Kind, s.Label, humanBytes(s.Bytes))
				_ = n.Send(ctx, s.Kind+":big:"+s.Key, msg)
				fired = append(fired, msg)
			}
		case "orphan_volume":
			// Always accumulate so the rollup sees every orphan, regardless
			// of individual size. The minimum-size threshold from previous
			// behaviour is dropped — many small orphans still matter in
			// aggregate.
			orphanCount++
			orphanBytes += s.Bytes
			biggest = append(biggest, orphanItem{s.Label, s.Bytes})
		}
	}

	if orphanCount > 0 {
		// We deliberately do NOT push the orphan rollup as a standalone
		// Telegram message — the end-of-scan report (cmd/ssm.buildScanReport)
		// includes its own Orphan section so the operator gets one rich
		// message instead of two thin ones. The dashboard "Findings" card
		// still shows this string as a rule-engine alert via `fired`.
		sort.Slice(biggest, func(i, j int) bool { return biggest[i].bytes > biggest[j].bytes })
		var preview string
		for i := 0; i < len(biggest) && i < 3; i++ {
			preview += fmt.Sprintf("\n• `%s` (%s)", biggest[i].label, humanBytes(biggest[i].bytes))
		}
		msg := fmt.Sprintf("🧹 %d orphan volumes — %s total%s",
			orphanCount, humanBytes(orphanBytes), preview)
		fired = append(fired, msg)
	}

	return fired
}

func fsPercent(s store.Sample) float64 {
	var extra struct {
		Total int64 `json:"total"`
		Avail int64 `json:"avail"`
	}
	if err := json.Unmarshal([]byte(s.Extra), &extra); err != nil || extra.Total == 0 {
		return 0
	}
	used := extra.Total - extra.Avail
	return 100 * float64(used) / float64(extra.Total)
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
