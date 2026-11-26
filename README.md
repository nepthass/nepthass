# NeptHass

**Your Home. Your Command.**

NeptHass is a secure, self-hosted Discord bridge for [Home Assistant](https://www.home-assistant.io/). It lets approved people view selected entities, control selected lights and switches, monitor sensors, and receive alerts without exposing Home Assistant to the internet.

NeptHass is an independent community project and is not affiliated with Home Assistant, Nabu Casa, or Discord.

## Features

- Deny-by-default, per-entity sharing with `read` or `control` access.
- Control limited to lights and switches.
- Guided `/setup`, aliases, synchronization, and audit logging.
- Live monitoring for supported temperature, humidity, battery, motion, door, window, opening, garage-door, and water-leak sensors.
- Best-effort Discord notifications with duration rules, hysteresis, unit-safe thresholds, cooldowns, and monitoring-down notices.
- Outbound-only connections to Discord and Home Assistant.
- SQLite persistence, non-root container, read-only root filesystem, dropped capabilities, and secret redaction.

> Notifications are not a life-safety alarm. Discord delivery can be delayed, duplicated, or lost during an outage or restart.

## How it works

```text
Discord <-- outbound gateway/REST --> NeptHass <-- local REST/WebSocket --> Home Assistant
                                      |
                                      `-- SQLite data volume
```

## Run with Docker — no repository clone

Version tags publish images to GitHub Container Registry. After the first public release image is available, create `nepthass.env` with the required values:

```env
DISCORD_BOT_TOKEN=
DISCORD_APPLICATION_ID=
DISCORD_GUILD_ID=
DISCORD_ADMIN_USER_IDS=
HA_TOKEN=
HA_URL=http://127.0.0.1:8123
```

Then run:

```bash
docker volume create nepthass-data

docker run -d \
  --name nepthass \
  --restart unless-stopped \
  --network host \
  --env-file nepthass.env \
  -v nepthass-data:/data \
  --read-only \
  --tmpfs /tmp \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  ghcr.io/nepthass/nepthass:latest
```

This command is intended for Linux. Docker Desktop users should set `HA_URL=http://host.docker.internal:8123` and omit `--network host` if host networking is unavailable.

Use a versioned image such as `ghcr.io/nepthass/nepthass:1.0.0` for predictable upgrades. The `latest` tag follows the newest stable release.

Full setup instructions: [docs/setup.md](docs/setup.md).

## Commands

| Command | Access | Purpose |
| --- | --- | --- |
| `/ping` | users | Check bot responsiveness |
| `/health` | users | Show Discord, Home Assistant, monitoring, notifications, and database health |
| `/devices` | users | List shared entities |
| `/sensors` | users | Show live readings from shared supported sensors |
| `/status` | users | Show an entity state |
| `/control` | users | Turn an authorized light or switch on or off |
| `/setup` | admins | Discover entities and configure sharing and aliases |
| `/sync` | admins | Refresh Home Assistant entities |
| `/permissions` | admins | Grant, list, or revoke entity access |
| `/notifications` | admins | Configure and inspect alerts |
| `/audit` | admins | Review recent actions and denials |

See [docs/commands.md](docs/commands.md) for command options and behavior.

## Configuration

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `DISCORD_BOT_TOKEN` / `_FILE` | yes | — | Discord bot token or secret-file path |
| `DISCORD_APPLICATION_ID` | yes | — | Discord application ID |
| `DISCORD_GUILD_ID` | yes | — | The Discord server NeptHass serves |
| `DISCORD_ADMIN_USER_IDS` | yes | — | Comma-separated administrator IDs |
| `DISCORD_ALLOWED_USER_IDS` | no | none | Comma-separated regular-user IDs |
| `HA_TOKEN` / `_FILE` | yes | — | Home Assistant long-lived token or secret-file path |
| `HA_URL` | no | `http://127.0.0.1:8123` | Home Assistant base URL |
| `DATABASE_PATH` | no | `/data/nepthass.db` | SQLite database path |
| `AUDIT_RETENTION_DAYS` | no | `90` | Audit retention, 1–3650 days |
| `LOG_LEVEL` / `LOG_FORMAT` | no | `info` / `json` | Logging configuration |

See [.env.example](.env.example) for every option.

## Documentation

- [Setup, upgrades, backups, and rollback](docs/setup.md)
- [Discord command reference](docs/commands.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Security model](docs/security.md)
- [Architecture](docs/architecture.md)
- [Development and testing](docs/development.md)
- [Contributing](CONTRIBUTING.md)
- [Release history](CHANGELOG.md)

## Development

```bash
gofmt -l .
go vet ./...
go test -race -shuffle=on -count=1 ./...
docker compose config --quiet
docker build -t nepthass:dev .
```

See [docs/development.md](docs/development.md) for the isolated Home Assistant integration environment.

## License

[MIT](LICENSE)
