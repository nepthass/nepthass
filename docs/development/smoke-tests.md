# Smoke tests

There are two separate kinds of checks. Keep them separate when you report results.

| Kind | Needs | Proves | Where it runs |
| --- | --- | --- | --- |
| **Automated integration tests** | the dev Home Assistant and a token | NeptHass's real REST client, entity service, SQLite and audit log behave correctly against a real Home Assistant | a command, repeatable, no Discord |
| **Manual Discord checks** | the dev stack **and** a real Discord test bot | the Discord transport: slash commands, ephemeral replies, autocomplete, authorization of real Discord identities | you, in Discord, by hand |

The automated tests do not talk to Discord. The manual checks are the only place the Discord connection is exercised. **A manual check counts as passed only when a person actually ran it.** Never treat a stub-based automated test as a substitute for a manual Discord check.

## 1. Automated integration tests (real Home Assistant, no Discord)

Prerequisites: the dev Home Assistant is running and onboarded, and `HA_TOKEN` is set in `.env.dev` (see [setup.md](setup.md)). Nothing else, no Discord credentials.

```bash
docker compose --env-file .env.dev -f compose.dev.yaml --profile test run --rm integration-tests
```

This starts a Go container on the same private network, mounts the repository read-only and runs `go test -tags=integration`. It needs no Go installation on your machine.

With a local Go toolchain you can also run them from the host against the published localhost port:

```bash
HA_URL=http://127.0.0.1:18123 HA_TOKEN=<token> go test -tags=integration -count=1 -v ./tests/integration/...
```

Safety: the tests only touch `nepthass_dev_*` entities and refuse to run unless the target Home Assistant contains the development simulation (`script.nepthass_dev_scenario_reset`). They reset the simulation before and after the run. Without the `integration` build tag the tests do not exist, so `go test ./...` and CI never need a Home Assistant.

What they verify, against the real Home Assistant:

| Area | Checks |
| --- | --- |
| Connectivity | token authentication works; a wrong token is rejected as unauthorized |
| Virtual entities | all nine entities exist with the documented IDs, baseline states, device classes, units and friendly names |
| Control | the virtual light and switch turn on and off through NeptHass's service layer and the real Home Assistant reports the new state |
| Read-only sensors | `Allow(control)` on every sensor is refused; a tampered database row that grants control is still refused; the REST client refuses sensor services without sending a request; the sensors stay untouched |
| Authorization | unshared entities are invisible (`Status`, `Devices`, autocomplete); sharing exposes only what was granted; control without permission or with read-only permission is denied and leaves the device alone |
| Audit | control on/off, grants and denials are recorded with the right actor, entity, action and outcome |
| Persistence | permissions and audit entries survive closing and reopening the database (a process restart) |
| Simulation | every scenario script changes the real Home Assistant state as documented; reset restores the baseline; one sensor can go offline and recover |
| Unavailable | offline sensors report `unavailable` and NeptHass classifies them as not reliable, never as safe |
| Units | after the instance is switched to US customary units (as the onboarding UI can do), the reset script restores metric and temperatures are reported as 31.2 and 21.5 degrees Celsius |
| Setup discovery (v0.2.0) | the wizard's discovery lists the real virtual entities with their states, device classes and units, and supports search, domain filter and paging against the real Home Assistant; offline sensors are flagged as not reporting |
| Setup flow (v0.2.0) | a full wizard run (select light, switch, temperature and door sensors, bulk control rule, alias, review, apply) against the real Home Assistant; nothing is written before confirmation; afterwards `Status`, `Control` (through the alias), `Devices` and the audit log reflect it; sensors never receive control |
| Stale confirmation (v0.2.0) | a permission revoked while a review is pending is not restored by the old confirmation, which is dead afterwards |
| Sessions (v0.2.0) | a setup session survives closing and reopening the database |
| Sync (v0.2.0) | first sync records a baseline; a newly reported entity is listed as new; shared-but-missing and shared-but-unavailable entities are reported; nothing is shared automatically |
| Monitoring sync (v0.3.0) | NeptHass authenticates to the real WebSocket, subscribes, synchronizes, and classifies the seven virtual sensors (category, raw state, unit, meaning); the light is not tracked; every health layer is true |
| Monitoring events (v0.3.0) | every scenario script (motion, door, window, high temperature, water leak, low battery) is received as a real `state_changed` event and tracked with the right meaning and a newer revision; reset returns to baseline in metric units |
| Monitoring unavailable (v0.3.0) | the offline scenario makes every sensor `unavailable`, interpreted as not reporting and never as safe; recovery restores the real values |
| Monitoring outage (v0.3.0) | through a TCP proxy in front of the real Home Assistant, cutting the network marks monitoring not healthy and keeps last values readable; restoring it reconnects and reconciles a change that happened while disconnected, with no event |
| Monitoring credentials (v0.3.0) | a wrong token is reported as an authentication failure, not a network failure |
| Monitoring authorization (v0.3.0) | with real events flowing, a shared sensor is readable, an unshared leak sensor is denied, the listing contains only shared sensors, a stranger is refused, and revocation applies on the next read |
| Monitoring shutdown (v0.3.0) | cancelling while healthy stops the monitor cleanly |

