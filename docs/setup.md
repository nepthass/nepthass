# Setup

This guide takes a Linux machine that already runs Home Assistant to a working NeptHass. Routine access management happens in Discord; only the bootstrap credentials are configured locally.

## 1. Create the Discord application

1. Open the [Discord Developer Portal](https://discord.com/developers/applications) and create a **New Application**.
2. Copy the **Application ID** (General Information).
3. Under **Bot**, create the bot and **Reset Token**. Copy it once; it is your `DISCORD_BOT_TOKEN`. NeptHass needs no privileged intents.
4. Invite the bot to your server with this URL (replace `APPLICATION_ID`). The `applications.commands` scope is required for slash commands; NeptHass needs no channel permissions in v0.1.0 because every reply is ephemeral:

   `https://discord.com/oauth2/authorize?client_id=APPLICATION_ID&scope=bot%20applications.commands&permissions=0`

5. In Discord, enable **Settings > Advanced > Developer Mode**. Right-click your server and **Copy Server ID** (`DISCORD_GUILD_ID`); right-click your own name and **Copy User ID** (`DISCORD_ADMIN_USER_IDS`).

## 2. Create a Home Assistant token

In Home Assistant open your profile, **Security**, **Long-Lived Access Tokens**, **Create Token**. Copy it (`HA_TOKEN`). The token carries the permissions of the user that created it; consider a dedicated user for NeptHass (see [security.md](security.md)).

## 3. Configure

```bash
git clone https://github.com/nepthass/nepthass.git
cd nepthass
cp .env.example .env
chmod 600 .env
```

Fill in the five required values. Run `docker compose config` only if you do not mind secrets being printed: it expands `.env`.

### Keeping secrets out of the environment (optional)

Every secret also accepts a `_FILE` variant, for example Docker secrets:

```yaml
services:
  nepthass:
    environment:
      DISCORD_BOT_TOKEN_FILE: /run/secrets/discord_token
      HA_TOKEN_FILE: /run/secrets/ha_token
    secrets: [discord_token, ha_token]
secrets:
  discord_token: { file: ./secrets/discord_token }
  ha_token: { file: ./secrets/ha_token }
```

Set either `NAME` or `NAME_FILE`, not both.

## 4. Start

```bash
docker compose up -d --build
docker compose logs -f nepthass
```

A healthy start logs `authenticated with home assistant`, `connected to discord gateway` and `slash commands registered`. Guild-scoped commands appear in Discord immediately.

## 5. Configure access with /setup

Run `/setup` (administrators only). The wizard is private to you:

1. **Connection health.** Discord, Home Assistant REST and SQLite are checked live. **Next** stays disabled until Home Assistant answers; setup never uses stale or invented entity data. (The Home Assistant WebSocket is not available in this release.)
2. **Discover devices.** Real entities with name, ID, state, device class and current sharing. Use **Search...** (words from the name, ID or device class), the domain menu (including **Shared only**), **Refresh** and the page buttons. Tick the entities you want to configure in the menu and press **Next**. Large homes work: the list is paged ten at a time.
3. **Device permissions.** Pick an entity, then **Read**, **Control** or **No access**. Control is offered for lights and switches only. **All: read** and **Lights and switches: control** apply a rule to everything selected. Newly selected entities start as read-only.
4. **Aliases.** Optionally give entities friendly names (letters, numbers, spaces and `- ' . ( )`, up to 32 characters, unique across NeptHass). They work in `/status`, `/control` and show in `/devices`. An alias never grants access.
5. **Review.** See exactly what is newly shared, modified or revoked, and which aliases change. Nothing has been written yet. **Confirm and apply** saves everything at once; it is disabled when there are blocking problems or nothing to change.
6. **Done.** The configuration is active immediately; no restart is needed. Try `/devices`, `/status` and `/control`.

**Interrupted setup.** Progress is saved. Run `/setup` again to **Resume** (or **Start over**), even after NeptHass restarted. A session expires after 24 hours of inactivity. **Cancel setup** applies nothing.

**Keeping things in sync.** `/sync` refreshes NeptHass's view of Home Assistant and tells you which entities are new, removed or unavailable, including shared ones. It never shares anything.

## 6. First commands

1. `/ping` and `/health` confirm the connection.
2. Share devices with `/setup` (above), or one at a time with `/permissions allow entity:light.living_room access:control` (autocomplete from Home Assistant). Only lights and switches can be granted `control`; any entity can be granted `read`.
3. `/devices`, `/status entity:...`, `/control entity:... action:on` (aliases work too).
4. `/audit` shows what happened. `/permissions list` and `/permissions revoke` manage the allowlist.

Other people can use NeptHass once their user ID is in `DISCORD_ALLOWED_USER_IDS` (or `DISCORD_ADMIN_USER_IDS`); restart the container after changing it. They see only what an administrator shared.

## 7. Optional: alerts

Notifications are **disabled until an administrator sets a channel**, so nothing changes until you opt in.

1. Create or choose a **private** text channel. Everyone who can read it will see the alerts, including sensor names and states.
2. Give the bot **View Channel**, **Send Messages** and **Embed Links** in that channel (check the channel's own permission overwrites as well as the server roles). The invite link above grants no permissions, so this is a manual step.
3. Run `/notifications channel` and pick the channel. NeptHass checks that it is a text channel of your server and that the bot can post there.
4. Share the sensors you want alerts for (`/setup` or `/permissions allow`), then add rules with `/notifications add`. Send a test with `/notifications test`.

Alerts are best-effort (10 minute staleness bound, possible duplicates), queued alerts are lost if NeptHass restarts, and a sensor that is already in its alert state when NeptHass starts is not announced. See [commands.md](commands.md#notifications) and [design/notifications.md](design/notifications.md).

## Networking

The default `compose.yaml` uses host networking so `HA_URL=http://127.0.0.1:8123` reaches Home Assistant running on the same host (Linux only). NeptHass opens no ports.

**Bridge networking** (for example Docker Desktop, or Home Assistant on a user-defined network): remove `network_mode: host` and point `HA_URL` at Home Assistant:

```yaml
services:
  nepthass:
    # network_mode: host   <- removed
    extra_hosts:
      - "host.docker.internal:host-gateway"   # HA published on the host
    # or join Home Assistant's network and use its container name:
    # networks: [homeassistant_default]
```

then `HA_URL=http://host.docker.internal:8123` (or `http://homeassistant:8123`). If Home Assistant sits behind a reverse proxy with TLS, use its `https://` URL; NeptHass verifies certificates normally and never follows redirects.

A new machine with no Home Assistant yet can use the optional combined stack in [deploy/compose.with-homeassistant.yaml](../deploy/compose.with-homeassistant.yaml). Never use it on an existing installation.

## Data, retention and backups

All state lives in one SQLite database at `DATABASE_PATH` (`/data/nepthass.db` in the container, on the `nepthass-data` volume). It holds the entity allowlist, aliases, setup sessions, the minimal known-entity registry written by `/sync` (IDs and first/last-seen times only) and the audit log; it contains no credentials.

- **Audit retention:** entries older than `AUDIT_RETENTION_DAYS` (default 90) are pruned at start-up and every 6 hours.
- **Backup:** the database runs in WAL mode, so copy it while NeptHass is stopped (or copy the `-wal` and `-shm` files with it):

  ```bash
  docker compose stop nepthass
  docker run --rm -v nepthass_nepthass-data:/data -v "$PWD":/backup alpine \
    tar czf /backup/nepthass-data.tgz -C /data .
  docker compose start nepthass
  ```

- **Restore:** stop NeptHass, extract the archive into the volume, and make sure the files are owned by UID 65532 (`chown -R 65532:65532 /data` inside a helper container), then start it.
- **Bind mounts:** if you replace the named volume with a host directory, `chown 65532:65532` it first; the container runs as that non-root user.
- Back up `.env` separately and securely; it is the only copy of your bootstrap secrets.

## Upgrading

```bash
git pull
docker compose up -d --build
```

Migrations run automatically at start-up. NeptHass refuses to start on a database written by a newer version, so downgrades require restoring a backup.

### From v0.1.0 to v0.2.0

1. **Back up first** (see "Data, retention and backups" above). Migration 0002 only adds tables, but a backup is the way back: a v0.1.0 build refuses to start on a v0.2.0 database.
2. Pull and rebuild as above. No configuration changes are needed and no new environment variables are required.
3. Your existing permissions, audit log and `/permissions` workflow are untouched. The new tables (`entity_aliases`, `setup_sessions`, `known_entities`) start empty.
4. Run `/setup` (to review and extend what is shared) and `/sync` (to record the baseline of known entities).

The upgrade was verified on a copy of a real v0.1.0 database: permissions and audit rows were preserved, the schema went from one migration to two, and re-opening the database was a no-op.

### From v0.2.0 to v0.3.0

- There is **no database migration and no new setting**; the v0.2.0 database is used unchanged.
- Rebuild and restart as usual (`git pull && docker compose up -d --build`). NeptHass now also opens a WebSocket connection to `HA_URL` (`http` becomes `ws`, `https` becomes `wss`, any path prefix is kept). It is outbound only and uses the same token.
- If Home Assistant sits behind a reverse proxy, allow WebSocket upgrades on `/api/websocket`.
- Run `/health`: the WebSocket line should read "Healthy" within seconds of start. Share the sensors you want people to see live (`/setup`), then try `/sensors`.
- Live sensor state is not stored: after a restart it is empty until the first synchronization finishes.

### From v0.3.0 to v0.4.0

1. **Back up first** (see "Data, retention and backups" above). Migration 0003 only adds two tables (`notification_settings` and `notification_rules`), but a v0.3.0 build **refuses to start on an upgraded database** ("database schema is newer than this build supports"); going back means restoring the backup, and notification settings created since the upgrade are lost with it.
2. There are **no new environment variables** and no configuration changes. Rebuild and restart as usual: `git pull && docker compose up -d --build`.
3. Notifications stay **disabled** until an administrator runs `/notifications channel`; `/health` shows a Notifications line that reads "Disabled". The slash commands are re-registered at start, so `/notifications` appears after the first start.
4. Existing permissions, aliases, setup sessions and the audit log are untouched.
5. Pending alerts live in memory only, so restarting NeptHass (including this upgrade) loses any alert that was queued at that moment.
