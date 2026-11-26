# Security model

NeptHass connects a chat platform to a home-automation system, so access is deny-by-default.

## Security boundaries

- Only the configured Discord guild is accepted.
- Only configured administrators and allowed users may interact.
- An entity is invisible until an administrator grants access.
- `read` permits status only; `control` is restricted to lights and switches.
- Administrator checks apply to setup, synchronization, permissions, notifications, and audit access.
- Wizard sessions are bound to their administrator, guild, version, and server-provided choices. Replays and stale confirmations cannot apply changes twice.
- Permission and control actions, including refused administrator commands, are audited with bounded retention and rate limiting.
- Monitoring data is permission-checked again every time it is read or delivered.

## Credential handling

- Store tokens in a protected environment file or use `_FILE` secrets.
- Tokens and configuration values are redacted from logs and errors.
- Home Assistant redirects and environment proxies are ignored to avoid forwarding its token to another host.
- Use a dedicated, non-administrator Home Assistant account when possible.
- Rotate a token immediately if it may have been exposed.

## Container hardening

The recommended container runs as UID/GID `65532`, uses a read-only root filesystem, drops Linux capabilities, enables `no-new-privileges`, and writes only to `/data` and the temporary filesystem.

Do not expose a NeptHass port: it does not provide an inbound HTTP service. It needs outbound HTTPS access to Discord and local REST/WebSocket access to Home Assistant.

## Notification privacy and reliability

Everyone who can read the configured alert channel can see its sensor names and states. Use a private channel and grant only View Channel, Send Messages, and Embed Links.

Messages disable mentions and escape untrusted names. Sharing is checked again before delivery, but a message already accepted by Discord cannot be recalled.

Notifications are best-effort. Queued messages are kept in memory, delivery may duplicate after an ambiguous failure, and changes that happen entirely during an outage may be missed. NeptHass is not a certified alarm or life-safety system.

## Threats outside the model

NeptHass cannot protect against:

- A compromised host, container runtime, Discord account, or Home Assistant token.
- Discord itself seeing messages sent through its service.
- Incorrect device classes or sensor data supplied by Home Assistant.
- Guaranteed notification delivery.

## Hardening checklist

1. Use dedicated Discord and Home Assistant credentials.
2. Enable multi-factor authentication for administrator accounts.
3. Grant the minimum entity access and prefer `read`.
4. Keep notification channels private.
5. Protect and back up the SQLite database.
6. Pin container versions and review the changelog before upgrading.
7. Keep Home Assistant on the local network; NeptHass does not require internet exposure.
8. Review `/permissions list` and `/audit` regularly.

## Reporting a vulnerability

Follow [SECURITY.md](../SECURITY.md). Do not open a public issue for a suspected vulnerability.
