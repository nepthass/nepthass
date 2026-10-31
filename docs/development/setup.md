# Development environment setup

This guide builds a complete local test bench: a **real Home Assistant Container** with **virtual devices** (no Zigbee hardware needed), NeptHass built from your checkout, and a **real Discord test bot**. Everything lives in its own Docker Compose project, `nepthass-dev`, so it cannot collide with a production NeptHass or an existing Home Assistant.

The production files (`compose.yaml`, `Dockerfile`, `.env.example`) are not used or changed by anything here.

## What you get

```text
Your PC
  browser --> http://127.0.0.1:18123 (localhost only) --+
                                                        |
  Docker project "nepthass-dev"  (private network nepthass-dev_dev)
  +------------------------------------------------------------------+
  |  homeassistant  (real Home Assistant, virtual devices)           |
  |       ^                                                          |
  |       | http://homeassistant:8123  (container to container)      |
  |  nepthass  (built from this checkout, profile "app")  ---------> | Discord (outbound)
  +------------------------------------------------------------------+
  volumes: nepthass-dev_ha-config, nepthass-dev_nepthass-data
```

- Home Assistant's UI is published on `127.0.0.1` only (default port `18123`, so it never clashes with a real Home Assistant on 8123). Nothing is exposed on other interfaces.
- NeptHass reaches Home Assistant by its Compose service name. `127.0.0.1` inside the NeptHass container is the NeptHass container itself, so it must not be used there.
- No container is privileged and no container name is fixed.

## Prerequisites

- Docker Engine or Docker Desktop with Compose v2.24 or newer (`docker compose version`).
- About 2 GB of free memory and internet access (to pull images and, for NeptHass, to reach Discord).
- A Discord account.

All commands below are run from the repository root. They work in Git Bash, PowerShell and Linux/macOS shells. `--env-file .env.dev` must be passed to every `docker compose` command so ports and secrets come from your dev file.

Tip: define a shorthand once per shell.

```bash
# bash / zsh
alias dc='docker compose --env-file .env.dev -f compose.dev.yaml'
```

```powershell
# PowerShell
function dc { docker compose --env-file .env.dev -f compose.dev.yaml @args }
```

The rest of this guide spells out the full command.

## 1. Create your dev env file

```bash
cp .env.dev.example .env.dev
```

`.env.dev` is git-ignored. Never commit it and never paste its contents into issues or chat. You will fill it in over the next steps.

## 2. Start Home Assistant

```bash
docker compose --env-file .env.dev -f compose.dev.yaml up -d
docker compose --env-file .env.dev -f compose.dev.yaml ps
```

The first start pulls the image and takes one to three minutes. `ps` shows `healthy` when Home Assistant answers. Follow the log with:

```bash
docker compose --env-file .env.dev -f compose.dev.yaml logs -f homeassistant
```

This starts **only Home Assistant**. NeptHass is in the `app` profile and starts later, because it needs a Home Assistant token that does not exist yet.

## 3. Home Assistant onboarding

1. Open <http://127.0.0.1:18123>.
2. **Create my smart home.** Choose any name, username and password. This is a throwaway development instance; use a password you do not use anywhere else.
3. When asked for a location, enter anything (or skip the map); do not use your real address. Decline analytics.
4. Finish. In the sidebar open **NeptHass Dev**: that dashboard lets you drive every virtual device (see [virtual-devices.md](virtual-devices.md)).
5. Create the token NeptHass will use: click your profile (bottom of the sidebar), open the **Security** tab, scroll to **Long-lived access tokens**, **Create token**, name it `nepthass-dev`, and copy it immediately (it is shown once).
6. Put it in `.env.dev`:

   ```text
   HA_TOKEN=<the copied token>
   ```

