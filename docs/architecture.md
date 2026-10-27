# Architecture

NeptHass is a modular monolith: one Go binary, one SQLite file, no public API.

```text
cmd/nepthass            wiring only (config, logger, dependencies, signals)
internal/config         environment parsing and validation, Secret type
internal/logging        slog logger with secret scrubbing
internal/storage        SQLite open + migration runner      <- migrations/ (embedded SQL)
internal/auth           guild/user allowlists, rate limiter
internal/audit          audit log table + retention
internal/entities       entity model, permission table, Service (the business rules)
internal/homeassistant/rest   Home Assistant REST client (service allowlist)
internal/discord        gateway session, interaction routing, retry/backoff
internal/discord/commands     slash command definitions and replies
tests/fakeha            in-memory Home Assistant used by tests
```

Dependency direction: `discord -> commands -> entities -> {auth, audit}`; `rest -> entities` (types only); `entities` depends on small interfaces (`HomeAssistant`, `Recorder`) that `rest` and `audit` satisfy. Discord-specific code lives only in `internal/discord` and `internal/discord/commands`; the rules about who may do what live in `entities` and know nothing about Discord.

## Interaction lifecycle

1. discordgo delivers an `InteractionCreate` event on the gateway.
2. `Bot.serve` (panic-safe) calls `handleInteraction`, which first runs `Authorizer.Authorize(guildID, userID)`. This happens for **every** interaction type, so no handler is reachable without being on an allowlist in the configured guild. Rejections get a generic ephemeral message and a WARN log.
3. Commands are rate limited per user (6 per 10 seconds), then acknowledged immediately with a deferred ephemeral response (Discord requires an answer within 3 seconds; Home Assistant may be slower).
4. `commands.Registry.Run` enforces the command's minimum level (user or admin), parses options, and calls `entities.Service`.
5. The reply is delivered by editing the deferred response. Mentions are disabled on every message.
6. Autocomplete requests follow the same authorization path and must answer within about 2.5 seconds; they read only the local allowlist, except administrator entity discovery, which has a 2 second Home Assistant timeout.

## Authorization layers

| Layer | Where | Rule |
| --- | --- | --- |
| Guild | `auth.Authorizer` | Interaction must come from `DISCORD_GUILD_ID` (DMs rejected) |
| User | `auth.Authorizer` | Admin list, or optional user list; everyone else rejected |
| Command level | `commands.Registry`, `entities.Service` | `/permissions` and `/audit` need admin, checked in both places |
| Entity | `entities.Permissions` | No row means no access, even for administrators |
| Domain | `entities.Domain.Controllable`, `rest.allowedServices` | Only `light` and `switch` can be controlled; checked at grant time, at execution time, and again inside the REST client |

Discord roles and channel permissions are never consulted.

## Data model (SQLite)

Migrations are embedded from `migrations/`, applied in order inside transactions, and recorded in `schema_migrations` with a SHA-256 checksum. Startup refuses a database written by a newer build and refuses a migration whose checksum changed. Never edit a released migration; add a new one.

| Table | Purpose |
| --- | --- |
| `entity_permissions` | entity ID, `read`/`control`, who granted it, when |
| `command_audit_logs` | control attempts and permission changes with outcome |
| `schema_migrations` | applied migrations and checksums |

Each package owns the SQL for its own table. Timestamps are fixed-width UTC text so they sort chronologically. The database runs in WAL mode with foreign keys on and a 5 second busy timeout; the file is created with owner-only permissions. Tables for later milestones (aliases, subscriptions, rules, outbox, setup sessions) are added by those milestones' migrations, not before.

## Failure handling

- **Discord unreachable at start:** token verification and gateway connection are retried with exponential backoff and jitter (1 s up to 1 min). A rejected token (HTTP 401) is permanent and stops the process with a clear error.
- **Discord drops after start:** discordgo reconnects and resumes the gateway by itself.
- **Home Assistant unreachable at start:** logged as a warning; NeptHass keeps running and uses it once it answers. A rejected token stops the process (a restart cannot fix it).
- **Home Assistant unreachable later:** commands answer with a friendly error; each request has a timeout and redirects are never followed.
- **Shutdown:** SIGTERM or SIGINT cancels the root context; the Discord session is closed and the database released.

## Logging and secrets

`log/slog`, text or JSON. Credentials are `config.Secret` values that print as `[REDACTED]` everywhere, and the logger additionally scrubs the literal secret values from messages, strings and errors. discordgo's own log lines are routed through the same logger (its only hook is a package-level variable, the one piece of global state in the program).

## Design decisions

- **Pure-Go SQLite (`modernc.org/sqlite`)** instead of CGO: a static binary, a distroless image, trivial cross-compilation.
- **Standard library first:** configuration parsing, HTTP, logging, migrations and rate limiting use only the standard library. Runtime dependencies are discordgo and the SQLite driver.
- **REST only in v0.1.0:** the WebSocket client arrives with real-time monitoring in v0.3.0.
