package config

import (
	"fmt"
	"os"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// Config is the top-level YAML schema loaded from disk at startup.
//
// All durations and cron expressions are parsed eagerly in Load so the rest
// of the program can rely on them being well-formed.
type Config struct {
	// Schedules holds cron expressions for the periodic jobs. Five-field
	// crontab syntax plus `@every Xs` shortcuts (robfig/cron/v3).
	Schedules SchedulesCfg `yaml:"schedules"`

	RetentionDays int `yaml:"retention_days"`

	Docker      DockerCfg   `yaml:"docker"`
	HostPaths   []PathCfg   `yaml:"host_paths"`
	Filesystems []string    `yaml:"filesystems"`
	Ignore      []string    `yaml:"ignore"`
	LLM          LLMCfg      `yaml:"llm"`
	HTTP         HTTPCfg     `yaml:"http"`
	Telegram     TelegramCfg `yaml:"telegram"`
	Alerts       AlertsCfg   `yaml:"alerts"`
	AllowActions bool        `yaml:"allow_actions"`
}

// SchedulesCfg holds the cron expressions for each periodic job. Any value
// may be left empty to disable that job. Five-field crontab plus robfig's
// shortcuts (`@every 1h`, `@hourly`, `@daily`) are supported.
type SchedulesCfg struct {
	// Scan runs the disk-usage walk + Docker inspection.
	Scan string `yaml:"scan"`
	// AIReview asks the LLM to judge recent growth and decide what to alert on.
	// Empty disables. Recommended: a few hours apart (e.g. "0 */6 * * *").
	AIReview string `yaml:"ai_review"`
	// Digest produces a single LLM-written summary (good for Telegram).
	// Empty disables. Recommended: daily wall-clock time (e.g. "0 8 * * *").
	Digest string `yaml:"digest"`
}

// Validate parses each non-empty entry with the same parser robfig/cron uses
// at runtime, so a typo surfaces at startup rather than silently never firing.
func (s SchedulesCfg) Validate() error {
	p := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	for name, expr := range map[string]string{
		"scan": s.Scan, "ai_review": s.AIReview, "digest": s.Digest,
	} {
		if expr == "" {
			continue
		}
		if _, err := p.Parse(expr); err != nil {
			return fmt.Errorf("schedules.%s: %w", name, err)
		}
	}
	return nil
}

type TelegramCfg struct {
	Enabled  bool   `yaml:"enabled"`
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
}

type AlertsCfg struct {
	FSWarnPct float64 `yaml:"fs_warn_pct"`
	FSCritPct float64 `yaml:"fs_crit_pct"`
	BigItemMB int64   `yaml:"big_item_mb"`
	GrowthPct float64 `yaml:"growth_pct"`
}

type DockerCfg struct {
	Enabled         bool   `yaml:"enabled"`
	Host            string `yaml:"host"`
	TrackVolumes    bool   `yaml:"track_volumes"`
	TrackImages     bool   `yaml:"track_images"`
	TrackLogs       bool   `yaml:"track_logs"`
	TrackBindMounts bool   `yaml:"track_bind_mounts"`
}

type PathCfg struct {
	Path           string `yaml:"path"`
	MaxDepth       int    `yaml:"max_depth"`
	AlertGrowthPct int    `yaml:"alert_growth_pct"`
}

type LLMCfg struct {
	Enabled        bool   `yaml:"enabled"`
	BaseURL        string `yaml:"base_url"`
	Model          string `yaml:"model"`
	APIKey         string `yaml:"api_key"`
	MaxTokens      int    `yaml:"max_tokens"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// HTTPCfg controls the dashboard/API listener.
//
// The dashboard is intended for use on a trusted LAN. BasicAuth provides a
// minimal access barrier; do NOT expose this listener directly to the public
// internet — put it behind a reverse proxy with TLS and stronger auth if you
// need remote access.
type HTTPCfg struct {
	Listen string `yaml:"listen"`
	// Password is a single shared secret. When non-empty, the dashboard
	// renders a login page that accepts only this value (no usernames).
	// Authentication state is held in an HttpOnly cookie.
	//
	// This is a LAN-only scheme intended for a small trusted group. Do NOT
	// expose the listener to the public internet — front it with a reverse
	// proxy that provides TLS and stronger auth if remote access is needed.
	Password string `yaml:"password"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// Apply defaults before validation so a minimal config "just works".
	if c.Schedules.Scan == "" {
		c.Schedules.Scan = "@every 1h"
	}
	if err := c.Schedules.Validate(); err != nil {
		return nil, err
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 90
	}
	if c.HTTP.Listen == "" {
		c.HTTP.Listen = ":8080"
	}

	applyEnvOverrides(&c)
	return &c, nil
}

// applyEnvOverrides lets operators inject secrets via environment variables
// (typically a Portainer stack env or a local .env file) instead of baking
// them into config.yaml. Any non-empty value here wins over the YAML value.
//
// This keeps the committed config.yaml free of credentials and makes
// password rotation a stack-restart instead of a file edit.
func applyEnvOverrides(c *Config) {
	if v := os.Getenv("SSM_HTTP_PASSWORD"); v != "" {
		c.HTTP.Password = v
	}
	if v := os.Getenv("SSM_LLM_BASE_URL"); v != "" {
		c.LLM.BaseURL = v
	}
	if v := os.Getenv("SSM_LLM_MODEL"); v != "" {
		c.LLM.Model = v
	}
	if v := os.Getenv("SSM_LLM_API_KEY"); v != "" {
		c.LLM.APIKey = v
	}
	if v := os.Getenv("SSM_TELEGRAM_ENABLED"); v == "true" || v == "1" {
		c.Telegram.Enabled = true
	}
	if v := os.Getenv("SSM_TELEGRAM_BOT_TOKEN"); v != "" {
		c.Telegram.BotToken = v
	}
	if v := os.Getenv("SSM_TELEGRAM_CHAT_ID"); v != "" {
		c.Telegram.ChatID = v
	}
}
