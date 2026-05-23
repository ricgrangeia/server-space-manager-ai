# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0]

### Added
- **Rich end-of-scan Telegram report.** One consolidated message per scan
  (cron + manual) with: disk overview (every filesystem with a 10-segment
  usage bar + GB used/total), top 5 container logs / volumes / bind mounts
  / biggest files, baseline anomalies (last-24h growth vs 7-day per-item
  average), orphan-volume rollup, and active alerts. Replaces the previous
  thin two-section summary.
- **Biggest files** card on the dashboard. The host walker keeps a min-heap
  of the top N largest individual files across all configured paths, useful
  for spotting leaked core dumps and unrotated logs.
- **Findings** card on the dashboard — a unified view of AI-review verdicts
  and rule-engine alerts from the last 7 days, with a severity dot, source
  badge, and pill filter (All / AI review / Rule alerts). Backed by a new
  `GET /api/findings` endpoint.
- End-of-scan Telegram report now fires for **cron** scans too, not only
  manual ones. Gated by `telegram.scan_reports` (default `true`) so it can
  be muted if hourly pings are too noisy.
- Diagnostic startup logging: SQLite file size + restore outcome, so a
  future "is my data persisting?" question is a single log line away.

### Fixed
- **Top Volumes was empty.** Replaced `/system/df`-based volume listing
  (which returns `UsageData: null` on newer Docker engines) with `/volumes`
  + direct filesystem walks under `/host/var/lib/docker/volumes/<name>/_data`.
- **Bogus multi-TB `bind_mount` totals.** ssm's own `/:/host` mount caused
  the walker to recurse through `/host` and count overlay layers many times.
  Bind-mount sources of `/`, `/proc`, `/sys`, `/dev`, and the Docker socket
  are now skipped; other bind mounts are read via `/host<source>`.
- **"Lost data" on every redeploy.** `LatestScan()` was Scanning a SQLite
  `MAX(taken_at)` into a Go `time.Time`, then re-querying with that value.
  The modernc driver round-trips timestamps through RFC3339 text in a way
  that didn't exactly match the stored string, so the `WHERE` matched zero
  rows. Rewritten as a single correlated subquery; data persists correctly.
- Long volume/path labels no longer overflow card tables — auto layout +
  `overflow-wrap: anywhere` for names, `white-space: nowrap` for sizes.

### Changed
- Orphan-volume alerts are rolled up into a single Telegram summary per
  scan (e.g. "🧹 23 orphan volumes — 4.2 GB total") with the three biggest
  named inline, instead of one ping per volume. The previous 100 MB
  per-volume threshold is removed; small orphans still matter in aggregate.

## [0.2.0]

### Added
- Manual-trigger buttons in the dashboard for scan / AI review / digest,
  with start + finish Telegram pings (mini report on scan finish).
- Cron-driven scheduler (`robfig/cron/v3`) replacing the single `time.Ticker`;
  `scan`, `ai_review`, and `digest` jobs each have their own cron expression
  in `config.yaml`.
- AI review feeds the LLM per-item baseline anomalies (last-24h growth vs the
  item's own 7-day daily average) in addition to top growers.
- `/api/ask` prompt now includes 7-day trends and recent decisions (rule
  alerts + prior AI verdicts) so answers build on history.
- Startup restore of the most recent scan from SQLite so the dashboard is
  never empty after a redeploy.
- `docs/architecture.svg` infographic, embedded in the README.

### Changed
- Dropped the Docker SDK in favour of a small HTTP client (4 endpoints).
  Image size down to ~21 MB, dependency tree massively reduced.
- Dropped the socket-proxy sidecar; ssm mounts `/var/run/docker.sock:ro`
  directly. Stack is one container.
- Default Docker network renamed `vllm-net` → `ai-network`.
- Config defaults baked into the image so the container starts without a
  mounted `config.yaml`. Secrets overlaid from env vars.
- Stack volume pinned to absolute name `ssm-data` so scan history survives
  stack rename / recreate.

### Security
- Login cookie now holds an opaque 32-byte random session token instead of
  the password itself. Sessions tracked server-side with 30-day expiry; map
  capped at 1024 entries with opportunistic sweep.
- Telegram dedupe map capped at 2048 entries with TTL-based sweep on
  overflow (was unbounded).
- Baseline security headers on every response: `X-Content-Type-Options`,
  `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, a strict CSP that
  blocks cross-origin scripts/styles and forbids iframe embedding.
- `/api/ask` is rate limited (5-request burst, refill 1 every 10 s) to keep
  a single tab from saturating the local LLM.

## [0.1.0]

### Added
- Initial scaffold: host filesystem scanner, Docker scanner (containers, logs,
  bind mounts, volumes, images), SQLite history with retention.
- Embedded HTML dashboard with auto-refresh and "Ask the AI" panel.
- vLLM / OpenAI-compatible LLM client for natural-language queries against the
  current snapshot.
- Telegram alerter with per-rule deduplication.
- Single-password cookie auth on the dashboard (LAN-only by design).
- Portainer-friendly `docker-compose.yml` with read-only Docker socket proxy.

[Unreleased]: https://github.com/ricgrangeia/server-space-manager-ai/compare/v0.3.0...HEAD
[0.3.0]:      https://github.com/ricgrangeia/server-space-manager-ai/compare/v0.2.0...v0.3.0
[0.2.0]:      https://github.com/ricgrangeia/server-space-manager-ai/compare/v0.1.0...v0.2.0
[0.1.0]:      https://github.com/ricgrangeia/server-space-manager-ai/releases/tag/v0.1.0
