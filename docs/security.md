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
| Someone replays or forges a wizard click | Custom IDs are untrusted: shape and action are whitelisted, the session is loaded from SQLite and must belong to the caller and guild, the rendered version must match, and selections must be a subset of the IDs the server offered |
| An outdated or replayed confirmation re-applies or restores a permission | Confirmation is bound to the reviewed session version; apply is a single transaction that completes the session; changes made outside the session abort apply, rebase the draft and require a fresh confirmation |
| A non-administrator uses setup | Administrator level is required at the command, the bot routing and every setup service call; other users get a bare refusal that leaks nothing |
| Alias abuse (mentions, markdown, impersonating an entity ID, probing for names) | Aliases are limited to letters, digits, spaces and `- ' . ( )`, 32 characters, unique, never entity-ID-shaped, escaped on output, and resolve only to entities the caller can already use |
| Dangerous startup states | Invalid configuration aborts start; a rejected token aborts start; a newer or tampered database aborts start |
| Unshared sensor data read through live monitoring | The monitor tracks every classified sensor in memory, but nothing reaches a user except through the `Reader`, which checks the caller's level and the entity's current permission at read time. An unshared entity is denied exactly like an unknown one |
| Revoked access still shows cached data | Permissions are re-read on every call and never cached with the data; revocation applies on the next read |
| Malicious or malformed Home Assistant data | Every field of an incoming state is validated (entity ID, lengths, timestamps); only the entity, state, timestamps and three attributes are read and kept; frames over 16 MiB (64 KiB before authentication) end the connection; a malformed frame ends the connection; memory is bounded |
| Stale values presented as current | After a disconnect the last values stay readable but are labeled stale, and `unavailable`, `unknown` and invalid values are never shown as a safe state (closed, clear, dry) |
| WebSocket token exposure | The token is sent once in the authentication message, never logged, never in a URL or an error, and the same redaction as the REST client applies |

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

## Known limitations of the setup wizard

- Alias uniqueness is case-insensitive but not confusable-aware: visually similar characters from different scripts count as different aliases.
- Discord shows entity names and states to Discord; the wizard's ephemeral messages are private to the administrator but not end-to-end encrypted.
- Automated tests drive the wizard with simulated Discord interactions and a real Home Assistant. The maintainer also reported passing manual checks on a live Discord server (user-reported, recorded in [development/smoke-tests.md](development/smoke-tests.md)); the automated suite itself never talks to Discord.

## Live monitoring (v0.3.0)

Two boundaries exist and must not be confused:

1. **Internal ingestion.** The WebSocket peer is Home Assistant, not a Discord user. It is trusted only as far as possession of the configured token implies, and everything it sends is validated as untrusted input. Nothing in an event can trigger an action: monitoring adds no write path and no new control capability.
2. **User-facing reads.** Everything shown to a Discord user passes the `Reader` (allowlisted user, plus current permission of at least `read`). `/health` shows aggregate status to any allowlisted user (state, layer flags, counts, last event time, failure category) and never URLs, tokens or raw errors.

Privacy notes: sensor values live in process memory only and are lost on restart; unshared sensors are held in that memory too (tracking is not exposure); the sensor list a person sees through `/sensors` is Discord-visible like any other reply.

Known limitations of monitoring: it observes only. It does not notify anyone, so it is not an alarm. A change that happens and reverses while NeptHass is disconnected leaves no trace. TLS uses the system trust store, as the REST client does (no custom CA or insecure option).

## Reporting a vulnerability

Follow [SECURITY.md](../SECURITY.md). Please do not open public issues for security problems.

## Security review status

The controls above are covered by automated tests (authorization matrix, domain allowlist, permission tampering, redaction, redirect handling, and for v0.2.0 session ownership, replay and concurrency, stale confirmations, forged selections, rollback, alias rules and escaping). For v0.2.0, 16 deliberate regressions of these controls were injected one at a time and each was caught by the test suite. No independent external audit has been performed; a dedicated review is part of the v1.0.0 milestone.
