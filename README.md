# NeptHass

<div align="center">

### Your Home. Your Command.

**Control and monitor Home Assistant directly from Discord.**

[![CI](https://github.com/nepthass/nepthass/actions/workflows/ci.yml/badge.svg)](https://github.com/nepthass/nepthass/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/nepthass/nepthass?display_name=tag&v=2)](https://github.com/nepthass/nepthass/releases)
[![Container](https://img.shields.io/badge/container-ghcr.io-2496ED?logo=docker&logoColor=white)](https://github.com/nepthass/nepthass/pkgs/container/nepthass)
[![Go](https://img.shields.io/github/go-mod/go-version/nepthass/nepthass?logo=go&v=2)](go.mod)
[![License](https://img.shields.io/github/license/nepthass/nepthass?v=2)](LICENSE)

[Get started](#quick-start) · [Commands](#discord-commands) · [Documentation](#documentation)

</div>

NeptHass is a secure, self-hosted bridge between [Discord](https://discord.com/) and [Home Assistant](https://www.home-assistant.io/). From one Discord server, approved users can check device states, control shared lights and switches, monitor sensors, and receive real-time notifications.

Home Assistant stays private. NeptHass connects outward to Discord and communicates with Home Assistant on your local network—no public Home Assistant endpoint is required.

## What NeptHass can do

- **Control devices from Discord** — turn authorized Home Assistant lights and switches on or off with `/control`.
- **See live device status** — inspect shared entities with `/devices`, `/sensors`, and `/status`.
- **Monitor important sensors** — track motion, doors, windows, openings, garage doors, water leaks, temperature, humidity, and battery levels.
- **Send configurable alerts** — build notification rules with thresholds, durations, cooldowns, and monitoring-health notices.
- **Discover and synchronize entities** — use guided setup and synchronization without restarting the service.
- **Keep access precise** — grant `read` or `control` permission per entity, with deny-by-default behavior.
- **Record important activity** — review configuration changes, control actions, and denied requests through the audit log.
- **Run cleanly with Docker** — use the published container image without cloning or building the repository.

## See it in action

```text
/control entity:Hallway Light action:on

🔱 NeptHass — Command Sent
Turned on light.hallway_light.
State: off → on
```

NeptHass also listens to Home Assistant's WebSocket event stream, keeps supported entities synchronized, and sends configured alerts to your chosen Discord channel.

## How it works

```text
┌─────────┐       Discord Gateway / REST       ┌──────────┐
│ Discord │ <────────────────────────────────> │ NeptHass │
└─────────┘                                    └────┬─────┘
                                                    │
                                         REST + WebSocket
                                                    │
                                              local network
                                                    │
                                             ┌──────▼───────┐
                                             │ Home Assistant│
                                             └──────────────┘
```

NeptHass stores configuration, permissions, notification rules, and audit records in a local SQLite database.

## Quick start

You need:

- A Discord application and bot token
- A dedicated Discord server
- A Home Assistant long-lived access token
- Docker

Create a file named `nepthass.env`:

```env
DISCORD_BOT_TOKEN=your_discord_bot_token
DISCORD_APPLICATION_ID=your_application_id
DISCORD_GUILD_ID=your_discord_server_id
DISCORD_ADMIN_USER_IDS=your_discord_user_id
HA_TOKEN=your_home_assistant_token
HA_URL=http://127.0.0.1:8123
```

Create the persistent data volume:

```bash
docker volume create nepthass-data
```

Run NeptHass:

```bash
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

The example above is for Linux. With Docker Desktop, set `HA_URL=http://host.docker.internal:8123` and omit `--network host` when host networking is unavailable.

For predictable upgrades, pin a release tag such as:

```text
ghcr.io/nepthass/nepthass:1.0.0
```

Complete setup, Discord invitation, backup, upgrade, and rollback instructions are available in the [setup guide](docs/setup.md).

## Discord commands

| Command | Who can use it | What it does |
| --- | --- | --- |
| `/ping` | Approved users | Checks bot responsiveness |
| `/health` | Approved users | Shows Discord, Home Assistant, monitoring, notification, and database health |
| `/devices` | Approved users | Lists entities shared with the user |
| `/sensors` | Approved users | Shows live readings from supported shared sensors |
| `/status` | Approved users | Shows the current state of an entity |
| `/control` | Approved users | Turns an authorized light or switch on or off |
| `/setup` | Administrators | Discovers entities and configures sharing and aliases |
| `/sync` | Administrators | Synchronizes Home Assistant entities |
| `/permissions` | Administrators | Grants, lists, or revokes entity access |
| `/notifications` | Administrators | Configures alert channels and notification rules |
| `/audit` | Administrators | Reviews recent actions and access denials |

See the [command reference](docs/commands.md) for every option and example.

## Security by design

- Access is denied until a Discord user and Home Assistant entity are explicitly allowed.
- Read and control permissions are managed independently for every entity.
- Administrative commands are restricted to configured administrator IDs.
- Device control is limited to shared lights and switches.
- Credentials are loaded from environment variables or secret files and are redacted from logs.
- The container runs as a non-root user and supports a read-only root filesystem, dropped Linux capabilities, and `no-new-privileges`.
- Actions, configuration changes, and denied requests are written to the audit log.

Read the complete [security model](docs/security.md) and [vulnerability reporting policy](SECURITY.md).

## Configuration

The required settings are:

| Variable | Purpose |
| --- | --- |
| `DISCORD_BOT_TOKEN` | Authenticates the Discord bot |
| `DISCORD_APPLICATION_ID` | Identifies the Discord application |
| `DISCORD_GUILD_ID` | Selects the Discord server NeptHass serves |
| `DISCORD_ADMIN_USER_IDS` | Defines NeptHass administrators |
| `HA_TOKEN` | Authenticates with Home Assistant |

Common optional settings include `DISCORD_ALLOWED_USER_IDS`, `HA_URL`, `HA_REQUEST_TIMEOUT`, `DATABASE_PATH`, `AUDIT_RETENTION_DAYS`, `LOG_LEVEL`, and `LOG_FORMAT`.

See [.env.example](.env.example) for the complete configuration reference.

## Documentation

- [Setup, backup, upgrade, and rollback](docs/setup.md)
- [Discord command reference](docs/commands.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Security model](docs/security.md)
- [Architecture](docs/architecture.md)
- [Development and testing](docs/development.md)
- [Contributing](CONTRIBUTING.md)
- [Release history](CHANGELOG.md)

## Build and test

```bash
gofmt -l .
go vet ./...
go test -race -shuffle=on -count=1 ./...
docker compose config --quiet
docker build -t nepthass:dev .
```

The [development guide](docs/development.md) covers the isolated Home Assistant integration environment and the full verification workflow.

## Community

Issues and pull requests are welcome. Please read the [contribution guide](CONTRIBUTING.md) before submitting changes.

NeptHass is an independent community project and is not affiliated with Home Assistant, Nabu Casa, or Discord.

## License

NeptHass is available under the [MIT License](LICENSE).
