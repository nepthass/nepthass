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

## Next: v0.5.0 (design approved)

**Theme: richer rules and safer start-up.** The five architecture decisions are approved and the [design](design/rules-v0.5.0.md) is written; implementation has not started. The milestone covers:

- **Duration rules** ("the door has been open for 10 minutes") and **hysteresis** (a separate recovery threshold), with numeric thresholds bound to the sensor's unit so a unit change cannot cause false alerts.
- **Initial-state alerting.** A critical hazard (water leak) that is already detected when NeptHass starts alerts immediately; other sensors already in an alert state are summarized once; duplicates across restarts are suppressed.
- **Audit of unauthorized administrative command attempts.**
- **A small typed sensor capability registry** that replaces the per-category switches spread over three packages (a prerequisite for the features above).

Not in v0.5.0: smoke, gas and carbon monoxide alerts (they need a deliberate safety decision first), quiet hours and a persisted outbox.

## Under evaluation (not scheduled)

- **More sensor types.** Garage door, opening, occupancy, power, air quality and others are one registry row each once the registry exists. A declarative JSON catalog was evaluated and is **not planned**: a typed Go registry is simpler and keeps the safety properties beside the engine.
- **Quiet hours.** Suppress non-critical alerts during chosen hours, with critical alerts exempt. It needs a time-zone and daylight-saving design and a decision about suppression versus deferral; the notes are in the design.
- **Delivery reliability.** A persisted outbox would survive restarts but changes the delivery model; it is only worth doing with evidence that restart losses matter in practice.
- **Per-rule alert channels, notification history, a configurable staleness bound.**

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
