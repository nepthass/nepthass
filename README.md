# NeptHass

**Your Home. Your Command.**

NeptHass is a secure, self-hosted, Discord-first control and monitoring bridge for [Home Assistant](https://www.home-assistant.io/). It runs next to Home Assistant on your own machine, talks to it over the local network, and lets the people you choose check and control selected devices from Discord slash commands.

NeptHass does not replace Home Assistant and has no device drivers. It works through Home Assistant entities and its APIs, and your Home Assistant automations keep working whether or not Discord is reachable.

> NeptHass is an independent community project. It is not affiliated with, endorsed by, or sponsored by Home Assistant (Nabu Casa) or Discord, and it does not use their logos.

## Project status

NeptHass is being built milestone by milestone. v0.1.0 (Foundation) and v0.2.0 (Interactive Setup) are released. v0.3.0 (Sensor Monitoring) is released. Nothing below is advertised unless it is implemented and verified.

| Milestone | Scope | Status |
| --- | --- | --- |
| v0.1.0 | Foundation: configuration, Discord connection, Home Assistant REST client, SQLite, authorization, basic commands, Docker | **Implemented** |
| v0.2.0 | Interactive `/setup` wizard, entity discovery, aliases, persistent setup sessions, `/sync` | **Implemented** |
| v0.3.0 | Home Assistant WebSocket, real-time sensor monitoring, reconnection and reconciliation | **Released** |
| v0.4.0 | Smart notifications (motion, door/window, leak, battery, temperature, humidity), cooldowns, monitoring-down notices | **Implemented on `feature/v0.4.0` (not released)** |
| v0.5.0 | Rules engine: thresholds, durations, quiet hours, hysteresis | Planned |
| v0.6.0 | Interactive `/home` dashboard | Planned |
| v1.0.0 | Stable release: hardening, release workflow, full docs | Planned |

### What v0.4.0 adds

- **Discord alerts** for shared sensors, sent to one administrator-chosen text channel: a door or window opens, motion is detected, a leak sensor gets wet, a battery is low, or a temperature, humidity or battery level goes above or below a threshold.
- `/notifications` (administrators, guild-only): `channel`, `add`, `list`, `pause`, `resume`, `remove`, `test` and `status`. Up to 200 rules; per-rule cooldowns with sensible defaults (door 1 min, motion 5 min, leak and temperature 15 min, battery 6 h).
- **Honest semantics:** a first observation never alerts, late alerts after a reconnect are marked as late, a leak that comes back wet from `unavailable` alerts and is marked uncertain, and a message is dropped (and counted) rather than sent after 10 minutes or after the sensor stopped being shared. No mention or ping is ever sent.
- **Monitoring-down notices:** one alert after five continuous minutes without live monitoring, and a recovery notice once that alert was delivered.
- **Best effort, not an alarm:** delivery is best-effort with bounded staleness and possible duplicates. Pending alerts are kept in memory only (there is **no persisted outbox**), so a crash or restart loses what was queued. Lost alerts are reconciled with one digest of the *current* state.
- Notification health in `/health` and `/notifications status`. Everyone who can read the alert channel sees the alerts, so use a private channel. Design and guarantees: [docs/design/notifications.md](docs/design/notifications.md); commands: [docs/commands.md](docs/commands.md).
- **Upgrading:** migration 0003 adds two tables. Back up the database first; a v0.3.0 binary refuses a database that has been upgraded (restore the backup to go back).

### What v0.3.0 adds

- A persistent, authenticated connection to the Home Assistant WebSocket API that receives `state_changed` events in real time and tracks the state of temperature, humidity, battery, motion, door, window and water-leak sensors (classified by Home Assistant's domain and device class, never by entity-ID text).
- Safe recovery: bounded, jittered reconnection backoff (a rejected token retries slowly, never in a hot loop), a fresh authoritative snapshot after every reconnect, no duplicate subscriptions, and a clean stop on shutdown.
- Honest health: `/health` reports transport, authentication, subscription, snapshot and proof of life separately, and only calls monitoring healthy when all of them hold.
- `/sensors`: live, interpreted readings of the sensors shared with you (`on` for a door means *open*, for motion *detected*), clearly marked as stale when monitoring is down. Access is re-checked on every read, so revoking a share takes effect immediately.
- Live state is kept in memory only: no new tables, no migration, no new settings.

Monitoring itself **observes only**; notifications are a separate layer added in v0.4.0, and rules are v0.5.0. Details: [docs/design/sensor-monitoring.md](docs/design/sensor-monitoring.md).

### What v0.2.0 adds

- `/setup`: an administrator-only, six-step wizard (connection health, discover devices, permissions, aliases, review, done) with search, domain filters, pagination, select menus, modals, Back / Next / Cancel / Resume, and an explicit confirmation before anything is written. Progress is stored in SQLite and survives restarts.
- Aliases: human-readable names for entities, usable in `/status` and `/control`, shown in `/devices`.
- `/sync`: refreshes the view of Home Assistant and reports new, removed and unavailable entities. It never shares anything.
- Setup writes the same permission table as `/permissions`, applies the same rules, and is audited.
- A migration (0002) that upgrades v0.1.0 databases in place; existing permissions and audit records are preserved.

Details: [docs/commands.md](docs/commands.md), [docs/design/interactive-setup.md](docs/design/interactive-setup.md).

### What works in v0.1.0

- Connects to the Discord gateway and recovers from network failures on its own.
- Authenticates to Home Assistant with a Long-Lived Access Token and reports its health.
- Slash commands: `/ping`, `/health`, `/devices`, `/status`, `/control`, `/permissions`, `/audit`.
- Deny-by-default access: an entity is invisible until an administrator shares it with `/permissions allow`.
- On/off control of **lights and switches only**. Locks, garage doors, alarm panels and every other domain are read-only at most.
- Audit log of control attempts and permission changes, with configurable retention.
- Ephemeral (only-you-can-see-it) replies, per-user rate limiting, secret redaction in logs.
- Single static binary, non-root distroless Docker image, SQLite with versioned migrations.

### What does not exist yet

No rules engine (durations, quiet hours, hysteresis and rule logic: v0.5.0), no `/home` dashboard, no `/alerts` or `/settings` commands, no persisted notification outbox and no notification history. Notifications are best-effort with possible duplicates and are not a certified alarm system; see [docs/design/notifications.md](docs/design/notifications.md).

## How it works

```text
Discord  <-- gateway (outbound) -->  NeptHass  <-- REST + WebSocket (local) -->  Home Assistant  <-->  devices
                                        |
                                     SQLite (/data)
```

NeptHass only makes outbound connections: nothing listens on a port and Home Assistant is never exposed to the internet. See [docs/architecture.md](docs/architecture.md).

## Quick start

Requirements: Linux, Docker Engine with Compose, a running Home Assistant, a Discord server where you can add bots.

1. **Create the Discord bot.** In the [Discord Developer Portal](https://discord.com/developers/applications) create an application, add a bot, and copy the bot token and the application ID.
2. **Create a Home Assistant token.** Profile page, Security, Long-Lived Access Tokens.
3. **Collect IDs.** In Discord enable Developer Mode, then copy your server (guild) ID and your user ID.
4. **Configure.**
   ```bash
   git clone https://github.com/nepthass/nepthass.git && cd nepthass
   cp .env.example .env
   # edit .env: DISCORD_BOT_TOKEN, DISCORD_APPLICATION_ID, DISCORD_GUILD_ID,
   #            DISCORD_ADMIN_USER_IDS, HA_TOKEN
   ```
5. **Invite the bot** to your server (replace `APPLICATION_ID`):
   `https://discord.com/oauth2/authorize?client_id=APPLICATION_ID&scope=bot%20applications.commands&permissions=0`
6. **Run it.**
   ```bash
   docker compose up -d --build
   docker compose logs -f nepthass
   ```
7. **Share devices.** In Discord run `/setup`, pick the entities to share, choose read or control, optionally add aliases, review and confirm. Then try `/devices`, `/status` and `/control`. (`/permissions allow` still works for a single entity.)

The full walkthrough, bridge-network setup and secret files are in [docs/setup.md](docs/setup.md).

## Commands

| Command | Who | What it does |
| --- | --- | --- |
| `/ping` | users | Check the bot responds; shows gateway latency |
| `/health` | users | Discord, Home Assistant REST, live monitoring (WebSocket) and database status |
| `/devices [page]` | users | List the entities shared with you (10 per page) |
| `/sensors [page]` | users | Live readings of the monitored sensors shared with you (10 per page) |
| `/status entity` | users | Show an entity's state (needs `read` or `control` access) |
| `/control entity action` | users | Turn a light or switch on or off (needs `control` access) |
| `/permissions allow|revoke|list` | admins | Manage which entities are shared and at what level |
| `/audit [count]` | admins | Recent control attempts and permission changes |

"Users" are the administrators plus anyone in `DISCORD_ALLOWED_USER_IDS`. Entity names are suggested by autocomplete and aliases work wherever an entity is expected. Full reference: [docs/commands.md](docs/commands.md).

## Configuration

All bootstrap settings are environment variables (see [.env.example](.env.example)). Secrets are never entered in Discord.

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `DISCORD_BOT_TOKEN` / `_FILE` | yes | | Bot token (or path of a file containing it) |
| `DISCORD_APPLICATION_ID` | yes | | Discord application ID |
| `DISCORD_GUILD_ID` | yes | | The one server NeptHass serves |
| `DISCORD_ADMIN_USER_IDS` | yes | | Comma-separated administrator user IDs |
| `DISCORD_ALLOWED_USER_IDS` | no | none | Comma-separated non-admin user IDs |
| `DISCORD_COMMAND_SCOPE` | no | `guild` | `guild` or `global` command registration |
| `HA_URL` | no | `http://127.0.0.1:8123` | Home Assistant base URL |
| `HA_TOKEN` / `_FILE` | yes | | Long-Lived Access Token (or file) |
| `HA_REQUEST_TIMEOUT` | no | `10s` | Per-request timeout (1s to 2m) |
| `DATABASE_PATH` | no | `data/nepthass.db` (`/data/nepthass.db` in the image) | SQLite file |
| `AUDIT_RETENTION_DAYS` | no | `90` | Audit log retention (1 to 3650) |
| `LOG_LEVEL` / `LOG_FORMAT` | no | `info` / `text` | `debug`..`error`; `text` or `json` |

`HA_URL` is also used for the WebSocket (`http` becomes `ws`, `https` becomes `wss`, a path prefix is kept), so live monitoring needs no extra setting. If Home Assistant is behind a reverse proxy it must allow WebSocket upgrades on `/api/websocket`.

## Security

Read-only by default, an explicit per-entity allowlist, control limited to `light` and `switch`, administrator and guild allowlists checked on every interaction, secrets redacted from logs, no listening ports, live sensor data readable only through a per-read permission check. Details and hardening advice: [docs/security.md](docs/security.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## Development

```bash
gofmt -l .                              # must print nothing
go vet ./...
go test -race -shuffle=on -count=1 ./...
docker compose config
docker build -t nepthass:dev .
```

No Go toolchain locally? Run the Go commands in a container: `docker run --rm -v "$PWD":/src -w /src golang:1.27 go test ./...`. Tests use mock servers and temporary SQLite files; they never need Discord or Home Assistant credentials. See [CONTRIBUTING.md](CONTRIBUTING.md).

### Local development environment

No Home Assistant or Zigbee hardware? `compose.dev.yaml` starts a real Home Assistant Container with virtual devices (light, switch, motion, door, window, temperature, humidity, water leak and battery sensors) next to NeptHass, in an isolated Compose project that never touches your production setup. See [docs/development/setup.md](docs/development/setup.md), the [virtual devices](docs/development/virtual-devices.md), and the [smoke tests](docs/development/smoke-tests.md).

More documentation: [commands](docs/commands.md), [architecture](docs/architecture.md), [setup](docs/setup.md), [security](docs/security.md), [troubleshooting](docs/troubleshooting.md).

## License

[MIT](LICENSE)
