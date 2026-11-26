# Setup

NeptHass needs a Discord bot, a Home Assistant long-lived access token, and persistent storage for its SQLite database.

## 1. Create the Discord bot

1. Create an application in the [Discord Developer Portal](https://discord.com/developers/applications).
2. Add a bot and copy its token and the application ID.
3. Enable Discord Developer Mode and copy your server ID and user ID.
4. Invite the bot, replacing `APPLICATION_ID`:

   ```text
   https://discord.com/oauth2/authorize?client_id=APPLICATION_ID&scope=bot%20applications.commands&permissions=0
   ```

NeptHass needs no privileged gateway intents. Notification channels require View Channel, Send Messages, and Embed Links.

## 2. Create the Home Assistant token

In Home Assistant, open your profile, select **Security**, and create a **Long-Lived Access Token**. Prefer a dedicated non-administrator Home Assistant user.

## 3. Create the environment file

Create `nepthass.env` outside the repository and restrict its permissions:

```env
DISCORD_BOT_TOKEN=
DISCORD_APPLICATION_ID=
DISCORD_GUILD_ID=
DISCORD_ADMIN_USER_IDS=
DISCORD_ALLOWED_USER_IDS=
HA_TOKEN=
HA_URL=http://127.0.0.1:8123
AUDIT_RETENTION_DAYS=90
LOG_LEVEL=info
LOG_FORMAT=json
```

On Linux:

```bash
chmod 600 nepthass.env
```

Secrets also support `_FILE` variables such as `DISCORD_BOT_TOKEN_FILE` and `HA_TOKEN_FILE`. Never set both the value and its `_FILE` variant.

## 4. Run the published image

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

Inspect startup:

```bash
docker logs -f nepthass
docker exec nepthass version
```

For Docker Desktop, omit `--network host` when unsupported and use `HA_URL=http://host.docker.internal:8123`.

For a remote Home Assistant, set `HA_URL` to its local-network URL. If it uses HTTPS, the certificate must be trusted by the container. Reverse proxies must support WebSocket upgrades on `/api/websocket`.

## 5. Configure NeptHass in Discord

1. Run `/health` and confirm every configured subsystem is healthy.
2. Run `/setup` as an administrator.
3. Select only the entities you intend to expose.
4. Grant `read` unless control is genuinely needed. Control is supported only for lights and switches.
5. Confirm the setup and test `/devices`, `/status`, and `/control`.
6. Optionally select a private alert channel with `/notifications channel` and add rules with `/notifications add`.

## Data and backups

All persistent data is stored in `/data/nepthass.db` inside the `nepthass-data` volume.

Back up before every upgrade:

```bash
docker stop nepthass
docker run --rm -v nepthass-data:/data -v "$PWD":/backup alpine \
  cp /data/nepthass.db /backup/nepthass.db.backup
docker start nepthass
```

Store the backup securely because it contains Discord IDs, permissions, aliases, notification rules, and audit history.

## Upgrade

Pinning versions is recommended:

```bash
docker pull ghcr.io/nepthass/nepthass:1.0.0
docker stop nepthass
docker rm nepthass
```

Re-run the command from section 4 with the new version tag. The named data volume survives container replacement and migrations run automatically.

## Rollback

If a release migrates the database, an older image may refuse the newer schema. Stop the container, restore the pre-upgrade database backup, then start the previously used image tag. Never force an old binary to open a newer database.

## Build from source

Source contributors can still use Compose:

```bash
git clone https://github.com/nepthass/nepthass.git
cd nepthass
cp .env.example .env
docker compose up -d --build
```

This path is optional; normal users only need the published image.