Not covered: anything that involves Discord, and Home Assistant being stopped (see manual checks M30, M31, S02 and L12). A real container restart is also not part of this suite (it needs container control); it was observed once with a temporary probe, see section 4.

The ordinary unit tests (`go test -race ./...`) additionally cover the Discord interaction logic with fakes: guild, user and administrator authorization for commands, components and autocomplete, rate limiting and the command replies. They do not prove the real Discord transport, which is what the manual checks are for.

### Notification tests (v0.4.0)

`tests/integration/notifications_test.go` runs the real chain **real Home Assistant -> real monitor -> real notification engine -> recording sender** against the dev stack's virtual sensors: a door opening and closing, motion, a water leak, a leak returning wet from unavailable, temperature above and battery below a threshold, cooldown, revocation, an already-open door at startup, a real network outage (through the TCP proxy) that hides an opening and yields a late alert, a five-minute disconnect with a down alert and a recovery notice (injected clock), a Discord outage that recovers through retries, queue saturation with a current-state digest, and a restart with persisted configuration. **Discord itself is replaced by a recording sender here.** The Discord REST adapter is covered separately by `internal/discord` tests against a **stub REST server**, which prove NeptHass's side of the protocol, not Discord's behaviour.


### Rules, start-up and audit tests (v0.5.0)

Against the real Home Assistant, with the recording sender: Home Assistant's `garage_door` and `opening` classes alert when they open; a duration rule on a real door never fires a second early and fires once (injected clock); a door that closes before its duration never alerts; hysteresis through real temperature setpoints (31, 29, 31, 28, 31, 27, 31) alerts only after a clear; a leak that is already wet at start is announced once and not again after a restart (persisted mark), or announced as "what Home Assistant reports" when no mark exists; sensors already open at start are listed in one summary; **a real unit change** (the instance is switched to US customary units, so Home Assistant itself reports the temperature in °F) makes a °C-bound rule dormant and it resumes when the unit returns; and **a real outage** (the WebSocket connection is cut) while a duration is pending fires nothing during the outage, restarts the count on recovery and says "at least". The denial audit and the `/notifications unit` command are covered by the unit, end-to-end and interaction tests, not by this suite.

## 2. Manual Discord checklist

Preconditions (all must be true before starting):

1. Dev Home Assistant running, onboarded, `NeptHass Dev` dashboard visible, `HA_TOKEN` set.
2. Discord test bot invited to your private test server; `.env.dev` filled in ([setup.md](setup.md)).
3. NeptHass started with `--profile app` and its log shows `authenticated with home assistant`, `connected to discord gateway`, `slash commands registered`.
4. The simulation is at baseline (run `NeptHass Dev: reset to baseline`).
5. You are in `DISCORD_ADMIN_USER_IDS`. For the unauthorized checks you also have a second Discord account, or you use the temporary-allowlist trick from [setup.md](setup.md).

In the steps below, "the user" is an account in `DISCORD_ALLOWED_USER_IDS` (or you, after moving yourself from the admin list to the allowed list), and "the stranger" is an account on neither list. Replies are ephemeral: only the person who ran the command sees them.

### A. Connectivity

| # | Step | Expected |
| --- | --- | --- |
| M01 | `/ping` as admin | `Pong! Discord gateway latency: N ms.` |
| M02 | `/health` as admin | embed: Discord gateway connected with latency; Home Assistant REST API connected with a version (`Home Assistant 2026.10.4` or the pinned version); Home Assistant WebSocket shows `Not available in this release (planned for v0.3.0)`; Database responding |

### B. Unauthorized access

| # | Step | Expected |
| --- | --- | --- |
| M10 | the stranger runs `/ping` | `You are not authorized to use NeptHass.` and nothing else |
| M11 | the stranger runs `/status entity:light.nepthass_dev_light` and `/devices` | the same refusal; no entity name, state or count is shown |
| M12 | the stranger starts typing in the `entity` option of `/status` | no suggestions appear |
| M13 | the stranger runs `/permissions list` and `/audit` | the same refusal |
| M14 | NeptHass log | `rejected unauthorized interaction` warnings with reason `not_authorized`; no secrets |
| M15 | (optional) run a command from a second Discord server where the bot is installed, using an admin account | refused as `wrong_guild` in the log, same generic reply |

### C. Permissions and private data

| # | Step | Expected |
| --- | --- | --- |
| M20 | admin: `/devices` on a fresh database | `No entities have been shared yet. ...` |
| M21 | admin: `/status entity:sensor.nepthass_dev_temperature` before sharing | `You do not have access to that entity. ...` |
| M22 | admin: `/permissions allow entity:light.nepthass_dev_light access:control`; the entity box autocompletes with friendly names | `light.nepthass_dev_light now has control access.` |
| M23 | admin: allow `switch.nepthass_dev_switch` as `control`; `sensor.nepthass_dev_temperature` and `binary_sensor.nepthass_dev_door` as `read` | each: `now has read` or `control access` as granted |
| M24 | admin: `/permissions allow entity:binary_sensor.nepthass_dev_motion access:control` | refused: `NeptHass can only control lights and switches. ...` |
| M25 | admin: `/permissions allow entity:light.typo access:read` | `Home Assistant does not know that entity.` |
| M26 | admin: `/permissions list` | lists the four shared entities with their levels, and nothing else |
| M27 | user (non-admin): `/permissions list`, `/permissions allow ...`, `/audit` | `This command is for NeptHass administrators.`; the allowlist is unchanged |
| M28 | user: `/devices` | the four shared entities with live states and levels; nothing unshared |
| M29 | user: `/status entity:binary_sensor.nepthass_dev_motion` (never shared) | `You do not have access to that entity. ...` |

