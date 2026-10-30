# v0.5.0 design: rule semantics, safe start-up and a sensor capability registry

**Status: design approved for implementation planning. Decisions D1 to D5 are final. No v0.5.0 code exists.** This replaces the earlier proposal text (Git history keeps it). It builds on the shipped design in [notifications.md](notifications.md) and on the [architecture](../architecture.md).

## 1. Summary

- **Theme: richer rules and safer start-up.** Duration rules, hysteresis, unit-safe thresholds, initial-state alerting for critical hazards, and audit of unauthorized administrative commands.
- **Recommendation for the sensor model: a small typed Go capability registry as a prerequisite (option B), not a JSON catalog.** The audit shows sensor knowledge is hardcoded in a closed table of eight classes plus about ten per-category switches across three packages, and that "critical" is a literal `CategoryLeak` check in three places. Duration, hysteresis, hazard and unit rules each need a per-category property; implementing them with more switches would make that worse (sections 3, 6, 8).
- **Not in v0.5.0:** new sensor categories beyond an optional pair of one-row additions, a declarative catalog, smoke, gas and carbon monoxide alerts, quiet hours, a persisted outbox.
- **Migration 0004** rebuilds `notification_rules` (D1) and adds two small pieces of persistence needed by D5. Its SQL was executed against the real 0003 schema with adversarial data (section 12).

## 2. Approved decisions

| ID | Decision | Specified in |
| --- | --- | --- |
| D1 | Transactional table rebuild for migration 0004, preserving rules, IDs and constraints | sections 5.3, 12 |
| D2 | Water leak rules stay immediate-only; no assumption that every `moisture` class means the same thing | sections 4, 5.3, 9.5 |
| D3 | Late-never-early duration semantics after a restart; no alert before its duration has truly elapsed | section 9.3 |
| D4 | Audit unauthorized administrative command attempts, with reason codes, rate limits and bounded retention | section 11 |
| D5 | Replace the unconditional silent first observation with an initial-state alerting model | section 10 |

## 3. How sensor support works today (audit)

### 3.1 The path

Home Assistant state → **parse** (`monitor.ParseState`, `internal/monitor/update.go:77`: size limits, `device_class`, unit, name, timestamps) → **classify** (`monitor.Classify`, `classify.go:41`, a lookup in the `classes` map at `classify.go:28`; unclassified entities are dropped from monitoring, `tracker.go:152` and `:245`) → **interpret** (`Interpret`, `meaning.go:68`) → **share and read** (`entities.Permissions`; `monitor.Reader` re-checks sharing on every read) → **rule creation** (`notify.Service.AddRule`: shared, monitored, compatible, `notify/service.go:236-239`; `compatible` is at `notify/service.go:355`) → **evaluation** (`notify/rules.go`: `violates`, `enters`, cooldown) → **delivery** (`Message.Lane`, `message.go:100`; the dispatcher is category-agnostic).

Discovery, sharing and `/status` work for **any** entity of any domain; only the live monitoring and notification layers are limited to the classes below.

### 3.2 Supported today (verified inventory)

| Category | Domain · `device_class` | Value | Default cooldown | Notes |
| --- | --- | --- | --- | --- |
| temperature | `sensor` · `temperature` | number, any unit | 15 min | unbounded range |
| humidity | `sensor` · `humidity` | number 0 to 100 | 15 min | out of range is invalid |
| battery | `sensor` · `battery` | number 0 to 100 | 6 h | |
| battery_low | `binary_sensor` · `battery` | `on` = low | 6 h | |
| motion | `binary_sensor` · `motion` | `on` = detected | 5 min | |
| door | `binary_sensor` · `door` | `on` = open | 1 min | |
| window | `binary_sensor` · `window` | `on` = open | 1 min | |
| leak | `binary_sensor` · `moisture` | `on` = wet | 15 min | **critical lane**, fails loud from unavailable |

Everything else (every other class, `sensor` entities without a class, other domains) is **unclassified**: discoverable, shareable and readable through `/status`, but not monitored and not usable in rules.

### 3.3 The twelve questions

| # | Question | Answer | Evidence |
| --- | --- | --- | --- |
| 1 | Hardcoded in Go? | Yes: a closed table plus per-category switches | `classify.go:28`, `meaning.go:88`, `notify/rules.go` (`alertMeaning`, `defaultCooldown`, `categoryNoun`) |
| 2 | Recognized domains and classes | `sensor` and `binary_sensor`; the eight rows in 3.2. Control: `light` and `switch` only | `classify.go:28`, `entities/entity.go:32` |
| 3 | Generic logic | Numeric versus binary handling (`Numeric()`), numeric parsing and thresholds, rule compatibility by value type, sharing, permissions, discovery, delivery. There is **no generic fallback category** | `classify.go:52`, `notify/service.go:355` |
| 4 | Behavior or display? | Behavior: class decides category, meaning, lane, cooldown and compatibility. Also displayed in discovery and used in search | `discovery.go:53,101`, `wizard/panels_discover.go:96` |
| 5 | Hardcoded entity IDs or models? | None in production code (a repository-wide search for entity-ID literals and common vendor names finds nothing) | `git grep` |
| 6 | Unknown class? | Dropped from monitoring, hidden from `/sensors`, rule creation refused with `ErrNotMonitored`; still readable through `/status` | `tracker.go:152,245`, `notify/service.go:236` |
| 7 | Numbers and units | Plain decimals only (hex, underscores, NaN and infinity rejected); humidity and battery bounded 0 to 100; **units are stored and displayed but never validated or bound**; thresholds are unit-less | `meaning.go:103-117`, `monitoring.go:136`, `notifytext.go:107` |
| 8 | Unavailable, unknown, malformed | `unavailable` and `unknown` are unreliable; anything not `on`/`off` (binary) or not a plain number (numeric) is invalid and also unreliable; none is ever treated as safe | `meaning.go:68-100`, `rules.go:46` |
| 9 | Home Assistant semantics respected? | For the eight classes yes (section 4). Gaps: polarity is implicitly "on means alert" for every row, units are not validated, `last_changed` is display-only | `meaning.go:88` |
| 10 | Cost of a new type | Code: a row plus up to ten switch edits in three packages. Tests: nothing forces them (no exhaustiveness test). Database: none for a new *category* (derived at runtime); a new rule *kind* needs a migration. Discord UI: autocomplete label and rule wording | see 3.4 |
| 11 | Arbitrary shared entities? | No. Rules exist only for shared, monitored, classified sensors | `notify/service.go:236-239` |
| 12 | Duplicated classification? | Yes: `monitor` (`classes`, `Label`, `Numeric`, `binaryMeanings`, `Text`), `notify` (`alertMeaning`, `defaultCooldown`, `categoryNoun`, `Lane`, the leak exception in `enters`, `compatible`) and `discord` (the rule-wording switch at `commands/notifications.go:344`, the noun at `notifyadmin.go:116`) | grep of `Category` references |

