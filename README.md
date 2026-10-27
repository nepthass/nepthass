# NeptHass

**Your Home. Your Command.**

NeptHass is a secure, self-hosted, Discord-first control and monitoring bridge for [Home Assistant](https://www.home-assistant.io/). It runs next to Home Assistant on your own machine, talks to it over the local network, and lets the people you choose check and control selected devices from Discord slash commands.

NeptHass does not replace Home Assistant and has no device drivers. It works through Home Assistant entities and its APIs, and your Home Assistant automations keep working whether or not Discord is reachable.

> NeptHass is an independent community project. It is not affiliated with, endorsed by, or sponsored by Home Assistant (Nabu Casa) or Discord, and it does not use their logos.

## Project status: v0.1.0 (Foundation)

NeptHass is being built milestone by milestone. This release is the foundation: a working, secure core you can run today. Nothing below is advertised unless it is implemented.

| Milestone | Scope | Status |
| --- | --- | --- |
| v0.1.0 | Foundation: configuration, Discord connection, Home Assistant REST client, SQLite, authorization, basic commands, Docker | **Implemented** |
| v0.2.0 | Interactive `/setup` wizard, entity discovery UI, aliases, `/sync` | Planned |
| v0.3.0 | Home Assistant WebSocket, real-time sensor monitoring, reconnection and reconciliation | Planned |
| v0.4.0 | Smart notifications (motion, door/window, leak, battery), cooldowns, outbox | Planned |
| v0.5.0 | Rules engine: thresholds, durations, quiet hours, hysteresis | Planned |
| v0.6.0 | Interactive `/home` dashboard | Planned |
| v1.0.0 | Stable release: hardening, release workflow, full docs | Planned |

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

No `/setup` wizard (access is managed with `/permissions`), no sensor monitoring, no alerts or notifications, no rules, no `/home` dashboard, no `/sync`, `/sensors`, `/alerts` or `/settings` commands. Discord notifications, when they arrive, will be best-effort and must not be relied on as an emergency alarm.

## How it works

```text
Discord  <-- gateway (outbound) -->  NeptHass  <-- REST (local) -->  Home Assistant  <-->  devices
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
7. **Share a device.** In Discord: `/permissions allow entity:light.living_room access:control`, then `/control entity:light.living_room action:on`.

The full walkthrough, bridge-network setup and secret files are in [docs/setup.md](docs/setup.md).

## Commands

| Command | Who | What it does |
| --- | --- | --- |
| `/ping` | users | Check the bot responds; shows gateway latency |
| `/health` | users | Discord, Home Assistant REST and database status |
| `/devices [page]` | users | List the entities shared with you (10 per page) |
| `/status entity` | users | Show an entity's state (needs `read` or `control` access) |
| `/control entity action` | users | Turn a light or switch on or off (needs `control` access) |
| `/permissions allow|revoke|list` | admins | Manage which entities are shared and at what level |
| `/audit [count]` | admins | Recent control attempts and permission changes |

"Users" are the administrators plus anyone in `DISCORD_ALLOWED_USER_IDS`. Entity names are suggested by autocomplete.

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

## Security

Read-only by default, an explicit per-entity allowlist, control limited to `light` and `switch`, administrator and guild allowlists checked on every interaction, secrets redacted from logs, no listening ports. Details and hardening advice: [docs/security.md](docs/security.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## Development

```bash
gofmt -l .                              # must print nothing
go vet ./...
go test -race -shuffle=on -count=1 ./...
docker compose config
docker build -t nepthass:dev .
```

No Go toolchain locally? Run the Go commands in a container: `docker run --rm -v "$PWD":/src -w /src golang:1.27 go test ./...`. Tests use mock servers and temporary SQLite files; they never need Discord or Home Assistant credentials. See [CONTRIBUTING.md](CONTRIBUTING.md).

More documentation: [architecture](docs/architecture.md), [setup](docs/setup.md), [security](docs/security.md), [troubleshooting](docs/troubleshooting.md).

## License

[MIT](LICENSE)
