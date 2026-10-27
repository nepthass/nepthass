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

## 5. First commands

1. `/ping` and `/health` confirm the connection.
2. `/permissions allow entity:light.living_room access:control` (type to autocomplete from Home Assistant). Only lights and switches can be granted `control`; any entity can be granted `read`.
3. `/devices`, `/status entity:...`, `/control entity:... action:on`.
4. `/audit` shows what happened. `/permissions list` and `/permissions revoke` manage the allowlist.

Other people can use NeptHass once their user ID is in `DISCORD_ALLOWED_USER_IDS` (or `DISCORD_ADMIN_USER_IDS`); restart the container after changing it. They see only what an administrator shared.

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

All state lives in one SQLite database at `DATABASE_PATH` (`/data/nepthass.db` in the container, on the `nepthass-data` volume). It holds the entity allowlist and the audit log; it contains no credentials.

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
