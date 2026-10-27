# Troubleshooting

Check the logs first: `docker compose logs --tail=100 nepthass`. Set `LOG_LEVEL=debug` for more detail. Secrets are never logged.

## NeptHass exits immediately

**`invalid configuration:` followed by a list.** Every invalid or missing setting is listed by name (values are never printed). Fix them in `.env`. IDs are numeric (17 to 20 digits); tokens must not contain spaces or a `Bot ` prefix; `HA_URL` must be an `http(s)` URL without credentials.

**`discord rejected the bot token`.** The token is wrong, was reset, or has a typo. Reset it in the Developer Portal and update `.env`.

**`home assistant rejected HA_TOKEN`.** The Long-Lived Access Token is wrong, expired or was deleted. Create a new one and update `.env`.

**`create database directory` / permission denied.** The container runs as UID 65532. A bind-mounted data directory must be owned by it: `sudo chown -R 65532:65532 ./data`.

**`database schema is newer than this build supports`.** The database was written by a newer NeptHass. Upgrade, or restore a backup taken with this version.

**`applied migration was modified`.** A released migration file was edited. Restore the original file from the repository.

## Slash commands do not appear

- The bot must be in the server and invited with the `applications.commands` scope (see [setup.md](setup.md)). The log shows `register slash commands` warnings with a hint when Discord refuses.
- `DISCORD_GUILD_ID` must be the server you are looking at.
- With `DISCORD_COMMAND_SCOPE=global`, commands can take up to an hour to propagate. Use `guild` while developing.
- Reload the Discord client (Ctrl+R) after the first registration.

## "The application did not respond" or "Interaction failed"

- Is the container running and connected? `/ping` works only while the gateway is up. Look for `connected to discord gateway`.
- A host clock that is far off breaks TLS and the gateway. Check NTP.
- Outbound access to `discord.com` and `gateway.discord.gg` on port 443 must be allowed.

## "You are not authorized to use NeptHass"

Your Discord user ID is not in `DISCORD_ADMIN_USER_IDS` or `DISCORD_ALLOWED_USER_IDS`, or you are using the command in a different server or a direct message. After editing `.env`, recreate the container: `docker compose up -d`.

## "You do not have access to that entity"

The entity has not been shared with the required level. An administrator runs `/permissions allow entity:... access:read|control`. Remember that `control` is possible only for lights and switches.

## "Home Assistant is not reachable right now"

- With the default host networking, `HA_URL=http://127.0.0.1:8123` must reach Home Assistant from the host. Test: `curl -i http://127.0.0.1:8123/api/` (a 401 means it is reachable).
- On bridge networking `127.0.0.1` is the container itself. Use `host.docker.internal` or the Home Assistant container name (see [setup.md](setup.md)).
- Docker Desktop on Windows and macOS does not support Linux host networking the same way; use bridge networking.
- Raise `HA_REQUEST_TIMEOUT` if Home Assistant is slow.

## `/health` shows Home Assistant "Access token rejected"

Same as the `HA_TOKEN` error above, but Home Assistant started rejecting the token after NeptHass was running. Create a new token and restart.

## "Home Assistant does not know that entity"

The entity ID is misspelled or the entity was removed or renamed. Use the autocomplete suggestions of `/permissions allow`. A shared entity that disappeared from Home Assistant shows as "not found in Home Assistant" in `/devices`; remove it with `/permissions revoke`.

## "You are sending commands too quickly"

The limit is 6 commands per 10 seconds per user. Wait a few seconds.

## A light did not change state

`/control` reports that Home Assistant accepted the command; devices (especially Zigbee) can take a moment to report back. Run `/status` again. If the device never responds, check it in Home Assistant itself; NeptHass only forwards commands.

## Still stuck

Open an issue with the NeptHass version (`docker run --rm nepthass:latest version`), the relevant log lines, and your deployment type. Redact tokens, IDs you consider private, and entity names. For security problems use [SECURITY.md](../SECURITY.md) instead.
