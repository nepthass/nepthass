# Smoke tests

There are two separate kinds of checks. Keep them separate when you report results.

| Kind | Needs | Proves | Where it runs |
| --- | --- | --- | --- |
| **Automated integration tests** | the dev Home Assistant and a token | NeptHass's real REST client, entity service, SQLite and audit log behave correctly against a real Home Assistant | a command, repeatable, no Discord |
| **Manual Discord checks** | the dev stack **and** a real Discord test bot | the Discord transport: slash commands, ephemeral replies, autocomplete, authorization of real Discord identities | you, in Discord, by hand |

The automated tests do not talk to Discord. The manual checks are the only place the Discord connection is exercised. **A manual check is "passed" only when a person actually ran it and recorded the result.** The result table at the end of this page ships with every row *Not executed*.

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

Not covered: anything that involves Discord, and Home Assistant being stopped (see manual checks M30 and M31).

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

## Known gaps

- There are no automated Discord checks; real Discord connectivity can only be verified by hand with real credentials.
- Home Assistant outages (M30, M31) are manual because they need container control.
- The integration tests poll the real Home Assistant with a bounded timeout after each action, because REST offers no push notification and template entities update a moment after a service call.
- `/health`'s WebSocket line is intentionally "not available" until v0.3.0.
