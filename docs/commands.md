# Discord command reference

All replies are ephemeral (only the person who ran the command sees them) and never ping anyone. Every interaction is checked against the configured guild and the administrator and user allowlists before anything runs. "Users" are administrators plus `DISCORD_ALLOWED_USER_IDS`; "administrators" are `DISCORD_ADMIN_USER_IDS`.

| Command | Who | Purpose |
| --- | --- | --- |
| [`/ping`](#ping) | users | Check the bot responds |
| [`/health`](#health) | users | Discord, Home Assistant, live monitoring and database status |
| [`/devices`](#devices) | users | List the entities shared with you |
| [`/sensors`](#sensors) | users | Live readings of the monitored sensors shared with you |
| [`/status`](#status) | users | Show a shared entity's state |
| [`/control`](#control) | users | Turn a shared light or switch on or off |
| [`/setup`](#setup) | administrators | Interactive wizard: discover, share and name entities |
| [`/sync`](#sync) | administrators | Refresh the view of Home Assistant's entities |
| [`/permissions`](#permissions) | administrators | Share or unshare a single entity |
| [`/audit`](#audit) | administrators | Recent control attempts and configuration changes |

Rate limits: slash commands 6 per 10 seconds per user; setup wizard clicks 30 per 10 seconds per user.

## Entity names: IDs and aliases

Wherever a command takes an `entity`, autocomplete suggests `Alias (entity_id)` for entities you may use, and submits the canonical Home Assistant entity ID. You can also type the exact alias. An alias resolves only to an entity you already hold the required access on; any other name behaves like an unknown name, so aliases of unshared entities are never revealed. A valid entity ID always wins, and aliases can never look like an entity ID, so resolution is deterministic.

## /ping

Replies with the Discord gateway latency.

## /health

Discord gateway, Home Assistant REST API (with its version) and the database. A fourth line, **Home Assistant WebSocket (live monitoring)**, reports the real-time connection. It lists five layers separately (Transport, Authenticated, Subscribed, Snapshot, and proof of life) and shows **Healthy** only when all of them hold:

- **Healthy: N sensors tracked, synchronized ..., last event ...**: connected, subscribed, synchronized and receiving.
- **Starting (connecting), (authenticating), (subscribing) or (synchronizing)**: the first start; not healthy yet. A connected, authenticated, subscribed monitor without a snapshot is not healthy.
- **Reconnecting (authenticating), (subscribing) or (synchronizing)**, and **Reconnecting to Home Assistant's WebSocket (the previous attempt failed: ...)**: the same stages after an earlier failure. A reconnection is never called "Starting". Last known values may be outdated.
- **Connected but silent**: nothing has been received recently; values may be outdated.
- **Cannot reach Home Assistant's WebSocket; retrying, next attempt at <time>**: the connection is down and will retry with backoff. The time is an absolute clock time in your time zone, because a relative one would read "... seconds ago" once the moment has passed while the message is still on screen. The same line is used when Home Assistant accepts the connection but does not answer the subscription (15 seconds) or the snapshot (60 seconds) in time.
- **Home Assistant rejected the access token for the WebSocket (check HA_TOKEN)**: a credential problem, retried slowly.
- **Unexpected response from Home Assistant's WebSocket; retrying**: a protocol problem.

It also shows the reconnect count and the number of invalid messages ignored. It never shows the Home Assistant URL, the token or raw error text. Monitoring problems do not stop `/devices`, `/status` or `/control`, which use the REST API.

The **Notifications** line (v0.4.0) shows one non-sensitive status for every allowed user: *Disabled*, *Enabled*, or *Degraded* with the reasons (for example a failing delivery channel, or a saved configuration that is not active). It never shows the channel, rules or sensors. Administrators get the detail from `/notifications status`.

## /devices

`/devices [page]` lists the entities you may see, 10 per page, with live state, your access level (`read` or `control`) and the alias if one is set. Entities Home Assistant no longer reports are marked "not found in Home Assistant". Unavailable and unknown states are spelled out as "not reporting", never as off or safe.

## /sensors

`/sensors [page]` (users) lists, ten per page, the monitored sensors that are shared with you at `read` or `control`, sorted by name. Each line shows the alias (or Home Assistant's friendly name), the entity ID, the category, the interpreted state, and when it last changed:

- Numeric sensors show the value with Home Assistant's unit (`21.5 °C`).
- Binary sensors show the meaning with the raw state: a door with raw `on` is *open (on)*, motion `on` is *motion detected (on)*, a moisture sensor `on` is *water detected (on)*, a battery sensor `on` is *low (on)*.
- `unavailable` and `unknown` are shown as *not reporting* with a warning icon, and an unexpected value as *invalid value*; they are never shown as closed, clear or dry.

When live monitoring is not healthy, an amber banner says the values are the last known ones and may be outdated, every line carries a warning icon, and the footer says "Last known values, not live". The footer says "Live readings from Home Assistant's WebSocket" only while monitoring is healthy and synchronized. The permission is checked again on every call: revoking a share removes the sensor from the very next `/sensors`. Entities that are not monitored sensors (lights, switches, other sensor types) are not listed here; use `/devices` and `/status`. For an authoritative read of one entity, `/status` queries Home Assistant directly.

**Before the first synchronization** (right after NeptHass starts, or while Home Assistant is unreachable at startup) `/sensors` says that live monitoring is still starting, cannot reach Home Assistant, cannot sign in, or is not running, and that sensor readings are not available yet. It does **not** say that nothing is shared: until Home Assistant has been read, that is unknown, and the answer is the same whether or not anything is shared. Once monitoring has synchronized, "No monitored sensors are shared with you" means exactly that.

A tracked sensor that reports `unavailable` or `unknown` without a device class keeps its previous kind and is listed as not reporting; it never disappears or reads as closed, clear or dry.

Supported kinds: temperature, humidity, battery level and battery low, motion, door, window and water leak.

## /status

`/status entity:<entity>` shows the name (alias if set, with the Home Assistant name next to it), state with unit, device class and when it last changed. Needs `read` or `control` access.

## /control

`/control entity:<entity> action:<on|off>` switches a light or switch. Needs `control` access, and only the `light` and `switch` domains can ever be controlled. Home Assistant may report the new state a moment later (Zigbee devices especially); the reply says when it has not changed yet. Every attempt, allowed or denied, is audited.

## /setup

Opens the setup wizard (administrators only). The wizard is private to you and resumes where you left off. See [setup.md](setup.md#5-configure-access-with-setup) for a walkthrough.

| Step | What you do |
| --- | --- |
| 1. Connection health | See Discord, Home Assistant REST and SQLite status. Next is disabled until Home Assistant answers. |
| 2. Discover devices | Browse real entities (name, ID, state, device class, current sharing), search, filter by domain or "Shared only", refresh, page, and tick entities in the menu. |
| 3. Device permissions | Choose `read`, `control` (lights and switches only) or no access per entity, or apply a rule to all selected. |
| 4. Aliases | Set, change or remove an alias per entity (modal; optional). |
| 5. Review | See newly shared, modified, revoked and alias changes, and what stays untouched. Blocking problems disable the confirm button. |
| 6. Done | Changes are saved and active immediately; try `/devices`, `/status`, `/control`. |

Buttons: Back, Next, Cancel setup (nothing is applied), Resume, Start over, Refresh, Retry. Nothing is written until you press **Confirm and apply** on the review step.

Facts worth knowing:

- One active session per administrator. Running `/setup` again offers **Resume** or **Start over**. Another administrator has their own session and cannot see yours.
- Sessions are stored in SQLite, survive restarts and expire after 24 hours without activity. Credentials are never entered or stored.
- A session holds at most 100 selected entities. Newly selected entities default to **read**.
- A panel that is no longer current ("out of date") is refreshed instead of acting; a replayed confirmation writes nothing.
- If an entity's permission or alias is changed elsewhere (for example `/permissions revoke`) while you are reviewing, Confirm is refused, the review is rebuilt from the live state and you must confirm again, so an old confirmation can never silently restore something that was revoked.

## /sync

Fetches Home Assistant's entities and reports: how many there are, how many can be controlled, **new** entities since the last sync, **removed** and **returned** entities, and shared entities that are missing from Home Assistant or unavailable. The first sync records a baseline and reports nothing as new. `/sync` never changes a permission or an alias; new entities stay private until you share them. Only entity IDs and first/last-seen times are stored; names and states are always read live.

## /permissions

`/permissions allow entity access`, `revoke entity`, `list`. Administrators only. The same rules apply as in the wizard (control only for lights and switches; the entity must exist). It writes the same permission table as `/setup`, so the two always agree.

## /notifications

`/notifications <subcommand>` (administrators, guild-only; every reply is ephemeral) configures alerts for shared sensors. Notifications are **disabled until a channel is set**. Reliability and privacy limits are summarized in [security.md](security.md).

| Subcommand | What it does |
| --- | --- |
| `channel <channel>` | Choose the text channel that receives alerts. NeptHass checks that it exists, belongs to this server, is a text channel (not voice, forum, announcement, thread or DM), and that the bot can View Channel, Send Messages and Embed Links. The reply warns that **everyone who can read that channel will see the alerts**. |
| `add <entity> [above] [below] [clear] [for] [cooldown]` | Create a rule. Without `above`/`below` it is a *state* rule for door, window, garage door, opening, motion, water leak and battery-low sensors. With `above` or `below` (not both) it is a threshold rule for temperature, humidity and battery-level sensors, **bound to the unit the sensor reports now**. `clear` (numeric rules): the value the reading must pass again before the rule can alert again (below the threshold for `above`, above it for `below`). `for` (1 to 1440 minutes): alert only after the condition has held that long, never for a water leak. `cooldown` (0 to 1440 minutes; 0 for none). |
| `list` | All rules with their number, name, what they watch (including the clear value, the duration and the unit) and their status: *active*, *paused*, *needs sharing*, *not monitored*, *incompatible*, or **not evaluating** (unit not confirmed, unit changed) with what to do about it. |
| `pause [rule]` / `resume [rule]` | Pause or resume one rule; **without a rule, pause or resume all notifications**. |
| `remove <rule>` | Delete a rule. |
| `unit <rule>` | Bind a rule created before v0.5.0 to its sensor's **current** unit, on your say-so that its threshold is written in that unit. Only rules that need it are offered. A unit is never rebound. |
| `test` | Send a test message to the alert channel. |
| `status` | Channel, rule counts (including rules waiting for a unit), last delivery and failure, queue depth, dropped counts by reason, pending and fired duration counts, what was announced since NeptHass started, circuit-breaker and monitoring-down state. |

Examples:

```
/notifications channel channel:#home-alerts
/notifications add entity:Front door
/notifications add entity:Living room temperature above:28 cooldown:30
/notifications add entity:Phone battery below:15
/notifications add entity:Front door for:10
/notifications add entity:Attic temperature above:30 clear:28 for:5
/notifications unit rule:4
/notifications pause rule:3
/notifications status
```

Details worth knowing:

- Autocomplete offers only sensors that are **shared and currently monitored**, shown by name; entity IDs are never shown in lists, status or alerts.
- `add` explains when monitoring has not synchronized yet, and refuses unshared, unmonitored or incompatible entities, a duplicate (one rule per entity, kind **and duration**), thresholds outside +/-1,000,000, a sensor whose unit is missing or not documented for its type, a duration or clear value the sensor type does not support (a water leak is always immediate), a `clear` on the wrong side of the threshold, and the 201st rule. If the sensor is **already in the rule's alert state**, the reply says what will happen: a water leak sends a critical alert, a duration counts from now, any other rule waits for the next change.
- Default cooldowns: door, window, garage door and opening 1 minute, motion 5 minutes, leak, temperature and humidity 15 minutes, battery 6 hours. A suppressed alert is not deferred and does not repeat later.
- A rule created before v0.5.0 on a temperature sensor shows **not evaluating** until you run `/notifications unit`: NeptHass cannot know whether its threshold was written in °C, °F or K. Humidity and battery rules are bound automatically. A rule whose sensor later changes unit is also not evaluating (it resumes if the unit returns); delete it and add it again to use the new unit.
- An unshared sensor's rules stay in `list` as *needs sharing* and resume when it is shared again.
- If a change is saved but cannot be loaded into the running engine, the reply says so ("saved but **not active**"), `status` and `/health` show *Degraded*, and NeptHass keeps retrying.

## /audit

`/audit [count]` shows the latest entries, newest first. Wizard changes appear as `setup.allow`, `setup.revoke`, `setup.alias` (one per entity) and `setup.apply` (a summary), written in the same transaction as the change.
