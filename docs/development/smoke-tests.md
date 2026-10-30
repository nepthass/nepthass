# Smoke tests

There are two separate kinds of checks. Keep them separate when you report results.

| Kind | Needs | Proves | Where it runs |
| --- | --- | --- | --- |
| **Automated integration tests** | the dev Home Assistant and a token | NeptHass's real REST client, entity service, SQLite and audit log behave correctly against a real Home Assistant | a command, repeatable, no Discord |
| **Manual Discord checks** | the dev stack **and** a real Discord test bot | the Discord transport: slash commands, ephemeral replies, autocomplete, authorization of real Discord identities | you, in Discord, by hand |

The automated tests do not talk to Discord. The manual checks are the only place the Discord connection is exercised. **A manual check is "passed" only when a person actually ran it and reported the result.** The blank result template in section 3 ships with every row *Not executed*; results that were actually reported are recorded separately in section 4 and are labeled by who reported them. The automated results of a release are never recorded as manual passes.

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

**Status: partly executed. The maintainer ran N15-N33 on a real Discord test server and the development Home Assistant on 2026-10-30 (user-reported: see section 4). No result was reported for N01-N14, N24 was not tested (no second Discord user) and N34 is deferred. Rows marked "Not executed" were not run. Never mark a row passed from the automated tests: they use a stub Discord.**

Setup: a dedicated **test** server (never production), the bot invited with `applications.commands`, the dev stack with the `app` profile built from this branch, the simulation at baseline and the seven virtual sensors shared. Put credentials in the env file only; never paste a token into chat, an issue or a commit. Prepare these channels: **#alerts** (bot has View Channel, Send Messages, Embed Links), **#no-send** (bot cannot send), **#no-embed** (bot cannot embed links), **#role-grant** (bot gets send only through a role), **#overwrite-deny** (a channel overwrite denies the bot's role), **#voice**, **#forum**, **#announcements**, a **thread** in #alerts, and a channel in a **second server**.

