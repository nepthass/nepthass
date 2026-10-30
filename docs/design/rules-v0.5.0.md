# v0.5.0 design proposal: richer notification rules

**Status: proposed, not implemented.** This is a design proposal for maintainers to accept, change or reject. Nothing here is released, and no v0.5.0 code exists yet. It builds on the shipped design in [notifications.md](notifications.md) and the [architecture](../architecture.md).

## 1. Problem statement

v0.4.0 alerts when a sensor *enters* an alert state or crosses a threshold. Two common situations are not covered well:

- **Time matters.** A door that is open for a moment is normal; a door that stays open for ten minutes is not. Today the only choices are an immediate alert (noise) or none.
- **Values hover around a threshold.** A temperature of 29.9, 30.1, 29.9, 30.1 re-arms and can re-alert repeatedly. Cooldowns hide the noise but cannot say "alert again only after it has really recovered".

The roadmap listed durations, quiet hours and hysteresis for v0.5.0. This document evaluates them against the actual code, chooses a focused scope, and plans it.

## 2. Scope decision

### Candidates evaluated

| Candidate | Value | Cost and risk (from the code) | Decision |
| --- | --- | --- | --- |
| **Duration rules** ("for N minutes") | High: the most requested rule shape for doors, windows, motion and temperature | Medium. Needs timers in the evaluator, restart behavior, and a schema change (the unique key). No new external integration: `Sensor.LastChanged` already carries Home Assistant's timestamp | **Must-have** |
| **Hysteresis** (separate clear threshold) | Medium: removes flapping for numeric rules | Low to medium. Needs an explicit "armed" state, which does not exist today, and a small mailbox change | **Must-have** |
| **Quiet hours** | Medium to high | High. Time zones and daylight saving, a decision between suppression and deferral, critical exceptions, a Discord UX for schedules. A deferral that survives restarts implies persistence; a suppression plus end-of-window digest does not | **Deferred** (design notes in section 10) |
| **Persistent outbox** | Low to medium | High. Changes the delivery model (at-least-once, deduplication keys, a SQLite write on the alert path, crash recovery, backpressure). Today's exposure is the alerts queued at the moment of a crash (at most 10 minutes of staleness) | **Deferred** until there is evidence it matters |

A smaller set of items found during the v0.4.0 review is proposed as **should-have** (section 3).

### Alternatives considered

- **A. Durations + hysteresis (recommended).** Both extend rule *semantics* inside `internal/notify`, share one migration, and leave the delivery path (mailbox lanes, dispatcher, breaker) untouched.
- **B. Quiet hours first.** Visible to users, but the design questions (time zone source, DST, suppress versus defer, UX) have no evidence-backed answer yet, and it does not improve rule expressiveness.
- **C. Outbox first.** Improves a failure mode that is bounded and documented, at the price of the largest redesign.
- **D. Everything the roadmap listed.** Three subsystems with different risk profiles in one milestone; rejected as too large to review and verify.

## 3. Must-have, should-have, deferred

**Must-have**

- M1. Duration rules for state rules (door, window, motion, battery low) and numeric rules (temperature, humidity, battery level). Not for water leak.
- M2. Hysteresis (`clear` threshold) for numeric rules.
- M3. Migration 0004, backward compatible: every existing rule behaves exactly as before.
- M4. `/notifications add` options, `list` and `status` output, messages, and documentation for both features.
- M5. Deterministic tests (injected clock), regression tests that fail if the behavior is removed, a real Home Assistant integration test, stress at constrained CPU.

**Should-have** (each small, may slip without blocking the release)

