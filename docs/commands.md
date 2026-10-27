# Discord command reference

All replies are ephemeral (only the person who ran the command sees them) and never ping anyone. Every interaction is checked against the configured guild and the administrator and user allowlists before anything runs. "Users" are administrators plus `DISCORD_ALLOWED_USER_IDS`; "administrators" are `DISCORD_ADMIN_USER_IDS`.

| Command | Who | Purpose |
| --- | --- | --- |
| [`/ping`](#ping) | users | Check the bot responds |
| [`/health`](#health) | users | Discord, Home Assistant and database status |
| [`/devices`](#devices) | users | List the entities shared with you |
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

Discord gateway, Home Assistant REST API (with its version) and the database. The Home Assistant WebSocket line says it is not available: monitoring arrives in v0.3.0.

## /devices

`/devices [page]` lists the entities you may see, 10 per page, with live state, your access level (`read` or `control`) and the alias if one is set. Entities Home Assistant no longer reports are marked "not found in Home Assistant". Unavailable and unknown states are spelled out as "not reporting", never as off or safe.

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
