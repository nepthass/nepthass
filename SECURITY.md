# Security Policy

## Supported versions

NeptHass is pre-1.0. Only the latest release receives security fixes.

| Version | Supported |
| --- | --- |
| latest 0.x release | yes |
| older releases | no |

## Reporting a vulnerability

**Please do not describe a security problem in a public issue.**

Report it privately:

1. If the repository's **Security** tab offers **Report a vulnerability** (GitHub's private vulnerability reporting), use it: only the maintainers see the report. It is not offered while the repository is private, and a maintainer has to enable it.
2. If it is not available, open an issue titled **Security contact request** that says only that you have a security concern and **contains no details** (no description, logs or entity names), or use the contact options on the maintainers' GitHub profile or organization page. A maintainer will then agree a private channel with you, normally a draft GitHub security advisory that is shared only with you.

Once you have a private channel, include:

- the NeptHass version and how you deploy it,
- what you did, what you expected and what happened,
- the impact you believe it has (for example "a non-allowlisted user can toggle a switch"),
- a minimal reproduction if you have one.

Redact tokens, Discord IDs and entity names you consider private. Never send real credentials; if you accidentally exposed a token, rotate it first.

You can expect an acknowledgement within 7 days. NeptHass is maintained on a best-effort basis; we will keep you informed, agree a disclosure date with you, and credit you in the changelog unless you prefer otherwise.

## Scope

In scope:

- authorization bypass (guild, user, administrator, entity or domain checks),
- controlling any device or Home Assistant service that is not explicitly allowed,
- leakage of secrets through logs, errors, Discord messages or files,
- injection (SQL, URL, markdown or mention abuse),
- denial of service that stops legitimate use through a single unauthorized user.

Out of scope:

- vulnerabilities in Discord or Home Assistant themselves,
- attacks that require a compromised host, container or administrator account,
- missing hardening that is documented as the operator's responsibility in [docs/security.md](docs/security.md).

## Security model in short

- Home Assistant is reached locally; NeptHass opens no ports and Home Assistant is never exposed.
- Every interaction is checked against a guild allowlist and an administrator or user allowlist before any command runs.
- Everything is read-only by default. An administrator must share each entity explicitly, at `read` or `control` level.
- Only `light` and `switch` entities can be turned on or off. Locks, garage doors, alarm panels and all other domains are never controlled.
- Secrets are configured locally (environment or secret files), never through Discord, and are redacted from logs.
- Discord notifications, when introduced, are best-effort and are not a guaranteed emergency channel.

Hardening guidance: [docs/security.md](docs/security.md).
