# Architecture

NeptHass is a modular monolith: one Go binary, one SQLite file, no public API.

```text
cmd/nepthass            wiring only (config, logger, dependencies, signals)
internal/config         environment parsing and validation, Secret type
internal/logging        slog logger with secret scrubbing
internal/storage        SQLite open + migration runner      <- migrations/ (embedded SQL)
internal/auth           guild/user allowlists, rate limiter
internal/audit          audit log table + retention (usable inside a transaction)
internal/entities       entity model, permissions, aliases, discovery cache, sync registry, the shared grant rule, Service
internal/setup          setup sessions: state machine, store, plan, atomic apply (Discord-free)
internal/homeassistant/rest   Home Assistant REST client (service allowlist)
internal/homeassistant/ws     Home Assistant WebSocket session: handshake, id correlation, reader, ping (one connection)
internal/monitor        live sensor monitoring: state machine, backoff, snapshot reconciliation, tracker, authorization Reader
internal/discord        gateway session, interaction routing, component and modal handling, retry/backoff
internal/discord/commands     slash command definitions and replies
internal/discord/wizard       the /setup wizard: panels, custom IDs, click handling
internal/discord/ui           shared embed helpers: branding, escaping, limits
tests/fakeha            in-memory Home Assistant (REST and a scriptable WebSocket endpoint) used by unit tests
tests/integration       tests against a real Home Assistant (build tag "integration")
```

Dependency direction: `discord -> {commands, wizard} -> {setup, entities} -> {auth, audit}`; `rest -> entities` (types only); `entities` and `setup` depend on small interfaces (`HomeAssistant`, `StateSource`, `Recorder`) that `rest` and `audit` satisfy. Discord-specific code lives only in `internal/discord/...`; the rules about who may do what live in `entities` and `setup` and know nothing about Discord. The wizard holds no business rules: every transition, validation and write happens in `internal/setup`.

## Interaction lifecycle

1. discordgo delivers an `InteractionCreate` event on the gateway.
2. `Bot.serve` (panic-safe) calls `handleInteraction`, which first runs `Authorizer.Authorize(guildID, userID)`. This happens for **every** interaction type, so no handler is reachable without being on an allowlist in the configured guild. Rejections get a generic ephemeral message and a WARN log.
3. Commands are rate limited per user (6 per 10 seconds), then acknowledged immediately with a deferred ephemeral response (Discord requires an answer within 3 seconds; Home Assistant may be slower).
4. `commands.Registry.Run` enforces the command's minimum level (user or admin), parses options, and calls `entities.Service`.
5. The reply is delivered by editing the deferred response. Mentions are disabled on every message.
6. Autocomplete requests follow the same authorization path and must answer within about 2.5 seconds; they read only the local allowlist and aliases, except administrator entity discovery, which uses the shared discovery snapshot (at most one Home Assistant fetch per 30 seconds) with a 2 second timeout.
7. **Components and modals** (the setup wizard) take the same path: guild and user authorization first, then a separate per-user click limit (30 per 10 seconds). A click is acknowledged with a deferred message update and the message is edited afterwards. Opening a modal is the only immediate response, so it does only database work. The wizard parses the custom ID (session, rendered version, whitelisted action), loads the session from SQLite, and the setup service again requires administrator level and ownership (session user and guild) before doing anything. A stale version refreshes the panel instead of acting.

## Authorization layers

| Layer | Where | Rule |
| --- | --- | --- |
| Guild | `auth.Authorizer` | Interaction must come from `DISCORD_GUILD_ID` (DMs rejected) |
| User | `auth.Authorizer` | Admin list, or optional user list; everyone else rejected |
| Command level | `commands.Registry`, `entities.Service` | `/permissions`, `/audit`, `/setup` and `/sync` need admin, checked in both places |
| Setup session | `setup.Service` | Every wizard operation requires administrator level and a session that belongs to the caller in this guild; anything else looks like a missing session |
| Entity | `entities.Permissions` | No row means no access, even for administrators |
| Domain | `entities.Domain.Controllable`, `rest.allowedServices` | Only `light` and `switch` can be controlled; checked at grant time, at execution time, and again inside the REST client |

Discord roles and channel permissions are never consulted.

## Data model (SQLite)

Migrations are embedded from `migrations/`, applied in order inside transactions, and recorded in `schema_migrations` with a SHA-256 checksum. Startup refuses a database written by a newer build and refuses a migration whose checksum changed. Never edit a released migration; add a new one.

| Table | Purpose |
| --- | --- |
| `entity_permissions` | entity ID, `read`/`control`, who granted it, when |
| `command_audit_logs` | control attempts and permission changes with outcome |
| `entity_aliases` | canonical entity ID to alias, unique case-insensitively (v0.2.0) |
| `setup_sessions` | persisted wizard sessions: JSON draft, step, status, version, expiry (v0.2.0) |
| `known_entities` | minimal /sync registry: entity ID, domain, first/last seen, missing since (v0.2.0) |
| `schema_migrations` | applied migrations and checksums |