### D. Reading and controlling

| # | Step | Expected |
| --- | --- | --- |
| M40 | `/status entity:sensor.nepthass_dev_temperature` | `21.5 °C`, device class `temperature`, a "last changed" time |
| M41 | run `NeptHass Dev: high temperature`, then `/status` again | `31.2 °C` |
| M42 | `/status entity:binary_sensor.nepthass_dev_door`, then run `NeptHass Dev: door opened`, then `/status` again | `off`, then `on` |
| M43 | run `NeptHass Dev: all sensors offline`, then `/status entity:sensor.nepthass_dev_temperature` | `unavailable (not reporting)` with a warning icon; never shown as off, closed or safe. Then reset |
| M44 | `/control entity:light.nepthass_dev_light action:on` | `Command Sent` embed; in the Home Assistant UI the light is on; `/status` shows `on` |
| M45 | `/control entity:light.nepthass_dev_light action:off` | light off in Home Assistant |
| M46 | `/control entity:switch.nepthass_dev_switch action:on`, then `off` | switch on, then off in Home Assistant |
| M47 | the `entity` box of `/control` | suggests only the light and the switch (control access), not the read-only sensors |
| M48 | `/control entity:sensor.nepthass_dev_temperature action:on` (typed by hand) | refused (`do not have access`); the sensor is unchanged |
| M49 | `/control entity:binary_sensor.nepthass_dev_motion action:on` (never shared) | `You do not have access to that entity. ...`; motion unchanged |
| M50 | `/control entity:../../etc action:on` | `That is not a valid entity ID ...` |
| M51 | send seven commands within a few seconds | the seventh: `You are sending commands too quickly. ...` |

### E. Revoking, restarting and auditing

| # | Step | Expected |
| --- | --- | --- |
| M60 | admin: `/permissions revoke entity:switch.nepthass_dev_switch` | `... was removed from the allowlist.` |
| M61 | user: `/control entity:switch.nepthass_dev_switch action:on` | denied; the switch did not change |
| M62 | admin: repeat the revoke | `... was not on the allowlist.` |
| M63 | admin: `/permissions list` | three entities |
| M64 | restart NeptHass: `docker compose --env-file .env.dev -f compose.dev.yaml --profile app restart nepthass`; wait for `slash commands registered` | |
| M65 | admin: `/permissions list` after the restart | the same three entities (persistence) |
| M66 | user: `/control entity:light.nepthass_dev_light action:on` after the restart | works |
| M67 | admin: `/audit` | newest first: the permission changes (`permissions.allow`, `permissions.revoke`) and each control attempt with the right actor mention, entity, action and icon: success, denied (the sensor, motion and revoked switch attempts) |
| M68 | compare M67 with what you did | every control attempt and permission change appears exactly once, none is missing, no secrets appear |

### F. Resilience

| # | Step | Expected |
| --- | --- | --- |
| M30 | `docker compose --env-file .env.dev -f compose.dev.yaml stop homeassistant`, then `/health` and `/control` | `/health`: Home Assistant REST API shows `Unreachable`; `/control`: `Home Assistant is not reachable right now. ...`; NeptHass keeps running |
| M31 | `... start homeassistant`, wait until healthy, repeat `/health` | Home Assistant connected again without restarting NeptHass |
| M32 | (optional) put a wrong value in `HA_TOKEN`, recreate NeptHass | the process exits with `home assistant rejected HA_TOKEN`; restore the token afterwards |

### G. v0.2.0 setup wizard and /sync

Use the dev stack with the `app` profile rebuilt from the v0.2.0 code (`up -d --build`). Start from a clean allowlist (`/permissions list` empty) so the effects are easy to see, run `NeptHass Dev: reset to baseline` first, and note that the wizard is private to the person who runs it.

