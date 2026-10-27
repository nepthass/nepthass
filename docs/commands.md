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
- **Starting (synchronizing)** and similar: not healthy yet. A connected, authenticated, subscribed monitor without a snapshot is not healthy.
- **Connected but silent**: nothing has been received recently; values may be outdated.
- **Cannot reach Home Assistant's WebSocket; retrying, next attempt ...**: the connection is down and will retry with backoff.
- **Home Assistant rejected the access token for the WebSocket (check HA_TOKEN)**: a credential problem, retried slowly.
- **Unexpected response from Home Assistant's WebSocket; retrying**: a protocol problem.

It also shows the reconnect count and the number of invalid messages ignored. It never shows the Home Assistant URL, the token or raw error text. Monitoring problems do not stop `/devices`, `/status` or `/control`, which use the REST API.

## /devices

`/devices [page]` lists the entities you may see, 10 per page, with live state, your access level (`read` or `control`) and the alias if one is set. Entities Home Assistant no longer reports are marked "not found in Home Assistant". Unavailable and unknown states are spelled out as "not reporting", never as off or safe.

## /sensors

`/sensors [page]` (users) lists, ten per page, the monitored sensors that are shared with you at `read` or `control`, sorted by name. Each line shows the alias (or Home Assistant's friendly name), the entity ID, the category, the interpreted state, and when it last changed:

- Numeric sensors show the value with Home Assistant's unit (`21.5 °C`).
- Binary sensors show the meaning with the raw state: a door with raw `on` is *open (on)*, motion `on` is *motion detected (on)*, a moisture sensor `on` is *water detected (on)*, a battery sensor `on` is *low (on)*.
- `unavailable` and `unknown` are shown as *not reporting* with a warning icon, and an unexpected value as *invalid value*; they are never shown as closed, clear or dry.

When live monitoring is not healthy, an amber banner says the values are the last known ones and may be outdated, and every line carries a warning icon. The permission is checked again on every call: revoking a share removes the sensor from the very next `/sensors`. Entities that are not monitored sensors (lights, switches, other sensor types) are not listed here; use `/devices` and `/status`. For an authoritative read of one entity, `/status` queries Home Assistant directly.

Supported kinds: temperature, humidity, battery level and battery low, motion, door, window and water leak.

## /status

`/status entity:<entity>` shows the name (alias if set, with the Home Assistant name next to it), state with unit, device class and when it last changed. Needs `read` or `control` access.

## /control

`/control entity:<entity> action:<on|off>` switches a light or switch. Needs `control` access, and only the `light` and `switch` domains can ever be controlled. Home Assistant may report the new state a moment later (Zigbee devices especially); the reply says when it has not changed yet. Every attempt, allowed or denied, is audited.

## /setup

Opens the setup wizard (administrators only). The wizard is private to you and resumes where you left off. See [setup.md](setup.md#configure-access-with-setup) for a walkthrough.

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

## /audit

`/audit [count]` shows the latest entries, newest first. Wizard changes appear as `setup.allow`, `setup.revoke`, `setup.alias` (one per entity) and `setup.apply` (a summary), written in the same transaction as the change.
