# Architecture

NeptHass is a single Go process with no inbound network service. It connects outward to Discord and to Home Assistant and stores configuration in SQLite.

## Components

```text
Discord interactions
        |
        v
Authorization and rate limiting
        |
        +--> setup / permissions / audit
        +--> entity discovery, status, and control
        +--> monitoring and notifications
                         |
Home Assistant <--- REST + WebSocket
                         |
                      SQLite
```

- `cmd/nepthass`: startup, dependency wiring, and graceful shutdown.
- `internal/config`: validated environment configuration and secret handling.
- `internal/auth`: guild/user authorization and rate limiting.
- `internal/discord`: gateway session, commands, interaction routing, and message rendering.
- `internal/homeassistant/rest`: discovery, state reads, and the restricted control client.
- `internal/homeassistant/ws`: authenticated WebSocket transport.
- `internal/entities`: permissions, aliases, entity resolution, discovery, and control policy.
- `internal/setup`: persistent setup wizard sessions and transactional apply.
- `internal/monitor`: synchronization, event processing, sensor interpretation, and reconnect state.
- `internal/notify`: notification configuration, rules, queues, cooldowns, and delivery state.
- `internal/audit`: bounded audit history and refused-command recording.
- `internal/storage`: SQLite opening, migration validation, and schema upgrades.

## Authorization

Every Discord interaction is checked for the configured guild and an allowed user. Administrator commands perform an additional administrator check. Entity reads require current `read` or `control` permission; writes require `control` and a server-side domain allowlist.

Control is deliberately limited to `light.turn_on`, `light.turn_off`, `switch.turn_on`, and `switch.turn_off`. Unknown, unshared, dangerous, or unsupported entities fail closed.

## Home Assistant

REST is used for authentication, discovery, state reads, and allowed service calls. WebSocket `state_changed` events provide live monitoring. A reconnect always performs a fresh snapshot so current state wins over cached events.

Incoming entity IDs, states, attributes, timestamps, and message sizes are bounded and validated. Unknown or unavailable readings are never presented as safe.

## Setup and permissions

Setup sessions live in SQLite and are bound to one administrator and guild. Every step carries a version. Confirmation applies the reviewed permission and alias changes in one transaction; stale or replayed confirmations cannot restore revoked access or write twice.

## Monitoring and notifications

The monitor tracks supported sensor classes in memory. User-facing reads and alert delivery check current permissions again, so revocation applies immediately to later reads and queued messages.

Notification rules and settings are persistent, but the delivery queue is intentionally in memory. Delivery uses bounded retries, rate limiting, cooldowns, staleness limits, and a circuit breaker. Discord remains best-effort and is not treated as an alarm transport.

## Storage

SQLite stores permissions, aliases, setup sessions, notification configuration, delivery marks, and audit records. Migrations are embedded and checksummed. A changed migration or a database created by a newer build is refused rather than guessed at.

## Failure boundaries

- Invalid configuration or rejected bootstrap credentials fail startup.
- Discord or Home Assistant outages use bounded retry and backoff.
- Monitoring is marked stale while disconnected and reconciled after recovery.
- Database errors fail closed for permission-sensitive operations.
- Shutdown is bounded and closes the Discord session, monitoring, notification workers, and database.

## Security assumptions

The host, Docker runtime, configured Discord accounts, and Home Assistant token must be trusted. NeptHass reduces exposed capability but cannot protect a compromised host or credential. See [security.md](security.md).
