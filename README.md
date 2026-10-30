# NeptHass

**Your Home. Your Command.**

NeptHass is a secure, self-hosted, Discord-first control and monitoring bridge for [Home Assistant](https://www.home-assistant.io/). It runs next to Home Assistant on your own machine, talks to it over the local network, and lets the people you choose check and control selected devices, and receive alerts about selected sensors, from Discord.

NeptHass does not replace Home Assistant and has no device drivers. It works through Home Assistant entities and its APIs, and your Home Assistant automations keep working whether or not Discord is reachable.

> NeptHass is an independent community project. It is not affiliated with, endorsed by, or sponsored by Home Assistant (Nabu Casa) or Discord, and it does not use their logos.

## Why it exists

You often want your family or housemates to check a sensor or switch a light without giving them a Home Assistant account, and you do not want to expose Home Assistant to the internet to do it. NeptHass is a small bridge for that: it makes only outbound connections, shares **nothing by default**, and lets an administrator decide, entity by entity, what each person may see or control, all from Discord.

## What it does

- **Control** lights and switches with `/control`. Every other kind of device is read-only at most.
- **Share explicitly.** An entity is invisible until an administrator shares it at `read` or `control` level, with a guided `/setup` wizard, aliases and an audit log.
- **Monitor live.** A persistent connection to the Home Assistant WebSocket tracks temperature, humidity, battery, motion, door, window and water-leak sensors; `/sensors` shows them, and `/health` says honestly whether monitoring is live.
- **Notify.** Administrators choose one Discord text channel and create rules (a door opens, motion, a leak, a low battery, a value above or below a threshold) with per-rule cooldowns. NeptHass also tells that channel when it has lost sight of Home Assistant. Alerts never ping anyone and respect sharing at every step.
- **Stay private.** No listening ports, secrets only in the environment, secret redaction in logs, replies visible only to the person who asked.

## Project maturity

NeptHass is **pre-1.0** and is developed milestone by milestone in a private repository; a public launch is planned for a later milestone. Each release is verified by automated tests, including a suite that runs against a real Home Assistant, and by manual testing on a Discord test server. Manual coverage is partial for v0.4.0; see [the known gaps](docs/development/smoke-tests.md#known-gaps).

**Notifications are best-effort, not an alarm system.** Delivery has a bounded staleness (10 minutes) and can occasionally duplicate. Queued alerts are kept in memory, so a crash or restart loses what was waiting, and a sensor that is already in its alert state when NeptHass starts is not announced. Do not rely on them for life-safety. Details and guarantees: [docs/design/notifications.md](docs/design/notifications.md).

| Version | Scope | Status |
| --- | --- | --- |
| v0.1.0 | Foundation: configuration, Discord connection, Home Assistant REST client, SQLite, authorization, basic commands, Docker | Released |
| v0.2.0 | Interactive `/setup` wizard, entity discovery, aliases, `/sync` | Released |
| v0.3.0 | Home Assistant WebSocket, real-time sensor monitoring | Released |
| v0.4.0 | Smart notifications and monitoring-down notices | Released (latest) |
| v0.5.0 and later | See the [roadmap](docs/roadmap.md) | Planned |

Release history: [CHANGELOG.md](CHANGELOG.md).

## How it works

```text
Discord  <-- gateway (outbound) -->  NeptHass  <-- REST + WebSocket (local) -->  Home Assistant  <-->  devices
   ^                                    |
   '---- alerts over REST (outbound) ---+--- SQLite (/data)
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
7. **Share devices.** In Discord run `/setup`, pick the entities to share, choose read or control, optionally add aliases, review and confirm. Then try `/devices`, `/status` and `/control`.
8. **Optional: alerts.** Run `/notifications channel` to pick a private text channel, then `/notifications add` for a sensor. The bot needs View Channel, Send Messages and Embed Links in that channel.

The full walkthrough, bridge-network setup, secret files and upgrade notes are in [docs/setup.md](docs/setup.md).

## Commands

| Command | Who | What it does |
| --- | --- | --- |
| `/ping` | users | Check the bot responds; shows gateway latency |
| `/health` | users | Discord, Home Assistant REST, live monitoring (WebSocket), notifications and database status |
| `/devices [page]` | users | List the entities shared with you |
| `/sensors [page]` | users | Live readings of the monitored sensors shared with you |
| `/status entity` | users | Show an entity's state (needs `read` or `control` access) |
| `/control entity action` | users | Turn a light or switch on or off (needs `control` access) |
| `/setup` | admins | Guided wizard: discover entities, share them, set aliases |
| `/sync` | admins | Refresh the view of Home Assistant's entities |
| `/permissions allow\|revoke\|list` | admins | Manage which entities are shared and at what level |
| `/notifications channel\|add\|list\|pause\|resume\|remove\|test\|status` | admins | Configure and monitor alerts |
| `/audit [count]` | admins | Recent control attempts and configuration changes |

"Users" are the administrators plus anyone in `DISCORD_ALLOWED_USER_IDS`. Aliases work wherever an entity is expected. Full reference: [docs/commands.md](docs/commands.md).

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

`HA_URL` is also used for the WebSocket (`http` becomes `ws`, `https` becomes `wss`, a path prefix is kept). If Home Assistant is behind a reverse proxy it must allow WebSocket upgrades on `/api/websocket`. Notification settings (the alert channel and the rules) are stored in the database and managed with `/notifications`, not through environment variables.

## Security

Read-only by default, an explicit per-entity allowlist, control limited to `light` and `switch`, administrator and guild allowlists checked on every interaction, secrets redacted from logs, no listening ports, live sensor data readable only through a per-read permission check, and administrator-only notification settings. Everyone who can read the alert channel sees the alerts, so use a private channel. Details and hardening advice: [docs/security.md](docs/security.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## Documentation

| Topic | Where |
| --- | --- |
| Install, configure, upgrade, back up | [docs/setup.md](docs/setup.md) |
| Command reference | [docs/commands.md](docs/commands.md) |
| Architecture and design records | [docs/architecture.md](docs/architecture.md), [docs/design/](docs/design/) |
| Security model | [docs/security.md](docs/security.md) |
| Troubleshooting | [docs/troubleshooting.md](docs/troubleshooting.md) |
| Roadmap | [docs/roadmap.md](docs/roadmap.md) |
| Contributing and local development | [CONTRIBUTING.md](CONTRIBUTING.md), [docs/development/setup.md](docs/development/setup.md) |

## Development

```bash
gofmt -l .                              # must print nothing
go vet ./...
go test -race -shuffle=on -count=1 ./...
docker compose config --quiet
docker build -t nepthass:dev .
```

No Go toolchain locally? Run the Go commands in a container: `docker run --rm -v "$PWD":/src -w /src golang:1.27 go test ./...`. Tests use mock servers and temporary SQLite files; they never need Discord or Home Assistant credentials. No Home Assistant hardware? `compose.dev.yaml` starts a real Home Assistant with virtual devices in an isolated Compose project; see [docs/development/setup.md](docs/development/setup.md). Contribution rules: [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
