package alert

import (
	"context"
	"encoding/json"
	"fmt"

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

// Evaluate inspects a batch of samples and fires alerts. Caller passes a Notifier
// (e.g. Telegram). Returns the list of alert messages that were fired (for the UI).
func Evaluate(ctx context.Context, n Notifier, r Rules, samples []store.Sample) []string {
	var fired []string
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
			if s.Bytes >= 100*1024*1024 {
				msg := fmt.Sprintf("🧹 orphan volume `%s` (%s) — no container references it",
					s.Label, humanBytes(s.Bytes))
				_ = n.Send(ctx, "orphan:"+s.Key, msg)
				fired = append(fired, msg)
			}
		}
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
