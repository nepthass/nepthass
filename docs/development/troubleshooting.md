# Development environment troubleshooting

Check the logs first:

```bash
docker compose --env-file .env.dev -f compose.dev.yaml --profile app ps
docker compose --env-file .env.dev -f compose.dev.yaml logs --tail=100 homeassistant
docker compose --env-file .env.dev -f compose.dev.yaml logs --tail=100 nepthass
```

## Docker and Compose

**`env file .env.dev not found` or variables are empty.** Copy `.env.dev.example` to `.env.dev` and pass `--env-file .env.dev` to every `docker compose` command.

**`unknown field "required"` or similar when running Compose.** Your Compose is older than v2.24. Update Docker Desktop or the Compose plugin (`docker compose version`).

**Git Bash on Windows turns `/some/path` arguments into `C:/Program Files/Git/some/path`** (for example `ls: C:/Program Files/Git/config/packages: No such file or directory`, or `the working directory 'C:/Program Files/Git/src' is invalid`). Prefix the command with `MSYS_NO_PATHCONV=1`, for example `MSYS_NO_PATHCONV=1 docker compose --env-file .env.dev -f compose.dev.yaml exec homeassistant ls /config/packages`, or run the command from PowerShell, which does not rewrite paths.

**`port is already allocated` for 18123.** Another program uses it. Set a different `HA_DEV_PORT` in `.env.dev` and recreate: `docker compose --env-file .env.dev -f compose.dev.yaml up -d`.

**The Docker engine does not respond (`docker version` hangs).** This is a Docker Desktop problem, not NeptHass. Restart Docker Desktop from its tray icon, wait until it says it is running, and try again. Restarting Docker Desktop stops all your containers temporarily, so do it deliberately.

**Other containers or volumes were affected.** They should not be: the project is named `nepthass-dev`, no container has a fixed name, and every volume is prefixed `nepthass-dev_`. List what belongs to the project with `docker volume ls --filter label=com.docker.compose.project=nepthass-dev`.

## Home Assistant

**It never becomes healthy.** The first start can take three minutes while the image is pulled and the configuration loads. Read `logs homeassistant`. A YAML mistake shows up as `Invalid config` near the top of the log; fix the file under `dev/homeassistant` and restart Home Assistant.

**The image cannot be pulled.** Check your internet connection and that `ghcr.io` is reachable. The default image is pinned in `compose.dev.yaml`; override it with `HA_IMAGE` in `.env.dev` to use another version (the template syntax in `dev/homeassistant` targets 2026.10).

**The `nepthass_dev_*` entities are missing.** Open **Settings > System > Logs** or the container log and look for `template` or `Invalid config` errors. Confirm the mounts: `docker compose --env-file .env.dev -f compose.dev.yaml exec homeassistant ls /config/packages` should list four YAML files (in Git Bash prefix it with `MSYS_NO_PATHCONV=1`, see above). After editing the files, restart Home Assistant.

**Entities have the wrong ID (for example a `_2` suffix).** Home Assistant remembers entities by `unique_id` in its own volume. Reset only Home Assistant's data (see [setup.md](setup.md), "Resetting only development data").

**I forgot the dev Home Assistant password.** Reset only Home Assistant's data and onboard again; you then need a new token for `.env.dev`.

**Temperatures show `°F` (for example `70.7` instead of `21.5`) or the integration tests report a non-metric instance.** The onboarding UI switched the instance to US customary units and Home Assistant converts temperature sensors accordingly. Run the `NeptHass Dev: reset to baseline` script (it re-applies the metric system from `configuration.yaml`), or restart Home Assistant.

**The `NeptHass Dev` dashboard is missing.** Hard-refresh the browser (Ctrl+Shift+R). It is a YAML dashboard defined in `configuration.yaml`; check the Home Assistant log for `lovelace` errors.

## NeptHass container

**`invalid configuration:` and a list of keys.** Fill in the named values in `.env.dev`, then recreate: `docker compose --env-file .env.dev -f compose.dev.yaml --profile app up -d --force-recreate nepthass`. Values are never printed.

**`home assistant is not reachable yet` in the log.**

- NeptHass must use the Compose service name, not `127.0.0.1`: inside its container `127.0.0.1` is NeptHass itself. The default `NEPTHASS_DEV_HA_URL=http://homeassistant:8123` is right; check it was not overridden.
- Do not set `HA_URL` in `.env.dev`; the Compose file derives it from `NEPTHASS_DEV_HA_URL`.
- Home Assistant may still be starting. NeptHass keeps running and uses it once it answers; `/health` shows the current state.
- If NeptHass started before Home Assistant was ready and still reports it unreachable, confirm `ps` shows Home Assistant as `healthy`, then recreate NeptHass with `up -d --force-recreate nepthass`. The log line `authenticated with home assistant` confirms container-to-container connectivity.

**`home assistant rejected HA_TOKEN`.** The token is wrong, was deleted, or came from a different Home Assistant instance (for example after you reset its data). Create a new Long-Lived Access Token and update `.env.dev`.

**`discord rejected the bot token`.** The token is wrong or was reset. Reset it in the Developer Portal (Bot > Reset Token) and update `.env.dev`.

**Slash commands do not appear in Discord.** The bot must be in your test server and invited with the `applications.commands` scope (see the invite URL in [setup.md](setup.md)). `DISCORD_GUILD_ID` must be that server. Reload the Discord client with Ctrl+R. The NeptHass log shows `slash commands registered` when registration succeeded, or a retry warning with a hint when Discord refused.

**Every command says `You are not authorized to use NeptHass.`** Your user ID is not in `DISCORD_ADMIN_USER_IDS` or `DISCORD_ALLOWED_USER_IDS`, or you are using the command in a different server. Copy your ID again (Developer Mode > right-click your name > Copy User ID). After editing `.env.dev`, recreate NeptHass; a plain `restart` does not re-read it.

**`The application did not respond`.** The bot is probably not connected. Check the log for `connected to discord gateway`, and that your machine can reach `discord.com` and `gateway.discord.gg` on port 443 (some networks and VPNs block it).

**Permission or database errors on `/data`.** The volume is created with the right owner automatically. If you replaced it with a bind mount, `chown` it to UID 65532.

## Integration tests

**`integration tests need HA_URL and HA_TOKEN`.** Put `HA_TOKEN` in `.env.dev` (see [setup.md](setup.md)). When running from the host also set `HA_URL=http://127.0.0.1:18123`.

**`this does not look like the NeptHass development Home Assistant`.** The target does not contain `script.nepthass_dev_scenario_reset`. The tests refuse to run against any other Home Assistant on purpose. Check that Home Assistant loaded the simulation (see "entities are missing" above).

**`Home Assistant rejected HA_TOKEN`.** The token belongs to a different Home Assistant instance or was deleted.

**The first run is slow.** The Go container downloads modules into the `go-mod-cache` volume once. Later runs reuse it.

**A state wait times out.** Home Assistant is overloaded or a script failed. Look at `logs homeassistant` for script or template errors and run the reset script by hand.

## Still stuck

Collect the version (`git rev-parse --short HEAD`), `docker compose version`, and the relevant log lines. Redact tokens, IDs and anything private, then open an issue. For security problems use [SECURITY.md](../../SECURITY.md) instead.
