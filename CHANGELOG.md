# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Development environment (`compose.dev.yaml`, `.env.dev.example`, `dev/homeassistant`): a real Home Assistant Container with virtual light, switch, motion, door, window, temperature, humidity, water-leak and battery entities, an optional NeptHass service, and one-click simulation scenarios (including unavailable sensors), in an isolated Compose project with localhost-only access.
- Integration tests against that real Home Assistant (`go test -tags=integration ./tests/integration/...`), runnable without a local Go toolchain through the `test` Compose profile.
- Development guides: setup, virtual devices and simulation, smoke-test checklist (automated and manual Discord checks kept separate), troubleshooting.

## [0.1.0] - 2026-10-27

First milestone: the foundation.

### Added

- Project scaffolding: Go module, repository layout, MIT license, contribution and security policies.
- Environment-based configuration with aggregated validation, `*_FILE` secret support and a `Secret` type that never prints its value.
- Structured `slog` logging that scrubs secrets from messages, attributes and errors.
- SQLite storage (pure Go, no CGO) with embedded, checksum-verified, transactional migrations and WAL mode.
- Home Assistant REST client: token authentication, timeouts, typed errors, no redirects, and an allowlist limited to `light` and `switch` `turn_on`/`turn_off`.
- Guild, administrator and user authorization checked on every interaction, plus per-user rate limiting.
- Entity permissions (`read` / `control`), deny by default, managed with `/permissions`.
- Audit log of control attempts and permission changes with configurable retention (`AUDIT_RETENTION_DAYS`).
- Discord slash commands: `/ping`, `/health`, `/devices`, `/status`, `/control`, `/permissions`, `/audit`, with autocomplete and ephemeral replies.
- Resilient start-up: Discord connection retried with exponential backoff and jitter; Home Assistant outages tolerated; rejected tokens fail fast.
- Docker image (static binary, distroless, non-root) and Docker Compose deployment, plus an optional combined Home Assistant example.
- GitHub Actions CI (format, vet, build, race-enabled tests, Compose validation, Docker build) and Dependabot.
- Documentation: README, architecture, setup, security and troubleshooting guides.

### Known limitations

- No `/setup` wizard, `/sync`, sensor monitoring, notifications, rules or `/home` dashboard yet (planned for v0.2.0 to v0.6.0).
- A single Discord guild is supported.
- `/devices` lists 10 entities per page via a `page` option; interactive pagination arrives with the dashboard.

[Unreleased]: https://github.com/nepthass/nepthass/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/nepthass/nepthass/releases/tag/v0.1.0