| # | Step | Expected |
| --- | --- | --- |
| S01 | admin: `/setup` | an ephemeral panel "Step 1 of 6: Connection health": gateway connected, Home Assistant connected with its version, SQLite responding, a WebSocket monitoring line (in v0.2.0 it read "not available in this release"; from v0.3.0 it shows the live monitoring state), NeptHass version; **Next** enabled |
| S02 | `stop homeassistant`, press **Retry checks**; then `start homeassistant`, wait until healthy, **Retry checks** | while stopped: Home Assistant shown as unreachable, **Next** disabled, an explanation that setup never uses stale data; after starting: all green, **Next** enabled |
| S03 | **Next** | "Step 2 of 6: Discover devices" with a real entity count (at least 33 on the dev instance), ten entities per page showing name, `entity_id`, state with unit, device class and "no access" |
| S04 | **Search...**, type `water leak`, submit; then **Clear search** | one match (NeptHass Dev Water Leak); clearing returns the full list |
| S05 | domain menu: choose `light`, then **Shared only**, then **All domains** | only lights; then an empty "no entities match" state; then everything; the page resets to 1 each time |
| S06 | page buttons and **Refresh** | **Page** buttons enable and disable at the ends; Refresh keeps filters and selection |
| S07 | run `NeptHass Dev: all sensors offline` in Home Assistant, **Refresh** | the sensors show `unavailable (not reporting)` with a warning icon (never as off or closed); then run the reset script |
| S08 | tick NeptHass Dev Light, Switch, Temperature and Door (use Search to bring each onto the page), watch the **Selected** counter, press **Next** | counter reaches 4; **Next** is disabled while 0 are selected |
| S09 | step 3: pick the light, **Control**; pick the switch, **Control**; pick a sensor | **Control** is disabled for sensors; the list shows "now: no access -> control" with a bullet for pending changes; **Lights and switches: control** sets sensors to read |
| S10 | step 4: pick the light, **Set alias...**, enter `Living Room Light`; then give the switch the same alias in any letter case | the first is accepted and shown in bold; the second is refused with a message naming `light.nepthass_dev_light` |
| S11 | try the alias `@everyone` and an empty alias on the light | `@everyone` is refused with a reason; an empty alias removes the alias |
| S12 | step 5 review | sections "Newly shared (4)" and "Alias changes (1)", "Not changed", and a note that nothing has been applied; in another Discord window `/devices` is still empty |
| S13 | **Confirm and apply** | "Setup complete"; `/devices` lists the four entities (the light under its alias) with levels; `/status entity:Living Room Light`; `/control entity:Living Room Light action:on` turns the light on in Home Assistant; `/audit` shows `setup.allow` (4), `setup.alias`, `setup.apply` |
| S14 | open `/setup` in two windows (desktop and web) as the same admin, advance in one, click **Next** in the stale one | "That panel was out of date, so it was refreshed. Nothing was changed." |
| S15 | start a setup, tick an entity, restart NeptHass (`restart nepthass`), run `/setup` | a prompt "You have a setup in progress: step N of 6, 1 entity selected"; **Resume** restores the panel with the selection; **Start over** begins at step 1 |
| S16 | **Cancel setup** at any step | "Setup cancelled. Nothing was changed."; `/devices` unchanged |
| S17 | `/setup`, **Shared only**, tick the switch, step 3 **No access**, review | review shows "Access revoked (1)" with a warning that the entity will lose access; after confirming, `/control` on the switch is denied |
| S18 | start a review with a pending change, then in another window run `/permissions revoke` for an entity in the draft, then press **Confirm and apply** | nothing is applied; a message names the changed entities and says the old confirmation was cancelled; the review is rebuilt (the revoked entity now appears as an explicit new share) and must be confirmed again |
| S19 | at the review, `stop homeassistant`, press **Confirm and apply** | "Home Assistant is not reachable ... Setup never continues on stale or invented entity data"; nothing applied; after `start homeassistant`, **Retry** returns to the review |
| S20 | `/sync` (first time), `/sync` again | first: "first synchronization", entity count, nothing reported as new; second: "New since the last sync (0)", footer "Nothing was shared or changed" |
| S21 | share the temperature sensor, run `NeptHass Dev: all sensors offline`, `/sync` | the temperature sensor is listed under "Shared but unavailable or unknown"; run the reset script afterwards |
| S22 | a regular (non-admin) user runs `/setup` and `/sync`; the stranger runs both | regular user: "This command is for NeptHass administrators."; stranger: "You are not authorized to use NeptHass." |
| S23 | existing commands: `/ping`, `/health`, `/devices`, `/status`, `/control`, `/permissions list`, `/permissions allow`, `/permissions revoke`, `/audit`; type in the `entity` box of `/control` | all behave as in v0.1.0; autocomplete shows `Living Room Light (light.nepthass_dev_light)` |
| S24 | upgrade: stop the v0.1.0 dev stack's NeptHass, start v0.2.0 on the same `nepthass-data` volume (back it up first) | NeptHass starts; `/permissions list` and `/audit` still show the old entries; the log shows no migration error |
| S25 | (optional) leave a session idle for 24 hours, press a button | "Setup expired"; nothing applied |

### H. v0.3.0 live monitoring

Use the dev stack with the `app` profile rebuilt from the v0.3.0 code (`up -d --build`), the simulation at baseline, and the seven virtual sensors shared (run `/setup` or `/permissions allow`; keep the light and switch shared too so the existing commands can be re-checked). The NeptHass log should show `home assistant websocket authenticated`, `home assistant event subscription active` and `home assistant initial synchronization completed`. Monitoring never sends a message by itself: you only see its effects through `/health` and `/sensors`. Rows marked (optional) need container or network control.

Manual results for this group were recorded on 2026-10-29 (section 4, user-reported). Still outstanding after that run: L06, L13, L14, the remaining L17 commands (`/audit` and `/setup` step 1) and a manual capture of the L16 pre-synchronization message.

