# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Initial scaffold: host filesystem scanner, Docker scanner (containers, logs,
  bind mounts, volumes, images), SQLite history with retention.
- Embedded HTML dashboard with auto-refresh and "Ask the AI" panel.
- vLLM / OpenAI-compatible LLM client for natural-language queries against the
  current snapshot.
- Telegram alerter with per-rule deduplication.
- Single-password cookie auth on the dashboard (LAN-only by design).
- Portainer-friendly `docker-compose.yml` with read-only Docker socket proxy.

[Unreleased]: https://github.com/ricgrangeia/server-space-manager-ai/compare/v0.0.0...HEAD
