# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Third milestone (in development on `feature/v0.3.0`, not released): real-time sensor monitoring. Notifications are not part of it.

### Added

- Home Assistant WebSocket client (`internal/homeassistant/ws`): URL derivation (`http` to `ws`, `https` to `wss`, path prefix kept), authentication handshake, strictly increasing command ids with result correlation, `state_changed` subscription, `get_states` snapshot, application-level ping with pong timeout, read and write deadlines, size limits (64 KiB before authentication, 16 MiB after), and typed errors. The token is sent once and never logged.
- Monitoring supervisor (`internal/monitor`): an explicit connection state machine (disconnected, connecting, authenticating, subscribing, synchronizing, ready, reconnecting, stopped) with a table of legal transitions, owned by the application and stopped with it.
- Reconnection with exponential backoff (1 s up to 60 s) and equal jitter; authentication failures use a slow schedule (30 s up to 10 min); the schedule resets only after a connection stayed ready for 30 s, so a flapping server cannot cause a reconnect storm.
- Reconciliation after every connect: subscribe, buffer events (bounded at 4096), fetch the authoritative snapshot, apply it, replay the buffered events through the per-sensor ordering rule, then go ready. Entities that vanished while disconnected are removed.
- Sensor classification by domain and `device_class` (temperature, humidity, battery level, battery low, motion, door, window, water leak) with the raw state kept apart from its interpreted meaning; `unavailable`, `unknown` and invalid values are never presented as a safe state.
- In-memory state tracker with per-sensor ordering by `last_updated`, duplicate and stale-event protection, attribute-only update handling, entity removal and reappearance, validation of untrusted payloads and a hard bound of 10,000 tracked sensors.
- `/sensors [page]`: live readings of the monitored sensors shared with you, with aliases, interpreted meaning, raw state for binary sensors, units, time of the last change and a clear stale banner when monitoring is not healthy.
- `/health` and the setup wizard's health step now report the monitoring state: the five layers separately, last synchronization and event times, reconnect and invalid-message counters, and the failure category (never a URL, token or raw error).
- Test infrastructure: a scriptable fake Home Assistant WebSocket endpoint (`tests/fakeha`), a failure-injection suite, real Home Assistant integration tests including a deterministic network outage through a TCP proxy, and authorization negatives.

### Changed

- `gorilla/websocket` upgraded from 1.4.2 to 1.5.3 (already in the dependency graph through discordgo; no new module).
- `/health` no longer says the WebSocket is "not available in this release".
- The repository moved from a personal account to the `nepthass` GitHub organization (`github.com/nepthass/nepthass`). Clone URLs, the changelog compare links and the image source label now point there. The Go module path was already `github.com/nepthass/nepthass` and is unchanged. Published release notes and history keep the earlier URLs, which GitHub redirects.

### Security

- Monitored data reaches users only through a `Reader` that checks the caller's level and the entity's *current* permission on every read (deny by default; an unshared entity is indistinguishable from an unknown one; revocation is immediate even for cached data).
- Home Assistant payloads are validated as untrusted input (entity ID, lengths, timestamps; only the entity, state, timestamps and three attributes are read).
- Monitoring adds no write path: control is unchanged (lights and switches only).

### Upgrade notes

- No database migration and no new environment variable. A v0.2.0 database works unchanged and v0.2.0 can read it back.
- The monitor reuses `HA_URL` and `HA_TOKEN`. A reverse proxy in front of Home Assistant must allow WebSocket upgrades on `/api/websocket`.

### Known limitations

- Monitoring observes only: no notifications (v0.4.0), no rules (v0.5.0), no dashboard (v0.6.0).
- Only the eight sensor categories above are tracked; other sensors stay available through `/devices` and `/status`.
- A change that happens and reverses while NeptHass is disconnected cannot be seen; Home Assistant does not replay events and NeptHass does not invent them.
- Live state is in memory: after a restart `/sensors` is empty or stale until the first synchronization completes (seconds).
- A Home Assistant instance whose `get_states` answer exceeds 16 MiB cannot synchronize.
- Manual Discord acceptance of v0.3.0 has not been performed; see `docs/development/smoke-tests.md` group H.