| # | Step | Expected |
| --- | --- | --- |
| L01 | `/health` shortly after start | the line "Home Assistant WebSocket (live monitoring)" reads "Healthy: N sensors tracked" (N is at least 7), synchronized and last-event times, and Transport, Authenticated, Subscribed, Snapshot all ticked |
| L02 | `/sensors` at baseline | the seven sensors listed with temperature `21.5 °C`, humidity `45 %`, battery `87 %`, motion *clear (off)*, door *closed (off)*, window *closed (off)*, water leak *dry (off)*; no stale banner; neither the light nor the switch is listed |
| L03 | run `NeptHass Dev: motion detected`, then `/sensors` | motion *motion detected (on)* with a recent "changed" time; **no message is sent to Discord** (notifications are v0.4.0) |
| L04 | run the door, window, high temperature, water leak and low battery scenarios one at a time, `/sensors` after each | door and window *open (on)*, temperature `31.2 °C`, water leak *water detected (on)*, battery `12 %` |
| L05 | run `NeptHass Dev: reset to baseline`, `/sensors` | everything back to the baseline values |
| L06 | run `NeptHass Dev: all sensors offline`, `/sensors`; then reset | every row shows *unavailable (not reporting)* with a warning icon; no row says closed, clear or dry; after the reset the real values return |
| L07 | after opening the door, compare `/sensors` with `/status entity:binary_sensor.nepthass_dev_door` | both agree (open) |
| L08 | the stranger runs `/sensors` | "You are not authorized to use NeptHass." and nothing else |
| L09 | share only the temperature and door sensors, trigger the water leak and motion scenarios, run `/sensors` as the user | only temperature and door appear; the leak and motion sensors are not shown or hinted at |
| L10 | with the door shown in `/sensors`, run `/permissions revoke` for it, then `/sensors` again (no restart) | the door is gone from the very next call |
| L11 | give the door an alias in `/setup`, run `/sensors` | the alias is shown instead of the Home Assistant name |
| L12 | restart Home Assistant only (`docker compose --env-file .env.dev -f compose.dev.yaml restart homeassistant`); run `/health` and `/sensors` while it is down, and again once it is healthy | while down: the monitoring line shows a warning ("Cannot reach Home Assistant's WebSocket; retrying, next attempt at <clock time>", then "Reconnecting ..." while it dials again, never "Starting", last known values may be outdated), the four layers are crossed, `/sensors` shows the amber banner and warning icons; afterwards: Healthy again, the Reconnects counter is at least 1, the banner is gone, values current. `/devices`, `/status` and `/control` keep working (when Home Assistant is up) |
| L13 | (optional) invalid token: delete the long-lived token in the Home Assistant profile, then restart Home Assistant | `/health` shows "Home Assistant rejected the access token for the WebSocket (check HA_TOKEN)" and retries slowly (30 s growing to 10 min); after creating a new token, updating `.env.dev` and `up -d`, it is Healthy again |
| L14 | (optional) network drop: with Home Assistant running, `docker network disconnect nepthass-dev_dev nepthass-dev-nepthass-1`, run `NeptHass Dev: door opened` from the Home Assistant UI, then `docker network connect nepthass-dev_dev nepthass-dev-nepthass-1`. This also cuts NeptHass's Discord connection until you reconnect | after reconnecting and a few seconds, `/sensors` shows the door open although NeptHass never saw the event (reconciliation) |
| L15 | `docker compose --env-file .env.dev -f compose.dev.yaml --profile app stop nepthass`, then read its log | it stops within a few seconds with exit code 0 and logs `home assistant monitoring stopped`; starting it again works |
| L16 | `restart nepthass`, run `/sensors` immediately and again after about ten seconds | right after start: `/sensors` says live monitoring is still starting (or cannot reach Home Assistant) and that readings are not available yet, and does **not** say that nothing is shared; ten seconds later: healthy with current values and a "Live readings" footer; permissions and aliases are unchanged |
| L17 | existing commands: `/ping`, `/devices`, `/status`, `/control` (light on and off), `/permissions`, `/audit`, and `/setup` step 1 | all behave as in v0.2.0; the wizard's health step shows the monitoring line instead of "not available in this release" |
| L18 | `docker compose --env-file .env.dev -f compose.dev.yaml logs nepthass` after all of the above | no token, no sensor values and no raw Home Assistant payloads anywhere in the log |

### I. v0.4.0 notifications (real Discord)

Setup: a dedicated **test** server (never production), the bot invited with `applications.commands`, the dev stack with the `app` profile built from this branch, the simulation at baseline and the seven virtual sensors shared. Put credentials in the env file only; never paste a token into chat, an issue or a commit. Prepare these channels: **#alerts** (bot has View Channel, Send Messages, Embed Links), **#no-send** (bot cannot send), **#no-embed** (bot cannot embed links), **#role-grant** (bot gets send only through a role), **#overwrite-deny** (a channel overwrite denies the bot's role), **#voice**, **#forum**, **#announcements**, a **thread** in #alerts, and a channel in a **second server**.

