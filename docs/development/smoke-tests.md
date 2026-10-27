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
| L12 | restart Home Assistant only (`docker compose --env-file .env.dev -f compose.dev.yaml restart homeassistant`); run `/health` and `/sensors` while it is down, and again once it is healthy | while down: the monitoring line shows a warning ("Cannot reach Home Assistant's WebSocket; retrying, next attempt ...", last known values may be outdated), the four layers are crossed, `/sensors` shows the amber banner and warning icons; afterwards: Healthy again, the Reconnects counter is at least 1, the banner is gone, values current. `/devices`, `/status` and `/control` keep working (when Home Assistant is up) |
| L13 | (optional) invalid token: delete the long-lived token in the Home Assistant profile, then restart Home Assistant | `/health` shows "Home Assistant rejected the access token for the WebSocket (check HA_TOKEN)" and retries slowly (30 s growing to 10 min); after creating a new token, updating `.env.dev` and `up -d`, it is Healthy again |
| L14 | (optional) network drop: with Home Assistant running, `docker network disconnect nepthass-dev_dev nepthass-dev-nepthass-1`, run `NeptHass Dev: door opened` from the Home Assistant UI, then `docker network connect nepthass-dev_dev nepthass-dev-nepthass-1`. This also cuts NeptHass's Discord connection until you reconnect | after reconnecting and a few seconds, `/sensors` shows the door open although NeptHass never saw the event (reconciliation) |
| L15 | `docker compose --env-file .env.dev -f compose.dev.yaml --profile app stop nepthass`, then read its log | it stops within a few seconds with exit code 0 and logs `home assistant monitoring stopped`; starting it again works |
| L16 | `restart nepthass`, run `/sensors` immediately and again after about ten seconds | right after start: not connected or stale; ten seconds later: healthy with current values; permissions and aliases are unchanged |
| L17 | existing commands: `/ping`, `/devices`, `/status`, `/control` (light on and off), `/permissions`, `/audit`, and `/setup` step 1 | all behave as in v0.2.0; the wizard's health step shows the monitoring line instead of "not available in this release" |
| L18 | `docker compose --env-file .env.dev -f compose.dev.yaml logs nepthass` after all of the above | no token, no sensor values and no raw Home Assistant payloads anywhere in the log |

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
| H. v0.3.0 live monitoring | L01-L18 | Not executed | | Manual acceptance of v0.3.0 is pending |

## Known gaps

- There are no automated Discord checks; real Discord connectivity can only be verified by hand with real credentials.
- Home Assistant outages (M30, M31) are manual because they need container control.
- The integration tests poll the real Home Assistant with a bounded timeout after each action, because REST offers no push notification and template entities update a moment after a service call.
- Monitoring recovery from a real Home Assistant container restart needs container control, so it is a manual check (L12). The automated suite covers outages with a TCP proxy; a real restart was observed once with a temporary probe (section 4).
- The automated tests render the wizard (embeds, buttons, select menus, modals) with simulated interactions and check it against Discord's documented limits; they never talk to Discord. Live Discord behavior is covered only by manual group G, whose recorded results (section 4) are user-reported.
- Session expiry after 24 hours is covered by automated tests with an injected clock; S25 is optional because it takes a day.

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

### v0.3.0 (`feature/v0.3.0`, unreleased): automated results (executed by the assistant, not manual)

These were produced by commands run against the branch, never by a person in Discord. **Manual group H (L01 to L18) was not executed**: the assistant has no Discord credentials, so no Discord behavior of `/sensors` or the new `/health` line is verified here, and v0.3.0 is not release-ready until the owner runs that checklist.

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