Check that the simulation loaded: **Settings > Devices & services > Entities**, search `nepthass_dev`. You should see the light, the switch, nine sensors, the helpers and the scenario scripts. If something is missing, see [Troubleshooting](#troubleshooting) below.

## 4. Create the Discord test bot

Use a **separate** Discord application and a **private test server** for development. Never reuse a production bot token.

1. Create the server: in Discord click **+ Add a Server > Create My Own > For me and my friends**. Name it, for example, `NeptHass Dev`.
2. Open the [Developer Portal](https://discord.com/developers/applications) and click **New Application** (name: `NeptHass Dev`).
3. **General Information:** copy the **Application ID** into `DISCORD_APPLICATION_ID`.
4. **Bot:** click **Reset Token**, copy the token into `DISCORD_BOT_TOKEN` in `.env.dev` (it is shown once). Leave all Privileged Gateway Intents **off**; NeptHass needs none. Under **Bot** you can turn **Public Bot** off so nobody else can invite it.
5. **Invite the bot** to your test server. Open this URL in a browser, replacing `APPLICATION_ID`:

   ```text
   https://discord.com/oauth2/authorize?client_id=APPLICATION_ID&scope=bot%20applications.commands&permissions=0
   ```

   Pick your test server and authorize. NeptHass needs no channel permissions in v0.1.0 because all replies are ephemeral.
6. **IDs.** In Discord enable **User Settings > Advanced > Developer Mode**. Right-click your test server and choose **Copy Server ID** (`DISCORD_GUILD_ID`). Right-click your own name and choose **Copy User ID** (`DISCORD_ADMIN_USER_IDS`).
7. **Slash-command permissions (Discord side).** By default every member of the server can see and run the commands. NeptHass does its own authorization, so anyone not on its allowlists is refused, but for tighter control restrict visibility too: **Server Settings > Integrations > NeptHass Dev > Manage**, then limit the commands to your role or a private channel. This is defense in depth, never a replacement for the NeptHass allowlists.

Your `.env.dev` should now contain: `DISCORD_BOT_TOKEN`, `DISCORD_APPLICATION_ID`, `DISCORD_GUILD_ID`, `DISCORD_ADMIN_USER_IDS` and `HA_TOKEN`. Leave `NEPTHASS_DEV_HA_URL=http://homeassistant:8123`.

### Storing secrets safely

- Secrets belong only in `.env.dev`, which is git-ignored (check with `git check-ignore -v .env.dev`). Never put them in source files, compose files, docs or screenshots.
- On Linux/macOS run `chmod 600 .env.dev`. On Windows keep the repository in your user profile (default ACLs already restrict it to you).
- If a token leaks, reset it right away: Discord Developer Portal > Bot > Reset Token; Home Assistant profile > Long-lived access tokens > delete.
- NeptHass also supports `DISCORD_BOT_TOKEN_FILE` / `HA_TOKEN_FILE` if you prefer Docker secrets; see [../setup.md](../setup.md).

### A second identity for unauthorized-access tests

To prove strangers are refused you need someone who is **not** on the allowlists. Either:

- use a second Discord account (a friend or an alt) that is a member of your test server, or
- test alone: temporarily set `DISCORD_ADMIN_USER_IDS` to a different, valid-looking ID (for example `100000000000000009`), restart NeptHass, and your own account becomes the stranger. Restore the value afterwards.

To test a regular user (not an administrator), move your own ID from `DISCORD_ADMIN_USER_IDS` to `DISCORD_ALLOWED_USER_IDS` (and put some other ID in the admin list, which must not be empty), then restart NeptHass.

## 5. Start NeptHass

```bash
docker compose --env-file .env.dev -f compose.dev.yaml --profile app up -d --build
docker compose --env-file .env.dev -f compose.dev.yaml logs -f nepthass
```

A healthy start logs, in order: `authenticated with home assistant`, `connected to discord gateway`, `slash commands registered`. Guild commands appear in Discord immediately (reload the client with Ctrl+R if they do not).

If NeptHass exits at once, read the log: it lists every invalid or missing setting by name (values are never printed). A rejected Discord token or Home Assistant token stops the process on purpose.

## Everyday commands

| Goal | Command |
| --- | --- |
| Start Home Assistant only | `docker compose --env-file .env.dev -f compose.dev.yaml up -d` |
| Start Home Assistant and NeptHass | `docker compose --env-file .env.dev -f compose.dev.yaml --profile app up -d --build` |
| Status | `docker compose --env-file .env.dev -f compose.dev.yaml --profile app ps` |
| Logs | `docker compose --env-file .env.dev -f compose.dev.yaml logs -f nepthass` |
| Restart NeptHass (after editing `.env.dev`) | `docker compose --env-file .env.dev -f compose.dev.yaml --profile app up -d --force-recreate nepthass` |
| Rebuild NeptHass after code changes | `docker compose --env-file .env.dev -f compose.dev.yaml --profile app up -d --build nepthass` |
| Restart Home Assistant (after editing `dev/homeassistant`) | `docker compose --env-file .env.dev -f compose.dev.yaml restart homeassistant` |
| **Stop** everything, keep data | `docker compose --env-file .env.dev -f compose.dev.yaml --profile app stop` |
| Remove containers and network, keep data | `docker compose --env-file .env.dev -f compose.dev.yaml --profile app down` |

Home Assistant keeps its users, tokens and history in the `ha-config` volume and NeptHass keeps its allowlist and audit log in `nepthass-data`, so both survive `stop` and `down`.

## Resetting only development data

These commands only ever touch the `nepthass-dev` project; other containers, networks and volumes are not affected.

```bash
# Wipe everything of this project (containers, network and both volumes):
docker compose --env-file .env.dev -f compose.dev.yaml --profile app --profile test down -v

# Wipe only NeptHass's database (allowlist + audit log), keep Home Assistant:
docker compose --env-file .env.dev -f compose.dev.yaml --profile app rm -sf nepthass
docker volume rm nepthass-dev_nepthass-data

# Wipe only Home Assistant (you must onboard again and create a new token):
docker compose --env-file .env.dev -f compose.dev.yaml rm -sf homeassistant
docker volume rm nepthass-dev_ha-config
```

List what belongs to the project before deleting anything:

```bash
docker volume ls --filter label=com.docker.compose.project=nepthass-dev
```

To return only the **simulation** to its baseline without losing Home Assistant data, run the `NeptHass Dev: reset to baseline` script from the dashboard.

## Trying the setup wizard against the virtual devices

With the stack running (`--profile app`), run `/setup` in your test server. The virtual light, switch, motion, door, window, temperature, humidity, water-leak and battery entities show up in discovery under the name "NeptHass Dev ...", with the states described in [virtual-devices.md](virtual-devices.md). Use the `NeptHass Dev` scenarios (for example "all sensors offline") to see unavailable states in the wizard. The step-by-step manual checklist is group G of [smoke-tests.md](smoke-tests.md).

## Trying live monitoring (v0.3.0)

With the stack running (`--profile app`), `/health` should show "Healthy: N sensors tracked" on the "Home Assistant WebSocket (live monitoring)" line, with all four layers ticked. Share the nine virtual sensors (`/setup`), then run `/sensors` and trigger scenarios in Home Assistant as described in [virtual-devices.md](virtual-devices.md#using-them-with-live-monitoring-v030). To see recovery, restart only Home Assistant (`docker compose --env-file .env.dev -f compose.dev.yaml restart homeassistant`) and watch `/health` go to reconnecting and back. The checklist is group H of [smoke-tests.md](smoke-tests.md).

## Trying notifications (v0.4.0)

With the stack running (`--profile app`) and the virtual sensors shared, create a **private** text channel in your test server, give the bot View Channel, Send Messages and Embed Links there, and run `/notifications channel`. Add rules for the virtual sensors with `/notifications add` (for example the door, the water leak, and the temperature with `above:30`), then trigger the scenarios in Home Assistant as described in [virtual-devices.md](virtual-devices.md#triggers-for-notifications-and-rules). To exercise monitoring-down notices, stop only Home Assistant for more than five minutes (see the commands above). The manual checklist is group I of [smoke-tests.md](smoke-tests.md).

## Running the Go tests

Unit tests need no Docker Home Assistant and no Discord:

```bash
go test -race -shuffle=on -count=1 ./...
# without a local Go toolchain (in Git Bash on Windows prefix with MSYS_NO_PATHCONV=1, see Troubleshooting below):
docker run --rm -v "$PWD":/src -w /src golang:1.27 go test -race -shuffle=on -count=1 ./...
```

The integration tests against the real development Home Assistant (still no Discord) are described in [smoke-tests.md](smoke-tests.md).

## Networking notes

- Inside Compose, NeptHass uses `http://homeassistant:8123`. To point it somewhere else, set `NEPTHASS_DEV_HA_URL` in `.env.dev`. Do not set `HA_URL` there: the Compose file derives it from `NEPTHASS_DEV_HA_URL`.
- To run the NeptHass binary directly on your host against the dev Home Assistant, use `HA_URL=http://127.0.0.1:18123` (the published localhost port).
- Change the published port with `HA_DEV_PORT` in `.env.dev` if 18123 is taken.

## Safety summary

- Home Assistant UI: `127.0.0.1` only. No other port is published.
- Dev data lives in project-scoped volumes; `down -v` cannot touch other projects.
- The dev Home Assistant has virtual devices only; there is no path from this stack to real hardware.
- Credentials: `.env.dev` only, never committed.
- This stack is for development. For a real installation use the root `compose.yaml` and [../setup.md](../setup.md).

## Troubleshooting

Check the logs first:

```bash
docker compose --env-file .env.dev -f compose.dev.yaml --profile app ps
docker compose --env-file .env.dev -f compose.dev.yaml logs --tail=100 homeassistant
docker compose --env-file .env.dev -f compose.dev.yaml logs --tail=100 nepthass
```

### Docker and Compose

**`env file .env.dev not found` or variables are empty.** Copy `.env.dev.example` to `.env.dev` and pass `--env-file .env.dev` to every `docker compose` command.

**`unknown field "required"` or similar when running Compose.** Your Compose is older than v2.24. Update Docker Desktop or the Compose plugin (`docker compose version`).

**Git Bash on Windows turns `/some/path` arguments into `C:/Program Files/Git/some/path`** (for example `ls: C:/Program Files/Git/config/packages: No such file or directory`, or `the working directory 'C:/Program Files/Git/src' is invalid`). Prefix the command with `MSYS_NO_PATHCONV=1`, for example `MSYS_NO_PATHCONV=1 docker compose --env-file .env.dev -f compose.dev.yaml exec homeassistant ls /config/packages`, or run the command from PowerShell, which does not rewrite paths.

**`port is already allocated` for 18123.** Another program uses it. Set a different `HA_DEV_PORT` in `.env.dev` and recreate: `docker compose --env-file .env.dev -f compose.dev.yaml up -d`.

**The Docker engine does not respond (`docker version` hangs).** This is a Docker Desktop problem, not NeptHass. Restart Docker Desktop from its tray icon, wait until it says it is running, and try again. Restarting Docker Desktop stops all your containers temporarily, so do it deliberately.

**Other containers or volumes were affected.** They should not be: the project is named `nepthass-dev`, no container has a fixed name, and every volume is prefixed `nepthass-dev_`. List what belongs to the project with `docker volume ls --filter label=com.docker.compose.project=nepthass-dev`.

### Home Assistant

**It never becomes healthy.** The first start can take three minutes while the image is pulled and the configuration loads. Read `logs homeassistant`. A YAML mistake shows up as `Invalid config` near the top of the log; fix the file under `dev/homeassistant` and restart Home Assistant.

**The image cannot be pulled.** Check your internet connection and that `ghcr.io` is reachable. The default image is pinned in `compose.dev.yaml`; override it with `HA_IMAGE` in `.env.dev` to use another version (the template syntax in `dev/homeassistant` targets 2026.10).

**The `nepthass_dev_*` entities are missing.** Open **Settings > System > Logs** or the container log and look for `template` or `Invalid config` errors. Confirm the mounts: `docker compose --env-file .env.dev -f compose.dev.yaml exec homeassistant ls /config/packages` should list four YAML files (in Git Bash prefix it with `MSYS_NO_PATHCONV=1`, see above). After editing the files, restart Home Assistant.

**Entities have the wrong ID (for example a `_2` suffix).** Home Assistant remembers entities by `unique_id` in its own volume. Reset only Home Assistant's data (see "Resetting only development data" above).

**I forgot the dev Home Assistant password.** Reset only Home Assistant's data and onboard again; you then need a new token for `.env.dev`.

**Temperatures show `°F` (for example `70.7` instead of `21.5`) or the integration tests report a non-metric instance.** The onboarding UI switched the instance to US customary units and Home Assistant converts temperature sensors accordingly. Run the `NeptHass Dev: reset to baseline` script (it re-applies the metric system from `configuration.yaml`), or restart Home Assistant.

**The `NeptHass Dev` dashboard is missing.** Hard-refresh the browser (Ctrl+Shift+R). It is a YAML dashboard defined in `configuration.yaml`; check the Home Assistant log for `lovelace` errors.

### NeptHass container

**`invalid configuration:` and a list of keys.** Fill in the named values in `.env.dev`, then recreate: `docker compose --env-file .env.dev -f compose.dev.yaml --profile app up -d --force-recreate nepthass`. Values are never printed.

**`home assistant is not reachable yet` in the log.**

- NeptHass must use the Compose service name, not `127.0.0.1`: inside its container `127.0.0.1` is NeptHass itself. The default `NEPTHASS_DEV_HA_URL=http://homeassistant:8123` is right; check it was not overridden.
- Do not set `HA_URL` in `.env.dev`; the Compose file derives it from `NEPTHASS_DEV_HA_URL`.
- Home Assistant may still be starting. NeptHass keeps running and uses it once it answers; `/health` shows the current state.
- If NeptHass started before Home Assistant was ready and still reports it unreachable, confirm `ps` shows Home Assistant as `healthy`, then recreate NeptHass with `up -d --force-recreate nepthass`. The log line `authenticated with home assistant` confirms container-to-container connectivity.

**`home assistant rejected HA_TOKEN`.** The token is wrong, was deleted, or came from a different Home Assistant instance (for example after you reset its data). Create a new Long-Lived Access Token and update `.env.dev`.

**`discord rejected the bot token`.** The token is wrong or was reset. Reset it in the Developer Portal (Bot > Reset Token) and update `.env.dev`.

**Slash commands do not appear in Discord.** The bot must be in your test server and invited with the `applications.commands` scope (see "Create the Discord test bot" above). `DISCORD_GUILD_ID` must be that server. Reload the Discord client with Ctrl+R. The NeptHass log shows `slash commands registered` when registration succeeded, or a retry warning with a hint when Discord refused.

**Every command says `You are not authorized to use NeptHass.`** Your user ID is not in `DISCORD_ADMIN_USER_IDS` or `DISCORD_ALLOWED_USER_IDS`, or you are using the command in a different server. Copy your ID again (Developer Mode > right-click your name > Copy User ID). After editing `.env.dev`, recreate NeptHass; a plain `restart` does not re-read it.

**`The application did not respond`.** The bot is probably not connected. Check the log for `connected to discord gateway`, and that your machine can reach `discord.com` and `gateway.discord.gg` on port 443 (some networks and VPNs block it).

**Permission or database errors on `/data`.** The volume is created with the right owner automatically. If you replaced it with a bind mount, `chown` it to UID 65532.

### Integration tests

**`integration tests need HA_URL and HA_TOKEN`.** Put `HA_TOKEN` in `.env.dev` (see "Create your dev env file" above). When running from the host also set `HA_URL=http://127.0.0.1:18123`.

**`this does not look like the NeptHass development Home Assistant`.** The target does not contain `script.nepthass_dev_scenario_reset`. The tests refuse to run against any other Home Assistant on purpose. Check that Home Assistant loaded the simulation (see "entities are missing" above).

**`Home Assistant rejected HA_TOKEN`.** The token belongs to a different Home Assistant instance or was deleted.

**The first run is slow.** The Go container downloads modules into the `go-mod-cache` volume once. Later runs reuse it.

**A state wait times out.** Home Assistant is overloaded or a script failed. Look at `logs homeassistant` for script or template errors and run the reset script by hand.

### Live monitoring checks

- **`/health` shows reconnecting after `restart homeassistant`.** Expected. Home Assistant takes roughly 30 to 90 seconds to come back; NeptHass retries (1 s growing to 60 s) and resynchronizes by itself. If it does not recover, read the NeptHass log for the failure category.
- **The WebSocket line says the token was rejected but REST commands work.** The token was revoked or replaced after NeptHass started. Update `HA_TOKEN` in `.env.dev` and `up -d`.
- **The monitoring integration tests fail with "condition not reached".** They wait (without polling) for real events; make sure the simulation is at baseline (`NeptHass Dev: reset to baseline`) and that Home Assistant is healthy. They never touch your running dev stack's containers: the outage test uses an in-test TCP proxy.

### Still stuck

Collect the version (`git rev-parse --short HEAD`), `docker compose version`, and the relevant log lines. Redact tokens, IDs and anything private, then open an issue. For security problems use [SECURITY.md](../../SECURITY.md) instead.