| # | Step | Expected | Result |
| --- | --- | --- | --- |
| N01 | Start the bot; open the slash-command picker as an administrator and as a normal member | `/notifications` is registered, guild-only (not offered in DMs) | Not executed |
| N02 | `/notifications status` before any setup | "Not configured"; no channel; zero rules | Not executed |
| N03 | `/health` as a normal allowed user | a Notifications line saying *Disabled*; no channel or rule information | Not executed |
| N04 | `/notifications status` and `add` as a non-administrator | refused with "This command is for NeptHass administrators."; nothing changes. **No audit row is written for this refusal** (the command gate refuses before the notification code runs, as for every administrator command) | Not executed |
| N05 | `/notifications channel #alerts` | accepted; the reply names the channel and warns that everyone who can read it sees the alerts | Not executed |
| N06 | `channel #no-send` | rejected, naming *Send Messages* | Not executed |
| N07 | `channel #no-embed` | rejected, naming *Embed Links* | Not executed |
| N08 | `channel` with the voice, forum, announcement channels and the thread (the picker should not offer them; try typing or pasting an ID where Discord allows) | none accepted; a clear "normal text channel" message if one reaches the bot | Not executed |
| N09 | A channel of the second server, or a direct-message channel (only if Discord lets you submit it) | rejected as not in this server | Not executed |
| N10 | `channel #role-grant` (send only via a role); `channel #overwrite-deny` (overwrite denies the bot's role); a channel where the bot has Administrator | the role grant is accepted; the overwrite deny is rejected naming the missing permission; Administrator is accepted | Not executed |
| N11 | `/notifications test` | a test message arrives in #alerts with no mention; `/audit` shows the test | Not executed |
| N12 | `add` with autocomplete as an administrator; try a sensor that is not shared | only shared, monitored sensors are offered, shown by name, never by entity ID; the unshared one is absent | Not executed |
| N13 | `add` errors: `above` and `below` together, `cooldown` 2000, a state rule on a thermometer, a duplicate, before monitoring has synchronized (right after start) | each gives its own clear message and creates nothing | Not executed |
| N14 | `add` Door, Window, Motion, Water leak (state), Temperature `above:30`, Battery `below:50`, each with `cooldown:0`; then `list` | seven rules, numbered, by name, all *active* | Not executed |
| N15 | Run `NeptHass Dev: door opened`, then reset | one alert in #alerts: name, "is now open", time; no mention; no entity ID; closing sends nothing | User-reported pass (section 4) |
| N16 | Run the motion, window, water leak, high temperature and low battery scenarios | one alert each, worded correctly (motion detected, wet, rose above 30, fell below 50 with the value and unit) | User-reported pass (section 4) |
| N17 | Leak unavailable then wet: `sensors offline`, `water leak`, then turn the leak "offline" helper off | an alert saying the state was unknown or unavailable before, "please check it" | User-reported pass (section 4) |
| N18 | Cooldown: add a Door rule with the default cooldown; open, close, reopen within a minute; `status` | one alert; `status` counts one muted; the next alert after the cooldown says how many were muted | User-reported pass (section 4) |
| N19 | `pause rule:<n>`, trigger it, `resume rule:<n>`, trigger it; then `pause` and `resume` with no rule; then `remove` | no alert while paused; alerts after resume; global pause stops everything; a removed rule never alerts | User-reported pass (section 4) |
| N20 | Revoke sharing: `/permissions revoke` the door, open it; `list`; share it again | no alert; the rule shows *needs sharing*; it resumes on the next real transition | User-reported pass (section 4) |
| N21 | Initial silence: leave the door open, restart NeptHass | no alert for the already-open door; the next real opening alerts | User-reported pass (section 4) |
| N22 | Restart persistence: restart NeptHass, `status` and `list` | channel and rules are still configured and *Enabled* | User-reported pass (section 4) |
| N23 | No mentions: give a sensor an alias containing `@everyone` and `<@your-user-id>`, trigger it | the text is shown literally; nobody is pinged | User-reported partial: alias validation passed; outbound mention suppression not exercised (section 4) |
| N24 | Privacy: view #alerts as a member who has no NeptHass access to the sensors | they can read the alerts (documented); `/sensors` and autocomplete still show them nothing | Not tested: no second Discord user |
| N25 | Channel permission loss: remove the bot's Send Messages in #alerts, trigger an alert, then check `status` and `/health` | the alert is not delivered; status names the missing permission; after repeated failures the breaker is open and health is *Degraded*; restoring the permission recovers within about a minute | User-reported pass (section 4) |
| N26 | Channel deletion: delete #alerts, trigger an alert | status shows the channel was deleted; rules are kept; choosing a new channel recovers | User-reported pass (section 4) |
| N27 | Network cut to Discord for 2 minutes while sensors change, then restore (the bot's gateway drops and reconnects) | alerts are retried and arrive after the restore if younger than 10 minutes; REST delivery works after the gateway reconnects; anything older is dropped, counted, and a digest of the current state follows | User-reported partial: full Docker network outage only, Discord-only outage not isolated (section 4) |
| N28 | Rate limit: with cooldown 0 on several sensors, flap them 20 times quickly | no failed messages; sends are paced (about one per second); `status` shows any retries; observe how a real 429 is handled | User-reported partial: burst and queue drain passed, no real 429 observed (section 4) |
| N29 | Monitoring down: stop the Home Assistant container for more than 5 minutes, then start it | one "monitoring down" alert after five minutes, not repeated; a recovery notice after it returns | User-reported pass (section 4) |
| N30 | While Home Assistant is down open a door, then bring it back | the recovery notice appears before the late alert marked "detected after NeptHass reconnected" | User-reported partial: general recovery passed, missed-event scenario not verified (section 4) |
| N31 | Stop NeptHass itself during an outage of less than five minutes, and again after more than five | no down alert is ever caused by NeptHass stopping | User-reported pass (section 4) |
| N32 | Sensors from unavailable at startup: restart NeptHass while Home Assistant is stopped, then start Home Assistant | a down alert only after five minutes counted from NeptHass's start | User-reported pass (section 4) |
| N33 | `/audit` after the above | notification changes, denied attempts and the test are listed; no tokens, channel names or Discord error text | User-reported partial: core audit behaviour passed, not every rejection variant (section 4) |
| N34 | Upgrade path: copy a v0.3.0 database, start v0.4.0 on it; then start a v0.3.0 binary on the upgraded copy | v0.4.0 starts with notifications *Not configured*; v0.3.0 refuses ("database schema is newer"); restoring the backup works | Deferred by decision (private pre-v1.0.0 development) |

Not covered by any row because it needs special conditions: the 200-rule limit and the queue-overflow digest with many real sensors (covered by automated tests), and a failed configuration reload (covered by an automated test that breaks the store).

### Reporting and recording results

Copy the table below into your notes or pull request and fill it in. **Leave a row as "Not executed" unless you ran it.** Never mark a row passed from the unit tests or the automated integration tests.

## 3. Result record

Version/commit tested: `________`   Tester: `________`   Date: `________`

| Group | Checks | Executed | Result | Notes |
| --- | --- | --- | --- | --- |
| Automated integration tests | all, see section 1 | Not executed | | |
| A. Connectivity | M01-M02 | Not executed | | |
| B. Unauthorized access | M10-M15 | Not executed | | |
| C. Permissions and private data | M20-M29 | Not executed | | |
| D. Reading and controlling | M40-M51 | Not executed | | |
| E. Revoking, restarting, auditing | M60-M68 | Not executed | | |
| F. Resilience | M30-M32 | Not executed | | |
| G. v0.2.0 setup wizard and /sync | S01-S25 | Partly: see section 4 (user-reported) | User-reported pass for S01, S03, S08, S09, S10, S13, S14, S15, S18, S22 | The other group G rows were not reported |
| H. v0.3.0 live monitoring | L01-L18 | Partly: see section 4 (user-reported, 2026-10-29) | User-reported pass for L01 to L05, L07 to L09, L11, L12, L15, L16 (recovery after start) and L18; L10 from earlier testing on an earlier revision; L17 partly | Not executed or not recorded: L06, L13, L14, the pending L17 commands, and the manual L16 pre-synchronization message |
| I. v0.4.0 notifications (real Discord) | N01-N34 | Partly: see section 4 (user-reported, 2026-10-30) | User-reported pass for N15-N22, N25, N26, N29, N31, N32; partial for N23, N27, N28, N30, N33 | No result reported for N01-N14; N24 not tested; N34 deferred |

## Known gaps

- There are no automated Discord checks; real Discord connectivity can only be verified by hand with real credentials. The v0.4.0 notification tests use a **stub Discord REST server**: they check NeptHass's requests and error handling, not what real Discord does (429 `retry_after` handling, real permission edge cases, REST delivery after a live gateway disconnect).
- Home Assistant outages (M30, M31) are manual because they need container control.
- The integration tests poll the real Home Assistant with a bounded timeout after each action, because REST offers no push notification and template entities update a moment after a service call.
- Monitoring recovery from a real Home Assistant container restart needs container control, so it is a manual check (L12). The automated suite covers outages with a TCP proxy; a real restart was observed once with a temporary probe (section 4).
- The automated tests render the wizard (embeds, buttons, select menus, modals) with simulated interactions and check it against Discord's documented limits; they never talk to Discord. Live Discord behavior is covered only by manual group G, whose recorded results (section 4) are user-reported.
- Session expiry after 24 hours is covered by automated tests with an injected clock; S25 is optional because it takes a day.
- Whether a third-party integration sends a state update without `device_class` cannot be exercised in the dev environment (its template sensors keep the class); the retained-class behavior is covered by automated tests and by Home Assistant's source, not by a real integration.

## 4. Recorded results

### v0.2.0 release (group G): user-reported manual results

**Source: the project owner.** The owner reported, in the v0.2.0 release request, that the manual Discord smoke tests for the setup wizard were completed successfully on their own Discord test server and development stack. The assistant that prepared the release did **not** execute or observe these checks (it has no Discord credentials), so they are recorded as *user-reported passes* and are kept apart from the automated results below. The commit that was tested was not recorded.

The report named these areas. They are mapped here only to the checks that match them, and nothing else is marked as passed.

| Area the owner reported | Checks | Status |
| --- | --- | --- |
| Interactive `/setup` wizard | S01 | User-reported pass |
| Entity discovery and selection | S03, S08 | User-reported pass |
| Permission configuration | S09 | User-reported pass |
| Alias creation and resolution | S10, and the alias part of S13 | User-reported pass |
| Real Home Assistant device control | S13 (the `/control` part) | User-reported pass |
| Restart persistence and setup session resume | S15 | User-reported pass |
| Stale interaction protection | S14 | User-reported pass |
| Stale confirmation protection | S18 | User-reported pass |
| Non-admin authorization checks | S22 | User-reported pass |

Not reported (so *not* recorded as passed): S02, S04, S05, S06, S07, S11, S12, S16, S17, S19, S20, S21, S23, S24, S25, and every check in groups A to F (v0.1.0 checks M01 to M68 stay *Not executed* in this record).

### v0.2.0 release: automated results (executed by the assistant, not manual)

These were produced by commands run against the release tree, never by a person in Discord:

| Check | Result |
| --- | --- |
| `gofmt -l .`, `go vet ./...`, `go vet -tags=integration ./tests/integration/...` | clean |
| `go test -race -shuffle=on -count=1 ./...` | all 11 packages pass |
| Integration suite against a real Home Assistant 2026.10.4 (`go test -tags=integration`) | 21 tests, 45 PASS lines, 0 failures, 0 skips |
| Security regression suite: 16 injected regressions of the setup, authorization, alias and escaping controls | all 16 caught by tests |
| Migration compatibility: v0.1.0 to v0.2.0 upgrade test, and a read-only copy of a real development database | upgrade preserves grants and audit rows; the copy passes SQLite's integrity check and re-opens as a no-op |
| `docker compose config` (production, combined example, development with all profiles) and the image build and hardened boot against the real Home Assistant | pass |

### v0.3.0 (group H): user-reported manual results

**Source: the project owner, 2026-10-29.** The owner reported these results from manual Discord and Home Assistant testing on their own development stack. The assistant that prepared this record has no Discord credentials and did **not** execute or observe these checks, so they are recorded as *user-reported* and kept apart from the automated results below. The Discord output and logs the owner captured are in the owner's possession; they are not stored in this repository.

**Revision.** The owner identified `0723116` as the manually tested revision (it contains the release-blocking fixes `e1feb86` to `5bb3bed`). **L10 is the exception:** it comes from earlier manual testing, before those fixes, and its exact revision was not recorded (the branch tip was at or before `6a7d4f2`; the permission check lives in `internal/monitor/reader.go`, which has not changed since `9037a08`). Commits made after `0723116` for the pull request change documentation and one test-support file, `tests/fakeha/ws.go` (a fix for a timing race in the fake Home Assistant that made one test flaky on a slow CI runner); no production code differs from `0723116`.

| Check | Result | Evidence, as reported |
| --- | --- | --- |
| L01 `/health` | User-reported pass | WebSocket line Healthy, seven sensors tracked, snapshot synchronized |
| L02 `/sensors` baseline | User-reported pass | |
| L03 motion detected | User-reported pass | |
| L04 door, window, water leak, temperature and battery scenarios | User-reported pass | user-confirmed manual testing; some of it with captured Discord output |
| L05 reset | User-reported pass | reported as the motion reset; a full "reset to baseline" run was not itemized |
| L06 all sensors offline | **Not recorded** | not part of the owner's final report (an earlier report mentioned unavailable-sensor handling in general, without a row-level result); treated as pending |
| L07 `/status` agrees with `/sensors` | User-reported pass | user-confirmed |
| L08 unauthorized user denied | User-reported pass | user-confirmed |
| L09 only shared sensors visible | User-reported pass | user-confirmed |
| L10 revoke and restore | User-reported pass, **earlier revision** | earlier manual testing; revision not recorded (see above) |
| L11 alias | User-reported pass | user-confirmed |
| L12 Home Assistant outage and reconnect | User-reported pass | captured health and log evidence: exponential reconnect delays during a deliberately induced outage, then successful reconnection, authentication, subscription and snapshot synchronization |
| L13 invalid token | **Not executed manually** | automated coverage only |
| L14 network drop | **Not executed separately** | the Home Assistant outage and reconnect (L12) was tested, which is a different scenario; automated coverage only (TCP proxy against a real Home Assistant) |
| L15 graceful shutdown | User-reported pass | the container exited with code 0 |
| L16 startup recovery | User-reported pass for the recovery after start; **pre-synchronization wording not captured** | synchronization completed too quickly to capture the wording; the pre-synchronization behavior has automated coverage only |
| L17 existing commands | **Partly** | see below |
| L18 log hygiene | User-reported pass for the logs inspected | successful WebSocket authentication and event subscription; initial synchronization of seven sensors; exponential reconnect delays during the outage; successful reconnection and resynchronization; clean shutdown and restart; no visible credentials, panic or fatal errors. Not an exhaustive log review |

**L17 evidence.** `/ping`, `/devices`, `/status` and `/control` passed. `/devices` showed five shared entities with read and control levels. `/control` turned `light.nepthass_dev_light` from `off` to `on`, and a later `/status` independently confirmed `State: on` and the alias `Living Room Light`. `/sync` (a group G command, not named in the L17 row) discovered 43 Home Assistant entities, reported zero new and zero removed, and stated that nothing was shared or changed. **Reported afterwards:** the owner also confirmed `/control` off and `/permissions list` (user-reported). **Still pending in L17:** `/audit` and `/setup` step 1 (the monitoring line in the wizard's health step).

**Outstanding manual coverage.** L06, L13 and L14; the pending parts of L17; and a manual capture of the L16 pre-synchronization message. Automated tests cover the underlying behavior of each of these (see section 1 and the results below), but an automated test is not a manual pass, and none of them is recorded as one.

### v0.3.0 (`feature/v0.3.0`, unreleased): automated results (executed by the assistant, not manual)

These were produced by commands run against the branch, never by a person in Discord. The assistant has no Discord credentials, so none of these results verifies any Discord behavior; that is covered only by the owner's user-reported results above. The first table was produced at commit `c14727d`, before the release-blocking fixes; the second table is the final validation of the code at `0723116` on 2026-10-29.

| Check | Result |
| --- | --- |
| `gofmt -l .`, `go vet ./...`, `go vet -tags=integration ./tests/integration/...` | clean |
| `go test -race -shuffle=on -count=1 ./...` | 13 packages pass; 227 passing test lines including subtests; 0 failures, 0 skips |
| Stress runs during development | `go test -race -shuffle=on -count=10 ./internal/monitor` and `-count=20 ./internal/homeassistant/ws` pass |
| Integration suite against a real Home Assistant 2026.10.4 (`go test -tags=integration -race`) | 28 tests (the 21 from v0.1.0 and v0.2.0 plus 7 monitoring tests), 0 failures, 0 skips |
| Real Home Assistant container restart (a temporary probe, not committed, with the container restarted from outside) | the monitor left `ready`, reported failure category `network` and not healthy while keeping its 7 cached sensors, then recovered by itself after about 14 seconds with a fresh synchronized snapshot and 0 invalid or dropped events |
| Ping and pong against real Home Assistant (a temporary probe, not committed) | over 7 seconds with a 2 second read timeout and no sensor events, the connection stayed healthy with 0 reconnects, so Home Assistant answers the application-level ping |
| Regression injection into the security and correctness controls | 20 deliberate regressions, one at a time (permission check removed, stale or duplicate event guards removed, snapshot ordering reversed, buffered events dropped, health ignoring readiness or proof of life, token echoed in an error, read limit removed, unescaped names, and others); all 20 caught. One needed an added assertion (stale flag in listings) and one was first misreported as surviving because of a harness timeout and was re-run correctly |
| `docker compose config` for `compose.yaml`, `deploy/compose.with-homeassistant.yaml` and `compose.dev.yaml` (all profiles) | pass; the production files are byte-identical to v0.2.0 |
| Image build and a hardened container (read-only root, all capabilities dropped, no-new-privileges) started against the real Home Assistant with fake Discord credentials | the application authenticated, the monitor authenticated, subscribed and synchronized 7 sensors, and `home assistant monitoring stopped` was logged before the process exited when Discord rejected the fake token; no token in the logs. No Discord connection was made |
| Documentation links | 17 Markdown files, 67 relative links checked; one broken anchor from before v0.3.0 (`docs/commands.md` to `setup.md`) |

#### Final validation before the pull request (executed by the assistant, 2026-10-29, code at `0723116`)

| Check | Result |
| --- | --- |
| `gofmt -l .`, `go mod verify`, `go vet ./...`, `go vet -tags=integration ./tests/integration/...`, `go build ./cmd/nepthass` | clean |
| `go test -race -shuffle=on -count=1 ./...` | 13 packages pass; 249 passing test lines including subtests; 0 failures, 0 skips |
| Integration suite against a real Home Assistant 2026.10.4 (an isolated Compose project, removed afterwards; the owner's dev stack was not used) | 28 tests, 0 failures, 0 skips |
| `docker compose config` for `compose.yaml`, `deploy/compose.with-homeassistant.yaml` and `compose.dev.yaml` (all profiles); image build | pass; the image carries `org.opencontainers.image.source=https://github.com/nepthass/nepthass` |
| During the release-blocking fixes (earlier the same day) | the changed packages passed `-race -shuffle=on -count=4` on one CPU; 9 deliberate regressions of the fixes (including retaining a class for a real value) were all caught by the tests |

### v0.4.0: automated results (executed by the assistant, not manual)

Executed on 2026-10-30 against the final release tree (before the pull request). These are automated results; the manual Discord results are in the next subsection and are user-reported and partial.

- `gofmt`, `go vet ./...` (also with the `integration` tag), `go build` and `go mod verify`: clean.
- `go test -race -shuffle=on -count=1 ./...`: 15 packages, 379 passing test lines, 0 failures, 0 skipped.
- Real Home Assistant 2026.10.4 integration suite (isolated Compose project, removed afterwards): 39 of 39, the 28 existing tests plus 11 notification tests (door, motion, leak, leak unavailable to wet, temperature and battery thresholds, cooldown, revocation, an already-open door at startup, a real outage with a late alert, a five-minute disconnect with down and recovery notices on an injected clock, a Discord outage with retries, queue saturation with a digest, restart persistence). The sender in those tests is a recorder, not Discord.
- Stub-Discord REST tests (`internal/discord`): delivery and empty allowed-mentions, hostile names (`@everyone`, `@here`, user, role and channel mentions) through the real send path, one HTTP request per attempt for every error class, a real HTTP 429 with `retry_after` through the dispatcher, channel validation including role and overwrite combinations, REST after the session is closed, audit of rejected actions, a failed reload that heals, a queued down alert withdrawn on recovery, an ambiguous failure producing a duplicate, a breaker that recovers, multi-identity authorization through the real interaction path, and shutdown with an in-flight delivery. A stub proves NeptHass's requests and error handling, not Discord's behaviour.
- Constrained-CPU stress with the race detector, one package at a time: 0.5 CPU for notify, discord, discord/commands, storage and cmd (8 runs each) and 0.25 CPU for the goroutine-heavy end-to-end, admin, dispatcher, engine and notifier tests (10 runs each): all pass, no failures.
- Migration: a v0.3.0-shaped database (migrations 0001-0002 with data) upgrades to 0003, survives a restart unchanged, and a v0.3.0 build is refused ("schema is newer"). This verifies the upgrade mechanics only; the manual N34 downgrade check is deferred.
- Docker Compose configuration validated for the three files CI checks, and the application image built locally with `docker build` (not published, removed afterwards).
- Audit findings fixed during the final audit (each with a regression test): the circuit breaker's probe slot consumed by a revoked message, and a dispatcher lost wake-up found by stress.
- **Not executed:** a real HTTP 429 or a Discord-only outage against real Discord, a second Discord user, and N34.

### v0.4.0 (group I): user-reported manual results

Reported by the maintainer on 2026-10-30 after running the checklist on a real Discord test server and the development Home Assistant. The release manager did not observe these runs; they are recorded as reported and are not upgraded to full passes.

- **Passed:** N15, N16, N17, N18, N19, N20, N21, N22 (events, cooldown, pause/resume/remove, revocation, restart silence and persistence), N25 (channel permission failure and recovery), N26 (deleted channel recovery), N29 (Home Assistant outage detection), N31 and N32 (restart and outage handling).
- **Partial:** N23 (alias validation passed; outbound mention suppression was not directly exercised on Discord), N27 (a full Docker network outage was tested; a Discord-only outage was not isolated), N28 (burst delivery and queue drain passed; no real HTTP 429 was observed and none was provoked on purpose), N30 (general recovery passed; the exact missed-event recovery scenario was not verified), N33 (core audit behaviour passed; not every rejection variant was verified).
- **Not tested:** N24 (needs a second Discord user).
- **Deferred:** N34 (v0.3.0 to v0.4.0 migration and downgrade compatibility), by decision, because the project is still in private pre-v1.0.0 development. A fresh v0.4.0 database, the 0003 migration, persistence and restart are covered by automated tests.
- **No result reported:** N01-N14.

The gaps above are covered by automated tests that do not depend on real Discord: mention suppression with hostile names (including role, channel and `@here` mentions) through the real send path, a real HTTP 429 with `retry_after` through the dispatcher, a Discord-only failure with retries, a missed opening found by the reconnect snapshot against a real Home Assistant, duplicate suppression, multi-identity authorization through the real interaction path, and the audit variants. A stub proves NeptHass's requests and error handling, not Discord's behaviour.