### 3.4 Findings that shape the design

1. **Knowledge is scattered.** The comment in `classify.go` says a new type is "one row here plus its interpretation in `meaning.go`"; in fact it also touches `notify` and `discord`, and no test fails if a switch is forgotten (the defaults happen to fail closed).
2. **"Critical" is a literal.** The critical lane (`message.go:100-113`), the fail-loud exception for unreliable-to-wet (`rules.go`, `enters`) and the rule wording in `/notifications list` (`notifications.go:344`) all special-case `CategoryLeak`. Smoke, gas or carbon monoxide would need a property, not another literal.
3. **Polarity is assumed.** Every mapped binary class means "alert when `on`". Home Assistant has classes where `on` is the healthy state (section 4).
4. **Units are a false-alert source.** The sensor base class converts values to the unit the user chose, so a unit can change under an existing rule; temperature may also be `K`. A threshold of 30 stays 30 while a sensor switches from °C to °F, and `withUnit` relabels the threshold with the sensor's *current* unit.
5. **Dead and rigid state:** `ruleState.active` is never read; the mailbox allows two latches per entity (`maxLatches`).
6. **Hazards at start-up are silent** (`reconcileStates`, `engine.go:272`).
7. **Denied administrative commands are not audited** (`interaction.go:77`, `registry.go:160`).

## 4. Home Assistant semantics we rely on

Sources are listed at the end. NeptHass uses only documented fields; no reliance on undocumented behavior was found, with one soft assumption (a class is normally retained on unavailable states, which `retainClass` already handles).

| Field | Documented behavior | Use in NeptHass | False-notification risk and mitigation |
| --- | --- | --- | --- |
| `entity_id`, domain | stable identifier; the domain is the prefix | key, domain filter | none |
| `device_class` | set by the integration, or by the user (templates and entity settings); decides labels and where the entity is used | classification | A user can label anything `moisture`. The class is trusted for meaning; **hazard status is explicit per registry row** and never inferred. See D2 below |
| `state` (binary) | `on` or `off`, plus `unavailable` and `unknown` | meaning per class | polarity differs per class (table below); registry must declare it |
| `state` (numeric) | a string holding a number for numeric classes | parsed strictly | anything else is invalid and unreliable |
| `unavailable` / `unknown` | `unavailable`: cannot provide its state (device unreachable, or integration not set up). `unknown`: no value yet | unreliable | never "safe"; hazards fail loud on recovery |
| `restored` | `true` when the integration has not set the entity up; the state is then `unavailable` | none | handled as unavailable |
| `unit_of_measurement` | the user's chosen unit; the sensor base class converts native values; temperature can be `°C`, `°F` or `K`; `humidity` and `battery` are `%` | displayed only | **bind the unit to the rule** (section 9.4) |
| `last_changed` | when the *state* last changed; not updated for attribute-only changes; entities with force update refresh it on every report; resets when Home Assistant restarts the entity | display only | **never used to start a duration** (D3) |
| `last_updated` | when the state or attributes last changed | event ordering | none |
| `state_changed` event, `get_states` | `old_state` and `new_state` may be null; the snapshot is authoritative | as designed in v0.3.0 | none |
| Start-up behavior | after a Home Assistant restart, entities may report `unavailable` first, and sleeping devices keep their **last known** state until they report | baseline handling | a stale hazard state is possible; D5 tells the reader the state is Home Assistant's report |

**Binary classes: `on` does not always mean "alert"** (Home Assistant core, `binary_sensor/const.py`): `battery` on = low; `connectivity` on = connected; `door`, `garage_door`, `opening`, `window` on = open; `lock` on = open (unlocked); `moisture` on = wet; `motion` on = detected; `occupancy` on = occupied; `plug` on = plugged in; `power` on = power detected; `presence` on = home; `problem` on = problem; `running` on = running; `safety` on = unsafe; `smoke`, `gas`, `carbon_monoxide` on = detected; `tamper`, `vibration`, `sound`, `glass_break` on = detected. A binary sensor without a class only has `on` and `off`, with no meaning.

**D2: `moisture`.** `binary_sensor` + `moisture` is documented as "on means wet, off means dry"; Home Assistant has no separate leak class. The `sensor` domain also has a `moisture` class, but it is a percentage (soil moisture) and is not mapped. NeptHass therefore keeps `binary_sensor` + `moisture` as the hazard "water detected" and keeps `sensor` + `moisture` unclassified (a test pins both). Over-alerting on a non-leak moisture sensor is the accepted, safe direction.

## 5. Capability model and the typed registry

### 5.1 Capabilities

Rules are derived from what a sensor *can do*, declared once per class in Go, not from names or switches:

| Capability | Meaning | Allowed when |
| --- | --- | --- |
| `StateAlert` | alert when the declared alert state is entered | binary class |
| `StateDuration` | alert when it has held for D | binary class, not a hazard |
| `Threshold` | alert above or below a number | numeric class |
| `ThresholdDuration` | the same, held for D | numeric class, not a hazard |
| `Hysteresis` | a separate clear value for re-arming | numeric class |
| `Critical` (hazard) | critical lane, immediate only, fails loud from unreliable, initial-state alert (D5) | explicit per class |
| `Recovery` | "back to normal" notice | **not in v0.5.0** |

### 5.2 The registry

One table in `internal/monitor/registry.go`, replacing `classes`, `Label`, `Numeric`, `binaryMeanings` and the per-category switches in `notify` and `discord`:

```go
type Spec struct {
    Category    Category      // stable name; never stored in the database
    Domain      string        // "sensor" or "binary_sensor"
    DeviceClass string        // Home Assistant device_class
    Label, Noun string        // "Door", "Door sensor"
    Value       ValueKind     // Binary or Numeric
    AlertState  string        // binary: the raw state that means "alert" ("on" or "off")
    On, Off     Meaning       // binary: meaning of each raw state
    Range       *[2]float64   // numeric: plausible bounds, for example {0, 100}
    Units       []string      // numeric: documented units (informational in v0.5.0)
    Cooldown    time.Duration // default cooldown
    Hazard      bool          // critical, immediate-only, fail-loud, initial-state alert
    Caps        Caps          // the capabilities above
}
```

`Lookup(domain, class) (Spec, bool)` and `All()` are the only entry points; `Classify` delegates to them. Nothing in `notify` or `discord` switches on a category name any more.

### 5.3 Fail-closed rules

- **No row, no support.** An unknown class is unclassified and cannot be monitored or used in rules (today's behavior, kept). There is **no generic fallback** in v0.5.0, so an unknown sensor can never be classified as critical or have arbitrary text read as a number.
- `Hazard` is **explicit per row** and is never derived from a class name pattern. D2 follows: a `Hazard` row has no duration or hysteresis capability, and creating such a rule fails with a specific message ("water leak alerts are always immediate").
- Unsupported capabilities fail closed with a clear message, never silently ignored (for example "that sensor type does not support a duration").
- **Registry invariants, enforced by tests:** every `Category` constant has exactly one row; keys are unique; a hazard has no duration or hysteresis capability; a numeric row has threshold capabilities and a binary row has `StateAlert`; `AlertState` is `on` or `off`; labels and nouns are non-empty; percent classes declare a range; the registry is covered by a golden listing so any change is visible in review.
- **Rows for today's eight classes** reproduce the table in 3.2 exactly (`AlertState` is `on` for all of them), so the refactor changes no behavior; a differential test compares the old and new classification and interpretation over a generated grid of domains, classes, states and units.

## 6. Catalog decision: typed Go registry, not JSON

| Option | Type safety | One source of truth | Contributor effort | Safety surface | Verdict |
| --- | --- | --- | --- | --- | --- |
| Current hardcoded | compile-time, but scattered | no (three packages) | high and error-prone | none new | replace |
| Embedded JSON catalog | runtime only | no (data in JSON, behavior in Go) | a second place to edit and keep in sync | schema and parser code to maintain | not justified |
| YAML catalog | runtime only | no | as JSON | **new dependency** (the project is standard-library first) | rejected |
| **Typed Go registry** | compile-time | **yes** | one row and its tests | none new | **chosen** |
| Hybrid (Go engine + JSON metadata) | split | no | as JSON | as JSON | not justified |

Why not JSON: there are eight classes, every contribution that matters is a Go change anyway (rule logic, message wording, tests), and the properties that carry safety (polarity, hazard, capabilities) must stay in Go beside the engine that enforces them. A catalog would add a parser, schema validation, startup failure modes and a second synchronization point without removing any Go. **External configuration is rejected outright**: an editable file could flip a hazard flag, polarity or unit and so bypass safety constraints, and it creates a version-compatibility surface for no benefit.

Revisit only if (a) the registry grows past roughly 25 mostly declarative rows, or (b) localization of labels becomes a requirement. If that happens the catalog must be **embedded at build time**, versioned (`"schema"` field), data only (no expressions, no executable logic, no external override), validated at startup with a hard failure on invalid data, and covered by the same invariant tests as the registry. Because no catalog is adopted, this document contains no JSON schema or example.

## 7. Capability matrix (current versus proposed)

D = discovery, R = read (`/status`; `/sensors` when classified), Rule = rule configuration, N = runtime notification. For every class below **D and R already work** (discovery and `/status` are domain-agnostic); the columns show what is missing. Priority: P0 supported and kept, P1 optional one-row addition in v0.5.0, P2 later via one registry row, S needs a safety review first, X not planned.

| Category | Domain · class · state and unit (Home Assistant) | Today (Rule / N) | Proposed | Safety, priority |
| --- | --- | --- | --- | --- |
| Door | `binary_sensor` · `door` · on = open | yes / yes | keep, add duration | P0 |
| Window | `binary_sensor` · `window` · on = open | yes / yes | keep, add duration | P0 |
| Garage door | `binary_sensor` · `garage_door` · on = open | no / no | one row, duration useful | P1, not a hazard |
| Opening | `binary_sensor` · `opening` · on = open | no / no | one row | P1 |
| Motion | `binary_sensor` · `motion` · on = detected | yes / yes | keep, add duration | P0 |
| Occupancy | `binary_sensor` · `occupancy` · on = occupied | no / no | one row | P2 |
| Presence | `binary_sensor` · `presence` · on = home, off = away | no / no | needs `AlertState = off`; privacy review | X for now |
| Water leak | `binary_sensor` · `moisture` · on = wet | yes / yes | keep, **immediate only**, initial-state alert | P0, hazard (D2) |
| Soil moisture | `sensor` · `moisture` · `%` | no / no | stays unclassified (pinned by a test) | X |
| Smoke, gas, carbon monoxide | `binary_sensor` · `smoke`, `gas`, `carbon_monoxide` · on = detected | no / no | would be hazards | **S**: life-safety; NeptHass is not an alarm system, so a deliberate product and safety decision comes first |
| Temperature | `sensor` · `temperature` · `°C`, `°F` or `K` | yes / yes | keep; add duration, hysteresis, **unit binding** | P0 |
| Humidity | `sensor` · `humidity` · `%` | yes / yes | same | P0 |
| Battery | `sensor` · `battery` · `%`; `binary_sensor` · `battery` · on = low | yes / yes | keep; numeric gets duration, hysteresis, unit binding | P0 |
| Battery charging | `binary_sensor` · `battery_charging` · on = charging | no / no | one row if wanted | P2 |
| Power, energy, voltage, current | `sensor` · `power` (`W`, `kW`, ...), `energy` (cumulative), `voltage`, `current` | no / no | numeric rows later; units must be bound first; `energy` is a poor threshold target | P2 |
| Pressure, air quality, CO₂, VOC, PM2.5 | `sensor` · `pressure`, `atmospheric_pressure`, `aqi`, `carbon_dioxide`, `volatile_organic_compounds`, `pm25` (units per Home Assistant) | no / no | numeric rows later (`Range`, `Units`) | P2 |
| Illuminance | `sensor` · `illuminance` · `lx` | no / no | numeric row; thresholds are diurnal | P2 |
| Vibration, tamper | `binary_sensor` · `vibration`, `tamper` · on = detected | no / no | one row each | P2 |
| Connectivity | `binary_sensor` · `connectivity` · on = connected | no / no | needs `AlertState = off` | P2 |
| Problem | `binary_sensor` · `problem` · on = problem | no / no | one row | P2 |
| Generic binary (no class) | `binary_sensor` with no class: only `on`/`off`, no meaning | no / no | stays unsupported: there is nothing to interpret | X |
| Generic numeric | `sensor` with a unit but an unknown class | no / no | a fallback spec could be added later (unit required, never a hazard); deferred | P2 |

Recognizing a class is not a claim about every physical model: the registry reflects Home Assistant's documented semantics, and a sensor that Home Assistant mislabels will be treated as labelled.

## 8. Scope

| Option | Assessment |
| --- | --- |
| A. Keep the architecture, add the rule features | Each feature adds another per-category switch (duration allowed, hysteresis allowed, hazard, unit policy) and extends the duplication already found. Rejected |
| **B. Small typed capability registry as a prerequisite** | One table replaces about ten switches, makes D2 and D5 properties instead of literals, and gives durations, hysteresis and units a place to live. About one file plus mechanical edits. **Recommended** |
| C. Declarative JSON catalog | Not justified by the evidence (section 6). Rejected |
| D. Defer the registry, do only targeted improvements | Viable for durations alone, but D2, D5 and units each need the same per-category property, so the targeted fixes would be the registry built three times. Rejected |

**Must-have:** the registry (T01) and its refactor with no behavior change; migration 0004 (D1); duration rules; hysteresis; unit binding; initial-state alerting (D5); authorization audit (D4); tests, documentation and upgrade notes.

**Should-have (may slip):** `garage_door` and `opening` rows (they prove the registry with one row each); `/notifications status` listing sensors currently in an alert state; alert-channel validation that does not trust cached bot roles.

**Deferred:** a catalog, new sensor categories beyond the optional pair, smoke, gas and carbon monoxide, quiet hours, a persisted outbox, per-rule channels, notification history, configurable staleness bound, idempotent retries through Discord's message nonce (discordgo v0.29.0 has no field for it).

## 9. Rule semantics

### 9.1 Model

`Rule` gains `Duration` (absent or zero means immediate), `Clear` (numeric rules only) and `Unit` (numeric rules only, captured when the rule is created). Definitions: a sensor is *violating* when it reliably meets the rule's condition; an **episode** is an unbroken run of reliable violating readings; a rule is **armed** when it may fire, disarms when it fires and re-arms as below. Cooldown applies exactly as in v0.4.0 and a suppressed alert is consumed, never deferred.

### 9.2 Hysteresis

For `above` with threshold `T` and `clear` value `C < T`: fire when a reliable reading exceeds `T` while armed, re-arm only when a reliable reading is **below `C`**; readings between `C` and `T` neither fire nor re-arm. `below` is mirrored (`C > T`). Without `clear` the v0.4.0 behavior is unchanged. With a duration, the pending phase uses only the trigger threshold. The mailbox additionally records that a reading crossed `clear` since the last drain, so a dip and return inside one coalescing window re-arms correctly. `ruleState.active` is replaced by an explicit armed and episode state.

### 9.3 Duration (D3)

For `for D`: when an episode begins and the rule is armed, record `start` and schedule `due = start + D`. A reliable non-violating reading, or an unreliable one, cancels the pending episode (continuity is unknown, so nothing is guessed). At `due` the evaluator **re-reads the current sensor**; if it is still reliably violating and sharing allows it, the rule fires, otherwise nothing is sent.

**Where `start` comes from decides whether an alert can be early, so NeptHass uses only its own clock:**

- **Live episode** (a transition NeptHass observed): `start` is the time NeptHass received the change.
- **Any other episode** (restart, reconnect, a rule created or reloaded while the sensor was already violating): `start` is the time NeptHass **first saw** the sensor violating.
- Home Assistant's `last_changed` is **not** used to shorten a duration: it moves with every value change for numeric sensors, is refreshed by every report on force-update entities, resets when Home Assistant restarts, and comes from another clock. Because `start` can only be at or after the true start, **an alert can never fire before the duration has really elapsed**. It can fire late.

Precision and limits: second granularity; delivery latency is not included; timers are in memory, so a restart discards elapsed time (a door open 9 of 10 minutes before a restart alerts 10 minutes after it); a numeric sensor that has been violating for hours before NeptHass starts alerts `D` after start. Messages say "for at least D" when `start` is not a live observation. An optional refinement using `last_changed` guarded by a measured clock-skew check is listed as an open decision (U3), not planned.

### 9.4 Unit binding

A numeric rule stores the sensor's unit at creation. A rule whose sensor now reports a different unit is **dormant**: it does not fire, does not re-arm, shows "unit changed from °C to °F; create the rule again" in `/notifications list`, counts as `incompatible` in `status`, and resumes if the unit returns. The notification text uses the rule's bound unit. Rules created before v0.5.0 have an empty unit; they are bound once, by a one-time audited update, at the first reload in which their sensor is reporting (open decision U1).

### 9.5 Hazard rules (D2)

A `Hazard` row allows only an immediate state rule. `AddRule` with a duration, or with `clear`, is refused with a specific message. Hazard alerts use the critical lane, fail loud from unavailable or unknown, and take part in the initial-state model (section 10).

## 10. Initial-state alerting (D5)

### 10.1 Principles

1. **Events alert; states summarize.** At start-up NeptHass has states, not events.
2. **Observing a state does not prove when it started**, and it does not prove the state is current (sleeping devices keep their last known state), so messages say what Home Assistant reports and what NeptHass does not know.
3. **Critical hazards never wait.** They alert at the first reliable observation.
4. **Noise is bounded**: one summary per start, a hazard budget, persistence only where it prevents duplicates of the most important alerts.

### 10.2 Observation contexts

| Context | How it is recognized | v0.4.0 | v0.5.0 |
| --- | --- | --- | --- |
| Live transition (reliable non-violating to violating) | change source live or replay, previous state known | alert | unchanged |
| Reconnect inside one process | snapshot change with a previous state | alert, marked late | unchanged |
| **Process baseline** | a sensor's first reliable observation since NeptHass started | silent | initial-state handling (10.3) |
| **New rule on a violating sensor** | the rule is created or reloaded while the sensor is violating | silent | initial-state handling; the creating administrator is also told in the reply |
| Unreliable to violating (not a hazard) | previous state unavailable, unknown or invalid | silent | unchanged |
| Unreliable to wet (hazard) | | alert, marked uncertain | unchanged |

### 10.3 Handling a rule whose sensor is violating at baseline

| Rule | Action |
| --- | --- |
| Hazard (critical, immediate-only) | **Alert at once** on the critical lane: "already detected when NeptHass started; Home Assistant last updated it at ...; when it began is unknown". Suppressed only by a delivery mark (10.5) |
| Immediate, not a hazard | No individual alert. Listed in the **start-up summary** (10.4) |
| Duration, not a hazard | Episode seeded with `start` = first sighting (9.3); fires normally at `due`. Not in the summary |
| Dormant, paused, not shared, not monitored, incompatible, notifications disabled | Skipped |

Unavailable and unknown sensors are never reported as alerts. `/notifications status` shows how many watched sensors are not reporting.

### 10.4 Start-up summary

One message, normal lane, 10 minute TTL, at most 10 lines plus "and N more", naming sensors by display name (never entity IDs). It is sent **once**, at the end of a **baseline window** (2 minutes after the first completed synchronization) so slow sensors after a Home Assistant restart settle first. Wording states that the listed sensors were already in an alert state when NeptHass started, that when they changed is unknown, and shows what Home Assistant reports. Sharing is re-checked per line before sending, like a digest. A sensor whose first reliable observation arrives after the window is baselined silently (a hazard still alerts immediately).

### 10.5 Persistence requirements (explicit)

Everything else stays in memory. Two small pieces are persisted, both written only after a **successful delivery** so a hazard that was never delivered is announced again after a restart:

- **`notification_marks`** (one row per hazard rule, at most 200): `episode_key` (Home Assistant's `last_changed` of the violating state, used only as an *identity* for "the same episode", never as a time claim) and `delivered_at`. A baseline hazard alert is suppressed when the sensor's current `last_changed` equals the stored key **or** an alert was delivered within the rule's cooldown. A Home Assistant restart changes `last_changed`, so a still-wet sensor is announced again after the cooldown, which is a useful reminder rather than a duplicate. A failed mark write is logged and does not block delivery.
- **`notification_settings.startup_summary_at`**: the last summary delivery time; a summary is not sent again within 10 minutes, so a crash loop cannot flood the channel.

### 10.6 Storm control

At most **3 individual hazard alerts per start**; further hazards are folded into one critical message. All non-hazard baseline states go into the single summary. Duplicate hazard alerts across restarts are bounded by the marks. A restart storm costs at most one summary per 10 minutes plus the marks' protection.

### 10.7 Permissions and enablement

The same pipeline applies: a rule must be enabled, the sensor shared at `read` or higher and monitored, notifications enabled and a channel set; sharing is read again before every send. Nothing is produced for a sensor the administrators have not shared.

### 10.8 Worked cases

- **Fresh install.** No marks, no summary timestamp. A wet leak sensor alerts once; open doors appear in one summary.
- **Crash loop with a wet sensor.** The first start alerts and records a mark on delivery; later starts within the cooldown or with the same `last_changed` send nothing.
- **Home Assistant restarts, NeptHass keeps running.** A reconnect, not a start: v0.4.0 behavior (late alerts, silent unreliable transitions, fail-loud for hazards). Door sensors that were open and return from `unavailable` produce no alert.
- **Administrator adds a leak rule on a sensor that is wet right now.** The rule alerts immediately, and the reply says so.

## 11. Authorization audit (D4)

`command_audit_logs` already supports this: `outcome` allows `denied`, `entity_id` is nullable, and `action` and `detail` are free text. **No schema change.**

| Aspect | Specification |
| --- | --- |
| What is recorded | A refused **administrative** command (a command whose minimum level is administrator: `permissions`, `audit`, `setup`, `sync`, `notifications`) |
| Reasons (fixed vocabulary) | `not_admin`: an allowed user who is not an administrator (`registry.go:160`). `not_allowed`: a user in the configured guild who is on no allowlist (`interaction.go:77`) |
| Not recorded | Interactions from other guilds, direct messages, autocomplete, and non-administrative commands from strangers (they are logged as today). Reason: attacker-controlled volume with no investigative value |
| Row | `outcome = denied`, `actor_id` = the Discord user ID, `command` = the command name, `action` = the subcommand name or empty, `entity_id` = NULL, `detail` = `reason=<code>` and, when rate limiting dropped rows, `suppressed=<N>` |
| What is never stored | Option values (they may hold entity IDs or free text), channel names, user input, Discord error text, tokens |
| No protected information leaks | The reply to the user stays generic and identical in form to today's; the audit row is visible only to administrators through `/audit` |
| Rate limit | Per actor: a burst of 3 rows then 1 per minute. Global: at most 60 rows per hour. Dropped rows increment an in-memory counter that is folded into the next row as `suppressed=N` |
| Bounded retention | The existing retention prune (default 90 days) plus a cap of 5,000 retained denied rows (oldest first). Worst case is a few megabytes |
| Failure | An audit write failure is logged and never changes the reply |
| Hooks | `Registry.Run` (level check) and `Bot.reject` (strangers), through one small recorder that owns the limiter |

Tests: every identity × administrative command combination yields the right row or none; rate limits and the global cap hold; no option value or secret appears in any row; autocomplete and other-guild interactions write nothing; replies are unchanged.

## 12. Migration 0004 (D1)

One migration, one transaction (the runner already applies each migration in a transaction). It rebuilds `notification_rules` for the new unique key and columns, and adds the two pieces of D5 persistence. **This SQL was executed against the real 0003 schema with adversarial data and passed all checks** (SQLite 3.53.4); the Go migration test (T05) must repeat them on the project's driver.

```sql
CREATE TEMP TABLE _rules_seq AS SELECT seq FROM sqlite_sequence WHERE name = 'notification_rules';

CREATE TABLE notification_rules_new (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_id        TEXT NOT NULL CHECK (length(entity_id) BETWEEN 3 AND 255),
    kind             TEXT NOT NULL CHECK (kind IN ('state', 'above', 'below')),
    threshold        REAL CHECK (threshold IS NULL OR (threshold BETWEEN -1000000 AND 1000000)),
    clear_threshold  REAL CHECK (clear_threshold IS NULL OR (clear_threshold BETWEEN -1000000 AND 1000000)),
    unit             TEXT NOT NULL DEFAULT '' CHECK (length(unit) <= 32),
    duration_seconds INTEGER NOT NULL DEFAULT 0 CHECK (duration_seconds BETWEEN 0 AND 86400),
    enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    cooldown_seconds INTEGER CHECK (cooldown_seconds IS NULL OR cooldown_seconds BETWEEN 0 AND 86400),
    created_by       TEXT NOT NULL CHECK (length(created_by) > 0),
    created_at       TEXT NOT NULL,
    updated_by       TEXT NOT NULL CHECK (length(updated_by) > 0),
    updated_at       TEXT NOT NULL,
    UNIQUE (entity_id, kind, duration_seconds),
    CHECK ((kind = 'state' AND threshold IS NULL AND clear_threshold IS NULL)
        OR (kind IN ('above', 'below') AND threshold IS NOT NULL)),
    CHECK (clear_threshold IS NULL
        OR (kind = 'above' AND clear_threshold < threshold)
        OR (kind = 'below' AND clear_threshold > threshold))
);

INSERT INTO notification_rules_new
    (id, entity_id, kind, threshold, enabled, cooldown_seconds, created_by, created_at, updated_by, updated_at)
SELECT id, entity_id, kind, threshold, enabled, cooldown_seconds, created_by, created_at, updated_by, updated_at
FROM notification_rules ORDER BY id;

DROP TRIGGER notification_rules_limit;
DROP TABLE notification_rules;
ALTER TABLE notification_rules_new RENAME TO notification_rules;

CREATE TRIGGER notification_rules_limit
BEFORE INSERT ON notification_rules
WHEN (SELECT count(*) FROM notification_rules) >= 200
BEGIN
    SELECT RAISE(ABORT, 'notification rule limit reached');
END;

INSERT INTO sqlite_sequence (name, seq)
    SELECT 'notification_rules', seq FROM _rules_seq
    WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'notification_rules');
UPDATE sqlite_sequence SET seq = (SELECT seq FROM _rules_seq)
    WHERE name = 'notification_rules' AND (SELECT seq FROM _rules_seq) > seq;
DROP TABLE _rules_seq;

CREATE TABLE notification_marks (
    rule_id      INTEGER PRIMARY KEY REFERENCES notification_rules (id) ON DELETE CASCADE,
    episode_key  TEXT NOT NULL CHECK (length(episode_key) <= 64),
    delivered_at TEXT NOT NULL
) STRICT;

ALTER TABLE notification_settings ADD COLUMN startup_summary_at TEXT;
```

Invariants (all verified by the run above):

- Rules survive **row for row** (a SHA-256 over every preserved column matched), keep their IDs, and become `duration_seconds = 0`, `unit = ''`, no `clear_threshold`. Existing behavior is unchanged.
- The **autoincrement sequence is preserved** (with deleted rules the next ID is 8, not 4; also when the table was emptied), so a rule ID is never reused in the audit trail.
- The **200-rule trigger** is recreated and enforced; "immediate and 10-minute rules on one entity and kind" are accepted and a duplicate of the same triple is rejected; `clear` on a state rule, on the wrong side of the threshold, and durations above 24 hours are rejected.
- `notification_settings` is preserved with the new column `NULL`; deleting a rule deletes its mark (`ON DELETE CASCADE`); `foreign_key_check` is empty and `integrity_check` is `ok`.
- **Rollback on failure:** a failure injected mid-migration leaves rows, sequence and schema exactly as in 0003, with no leftover tables.
- Idempotency and immutability are the runner's job (checksum, applied once); the Go test also checks that a second `Migrate` is a no-op and that a v0.4.0 binary refuses a 0004 database ("schema is newer"), so upgrade notes say **back up first**. The existing index on rules is the unique constraint, which this migration replaces by design.

## 13. Discord UX

- `/notifications add entity [above] [below] [clear] [for] [cooldown]`: `for` in whole minutes (1 to 1440, omitted means immediate) and `clear` a number, both optional and validated server-side. Autocomplete stays administrator-only and shows names, never IDs.
- Errors are specific: duration or `clear` on a hazard ("water leak alerts are always immediate"), `clear` on the wrong side or on a state rule, a duration on a class that has no duration capability, a duplicate of the same entity, kind and duration, and a rule on an unclassified sensor.
- `list` states rules in words ("when it opens and stays open for 10 minutes", "rises above 30 °C (re-arms below 28 °C)") and shows dormant states ("unit changed from °C to °F").
- Replying to `add` on a sensor that is **already in its alert state** says so (ephemeral, to the administrator); hazards also alert (10.2).
- Alert text states the held time ("for 10 minutes", or "for at least 10 minutes" when `start` was not a live observation) and keeps the late, uncertain and cooldown lines. The start-up summary and the hazard-at-start-up alert say what Home Assistant reports and that when it began is unknown.
- `status` gains: waiting episodes, sensors not reporting, sensors currently in an alert state (should-have), start-up counters.

## 14. Failure handling, security, observability, performance

| Area | Design |
| --- | --- |
| Failure | A missed event is corrected at `due` by re-reading the tracker; a reload failure keeps the previous rules and timers; a failed mark write is logged and delivery continues; a migration failure rolls back and startup stops with the database untouched |
| Security | No new authorization surface; sharing is read before an alert is queued and before every send; durations are bounded (1 minute to 24 hours) and the 200-rule limit bounds timers; denial audit stores no input; the registry cannot be changed at runtime |
| Observability | Counters for pending, fired, cancelled and late episodes; baseline hazard alerts, suppressed duplicates, summaries; denied-command rows and suppressed counts; shown in `/notifications status` |
| Performance | At most 200 pending timers in one heap on the evaluator goroutine; registry lookups are map reads; no extra work per event for rules without a duration; marks are written only when a hazard alert is delivered |

## 15. Testing strategy

- **Registry:** invariants (5.3), the golden listing, and a differential test of old versus new classification and interpretation over a generated grid; a test that `sensor` + `moisture` stays unclassified.
- **Pure semantics (table-driven, injected clock):** episode start, cancel on clear and on unavailable, fire at `due`, re-read at `due`, cooldown, re-arm, hysteresis bands including a dip and return inside one coalescing window, never-early for every start context, unit binding and dormancy.
- **Properties:** a rule never fires twice without a re-arm; a duration rule never fires before `D` elapsed on NeptHass's clock; a rule without `for`, `clear` or a unit change behaves exactly as in v0.4.0 (differential test against the v0.4.0 evaluator).
- **Initial-state alerting:** every cell of 10.2 and 10.3, the baseline window, the hazard budget, mark-based suppression, the summary rate limit, a crash loop, unavailable and unknown, unshared and disabled rules, and a failed mark write.
- **Migration:** the checks in section 12 on the project's driver, with a golden v0.4.0 database and an injected failure.
- **Authorization audit:** section 11.
- **Concurrency:** race detector, shuffled order, stress at 0.5 and 0.25 CPU, goroutine-leak checks, and the monitor callback still never blocks.
- **Real Home Assistant:** door duration with an injected clock, cancellation on close, hysteresis through the virtual temperature setpoints, a hazard already wet at start-up using the virtual leak sensor, and a restart with marks.
- **Regression injections:** at least 25 deliberate regressions of the new logic, each caught by a test.
- **Manual Discord rows** for durations, hysteresis, the start-up summary and denial audit; anything not run is listed as not run.

## 16. Acceptance criteria

1. The registry reproduces today's classification exactly: all existing tests pass unchanged, the differential test passes, and no switch on a category name remains in `notify` or `discord`.
2. Migration 0004 upgrades a v0.4.0 database with every rule, ID, threshold, cooldown and the ID sequence preserved; a failure rolls back completely; the 200-rule limit still holds.
3. A duration rule alerts once per episode only after the condition has held for its duration on NeptHass's clock, and never earlier in any start context (live, reconnect, restart, new rule).
4. A condition that clears, or becomes unavailable, before the duration expires sends nothing.
5. A hysteresis rule re-arms only past its clear value; readings in the band never re-alert; a dip below `clear` inside one coalescing window re-arms.
6. A rule whose sensor changes unit goes dormant with a clear message and resumes when the unit returns; no alert is ever produced by comparing values in different units.
7. Water leak rules stay immediate-only; a duration or `clear` on a hazard is refused with a clear message; `sensor` + `moisture` stays unclassified.
8. At start-up a wet leak sensor alerts once; other sensors already in an alert state appear in one summary; a crash loop produces no repeat hazard alert and at most one summary per 10 minutes; unavailable sensors never alert; nothing is sent for unshared, disabled or paused rules.
9. A reconnect keeps v0.4.0 behavior.
10. Unauthorized administrative command attempts are audited with reason codes, within the rate limits and the retention cap, without any option value or secret, and replies are unchanged.
11. No new race, goroutine leak or blocking monitor callback; stress passes at 0.5 and 0.25 CPU; at least 25 injected regressions are caught.
12. Documentation, changelog, upgrade notes and the manual checklist match the implementation, including the changed first-observation behavior.

## 17. Implementation tasks

Small commits in phases. Complexity: S (hours), M (about a day), L (several days). No dates.

**Phase 0: preparation (no behavior change)**

- **T01 Registry.** Add `registry.go`; replace `classes`, `Label`, `Numeric`, `binaryMeanings`, `notify.alertMeaning`, `defaultCooldown`, `categoryNoun`, the lane and fail-loud literals, `compatible`, and the `discord` rule-wording and noun switches. *Files:* `internal/monitor/{classify,meaning,registry}.go`, `internal/notify/{rules,message,service}.go`, `internal/discord/{commands/notifications.go,notifyadmin.go}`. *Depends:* none. *Tests:* invariants, golden listing, differential grid, all existing suites unchanged. *Complexity:* M. *Risk:* behavior drift (the differential test is the guard).
- **T02 Engine split and dead state.** Split `engine.go` into evaluator, digest, watcher and health files; delete `ruleState.active`. *Complexity:* S.
- **T03 Latch slice.** Replace `maxLatches` with a bounded slice. *Tests:* many rules on one entity. *Complexity:* S.
- **T04 Shared test clock.** One fake clock for `notify`, `discord` and integration tests. *Complexity:* S.

**Phase 1: schema and model**

- **T05 Migration 0004.** The SQL in section 12, store and model changes. *Depends:* none. *Tests:* every invariant in section 12 on the project's driver, golden database, injected failure, second `Migrate` no-op, older binary refusal. *Complexity:* L. *Risk:* migration safety.
- **T06 Model, validation, unit binding.** `Rule.Duration`, `Clear`, `Unit`; validation from registry capabilities; one-time audited binding of legacy rules (U1). *Depends:* T01, T05. *Complexity:* M.
- **T07 Command surface.** `for` and `clear` options, error messages, `list` wording, dormant states. *Depends:* T06. *Complexity:* M.

**Phase 2: rule semantics**

- **T08 Armed and episode state.** The explicit state machine as pure functions, differential test against v0.4.0. *Depends:* T02, T06. *Complexity:* M.
- **T09 Hysteresis.** `clear` semantics and the mailbox crossing bit. *Depends:* T03, T08. *Complexity:* M.
- **T10 Duration timers.** Pending-episode heap, injected-clock waits, cancel, re-read at `due`, never-early start rules, reload handling. *Depends:* T08. *Complexity:* L. *Risk:* ordering between events and timers (one goroutine by design).
- **T11 Messages.** Held-time wording, "at least", unit mismatch and dormancy text. *Depends:* T10. *Complexity:* S.

**Phase 3: initial-state alerting (D5)**

- **T12 Baseline contexts.** Per-sensor baseline detection, hazard initial alert, new-rule-on-violating-sensor handling. *Depends:* T01, T05, T08. *Complexity:* M to L.
- **T13 Marks, summary, storm control.** `notification_marks` store and write-on-delivery, suppression rule, start-up summary message and lane, `startup_summary_at` rate limit, hazard budget. *Depends:* T12. *Complexity:* L. *Risk:* getting at-least-once for hazards without duplicates.
- **T14 Administrator-facing state.** Reply to `add` on an alerting sensor; `status` additions. *Depends:* T12. *Complexity:* S to M.

**Phase 4: authorization audit (D4)** (parallel to phases 1 to 3)

- **T15 Denial audit.** Recorder with limiter, hooks in `Registry.Run` and `Bot.reject`, retention cap, tests. *Depends:* none. *Complexity:* M.

**Phase 5: verification and documentation**

- **T16 Real Home Assistant tests.** *Depends:* T09, T10, T13. *Complexity:* M.
- **T17 Stress and regression injections.** At least 25. *Depends:* all semantics tasks. *Complexity:* M.
- **T18 Documentation.** `commands.md`, `notifications.md` (the first-observation rules change), `security.md`, `setup.md` (0003 to 0004 upgrade), changelog, README maturity text, the manual checklist. *Depends:* all. *Complexity:* M.

**Optional:** **T19** `garage_door` and `opening` rows with their tests (S, after T01). **T20** `last_changed` refinement with a skew guard (not planned, U3).

## 18. Risk register

| ID | Risk | Impact | Mitigation | Task |
| --- | --- | --- | --- | --- |
| R1 | Migration loses or corrupts rules | High | Proven SQL, golden test, rollback test, backup-first notice | T05 |
| R2 | Registry refactor changes behavior | High | Differential grid, existing tests unchanged | T01 |
| R3 | Duration timer races or ordering bugs | High | Single evaluator goroutine, injected clock, race and stress runs | T10 |
| R4 | A hazard is missed or duplicated across restarts | High | Write-on-delivery marks, cooldown rule, crash-loop tests | T13 |
| R5 | Start-up alerts create noise or confusion | Medium | One summary, window, rate limit, hazard budget, clear wording, upgrade notes | T13, T18 |
| R6 | Unit binding makes rules dormant unexpectedly | Medium | Clear dormant message, automatic resume, legacy binding audited | T06, T07 |
| R7 | Denial audit grows the database or is abused | Medium | Per-actor and global limits, retention cap, no stranger spam from other guilds | T15 |
| R8 | Stale hazard state from sleeping devices causes a false alert at start-up | Medium | Accepted: safe direction; message states it is Home Assistant's report and its last update | T13 |
| R9 | A user labels a non-leak sensor `moisture` | Low | Accepted over-alerting; documented | T01 |
| R10 | Scope is large for one milestone | Medium | Phases and a cut line: should-haves slip first; D4 is independent | all |
| R11 | Clock differences between Home Assistant and NeptHass | Medium | Durations use NeptHass's clock only | T10 |
| R12 | Discord option limits constrain the command | Low | Whole-minute `for`, optional options, tested early | T07 |
| R13 | Mark write failures | Low | Logged, delivery continues; worst case one repeat alert | T13 |

## 19. Open decisions

None block implementation planning; each has a recommendation.

- **U1 Legacy unit binding.** Persisted one-time audited update (recommended) or in-memory binding only.
- **U2 Optional rows.** Include `garage_door` and `opening` in v0.5.0 (recommended, T19).
- **U3 `last_changed` refinement.** Use it to shorten restart-seeded durations when a measured clock-skew guard allows (not recommended for v0.5.0).
- **U4 Summary control.** Always on with visibility in `status` (recommended); add a per-guild switch only if feedback asks for it.
- **U5 Smoke, gas and carbon monoxide.** A deliberate product and safety policy decision before any registry row exists.
- **U6 Scope of denial auditing.** Allowed non-administrators and strangers, administrative commands only (recommended).

## 20. Definition of done

- Every criterion in section 16 is met and verified by automated tests; nothing partial is described as a full pass.
- `gofmt`, `go vet` (also with the `integration` tag), `go test -race -shuffle=on -count=1 ./...`, the real Home Assistant suite, constrained-CPU stress, the CI Compose validations and the image build all pass.
- The migration is proven on a golden v0.4.0 database and the upgrade notice is in the changelog and `setup.md`.
- The documentation set matches the implementation, including the changed start-up behavior and the registry, with known limitations stated plainly.
- The manual Discord checklist covers the new rows; any row not run is listed as not run.
- Release follows the maintainers' procedure in [CONTRIBUTING.md](../../CONTRIBUTING.md).

## Sources

Home Assistant documentation and source, consulted during this review (not copies of upstream text):

- State object (states, `unavailable`, `unknown`, `restored`, `last_changed`, `last_updated`, force update): <https://www.home-assistant.io/docs/configuration/state_object/>
- Binary sensor device classes and the meaning of `on` and `off`: `homeassistant/components/binary_sensor/const.py` in <https://github.com/home-assistant/core>, and <https://www.home-assistant.io/integrations/binary_sensor/>
- Sensor device classes and units (`DEVICE_CLASS_UNITS`): `homeassistant/components/sensor/const.py` in <https://github.com/home-assistant/core>, and <https://www.home-assistant.io/integrations/sensor/>
- Sensor entity contract (native unit, user unit conversion, numeric state): <https://developers.home-assistant.io/docs/core/entity/sensor/>
- WebSocket API (`subscribe_events`, `get_states`), as used since v0.3.0: <https://developers.home-assistant.io/docs/api/websocket/>
