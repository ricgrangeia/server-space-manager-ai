# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/ricgrangeia/server-space-manager-ai/compare/v0.2.0...HEAD
[0.2.0]:      https://github.com/ricgrangeia/server-space-manager-ai/compare/v0.1.0...v0.2.0
[0.1.0]:      https://github.com/ricgrangeia/server-space-manager-ai/releases/tag/v0.1.0
