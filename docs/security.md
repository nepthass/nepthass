# Security

NeptHass sits between a chat platform and your home, so it is deny-by-default and small on purpose.

## What it protects against

| Threat | Control |
| --- | --- |
| A stranger in your Discord server runs commands | Guild plus user allowlists checked on every interaction; unlisted users get a generic refusal |
| An allowed user reaches devices they should not | Per-entity allowlist; no row means no access, even for administrators |
| Someone tries to unlock a door or open a garage | Control is limited to `light` and `switch`; enforced when granting, when executing, and again in the REST client, which has no code path for other services |
| Arbitrary Home Assistant service execution | The REST client sends only `light` and `switch` `turn_on`/`turn_off` with an entity ID, nothing else |
| Secrets leak through logs or Discord | `Secret` type prints `[REDACTED]`, the logger scrubs literal values, errors never echo configuration values, secrets are never requested through Discord |
| Token forwarded to another host | HTTP redirects are never followed, environment proxies are ignored for Home Assistant |
| Injection | Entity IDs are validated to `[a-z0-9_]` before use in URLs or SQL; all SQL is parameterized |
| Mention abuse via device names | Friendly names are markdown-escaped and every message disables mentions |
| Command flooding | Per-user rate limit (6 per 10 seconds); no unauthenticated work is tracked |
| Silent privilege changes | Permission changes and control attempts (including denied ones) are audited |
| Dangerous startup states | Invalid configuration aborts start; a rejected token aborts start; a newer or tampered database aborts start |

## What it does not protect against

- **A compromised host or container.** The Home Assistant token is stored in the environment and has the power of the Home Assistant user that created it. NeptHass limits what *it* will do with that token, not what the token could do.
- **A compromised Discord account** that is on the allowlist. Protect administrator accounts with Discord two-factor authentication.
- **Discord itself.** Messages pass through Discord's servers. Replies are ephemeral, but device names and states are visible to Discord. Do not share entities whose state you consider private.
- **Guaranteed delivery.** Discord is best-effort; do not rely on it as a safety or emergency alarm.

## Hardening checklist

1. Create a **dedicated Home Assistant user** for NeptHass (non-administrator) and generate the token from it, so the token cannot manage Home Assistant itself.
2. Keep `.env` at mode `600`, out of backups you do not control, and never commit it. Prefer `_FILE` secrets.
3. Put only trusted people in `DISCORD_ADMIN_USER_IDS`; use `DISCORD_ALLOWED_USER_IDS` for everyone else. Review `/permissions list` and `/audit` periodically.
4. Share the minimum: grant `read` unless the person genuinely needs `control`.
5. Keep the compose hardening (read-only root filesystem, dropped capabilities, `no-new-privileges`, non-root user).
6. Do not expose Home Assistant to the internet for NeptHass; it only needs a local connection.
7. Restrict who can invite bots or change roles in your Discord server, and rotate the bot token if it may have leaked (Developer Portal, Reset Token).
8. Update regularly (`git pull && docker compose up -d --build`) and watch the changelog.

## Reporting a vulnerability

Follow [SECURITY.md](../SECURITY.md). Please do not open public issues for security problems.

## Security review status

The v0.1.0 controls above are covered by automated tests (authorization matrix, domain allowlist, permission tampering, redaction, redirect handling). No independent external audit has been performed; a dedicated review is part of the v1.0.0 milestone.