## [0.2.0] - 2026-10-27

Second milestone: interactive setup.

### Added

- `/setup`: an administrator-only interactive wizard (connection health, discover devices, permissions, aliases, review, done) built on an explicit state machine, with search, domain and "shared only" filters, pagination, select menus, modals, Back / Next / Cancel / Resume and a confirmation before anything is written.
- Persistent setup sessions in SQLite: survive restarts, resumable, 24 hour idle expiry, one active session per administrator and guild, optimistic concurrency, replay-safe confirmation, and atomic apply with audit entries (`setup.allow`, `setup.revoke`, `setup.alias`, `setup.apply`).
- Aliases: human-readable, unique, character-restricted names for entities, shown in `/devices` and `/status`, accepted by `/status` and `/control` (and suggested by autocomplete), never granting access and never revealing aliases of unshared entities.
- `/sync`: refreshes the discovery snapshot and reports new, removed, returned and shared-but-missing or unavailable entities. It persists only entity IDs and first/last-seen times and never shares anything.
- A shared discovery cache so paging and searching do not hit Home Assistant on every click.
- Migration 0002 (`entity_aliases`, `setup_sessions`, `known_entities`); v0.1.0 databases upgrade in place with permissions and audit records preserved.
- Component and modal handling in the bot, with a separate per-user click rate limit (30 per 10 seconds).
- Integration tests for discovery, the full setup flow, stale confirmations, restart persistence and `/sync` against the real development Home Assistant; a command reference and a design document; a manual Discord checklist for the wizard.

### Changed

- `/permissions allow` and the wizard share one grant rule (`entities.CheckGrant`) and one permission table.
- Shared Discord presentation helpers moved to `internal/discord/ui` (behavior unchanged).
- Upgrade note: back up your data before upgrading; a v0.1.0 build refuses to start on a v0.2.0 database.

### Fixed

- Development environment: the integration tests failed (31.2 C reported as 88.16 F, 21.5 C as 70.7 F) when the onboarding UI switched the Home Assistant instance to US customary units. The reset script now re-applies the pinned metric system, the suite refuses to run on a non-metric instance, and a regression test covers it.

### Known limitations

- No sensor monitoring, Home Assistant WebSocket, notifications, rules or `/home` dashboard yet (planned for v0.3.0 to v0.6.0).
- A single Discord guild is supported.
- A setup session holds at most 100 selected entities, and entities whose ID is longer than 100 characters cannot be offered in the wizard's select menu.
- Alias uniqueness is case-insensitive but not aware of visually confusable characters from different scripts.

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
- Development environment (`compose.dev.yaml`, `.env.dev.example`, `dev/homeassistant`): a real Home Assistant Container with virtual light, switch, motion, door, window, temperature, humidity, water-leak and battery entities, an optional NeptHass service, and one-click simulation scenarios (including unavailable sensors), in an isolated Compose project with localhost-only access.
- Integration tests against that real Home Assistant (`go test -tags=integration ./tests/integration/...`), runnable without a local Go toolchain through the `test` Compose profile.
- Development guides: setup, virtual devices and simulation, smoke-test checklist (automated and manual Discord checks kept separate), troubleshooting.

### Known limitations

- No `/setup` wizard, `/sync`, sensor monitoring, notifications, rules or `/home` dashboard yet (planned for v0.2.0 to v0.6.0).
- A single Discord guild is supported.
- `/devices` lists 10 entities per page via a `page` option; interactive pagination arrives with the dashboard.

[Unreleased]: https://github.com/nepthass/nepthass/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/nepthass/nepthass/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/nepthass/nepthass/releases/tag/v0.1.0
