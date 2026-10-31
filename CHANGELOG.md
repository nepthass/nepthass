# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Fifth milestone: richer rules and safer start-up (implemented on `feature/v0.5.0`, not yet released; manual Discord QA is pending).

### Added

- **Duration rules.** `/notifications add ... for:<minutes>` (1 to 1440) alerts only after the condition has held that long, once per episode, for door, window, garage door, opening, motion, battery-low and numeric sensors. The count uses NeptHass's own clock and **never fires early**: it is cancelled when the condition clears or the sensor becomes unavailable, re-reads the sensor when it comes due, does not fire while monitoring is down, and restarts after an outage or a restart (messages say "at least" when NeptHass did not see the condition begin). A sensor can have several rules of one kind with different durations. Water leak alerts stay immediate.
- **Hysteresis.** `clear:<value>` on a numeric rule: it re-arms only after the reading passes the clear value, so a temperature hovering around 30 °C does not alert repeatedly.
- **Unit-safe thresholds.** A numeric threshold is bound to the unit its sensor reports (temperature: °C, °F or K; humidity and battery: %). A reading in another unit is never compared with it; a rule whose sensor changed unit stops evaluating until the unit returns.
- **`/notifications unit`** binds a rule created before v0.5.0 to its sensor's current unit. Humidity and battery rules are bound automatically once (audited as the system); temperature rules need an administrator, because the unit cannot be known.
- **Initial-state alerting.** A water leak that is already wet when NeptHass starts (or when its rule is created) is announced once, worded as what Home Assistant reports and not as a new event; other conditions already present are listed in **one start-up summary** after two minutes. At most three individual hazard alerts per start and one summary every ten minutes, across restarts. Delivered hazard alerts leave a small mark so a restart does not repeat them.
- **Audit of refused administrator commands**: allowed users who are not administrators, strangers, other servers and direct messages, with fixed reasons, no option values, rate limits and a retention cap.
- `garage_door` and `opening` binary sensors (on means open) are monitored and can have rules.
- A typed sensor capability registry replaces the scattered per-category switches.
- Virtual garage door and opening sensors and scenarios in the development Home Assistant; a manual QA group J in the smoke tests.

### Changed

- **A first observation is no longer always silent**: see initial-state alerting above. Behavior after a reconnect inside one process is unchanged.
- `/notifications add`, `list`, `status` and the autocomplete show durations, clear values, units, dormant rules and start-up information.
- Rules are unique per sensor, kind and duration.

### Database

- Migration `0004` rebuilds `notification_rules` in one transaction (every rule, ID and the ID sequence preserved; existing rules become immediate rules with no clear value) and adds `notification_marks` and `notification_settings.startup_summary_at`. **Back up the database first**: a v0.4.0 binary refuses a database that has migration 0004 (restore the backup to go back).

### Known limitations

- A start-up summary or hazard alert can be repeated **once** after a crash that happens after Discord accepted a message and before NeptHass wrote its mark.
- Duration counts are in memory: after a restart or a monitoring outage they restart, so an alert can be later than the real duration (never earlier).
- Smoke, gas and carbon monoxide are not supported; they need a dedicated safety policy.
- No persisted outbox, a 10 minute staleness bound, one alert channel and possible duplicates remain as in v0.4.0.
- Manual QA on a real Discord server has not been run for v0.5.0; see the checklist (group J) in [docs/development/smoke-tests.md](docs/development/smoke-tests.md).

## [0.4.0] - 2026-10-30

Fourth milestone: smart notifications.

### Added

- Ordered change feed from the monitor tracker (`Change` with a strictly increasing sequence, source live/replay/snapshot, previous and current state) delivered through `Config.OnChange` after the tracker lock is released.
- Notification configuration in SQLite (migration `0003`: `notification_settings`, `notification_rules`), an administrator-only service with an atomic audit entry per change, at most 200 rules, thresholds within +/-1,000,000, cooldowns up to 24 hours.
- Notification engine (`internal/notify`): a per-entity mailbox that never blocks the monitor callback, a pure rule evaluator (door/window open, motion detected, leak wet, battery low, temperature/humidity/battery above or below), per-rule cooldowns with category defaults, two bounded delivery lanes (critical 32, normal 64) with a 10 minute absolute TTL, a dispatcher with exponential backoff, a token bucket and a circuit breaker, a current-state digest after overflow, and a monitoring-down state machine (one alert after five unhealthy minutes, recovery notice only after the alert was delivered).
- Discord delivery over REST with explicit empty allowed-mentions, one retry layer, and classified errors (429 with `retry_after`, 5xx and network errors retried, 401/403/404/400 permanent).
- `/notifications` (administrators, guild-only, ephemeral): `channel`, `add`, `list`, `pause`, `resume`, `remove`, `test`, `status`, with autocomplete limited to shared, monitored sensors. Channel validation: the channel exists, belongs to the configured guild, is a text channel, and the bot has View Channel, Send Messages and Embed Links.
- A notification line in `/health` and detailed delivery health in `/notifications status`.
- Interaction parsing for Channel and Number options; any other option type makes the request fail visibly instead of running without it.
- Documentation: [docs/design/notifications.md](docs/design/notifications.md), command reference, security notes, troubleshooting and a real-Discord manual QA checklist.

