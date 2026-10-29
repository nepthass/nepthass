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

## /setup problems

**"This setup panel is out of date" / "That panel was out of date, so it was refreshed."** You clicked a button from an older copy of the panel (another window, or after another click changed the session). Nothing was changed; use the refreshed panel.

**"Home Assistant is not reachable right now" inside the wizard.** The wizard never continues on stale data. Make sure Home Assistant is running, then press **Retry**. Your progress is saved.

**"Setup expired".** A session expires after 24 hours without activity. Run `/setup` again.

**"Setup already applied".** The confirmation was pressed twice (or replayed); the second one wrote nothing. Check `/devices`.

**"These entities were changed outside this setup".** Someone changed the same entity (for example with `/permissions`) while you were reviewing. Nothing was applied. Review the updated differences and confirm again.

**"You are not authorized to use NeptHass setup".** Only administrators (`DISCORD_ADMIN_USER_IDS`) can use the wizard, and only the administrator who started a session can use its buttons.

**An entity is missing from the list.** Press **Refresh**, check the domain filter and the search words, and remember entities with IDs longer than 100 characters cannot be offered (the panel says how many). Use `/sync` to see what changed in Home Assistant.

**Control is greyed out for an entity.** Only lights and switches can be controlled; everything else is read-only by design.

**An alias is rejected.** Use letters, numbers, spaces and `- ' . ( )`, at most 32 characters, not shaped like an entity ID, and not already used (the message names the other entity).

## "You are sending commands too quickly"

The limit is 6 commands per 10 seconds per user (setup wizard clicks have their own, more generous limit of 30 per 10 seconds). Wait a few seconds.

## A light did not change state

`/control` reports that Home Assistant accepted the command; devices (especially Zigbee) can take a moment to report back. Run `/status` again. If the device never responds, check it in Home Assistant itself; NeptHass only forwards commands.

## Live monitoring problems

`/health` shows the state of live monitoring on its "Home Assistant WebSocket (live monitoring)" line. Commands that use REST (`/devices`, `/status`, `/control`) work even when monitoring is down.

- **"Reconnecting" instead of "Starting".** "Starting" appears only on the first start. After any failure the same stages read "Reconnecting", with the previous failure category, so a retry is never mistaken for a fresh start. The next retry time is shown as a clock time.
- **"Cannot reach Home Assistant's WebSocket; retrying".** The WebSocket cannot be opened or was lost. Check that Home Assistant is running and reachable at `HA_URL` from the NeptHass container (see the networking notes in [setup.md](setup.md)), and that a reverse proxy forwards WebSocket upgrades on `/api/websocket`. NeptHass retries on its own (1 s growing to 60 s) and resynchronizes when it succeeds.
- **"Home Assistant rejected the access token for the WebSocket".** The token was revoked or replaced after start. Create a new Long-Lived Access Token, update `HA_TOKEN` (or its file) and restart. Retries are deliberately slow (30 s up to 10 minutes) so a bad token never hammers Home Assistant.
- **"Unexpected response from Home Assistant's WebSocket".** A proxy or an unsupported server answered in a way NeptHass could not use. Look for messages over 16 MiB (a very large installation) or a proxy that rewrites WebSocket frames.
- **"Connected but silent".** The connection is open but nothing arrived within 90 seconds, even the ping replies. NeptHass closes and reconnects by itself; if it persists, check the network path.
- **"Starting (synchronizing)" or "Reconnecting (synchronizing)" that keeps coming back.** The snapshot is being fetched. If Home Assistant does not answer within 60 seconds (or the subscription within 15 seconds) NeptHass drops the connection and retries with backoff instead of waiting forever. If it repeats, the snapshot is probably too large (over 16 MiB) or Home Assistant is overloaded.
- **`/sensors` says sensor readings are not available yet.** Live monitoring has not synchronized with Home Assistant yet: it is still starting, cannot reach Home Assistant (check `/health`), cannot sign in (check `HA_TOKEN`), or is not running. This message says nothing about what is shared with you. It resolves by itself once `/health` shows Healthy.
- **`/sensors` says no monitored sensors are shared with you.** Monitoring is synchronized and none of the sensors shared with you is a monitored kind: temperature, humidity, battery, motion, door, window, water leak, identified by their Home Assistant device class. Share one with `/setup` or `/permissions allow`. A sensor without a device class is not tracked.
- **A sensor looks "stale".** Every line gets a warning icon and a banner while monitoring is not healthy. The values are the last known ones, not current.
- **The log.** Look for `home assistant websocket authenticated`, `event subscription active`, `initial synchronization completed` and `connection lost; reconnect scheduled` (with the failure category and delay). Raw payloads and tokens are never logged.

## Still stuck

Open an issue with the NeptHass version (`docker run --rm nepthass:latest version`), the relevant log lines, and your deployment type. Redact tokens, IDs you consider private, and entity names. For security problems use [SECURITY.md](../SECURITY.md) instead.
