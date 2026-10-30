# Roadmap

NeptHass is developed in small milestones. Each one is released only when it is implemented and verified; this page states direction, not promises or dates. What a release actually contains is in the [changelog](../CHANGELOG.md).

## Principles

- **Secure by default.** Nothing is visible or controllable until an administrator shares it, and every new capability keeps that rule.
- **Honest semantics.** NeptHass says when data is stale, when an alert is late or uncertain, and when something was lost. It does not claim guarantees it cannot keep.
- **Small, verified milestones.** One focused theme per milestone, with tests that fail when the behavior is removed.
- **No unproven scope.** A feature enters a milestone when there is evidence it is needed and a design that fits the existing architecture.

## Released

| Version | Theme |
| --- | --- |
| v0.1.0 | Foundation: configuration, Discord connection, Home Assistant REST client, SQLite, authorization, basic commands, Docker |
| v0.2.0 | Interactive `/setup`, entity discovery, aliases, `/sync` |
| v0.3.0 | Home Assistant WebSocket, real-time sensor monitoring, reconnection and reconciliation |
| v0.4.0 | Smart notifications: rules, cooldowns, one alert channel, monitoring-down notices |

## Next: v0.5.0 (proposed)

**Theme: richer notification rules.** The proposal is to add **duration rules** ("the door has been open for 10 minutes") and **hysteresis** (a separate recovery threshold for numeric rules), plus a few small hardening items found while reviewing v0.4.0. It is a proposal, not a commitment: see the [design proposal](design/rules-v0.5.0.md) for the reasoning, the alternatives that were considered, and the task breakdown.

## Under evaluation (not scheduled)

- **Quiet hours.** Suppress non-critical alerts during chosen hours, with critical alerts (water leak, monitoring down) exempt. It needs a time-zone and daylight-saving design and a decision about suppression versus deferral; the notes are in the proposal.
- **Delivery reliability.** A persisted outbox would survive restarts but changes the delivery model (at-least-once, deduplication, new write path). It is only worth doing with evidence that restart losses matter in practice.
- **Per-rule alert channels, notification history, configurable staleness bound.**

## Later

- **v0.6.0: interactive `/home` dashboard.**
- **v1.0.0: stable release.** Hardening, an independent security review, complete documentation, and a public launch.

### Before a public launch

These are open maintainer decisions or tasks, not features:

- Choose and publish a verified **private security reporting channel** (GitHub's private vulnerability reporting is not available while the repository is private, and no contact address has been decided). `SECURITY.md` and the Code of Conduct both depend on it.
- Enable the repository protections that are unavailable on the current plan (branch protection and required checks, secret scanning, private vulnerability reporting).
- Decide how releases will be distributed (the project currently ships source and a Dockerfile; no image is published).
- Complete the manual Discord checks that automated tests cannot replace (see the known gaps in [smoke-tests](development/smoke-tests.md#known-gaps)).

## How priorities are decided

Security and correctness defects are prioritized ahead of new features. Feature work follows the milestone themes above; open an issue describing the problem you want solved (see [CONTRIBUTING.md](../CONTRIBUTING.md)) and it will be weighed against them.