- S1. Audit refusals at the command gate for all administrator commands (today a non-administrator's refusal is not audited; see [security.md](../security.md)).
- S2. `/notifications status` lists the watched sensors that are **currently** in an alert state. This is the cheap mitigation for the documented limitation that a sensor already violating at startup is not announced, without changing the approved "first observation is silent" rule.
- S3. Validate the alert channel without trusting cached bot roles (the bot connects with the `Guilds` intent only, so changes to its own roles may not reach discordgo's cache).
- S4. Housekeeping that the features need anyway: split `engine.go`, remove the unused `ruleState.active`, replace the fixed `maxLatches` array, share one test clock.

**Deferred** (not in v0.5.0)

- Quiet hours, persistent outbox, per-rule channels, notification history, configurable TTL, idempotent retries through Discord's message nonce (discordgo v0.29.0 has no field for it, so it would need a raw REST call and verification against real Discord).

## 4. v0.4.0 limitations reviewed against the code

| Limitation | Classification | Evidence and action |
| --- | --- | --- |
| No persisted outbox | Documented design tradeoff | Approved decision; exposure is bounded by the 10 minute TTL. Deferred |
| Possible duplicate delivery | Documented design tradeoff | Follows from retrying ambiguous failures. Mitigation by nonce is a backlog investigation |
| 10 minute staleness bound | Acceptable limitation | A constant in `EngineConfig`; configurability is a backlog item |
| First observation is silent | Product decision with a safety edge | A leak already wet at startup is not announced. S2 mitigates without reversing the decision. A reversal for critical categories needs an explicit owner decision |
| Discord-only outage and real HTTP 429 not seen on real Discord | Test gap | Covered by stub-server tests; add manual rows when the sender changes. Backlog |
| Permission checks may use cached session state | Confirmed limitation, low impact | `session.Identify.Intents = IntentsGuilds`; the bot's own role changes may not reach the cache. A failed send still reveals the real state. S3 |
| Audit does not cover refusals at the command gate | Confirmed gap (all administrator commands) | `Registry.Run` returns before any service is called. S1 |
| No v0.4.0 upgrade guidance in `setup.md` | Confirmed documentation defect | Fixed in the cleanup that accompanies this proposal |
| `ruleState.active` is written but never read | Technical debt | Dead state; replaced by the explicit armed state in T01/T06 |

## 5. User stories

- As an administrator, I want to be told when the **front door has stayed open for 10 minutes**, not when it opens for a second.
- As an administrator, I want a **temperature alert above 30 °C that does not repeat until it has fallen below 28 °C**.
- As an administrator, I want to keep my existing v0.4.0 rules working unchanged after upgrading.
- As an administrator, I want to see in `/notifications list` exactly when each rule fires and when it re-arms.
- As a maintainer, I want the new rule logic to be deterministic and testable with a fake clock.

## 6. Goals and non-goals

Goals: duration rules, hysteresis, zero behavior change for existing rules, deterministic evaluation, no new external dependency, no change to the delivery path.

Non-goals: quiet hours, outbox, scheduling by time of day, compound rules (AND/OR across sensors), per-rule channels, duration rules for water leak (leak alerts stay immediate on purpose), reversing the silent first observation.

## 7. Design

### 7.1 Rule model

`Rule` gains two optional fields: `Duration` (how long the condition must hold; absent or zero means immediate, as today) and `Clear` (numeric rules only: the value at which the rule re-arms). Definitions:

- A sensor is *violating* when it reliably meets the rule's condition (unchanged). *Unavailable*, *unknown* and invalid readings are unreliable.
- An **episode** is an unbroken run of reliable violating readings.
- A rule is **armed** when it may fire. It disarms when it fires and re-arms as described below.

### 7.2 Duration semantics

For a rule with `for D`:

1. When an episode **begins** (a qualifying transition from a reliable non-violating state, exactly the v0.4.0 `enters` rule) and the rule is armed, the evaluator records `start` and schedules `due = start + D`.
2. If the sensor **stops violating reliably** before `due`, the pending episode is cancelled and nothing is sent.
3. If the sensor becomes **unreliable** (unavailable, unknown, invalid) the pending episode is cancelled: continuity is unknown, so NeptHass does not guess. (A later reliable violating reading begins a new episode.)
4. At `due` the evaluator **re-reads the current sensor** instead of trusting its own bookkeeping. If it is still reliably violating and sharing still allows it, the rule fires with the elapsed time; otherwise nothing is sent. A missed clear event therefore cannot cause a false alert.
5. After firing, the rule is disarmed until it re-arms (7.3). Cooldown applies exactly as in v0.4.0: an alert suppressed by cooldown is consumed, never deferred.

`start` comes from Home Assistant's own `last_changed` timestamp, not from when NeptHass received the event:

- For **binary sensors** (door, window, motion, battery low) `last_changed` is exactly when the state last changed, so it is exact, including after a reconnect and after a restart.
- For **numeric sensors** `last_changed` moves with every new value. NeptHass records `start` from its own observation of the first violating reading while running. After a **restart** it can only use the current reading's `last_changed`, which is later than the true start: the alert may be late, never early. This is documented behavior.

Interaction with existing rules:

- **First observation is silent.** After a restart a sensor that is already violating is baselined silently; the episode is marked as already past, so it does not alert until it clears and violates again. A duration rule that comes due *after* the restart (violating, elapsed less than `D`) does fire at the right time, because `start` comes from `last_changed`.
- **Late alerts.** If a reconnect snapshot reveals a violating sensor whose `start + D` has already passed, the rule fires immediately and is marked late, with the real elapsed time ("open for 12 minutes, detected after NeptHass reconnected").
- **Unavailable while pending** cancels (above). A leak sensor is excluded from duration rules, so the leak fail-loud rule is unchanged.

Timers live **in memory only** (no persistence). Restart safety comes from re-deriving episodes from the tracker after startup, as above.

### 7.3 Hysteresis semantics

For an `above` rule with threshold `T` and `clear` value `C` (`C < T`): the rule fires when a reliable reading exceeds `T` while armed; it re-arms only when a reliable reading falls **below `C`**. Readings between `C` and `T` neither fire nor re-arm. For a `below` rule the sense is mirrored (`C > T`). Without `clear` the behavior is exactly v0.4.0 (re-arm as soon as the reading no longer violates).

Combined with a duration, the pending phase uses only the trigger threshold (a reading back at or below `T` cancels the pending episode); `clear` governs only re-arming after a fire.

Validation: `clear` only on `above` and `below` rules; strictly on the recovery side of the threshold; within the same bounds as thresholds (finite, at most 1,000,000 in magnitude).

### 7.4 Engine changes

- Replace the unused `ruleState.active` with an explicit state: `armed`, and an optional `episode` (`start`, `due`, `fired`).
- **Timers.** The evaluator keeps a min-heap of pending episodes keyed by `due` (at most 200 rules, so at most 200 entries) and waits on the injected `Clock` for the earliest one in the same `select` that already waits for the mailbox. Everything (events, reloads, timers) runs on the evaluator goroutine, so there are no new locks and no new races. The monitor callback is unchanged: it still only records into the mailbox.
- **Mailbox.** The slot's fixed `maxLatches = 2` array assumes at most one `above` and one `below` rule per entity; duration rules break that assumption (several rules per entity and kind). It becomes a bounded slice. For hysteresis rules the slot additionally records that the reading crossed `clear` since the last drain, so a dip and a return inside one coalescing window is not mistaken for "never re-armed".
- **Reload.** Episodes of rules that did not change are preserved; changed or removed rules drop their timers; new rules are seeded silently from the tracker (as today).
- **Delivery path:** the lanes, dispatcher, breaker, digest and monitoring-down machine are **not modified**.

### 7.5 Data model: migration 0004

The `notification_rules` constraint `UNIQUE (entity_id, kind)` forbids "door opened" **and** "door open for 10 minutes" on one door, which is the main use case. SQLite cannot alter a constraint, so the migration rebuilds the table (inside the migration transaction):

```sql
CREATE TABLE notification_rules_new (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_id        TEXT NOT NULL CHECK (length(entity_id) BETWEEN 3 AND 255),
    kind             TEXT NOT NULL CHECK (kind IN ('state', 'above', 'below')),
    threshold        REAL CHECK (threshold IS NULL OR threshold BETWEEN -1000000 AND 1000000),
    clear_threshold  REAL CHECK (clear_threshold IS NULL OR clear_threshold BETWEEN -1000000 AND 1000000),
    duration_seconds INTEGER NOT NULL DEFAULT 0 CHECK (duration_seconds BETWEEN 0 AND 86400),
    enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    cooldown_seconds INTEGER CHECK (cooldown_seconds IS NULL OR cooldown_seconds BETWEEN 0 AND 86400),
    created_by TEXT NOT NULL, created_at TEXT NOT NULL, updated_by TEXT NOT NULL, updated_at TEXT NOT NULL,
    UNIQUE (entity_id, kind, duration_seconds),
    CHECK ((kind = 'state' AND threshold IS NULL AND clear_threshold IS NULL)
        OR (kind IN ('above', 'below') AND threshold IS NOT NULL))
);
INSERT INTO notification_rules_new (id, entity_id, kind, threshold, enabled, cooldown_seconds,
                                    created_by, created_at, updated_by, updated_at)
    SELECT id, entity_id, kind, threshold, enabled, cooldown_seconds,
           created_by, created_at, updated_by, updated_at FROM notification_rules;
DROP TRIGGER notification_rules_limit;
DROP TABLE notification_rules;
ALTER TABLE notification_rules_new RENAME TO notification_rules;
-- recreate the 200-rule trigger and carry over the AUTOINCREMENT sequence
```

Safety properties to prove with tests: every existing row survives with identical values and becomes `duration_seconds = 0` and no `clear`; rule IDs are preserved; the 200-rule trigger is recreated and still fires; the migration is idempotent on restart; an older (v0.4.0) binary refuses the upgraded database (the existing newer-schema refusal), so the upgrade notes must say "back up first". No foreign key references the table. **Decision D1 for maintainers:** if a table rebuild is judged too risky for the benefit, the fallback is to add the two columns and keep `UNIQUE (entity_id, kind)`, accepting one rule per entity and kind (no "immediate plus delayed" pair).

### 7.6 Discord UX

- `/notifications add entity [above] [below] [clear] [for] [cooldown]`: `for` is whole **minutes** (1 to 1440, omitted means immediate) and `clear` is a number. Both are optional, validated server-side, and Discord's own option bounds mirror the limits. Autocomplete stays administrator-only and shows names, never entity IDs.
- Errors are specific: `clear` without `above`/`below`, `clear` on the wrong side of the threshold, `for` on a leak sensor ("water leak alerts are always immediate"), a duplicate of the same entity, kind and duration.
- `list` shows the rule in words: "when it opens and stays open for 10 minutes", "rises above 30 (re-arms below 28)".
- Alert text for a duration rule states the held time: "**Front door** has been open for 10 minutes", plus the existing late/uncertain/cooldown lines.
- `status` gains "waiting: N" (pending duration episodes) and the counters in 7.8.

### 7.7 Home Assistant integration

No new subscription, request or permission. The design uses what the monitor already provides (`Sensor.LastChanged`, the change feed, the snapshot after reconnects).

### 7.8 Failure handling, security, observability, performance

- **Failure handling.** A missed or reordered event is corrected at `due` by re-reading the tracker. A reconnect snapshot reconciles episodes. A reload failure keeps the previous rules and timers (unchanged v0.4.0 behavior). Timers are re-derived after a restart.
- **Security.** No new authorization surface: configuration stays administrator-only; sharing is read again when an episode comes due (before the alert is queued) and again before every send. Durations are bounded (1 minute to 24 hours) and the 200-rule limit bounds the number of timers. Audit entries for rule creation include the duration and clear value (not sensitive). If S1 ships, refused administrator commands are audited with fixed wording.
- **Observability.** Counters for pending episodes, fires by duration, cancellations (cleared or unreliable before `due`) and late fires, shown in `/notifications status`.
- **Performance.** At most 200 heap entries, one evaluator goroutine, wake-ups only at due times; no extra work per event for rules without a duration.

### 7.9 Rollout and compatibility

- Migration 0004 is applied on first start. Existing rules keep their exact behavior. Feature use is opt-in per rule.
- A v0.4.0 binary refuses a 0004 database; the changelog and `setup.md` carry the same backup-first notice as for 0003.
- No configuration or environment change.

## 8. Testing strategy

- **Pure semantics tests** (table-driven, injected clock): episode start, cancel on clear, cancel on unavailable, fire at `due`, re-read at `due`, cooldown interaction, re-arm, hysteresis bands (including a dip below `clear` and a return within one coalescing window), restart seeding from `last_changed`, late fire after a snapshot, silent baseline.
- **Property-style invariants:** a rule never fires twice without a re-arm; a duration rule never fires before `D` elapsed since `start`; a rule without `for`/`clear` behaves identically to v0.4.0 (differential test against the v0.4.0 evaluator on generated sequences).
- **Migration tests:** a golden v0.4.0 database upgrades with identical rule rows, trigger intact, restart idempotent, older binary refuses.
- **Command tests:** option parsing and validation, error messages, `list` wording, administrator-only autocomplete, no entity IDs in output.
- **Concurrency:** race detector, shuffled order, constrained-CPU stress (0.5 and 0.25 CPU), goroutine-leak checks, a test that the monitor callback still never blocks.
- **Real Home Assistant integration:** a door held open for a duration (injected clock), cancellation on close, restart seeding, and hysteresis through the virtual temperature setpoints documented in [virtual-devices.md](../development/virtual-devices.md).
- **Regression injections:** at least ten deliberate regressions of the new logic, each caught by a test (fire before `D`, ignore cancellation, skip the re-read, wrong hysteresis side, lose the `sawClear` flag, drop episodes on reload, and so on).
- **Manual Discord rows** added to the checklist for duration and hysteresis.

## 9. Acceptance criteria

1. An existing v0.4.0 database upgrades; every rule keeps its behavior; the 200-rule limit still holds.
2. A duration rule alerts only when the condition has held for the configured time, once per episode, with the elapsed time in the message.
3. A condition that clears, or becomes unavailable, before the duration expires sends nothing.
4. After a restart, a duration rule neither alerts for an already-violating sensor at startup nor fires early; a pending episode that comes due after the restart fires on time for binary sensors.
5. After a reconnect, a violating episode already past its duration fires once, marked late.
6. A hysteresis rule re-arms only below `clear`; readings in the band never re-alert; a dip below `clear` inside one coalescing window re-arms.
7. Leak rules remain immediate; `for` on a leak is refused with a clear message.
8. No new race, goroutine leak or blocking monitor callback; stress passes at 0.5 and 0.25 CPU.
9. All existing tests pass unchanged; the new regression injections are all caught.
10. Documentation, changelog and the upgrade notes match the implementation, and the manual checklist is extended.

## 10. Notes for deferred work

**Quiet hours** (for a later milestone): a guild-level setting (start, end, IANA time zone via `time.LoadLocation`, evaluated as local wall-clock so daylight saving needs no special case); critical alerts (water leak, monitoring down) always exempt; non-critical alerts **suppressed** during the window and summarized by **one digest of the current state** at the end, which reuses the existing digest mechanism and needs no persistence; per-rule overrides later. The open decisions are the source of the time zone (a setting versus the host), suppression versus true deferral, and how `/notifications` presents schedules.

**Outbox:** revisit only with evidence of material restart losses; the design questions are an at-least-once contract, deduplication keys, a bounded write path, recovery ordering, and what to do with expired entries.

## 11. Implementation roadmap

Small, reviewable commits grouped in phases. Complexity: S (hours), M (about a day), L (several days). There are no dates.

### Phase 0: preparation (no behavior change)

- **T01. Split and clean the engine.** *Objective:* split `engine.go` into evaluator, digest, watcher and health files; delete the unused `ruleState.active`. *Files:* `internal/notify/engine*.go`. *Depends on:* none. *Behavior:* identical. *Tests:* the whole existing suite passes unchanged. *Acceptance:* no diff in test results; `engine.go` shrinks. *Complexity:* S. *Risks:* merge churn.
- **T02. Bounded latch slice.** *Objective:* replace `maxLatches = 2` in the mailbox with a bounded slice. *Files:* `internal/notify/mailbox.go`. *Depends on:* none. *Tests:* the existing mailbox tests plus a test with many rules on one entity. *Acceptance:* behavior unchanged for current rule shapes. *Complexity:* S. *Risks:* memory bound must stay explicit (at most the per-entity rule count).
- **T03. Shared test clock.** *Objective:* one fake clock for `notify`, `discord` and integration tests. *Files:* a test-only package, three test files. *Tests:* the existing suites. *Acceptance:* three copies removed. *Complexity:* S. *Risks:* import cycles in test packages.

### Phase 1: schema and model

- **T04. Migration 0004 and the store.** *Objective:* rebuild `notification_rules` with `clear_threshold`, `duration_seconds` and the new unique key; extend `Rule`, `NewRule`, the store and validation. *Files:* `migrations/0004_*.sql`, `internal/notify/{types,store,service}.go`, `internal/storage` tests. *Depends on:* none. *Tests:* golden-database upgrade, row identity, trigger, ID preservation, restart idempotence, older-binary refusal, constraint checks, validation table. *Acceptance:* criterion 1. *Complexity:* L. *Risks:* migration safety (mitigated by tests and the backup notice); decision D1.
- **T05. Command surface.** *Objective:* `for` and `clear` options, validation messages, `list` wording. *Files:* `internal/discord/commands/notifications.go` and tests. *Depends on:* T04. *Tests:* option parsing, every error message, no entity IDs, administrator-only autocomplete. *Acceptance:* criterion 7 (message) and the UX in 7.6. *Complexity:* M. *Risks:* Discord option limits.

### Phase 2: rule semantics

- **T06. Armed and episode state.** *Objective:* the explicit armed/episode state machine in the evaluator, as pure functions. *Files:* `internal/notify` evaluator. *Depends on:* T01, T04. *Tests:* table-driven episode transitions, differential test against v0.4.0 behavior for rules without `for` or `clear`. *Acceptance:* criterion 9 (no regression). *Complexity:* M. *Risks:* subtle re-arm differences, hence the differential test.
- **T07. Hysteresis.** *Objective:* `clear` semantics, the mailbox `sawClear` bit, seeding rules. *Files:* `internal/notify/{mailbox,evaluator}.go`. *Depends on:* T02, T06. *Tests:* bands, coalesced dip-and-return, seeding inside the band. *Acceptance:* criterion 6. *Complexity:* M. *Risks:* coalescing correctness.
- **T08. Duration timers.** *Objective:* the pending-episode heap, injected-clock waits, cancel on clear and on unreliable, re-read at `due`, sharing check at `due`, seeding from `last_changed`, late fire, reload handling. *Files:* `internal/notify` evaluator. *Depends on:* T06. *Tests:* criteria 2 to 5 in pure tests with a fake clock; reload and removal tests; goroutine and race checks. *Acceptance:* criteria 2 to 5, 8. *Complexity:* L. *Risks:* ordering between events and timers (both on one goroutine by design), restart semantics for numeric sensors (documented).
- **T09. Messages.** *Objective:* held-time wording, late wording, digest behavior for duration rules. *Files:* `internal/notify/message.go`, `internal/discord/notifytext.go`. *Depends on:* T08. *Tests:* formatting for every case, no entity IDs, escaping. *Acceptance:* alert text in 7.6. *Complexity:* S.

### Phase 3: operations and should-haves

- **T10. Health and status.** *Objective:* the counters and "waiting: N". *Depends on:* T08. *Tests:* counters under cancel, fire and late fire. *Complexity:* S.
- **T11 (S1). Audit refusals at the gate.** *Objective:* record refused administrator commands with fixed wording. *Files:* `internal/discord/commands/registry.go`, `internal/audit`. *Tests:* a non-administrator and a stranger produce the expected rows; nothing sensitive stored. *Acceptance:* the documented gap is closed. *Complexity:* S to M. *Risks:* it changes existing audit behavior and volume, so it needs an explicit maintainer decision (D4).
- **T12 (S3). Alert-channel validation freshness.** *Objective:* do not rely on cached bot roles. *Files:* `internal/discord/notifier.go`. *Tests:* stub tests with a stale-state simulation. *Complexity:* S.
- **T13 (S2). Currently violating in `status`.** *Objective:* list watched sensors now in an alert state (administrators only, names not IDs). *Depends on:* T06. *Tests:* shared and unshared entities, unreliable states. *Complexity:* M.

### Phase 4: verification and documentation

- **T14. Real Home Assistant integration tests.** *Objective:* duration with an injected clock, cancel on close, restart seeding, hysteresis through the virtual temperature setpoints. *Depends on:* T07, T08. *Acceptance:* all pass against the dev Home Assistant. *Complexity:* M.
- **T15. Stress and regression injections.** *Objective:* race, shuffle, 0.5 and 0.25 CPU stress, at least ten injected regressions, goroutine-leak checks. *Depends on:* T08. *Complexity:* M.
- **T16. Documentation and manual checks.** *Objective:* update `commands.md`, `design/notifications.md`, `setup.md` (0003 to 0004 upgrade), the changelog and the roadmap; add duration and hysteresis rows to the manual checklist. *Depends on:* all. *Complexity:* S to M.

### Decisions for the maintainers

- **D1.** Table rebuild (recommended) or columns only with the old unique key.
- **D2.** Keep leak rules immediate-only (recommended).
- **D3.** Accept the documented numeric restart semantics (late, never early).
- **D4.** Include S1 (changes existing audit behavior) in v0.5.0 or defer it.
- **D5.** Keep the silent first observation (recommended, with S2 as mitigation).

## 12. Definition of done

- Every acceptance criterion in section 9 is met and verified by automated tests; nothing partial is described as a full pass.
- `gofmt`, `go vet` (also with the `integration` tag), `go test -race -shuffle=on -count=1 ./...`, the real Home Assistant integration suite, constrained-CPU stress, the CI Compose validations and the image build all pass.
- The migration is proven on a golden v0.4.0 database and the upgrade notice is in the changelog and `setup.md`.
- The documentation set (commands, design, security notes, troubleshooting, setup, roadmap) matches the implementation, with known limitations stated plainly.
- New behavior has regression tests that fail when it is removed.
- The manual Discord checklist covers the new rows; any row not run is listed as not run.
- Release follows the maintainers' procedure in [CONTRIBUTING.md](../../CONTRIBUTING.md).