| # | Step | Expected |
| --- | --- | --- |
| N01 | Start the bot; open the slash-command picker as an administrator and as a normal member | `/notifications` is registered, guild-only (not offered in DMs) |
| N02 | `/notifications status` before any setup | "Not configured"; no channel; zero rules |
| N03 | `/health` as a normal allowed user | a Notifications line saying *Disabled*; no channel or rule information |
| N04 | `/notifications status` and `add` as a non-administrator | refused with "This command is for NeptHass administrators."; nothing changes. **No audit row is written for this refusal** (the command gate refuses before the notification code runs, as for every administrator command) |
| N05 | `/notifications channel #alerts` | accepted; the reply names the channel and warns that everyone who can read it sees the alerts |
| N06 | `channel #no-send` | rejected, naming *Send Messages* |
| N07 | `channel #no-embed` | rejected, naming *Embed Links* |
| N08 | `channel` with the voice, forum, announcement channels and the thread (the picker should not offer them; try typing or pasting an ID where Discord allows) | none accepted; a clear "normal text channel" message if one reaches the bot |
| N09 | A channel of the second server, or a direct-message channel (only if Discord lets you submit it) | rejected as not in this server |
| N10 | `channel #role-grant` (send only via a role); `channel #overwrite-deny` (overwrite denies the bot's role); a channel where the bot has Administrator | the role grant is accepted; the overwrite deny is rejected naming the missing permission; Administrator is accepted |
| N11 | `/notifications test` | a test message arrives in #alerts with no mention; `/audit` shows the test |
| N12 | `add` with autocomplete as an administrator; try a sensor that is not shared | only shared, monitored sensors are offered, shown by name, never by entity ID; the unshared one is absent |
| N13 | `add` errors: `above` and `below` together, `cooldown` 2000, a state rule on a thermometer, a duplicate, before monitoring has synchronized (right after start) | each gives its own clear message and creates nothing |
| N14 | `add` Door, Window, Motion, Water leak (state), Temperature `above:30`, Battery `below:50`, each with `cooldown:0`; then `list` | seven rules, numbered, by name, all *active* |
| N15 | Run `NeptHass Dev: door opened`, then reset | one alert in #alerts: name, "is now open", time; no mention; no entity ID; closing sends nothing |
| N16 | Run the motion, window, water leak, high temperature and low battery scenarios | one alert each, worded correctly (motion detected, wet, rose above 30, fell below 50 with the value and unit) |
| N17 | Leak unavailable then wet: `sensors offline`, `water leak`, then turn the leak "offline" helper off | an alert saying the state was unknown or unavailable before, "please check it" |
| N18 | Cooldown: add a Door rule with the default cooldown; open, close, reopen within a minute; `status` | one alert; `status` counts one muted; the next alert after the cooldown says how many were muted |
| N19 | `pause rule:<n>`, trigger it, `resume rule:<n>`, trigger it; then `pause` and `resume` with no rule; then `remove` | no alert while paused; alerts after resume; global pause stops everything; a removed rule never alerts |
| N20 | Revoke sharing: `/permissions revoke` the door, open it; `list`; share it again | no alert; the rule shows *needs sharing*; it resumes on the next real transition |
| N21 | Initial silence: leave the door open, restart NeptHass | no alert for the already-open door; the next real opening alerts |
| N22 | Restart persistence: restart NeptHass, `status` and `list` | channel and rules are still configured and *Enabled* |
| N23 | No mentions: give a sensor an alias containing `@everyone` and `<@your-user-id>`, trigger it | the text is shown literally; nobody is pinged |
| N24 | Privacy: view #alerts as a member who has no NeptHass access to the sensors | they can read the alerts (documented); `/sensors` and autocomplete still show them nothing |
| N25 | Channel permission loss: remove the bot's Send Messages in #alerts, trigger an alert, then check `status` and `/health` | the alert is not delivered; status names the missing permission; after repeated failures the breaker is open and health is *Degraded*; restoring the permission recovers within about a minute |
| N26 | Channel deletion: delete #alerts, trigger an alert | status shows the channel was deleted; rules are kept; choosing a new channel recovers |
| N27 | Network cut to Discord for 2 minutes while sensors change, then restore (the bot's gateway drops and reconnects) | alerts are retried and arrive after the restore if younger than 10 minutes; REST delivery works after the gateway reconnects; anything older is dropped, counted, and a digest of the current state follows |
| N28 | Rate limit: with cooldown 0 on several sensors, flap them 20 times quickly | no failed messages; sends are paced (about one per second); `status` shows any retries; observe how a real 429 is handled |
| N29 | Monitoring down: stop the Home Assistant container for more than 5 minutes, then start it | one "monitoring down" alert after five minutes, not repeated; a recovery notice after it returns |
| N30 | While Home Assistant is down open a door, then bring it back | the recovery notice appears before the late alert marked "detected after NeptHass reconnected" |
| N31 | Stop NeptHass itself during an outage of less than five minutes, and again after more than five | no down alert is ever caused by NeptHass stopping |
| N32 | Sensors from unavailable at startup: restart NeptHass while Home Assistant is stopped, then start Home Assistant | a down alert only after five minutes counted from NeptHass's start |
| N33 | `/audit` after the above | notification changes, rejected channels (`rejected_*` actions) and the test are listed; no tokens, channel names or Discord error text. A non-administrator's refused command is not audited |
| N34 | Upgrade path: copy a v0.3.0 database, start v0.4.0 on it; then start a v0.3.0 binary on the upgraded copy | v0.4.0 starts with notifications *Not configured*; v0.3.0 refuses ("database schema is newer"); restoring the backup works |

Not covered by any row because it needs special conditions: the 200-rule limit and the queue-overflow digest with many real sensors (covered by automated tests), and a failed configuration reload (covered by an automated test that breaks the store).

### J. v0.5.0 rules, units, start-up alerts and audit (real Discord)

**Results are not recorded in this file.** This checklist has no result column on purpose: record results (PASS, FAIL, BLOCKED, NOT TESTED or DEFERRED) in your own notes, never in this repository, and mark a row PASS only if you ran it and saw the result. Automated tests are not a substitute for these rows. On 2026-10-31 the maintainer ran the two mandatory smoke checks M1 (start-up alert, summary and deduplication) and M2 (authorization and audit) from the [release readiness report](v0.5.0-release-readiness.md) and reported both as passed; the optional M3 (V32) was not run.

**Setup.** A dedicated test server (never production); the dev stack with the `app` profile built from `feature/v0.5.0`; run the `NeptHass Dev: reset to baseline` script; share the nine virtual sensors with `/setup`; a **private** text channel where the bot has View Channel, Send Messages and Embed Links; `/notifications channel`. Use the NeptHass Dev dashboard (or the scenario scripts) to create every event: **no real hazard, smoke or gas is ever needed.** Durations below use the 2 minute setting; the minimum is 1 minute. Restarting NeptHass only: `docker compose --env-file .env.dev -f compose.dev.yaml --profile app restart nepthass`. Never touch the `nepthass-dev_*` data volumes; for the upgrade rows use a **copy** of a v0.4.0 database in a separate volume.

| # | Step | Expected |
| --- | --- | --- |
| V01 | `/notifications add` autocomplete, typing "garage" and "leak" | Garage door and Opening are offered; the leak says "immediate alerts only"; numeric sensors show their unit; no entity IDs |
| V02 | Add an immediate rule for the garage door and for the opening; run `scenario_garage_door_opened` and `scenario_opening_opened` | One alert each, "is now open", no ping |
| V03 | `/notifications add entity:<water leak> for:5` | Refused: water leak alerts are always immediate; nothing is saved |
| V04 | `/notifications add entity:<door> clear:5` | Refused: a clear value only applies to a numeric rule |
| V05 | Add `<door> for:2`; open the door and wait | No alert before 2 minutes; at 2 minutes one alert "has been open for 2 minutes"; none more while it stays open |
| V06 | Open the door, close it after 1 minute, wait 3 minutes | No alert; `/notifications status` counts one episode that ended without alerting |
| V07 | Open the door; after 1 minute turn its offline helper on, after 10 seconds off (still open) | The count restarts when the sensor returns: the alert comes about 2 minutes after it returned, not earlier |
| V08 | Open the door; after 1 minute `/permissions revoke` the door; wait 3 minutes | No alert; the rule is listed as needing sharing |
| V09 | Open the door; `/notifications pause rule` at 1 minute, `resume` at 1.5 minutes | The alert comes 2 minutes after the resume, not earlier |
| V10 | Open the door; after 1 minute restart NeptHass | No alert for at least 2 minutes after the restart; the alert says "at least"; never earlier |
| V11 | Open the door; stop Home Assistant for longer than the duration, then start it | No duration alert while monitoring is down (the monitoring-down notice follows after 5 minutes); after recovery the count restarts; the alert is late and says "at least" |
| V12 | On one door: an immediate rule, `for:2` and `for:3`; open it | Three alerts, about 0, 2 and 3 minutes after it opened |
| V13 | `<temperature> above:30 for:2`; set the temperature to 31.2 and hold; separately set 31.2 then 29 within a minute | First case: one alert after 2 minutes with the value and °C. Second case: no alert |
| V14 | `<temperature> above:30 clear:28 cooldown:0`; set 31, 29, 31, 27, 31 (one at a time) | An alert at the first 31 and at the last 31 only |
| V15 | `<temperature> below:5 clear:7 cooldown:0`; set 4, 6, 4, 8, 4 | An alert at the first 4 and at the last 4 only |
| V16 | `above:30 clear:30`, `above:30 clear:31`, `below:5 clear:4` | Each refused with the recovery-side message |
| V17 | With the temperature at 31 and an `above:30 clear:28` rule, restart NeptHass; then set 29, 31, 27, 31 | No alert at the restart; none for 29 or the following 31; an alert after 27 then 31 |
| V18 | `/notifications list` | Each rule shows its threshold with the unit, the clear value, the duration and "active" |
| V19 | Change the temperature sensor's unit to °F (see the virtual devices guide); `/notifications list`, then set a value above the threshold | The rule says it is not evaluating because the unit changed; no alert; `status` warns about it |
| V20 | Change the unit back to °C | The rule resumes by itself and works again |
| V21 | Start v0.5.0 on a copy of a v0.4.0 database that has a humidity rule and a temperature rule | The humidity rule is bound by itself (audit shows the system action) and evaluates; the temperature rule says it needs its unit confirmed and sends nothing |
| V22 | As a non-administrator and as an administrator: `/notifications unit rule:<temperature rule>` | Refused for the non-administrator; for the administrator it binds to the sensor's current unit and the rule evaluates; running it again is refused ("never rebound"); autocomplete offered only that rule |
| V23 | Turn the water leak helper on, restart NeptHass | One critical alert: wet according to Home Assistant, "not a new event", when it was last updated, "please check it"; no earlier alert |
| V24 | Restart NeptHass again straight away, then again after 20 minutes (leak still on, Home Assistant not restarted) | No second alert either time |
| V25 | Leak on, restart NeptHass (the first alert). Stop NeptHass, turn the leak off and on again (a new episode: the simulation resets on a Home Assistant restart, so do not rely on one), wait until 20 minutes after the first alert, start NeptHass | One reminder alert (the leak's last-changed time is a new episode identity) |
| V26 | Open the door, the window, the garage door and the opening (immediate rules on each), restart NeptHass | No individual alerts; one start-up summary about 2 minutes after the start that lists the open ones by name |
| V27 | Restart again within 10 minutes, then after 10 minutes | No summary within 10 minutes; a new one after |
| V28 | Turn every offline helper on, restart NeptHass, wait 3 minutes; then turn the leak on and the leak's offline helper off | Nothing sent at the start; when the leak sensor returns wet one "uncertain" alert (not a start-up alert) |
| V29 | Create a door rule on an already open door; create a leak rule on a wet leak sensor | The door reply says nothing is sent now and no summary ever lists it; the leak reply says an alert is on its way and one arrives |
| V30 | Restart only Home Assistant (the simulation returns to baseline) | No start-up alert or summary (a reconnect is not a start). A change hidden by an outage and reported as late is covered by the automated outage tests, because a restart cannot produce one in the simulation |
| V31 | Pause all notifications, or unshare the sensor, then restart with the leak wet | Nothing sent |
| V32 | Remove the bot's Send Messages permission in the alert channel, restart with the leak wet; restore it and restart again | The first attempt fails (visible in `status`), no mark is kept, the next start announces it |
| V33 | As a non-administrator run `/notifications add` and `/permissions allow` | Refused with the usual message; `/audit` shows denied rows with reason not_admin, the command and subcommand names, and no sensor name or option value |
| V34 | As a user on no allowlist run an administrator command | Refused; `/audit` shows reason not_allowed |
| V35 | (Needs the bot in a second server with global command scope) run an administrator command there and in a direct message | Refused; `/audit` shows reason wrong_guild with the guild ID or none |
| V36 | Run a refused administrator command about 10 times in a minute | Replies unchanged; at most 3 rows, later rows say suppressed=N |
| V37 | `/audit` after all of the above | Denials, rule changes and unit bindings are listed; no tokens, no option values, no entity IDs of refused attempts |

Not testable with the virtual devices: more than three simultaneous hazards (there is one virtual leak sensor; covered by automated tests only), and a Discord-only outage or a real rate limit (see the known gaps).

## Known gaps

**v0.5.0 (group J):** the two mandatory real-Discord smoke checks (M1 start-up alert, summary and deduplication; M2 authorization and audit) were run by the maintainer on 2026-10-31 and passed (user-reported); the optional M3 (a channel without Send Messages, V32) was not run, and the remaining rows rely on automated tests. The automated tests use a stub Discord server, so they prove NeptHass's requests and decisions, not what Discord shows.

What the automated tests do not prove, and what is still checked by hand:

- **Real Discord.** No automated test talks to Discord. The v0.4.0 notification tests use a **stub Discord REST server**: they check NeptHass's requests and error handling, not what real Discord does. Not exercised against real Discord by any automated test: an actual HTTP 429 and its `retry_after`, a Discord-only outage (as opposed to a full network outage), real permission and overwrite edge cases, and delivery over REST after a live gateway disconnect. Run the relevant rows of group I when changing the Discord sender.
- **A second Discord user.** Rows that need a second, non-administrator account (N03, N04, N12 and N24) can be approximated by running the bot with your own account as a normal user.
- **Upgrade and downgrade.** The automated tests cover an upgrade from a v0.3.0-shaped database, a restart, and the refusal of a newer schema by an older build. A manual downgrade check on a real v0.3.0 installation (N34) has not been run.
- **Home Assistant outages** (M30, M31) are manual because they need container control. The automated suite covers outages with a TCP proxy, and a real Home Assistant restart is the manual check L12.
- **Polling.** The integration tests poll the real Home Assistant with a bounded timeout after each action, because REST offers no push notification and template entities update a moment after a service call.
- **The setup wizard** is rendered with simulated interactions and checked against Discord's documented limits; live Discord behavior is covered only by the manual group G. Session expiry after 24 hours is covered with an injected clock, so S25 is optional.
- **Third-party integrations** that send a state update without `device_class` cannot be exercised in the dev environment (its template sensors keep the class); the retained-class behavior is covered by automated tests and by Home Assistant's source, not by a real integration.
