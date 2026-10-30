# Smart notifications (v0.4.0)

This document describes how NeptHass turns live sensor changes into Discord alerts, what it guarantees, and what it deliberately does not. It describes the shipped design, not a plan.

## Goals and non-goals

Goals:

- Tell an administrator-chosen channel when a shared sensor does something worth knowing about: a door or window opens, motion is detected, a leak sensor gets wet, a battery is low, or a temperature, humidity or battery level crosses a threshold.
- Never block, slow down or crash live monitoring because Discord is slow or down.
- Be honest: say when an alert is late, when a state was uncertain, when alerts were lost, and when NeptHass itself cannot see Home Assistant.
- Respect the sharing model: a sensor that is not shared, or has been unshared, never produces a notification.

Non-goals (v0.4.0):

- **No persisted outbox.** Pending alerts live in memory. A crash or restart loses what was queued (see [Delivery guarantee](#delivery-guarantee)).
- No per-user direct messages, no mentions or pings of any kind, no per-rule channels (one channel per guild), no quiet hours, durations, hysteresis or rule logic beyond a single threshold (those are v0.5.0).
- No notification history. The audit log records configuration changes, not individual alerts.

## Data flow

```
Home Assistant WebSocket
        |
   monitor (v0.3.0)  --- Config.OnChange (ordered Change feed, never blocks) --->
        |                                                                       |
   Tracker (state)                                              Engine.OnChange (monitor goroutine)
                                                                  atomic rule-set lookup, O(1) mailbox update
                                                                                |
                                                                        evaluator goroutine
                                                    rules, re-arm, cooldown, sharing, message building
                                                                                |
                                                     two bounded lanes: critical (32) and normal (64)
                                                                                |
                                                                       dispatcher goroutine
                                    permission re-check, TTL, rate limit, breaker, retries, Sender.Send
                                                                                |
                                                            Discord REST (internal/discord.Notifier)
```

Three goroutines belong to the engine (evaluator, dispatcher, health watcher). `Engine.Run` starts and joins them; the application waits for it at shutdown, bounded to three seconds.

### The callback never does I/O

`Engine.OnChange` runs on the monitor's goroutine. It performs one atomic load of the current rule set, one map lookup, and for a watched entity an O(1) update of a mailbox. It does no database access, no network call, no allocation per event beyond the bounded mailbox, and starts no goroutine. With notifications disabled the rule set is empty and it returns after the lookup. This is tested with a sender that never answers and with the database closed.

### The mailbox and what survives coalescing

The mailbox holds at most one slot per watched entity (so its size is bounded by the 200-rule limit, not by the event rate). While the evaluator is busy, a burst of changes to one entity collapses into its slot.

What survives:

- the latest sensor state;
- per rule, the **first qualifying transition** since the last evaluation: its sequence number, its source (live, replay or snapshot), its time, the violating sensor state, and whether the previous state was unreliable;
- a reset flag when the entity was removed or first observed (every rule of that entity re-baselines silently);
- whether a snapshot change was merged, and the time of the oldest unprocessed change (used for the lag health signal).

What does **not** survive: intermediate values, later qualifying transitions inside the same window (they are counted as coalesced), and a pending transition of an entity that was removed before evaluation.

Consequence: a door that opens and closes before the evaluator runs still alerts (marked "already back to normal"), because the latch keeps the opening.

## Rules

A rule belongs to one entity and has a kind:

| Kind | Applies to | Alerts when |
| --- | --- | --- |
| `state` | door, window, motion, leak, battery-low | the sensor enters its alert state (open, motion detected, wet, low) |
| `above` | temperature, humidity, battery level | the reading rises above the threshold (strictly greater) |
| `below` | temperature, humidity, battery level | the reading falls below the threshold (strictly less) |

An entity has at most one rule of each kind. At most **200 rules** exist (enforced in the service and by a database trigger). A threshold must be a finite number between -1,000,000 and 1,000,000. A rule is `active`, `paused`, or shows a dormant status computed from current facts: `needs_sharing`, `not_monitored`, `incompatible`.

### When a rule alerts

An alert needs a **qualifying transition**: the sensor was last seen reliably not violating and now violates. Therefore:

- A **first observation never alerts**. This includes the initial snapshot after NeptHass starts, a rule created while the sensor is already in its alert state, and an entity that reappears.
- A state that merely persists, an attribute-only update, a duplicate and a stale event never alert.
- After an alert the rule re-arms only once the sensor is reliably not violating again.
- `unavailable`, `unknown` and unparseable readings are unreliable. They neither re-arm nor fire a rule, with one exception: **a leak sensor that goes from unavailable or unknown to wet alerts**, marked as uncertain ("its state was unknown before, please check it"). A missed leak is judged worse than a possibly redundant one.
- A change found by a **snapshot after a reconnect** alerts and is marked late ("detected after NeptHass reconnected, it may have happened earlier").

**Initial snapshot limitation.** Because a first observation is silent, a leak that is already wet when NeptHass starts, or when the rule is created, is not announced. NeptHass tells you when it changes, not what already was. Use `/sensors` to see the current state. The approved v0.5.0 design replaces this behavior for critical hazards and adds a one-time start-up summary for the rest: see [initial-state alerting](rules-v0.5.0.md#10-initial-state-alerting-d5).

### Cooldown

Each rule has a cooldown: the minimum time between two alerts for that rule. Defaults by category:

| Category | Default cooldown |
| --- | --- |
| motion | 5 minutes |
| door, window | 1 minute |
| leak | 15 minutes |
| temperature, humidity | 15 minutes |
| battery level, battery low | 6 hours |

A rule can override its cooldown (0 to 1440 minutes in `/notifications add`); **0 disables the cooldown**. A violation suppressed by cooldown is *consumed*: no deferred alert and no reminder is produced later, even if the sensor is still violating when the cooldown ends. The next alert after a re-arm reports how many were muted. Suppression is counted in `/notifications status`.

## Sharing and authorization

- Only administrators can configure notifications (`/notifications`, guild-only, ephemeral replies).
- A rule can only be created for an entity that is shared and currently monitored and whose category fits the rule kind.
- Sharing is read before an alert is produced and **again immediately before every delivery attempt**, including retries. A message whose entity is no longer shared at `read` or higher is dropped (counted as revoked). A digest is re-checked line by line.
- Revoking an entity does not delete its rules. They become dormant (`needs_sharing`) and resume by themselves if the entity is shared again.
- **Unavoidable race:** a revocation that lands after the last check but before Discord accepts the HTTP request cannot be recalled. The window is the length of one request.
- Messages carry a display name (alias, else Home Assistant's friendly name, else a generic label), a state or value with its unit, and times. They never carry entity IDs, raw attributes or credentials. Names are escaped, and every message is sent with an empty allowed-mentions list, so a hostile name cannot ping anyone.

## One channel per guild

There is a single alert channel, set with `/notifications channel`. It must be a text channel of the configured guild, and the bot needs View Channel, Send Messages and Embed Links there (checked when it is chosen, and noticed again on the first send after permissions change). **Everyone who can read that channel sees the alerts**, including sensor names and states, whether or not they have access to those sensors in NeptHass. Choose a private channel if that matters.

If the channel is deleted or the bot loses access, sends fail permanently, the circuit breaker opens, and `/notifications status` and `/health` show the problem. Rules are kept.

## Delivery guarantee

**Best-effort delivery with bounded staleness and possible duplicates.** It is not exactly-once, not at-least-once and not at-most-once.

- **Bounded staleness (TTL):** every message has an absolute deadline of **10 minutes** from the observation. Retries never extend it. After the deadline the message is dropped, counted, and the rule is flagged for reconciliation.
- **Retries:** network errors, ambiguous timeouts, Discord 5xx and 429 are retried with exponential backoff (1 s doubling to 30 s, equal jitter), or after Discord's `retry_after` for a 429. Permanent failures (401, 403, 404, 400) are never retried.
- **Duplicates:** if Discord accepted a message but the answer was lost, the retry delivers it twice. This is accepted on purpose: a duplicate alert is cheaper than a missed leak alert. It is covered by a test.
- **Rate limiting and breaker:** a token bucket (1 per second, burst 5) paces sends. Three consecutive permanent failures open a circuit breaker; one probe is allowed per minute, and a success closes it.
- **No outbox:** queued messages exist only in memory. A crash or restart loses them, and NeptHass does not replay history after a restart. Graceful shutdown drains for up to three seconds, then drops and counts the rest.
- **Ordering:** a critical message may overtake a normal one, and a message waiting for a retry may be overtaken by later ones. Order is otherwise preserved per lane.

## Overflow and current-state reconciliation

Two bounded lanes hold pending messages: **critical** (32: leak alerts, monitoring-down, recovery and digest) and **normal** (64: every other alert). Ingestion never blocks.

- The normal lane drops its oldest message when full.
- The critical lane evicts the lowest-priority, oldest message only if the newcomer is at least as important (leak alert, then monitoring-down, then digest, then recovery). An active leak alert is never discarded for a less important message.
- **No silent drops.** Every dropped message is counted by reason (overflow, expired, revoked, channel failure, breaker, disabled, shutdown, withdrawn) and degrades health.
- A rule whose alert was lost (overflow, expiry, channel failure, breaker) is flagged. When the queues have drained below 25%, NeptHass sends **one digest** (at most 10 lines plus "and N more", at most once a minute) that re-reads each flagged rule's sensor and sharing *now* and lists only those still violating.

The digest reports **current state**. It does not and cannot recover a transition that happened and ended while delivery was failing.

## Monitoring-down notices

Notifications are only as good as monitoring. A small state machine (starting, healthy, down, down-alerted, recovered) samples health every 10 seconds (health can turn false by the passage of time alone, so it is polled):

- **One alert** after **five continuous unhealthy minutes** (counted from process start if monitoring was never healthy). It is not repeated while the outage lasts.
- If that alert was *dropped*, it is re-sent once, and only while delivery works.
- **A recovery notice is sent only if the down alert was actually delivered.** "Queued" is not "delivered". If monitoring recovers first, a queued down alert is withdrawn and nothing is said; if the alert was already in flight and arrives after recovery, the recovery notice follows it.
- An intentional shutdown, a deliberately stopped monitor and disabled notifications never produce a down alert.
- The recovery notice is queued before the late alerts found by the reconnect snapshot, so the channel reads "monitoring is back" before "detected after reconnect". The evaluator takes a health sample before it produces late alerts to guarantee this; if the monitor is still synchronizing at that instant, the notice follows on the next tick.

## Configuration, reload and consistency

Configuration lives in SQLite (migration `0003`): `notification_settings` (one row per guild; no row means disabled) and `notification_rules`. Every mutation and its audit entry are committed in one transaction. After every successful change the running engine is reloaded.

If the change commits but the reload fails, the administrator is told the change is **saved but not active**, the engine keeps its previous configuration (it never half-loads), health shows degraded with the reason "saved configuration is not active", and the engine retries every health tick until the saved configuration is running. `/health` never reports "disabled" in that state.

Pending runtime state (queues, cooldown timers, the baseline of each rule) is not persisted. After a restart rules are re-seeded from the sensors' current state, silently.

## Audit

Every configuration change is audited atomically. Failed or denied administrative actions are audited too: a rejected channel (with a fixed reason code such as `rejected_not_text`, never the channel or Discord's text), the outcome of `/notifications test`, and any denial that reaches the service layer. A non-administrator is refused earlier, by the command gate, and that refusal is **not** audited, exactly as for every other administrator command; auditing refusals at the gate is a candidate for a later milestone. Audit detail contains only fixed sentinel wording (anything else is stored as "internal error"), never user input, tokens or Discord error messages. A successful channel change records the new channel ID, which is not a secret.

## Observability

- `/health` (any allowed user) shows one non-sensitive line: disabled, enabled, or degraded with the reasons.
- `/notifications status` (administrators) shows the channel, rule counts by status, last successful delivery and last failure, queue depths, counters, dropped counts by reason, circuit-breaker state, and the monitoring-down state.

## Upgrade, backup and downgrade

Migration `0003` creates the two notification tables on first start of v0.4.0. It touches no existing table, so an upgraded database with notifications left unconfigured behaves like v0.3.0.

- **Back up before upgrading.** Stop NeptHass and copy the database file (and its `-wal` and `-shm` files if present), or use `sqlite3 <db> ".backup <file>"`.
- **There is no automatic downgrade.** A v0.3.0 binary refuses to start on a database that has migration `0003` ("database schema is newer than this build supports"); it never runs on a newer schema. To go back to v0.3.0, restore the pre-upgrade backup. Alerts configured since the upgrade are lost with it.

## What is verified and what is not

Verified by automated tests: rule semantics, the mailbox, queues, dispatcher, breaker, monitoring-down machine, reload failure and recovery, audit, channel validation including role and overwrite combinations, the Discord sender against a **stub REST server**, and the full chain against a **real Home Assistant** (with a recording sender in place of Discord).

Manual verification on a real Discord test server is partial. Not verified against real Discord: a real HTTP 429, a Discord-only outage and a second user; those behaviors are covered by stub-server tests only, and the v0.3.0 to v0.4.0 downgrade check is deferred. See the [known gaps](../development/smoke-tests.md#known-gaps).