Each package owns the SQL for its own table. Timestamps are fixed-width UTC text so they sort chronologically. The database runs in WAL mode with foreign keys on and a 5 second busy timeout; the file is created with owner-only permissions. Tables for later milestones (subscriptions, rules, outbox) are added by those milestones' migrations, not before. Migration 0002 only adds tables, so a v0.1.0 database upgrades in place with its permissions and audit log intact; the design notes are in [design/interactive-setup.md](design/interactive-setup.md).

## Failure handling

- **Discord unreachable at start:** token verification and gateway connection are retried with exponential backoff and jitter (1 s up to 1 min). A rejected token (HTTP 401) is permanent and stops the process with a clear error.
- **Discord drops after start:** discordgo reconnects and resumes the gateway by itself.
- **Home Assistant unreachable at start:** logged as a warning; NeptHass keeps running and uses it once it answers. A rejected token stops the process (a restart cannot fix it).
- **Home Assistant unreachable later:** commands answer with a friendly error; each request has a timeout and redirects are never followed.
- **Shutdown:** SIGTERM or SIGINT cancels the root context; the monitor closes its WebSocket and returns, the Discord session is closed and the database released. Shutdown waits for the monitor and the audit retention task.

## Logging and secrets

`log/slog`, text or JSON. Credentials are `config.Secret` values that print as `[REDACTED]` everywhere, and the logger additionally scrubs the literal secret values from messages, strings and errors. discordgo's own log lines are routed through the same logger (its only hook is a package-level variable, the one piece of global state in the program).

## Design decisions

- **Pure-Go SQLite (`modernc.org/sqlite`)** instead of CGO: a static binary, a distroless image, trivial cross-compilation.
- **Standard library first:** configuration parsing, HTTP, logging, migrations and rate limiting use only the standard library. Runtime dependencies are discordgo and the SQLite driver.
- **REST for commands, WebSocket for monitoring:** commands read and control through REST (authoritative, one request per use). Live sensor state comes from one WebSocket connection (v0.3.0). The REST path never depends on the WebSocket, so commands keep working while monitoring is down.

## Interactive setup (v0.2.0)

The wizard is an explicit state machine (`internal/setup`): steps `health, discover, permissions, aliases, review, done`, events `next, back, confirm, cancel`, per-step guards, and draft operations that are only accepted at their own step. A session is a JSON draft (selected entities with desired access and alias, plus the base each was derived from) in `setup_sessions`, saved with a compare-and-swap on a `version` counter.

- **Concurrency.** Two simultaneous clicks: one wins, the other gets a refreshed panel. One active session per administrator and guild (partial unique index); different administrators are independent.
- **Replay and stale panels.** Component IDs carry the session version they were rendered for. The confirmation additionally applies only to the reviewed version, and applying marks the session completed in the same transaction as the writes, so a replay writes nothing.
- **Atomic apply.** One transaction: verify the session, re-read the live configuration, detect changes made outside the session (per field), validate (grant rule, entity exists, alias uniqueness), write permissions and aliases, write audit entries, complete the session. Any failure rolls everything back. Home Assistant is queried before the transaction.
- **Stale confirmations.** If an entity changed outside the session, apply writes nothing, rebases the draft onto the live state, bumps the version (killing the old confirmation) and sends the administrator back to review.
- **Discovery and sync.** `entities.Discovery` keeps one immutable snapshot of compact items (30 second reuse; Refresh, `/sync` and apply always refetch). `/sync` additionally records entity IDs in `known_entities` to report new, removed and returned entities across restarts; names and states are never stored.
- **Aliases.** Stored by canonical ID; resolution tries an entity ID first and only resolves an alias to an entity the caller already has the required access on.

## Sensor monitoring (v0.3.0)

The full design, protocol and rationale are in [design/sensor-monitoring.md](design/sensor-monitoring.md). In short:

- `internal/monitor.Monitor` is created in `serve()`, started in the application's background `WaitGroup` and stopped by the same context as the bot. There is no global state.
- Each connection is an `internal/homeassistant/ws.Session` with exactly two goroutines (reader and pinger). A new session is never dialed before the previous one has been closed and joined, so a subscription can never be duplicated.
- The connection moves through `connecting, authenticating, subscribing, synchronizing, ready`, and `reconnecting` (backoff) after any failure. Health is five separate facts (transport, authentication, subscription, snapshot, proof of life); `/health` shows them separately and only `ready` plus a recent frame is "healthy".
- After every (re)connect NeptHass subscribes to `state_changed`, buffers events, fetches `get_states`, applies the snapshot as authoritative, then replays the buffered events through the per-sensor ordering rule (`last_updated`). Home Assistant does not replay missed events, and NeptHass never invents them.
- `Tracker` keeps the latest state of every classified `sensor`/`binary_sensor` (temperature, humidity, battery, motion, door, window, water leak, battery low) in memory only: duplicates and stale events are dropped, memory is bounded, nothing is written to SQLite and there is no migration.
- `Reader` is the only way monitored data reaches a Discord user. It re-checks the caller's level and the entity's *current* permission on every read, so the cache grants nothing and revocation is immediate. `/sensors` and the `/health` monitoring line are built on it.
- Dependency direction: `discord/commands -> monitor -> {entities, auth, config, homeassistant/ws}`. `monitor` knows nothing about Discord.