### Changed

- The recovery notice is queued ahead of the late alerts found by the reconnect snapshot.
- A rejected channel, the outcome of `/notifications test` and any denial that reaches the notification service are audited with fixed sentinel wording only. A non-administrator is refused earlier by the command gate, which does not audit refusals (as for every administrator command).
- A configuration change that is saved but could not be loaded into the running engine is reported as "saved but not active", shows degraded health, and is retried automatically; health never reports "disabled" in that state.

### Fixed

- The notification dispatcher no longer sleeps through a wake-up time that passes while it registers its idle timer (found by stress testing), and the circuit breaker's single probe slot is no longer consumed by a message that is revoked or whose permission could not be read.

### Security

- Sharing is checked before an alert is produced and again before every delivery attempt; a revocation that lands after the last check but before Discord accepts the request cannot be recalled.
- Messages never contain entity IDs, raw attributes or credentials, names are escaped, and no mention of any kind is sent.
- Everyone who can read the alert channel sees the alerts. Use a private channel.

### Upgrade notes

- Migration `0003` only adds tables. **Back up the database before upgrading.** A v0.3.0 binary refuses a database that has migration `0003` ("database schema is newer than this build supports"); to go back, restore the backup.
- Notifications are disabled until an administrator sets a channel.

### Known limitations

- There is **no persisted outbox**: queued alerts are lost on a crash or restart. Delivery is best-effort with possible duplicates and a 10 minute TTL.
- A first observation is silent, so a leak that is already wet when NeptHass starts or when its rule is created is not announced.
- One alert channel per server; no direct messages, quiet hours or rule logic (v0.5.0).
- Real Discord was exercised manually by the maintainer on a test server (partial; see the [known gaps](docs/development/smoke-tests.md#known-gaps)). Not exercised against real Discord: an actual HTTP 429, a Discord-only outage and a second user; those are covered by stub-server tests. The v0.3.0 to v0.4.0 downgrade test is deferred while the project is in private pre-v1.0.0 development.
- A non-administrator's refused `/notifications` command is not audited (the command gate does not audit refusals for any administrator command).

## [0.3.0] - 2026-10-29

Third milestone: real-time sensor monitoring. Notifications are not part of it.

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

### Fixed

- `/sensors` no longer answers "No monitored sensors are shared with you" before the first synchronization (right after a restart, or while Home Assistant is unreachable at startup). It now says that readings are not available yet, with the reason, and nothing about what is shared. The real empty state is kept for a synchronized monitor.
- `/health` no longer calls a reconnection "Starting": after an earlier failure the stages read "Reconnecting", with the previous failure category. The next retry time is an absolute clock time instead of a relative Discord timestamp that turned into "seconds ago".
- The `/sensors` footer says "Live readings" only while monitoring is healthy and synchronized, and "Last known values, not live" otherwise.
- `subscribe_events` and `get_states` are bounded by timeouts (15 s and 60 s). A peer that answered pings but never answered one of them used to leave the monitor waiting forever; the attempt now ends and the normal backoff takes over.
- A tracked sensor that reports `unavailable` or `unknown` with no `device_class` attribute keeps its previous kind instead of disappearing from `/sensors`.

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
- Whether a given third-party integration ever sends a state update without `device_class` cannot be settled without that integration; the tracker keeps the previous kind only for unavailable or unknown states, and stops tracking a real value that has no class.

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

[Unreleased]: https://github.com/nepthass/nepthass/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/nepthass/nepthass/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/nepthass/nepthass/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/nepthass/nepthass/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/nepthass/nepthass/tree/v0.1.0
