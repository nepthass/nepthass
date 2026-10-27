# Virtual devices and sensor simulation

The development Home Assistant (`dev/homeassistant/`) defines virtual devices with Home Assistant's own template and helper integrations. They are real entities served by a real Home Assistant, but there is no hardware behind them. Their IDs and baseline states are fixed, so tests and manual checks are repeatable.

## Entity list

| Entity ID | Name | Kind | Device class | Unit | Baseline | NeptHass can |
| --- | --- | --- | --- | --- | --- | --- |
| `light.nepthass_dev_light` | NeptHass Dev Light | template light | | | `off` | read, **control** |
| `switch.nepthass_dev_switch` | NeptHass Dev Switch | template switch | | | `off` | read, **control** |
| `binary_sensor.nepthass_dev_motion` | NeptHass Dev Motion | binary sensor | `motion` | | `off` (no motion) | read only |
| `binary_sensor.nepthass_dev_door` | NeptHass Dev Door | binary sensor | `door` | | `off` (closed) | read only |
| `binary_sensor.nepthass_dev_window` | NeptHass Dev Window | binary sensor | `window` | | `off` (closed) | read only |
| `binary_sensor.nepthass_dev_water_leak` | NeptHass Dev Water Leak | binary sensor | `moisture` | | `off` (dry) | read only |
| `sensor.nepthass_dev_temperature` | NeptHass Dev Temperature | sensor | `temperature` | `°C` | `21.5` | read only |
| `sensor.nepthass_dev_humidity` | NeptHass Dev Humidity | sensor | `humidity` | `%` | `45` | read only |
| `sensor.nepthass_dev_battery` | NeptHass Dev Battery | sensor | `battery` | `%` | `87` | read only |

For binary sensors `on` means the condition is **active**: motion detected, door or window **open**, water detected. (Home Assistant calls the leak class `moisture`.)

The light and switch are real template entities. `light.turn_on`, `light.turn_off`, `switch.turn_on` and `switch.turn_off` run actual Home Assistant services that flip a backing helper, and the entity state is derived from that helper, like a device that reports back. They accept control from the Home Assistant UI, from the REST API and from NeptHass.

Every virtual sensor also becomes `unavailable` while its offline helper is on (see below), which is what a real device that lost its connection looks like.

### Units

Home Assistant converts temperature sensors to the instance's unit system, so on a US customary instance the temperature sensor would report `70.7 °F` instead of `21.5 °C`. The development configuration pins `unit_system: metric`, but the onboarding UI can switch a running instance to US customary until its next restart. The `NeptHass Dev: reset to baseline` script therefore re-applies the pinned metric system (`homeassistant.reload_core_config`) and refreshes the numeric sensors, and the integration tests refuse to run on a non-metric instance.

### Baseline and restarts

Every helper has an `initial` value, so the whole simulation returns to the baseline above each time Home Assistant restarts. You can also return to it at any time with the `NeptHass Dev: reset to baseline` script.

## Helpers that drive the simulation

| Helper | Controls | Values |
| --- | --- | --- |
| `input_boolean.nepthass_dev_sim_motion` | motion sensor | `on` = motion detected |
| `input_boolean.nepthass_dev_sim_door_open` | door sensor | `on` = open |
| `input_boolean.nepthass_dev_sim_window_open` | window sensor | `on` = open |
| `input_boolean.nepthass_dev_sim_leak` | water leak sensor | `on` = wet |
| `input_number.nepthass_dev_sim_temperature` | temperature | -40 to 80, step 0.1 |
| `input_number.nepthass_dev_sim_humidity` | humidity | 0 to 100 |
| `input_number.nepthass_dev_sim_battery` | battery | 0 to 100 |
| `input_boolean.nepthass_dev_offline_motion` / `_door` / `_window` / `_temperature` / `_humidity` / `_leak` / `_battery` | availability of the matching sensor | `on` = sensor `unavailable` |
| `input_boolean.nepthass_dev_light_backing`, `_switch_backing` | state of the virtual light and switch | internal; use the light and switch instead |

## Scenario scripts

One click (or one API call) per situation the future milestones will react to.

| Script | Effect | Resulting state |
| --- | --- | --- |
| `script.nepthass_dev_scenario_reset` | everything back to baseline | see the entity list |
| `script.nepthass_dev_scenario_motion_detected` | motion goes inactive to active | `binary_sensor.nepthass_dev_motion` = `on` |
| `script.nepthass_dev_scenario_door_opened` | door opens | `binary_sensor.nepthass_dev_door` = `on` |
| `script.nepthass_dev_scenario_window_opened` | window opens | `binary_sensor.nepthass_dev_window` = `on` |
| `script.nepthass_dev_scenario_high_temperature` | temperature above a 30 °C threshold | `sensor.nepthass_dev_temperature` = `31.2` |
| `script.nepthass_dev_scenario_water_leak` | leak detected | `binary_sensor.nepthass_dev_water_leak` = `on` |
| `script.nepthass_dev_scenario_low_battery` | battery drops | `sensor.nepthass_dev_battery` = `12` |
| `script.nepthass_dev_scenario_sensors_offline` | every sensor loses connection | all seven sensors `unavailable` |

Scenarios only change simulation helpers. To undo a single one, toggle its helper back (for example close the door with the `Simulate door open` switch) or run the reset script.

## How to change a state

### In the Home Assistant UI

1. Open <http://127.0.0.1:18123> and choose **NeptHass Dev** in the sidebar.
2. The dashboard has five cards: the controllable devices, the read-only sensors, the scenario scripts (click **Run**), the simulation inputs (toggle or type a number), and the connection-loss switches.

Alternatively use **Developer Tools > Actions**, pick `input_boolean.turn_on` (or `turn_off`, `input_number.set_value`, `script.turn_on`) and choose the entity.

### Through the Home Assistant REST API

Create a Long-Lived Access Token first (see [setup.md](setup.md)), then, with the default port:

```bash
export HA_TOKEN=...   # your dev token; do not commit it
BASE=http://127.0.0.1:18123

# run a scenario
curl -fsS -X POST -H "Authorization: Bearer $HA_TOKEN" -H "Content-Type: application/json" -d '{}' \
  $BASE/api/services/script/nepthass_dev_scenario_water_leak

# switch a simulated condition
curl -fsS -X POST -H "Authorization: Bearer $HA_TOKEN" -H "Content-Type: application/json" \
  -d '{"entity_id":"input_boolean.nepthass_dev_sim_door_open"}' $BASE/api/services/input_boolean/turn_on

# set a number
curl -fsS -X POST -H "Authorization: Bearer $HA_TOKEN" -H "Content-Type: application/json" \
  -d '{"entity_id":"input_number.nepthass_dev_sim_temperature","value":31.2}' $BASE/api/services/input_number/set_value

# turn the virtual light on (what NeptHass does for /control)
curl -fsS -X POST -H "Authorization: Bearer $HA_TOKEN" -H "Content-Type: application/json" \
  -d '{"entity_id":"light.nepthass_dev_light"}' $BASE/api/services/light/turn_on

# read a state
curl -fsS -H "Authorization: Bearer $HA_TOKEN" $BASE/api/states/sensor.nepthass_dev_temperature
```

PowerShell:

```powershell
$env:HA_TOKEN = "..."   # your dev token
$h = @{ Authorization = "Bearer $env:HA_TOKEN" }
$base = "http://127.0.0.1:18123"
Invoke-RestMethod -Method Post -Headers $h -ContentType 'application/json' -Body '{}' `
  -Uri "$base/api/services/script/nepthass_dev_scenario_water_leak"
Invoke-RestMethod -Headers $h -Uri "$base/api/states/binary_sensor.nepthass_dev_water_leak"
```

## Triggers for the upcoming milestones

These are the situations the v0.3.0 (real-time monitoring) and v0.4.0 (notifications) work will need. Nothing in NeptHass v0.1.0 listens for these events yet: today you can only observe them with `/status` and `/devices`.

| To trigger | Do this | Entity transition |
| --- | --- | --- |
| Motion detected | run `scenario_motion_detected` | `binary_sensor.nepthass_dev_motion`: `off` to `on` |
| Motion cleared | turn `nepthass_dev_sim_motion` off | `on` to `off` |
| Door opened | run `scenario_door_opened` | `binary_sensor.nepthass_dev_door`: `off` to `on` |
| Door closed (cancels "left open" timers) | turn `nepthass_dev_sim_door_open` off | `on` to `off` |
| Window opened / closed | `scenario_window_opened`; turn `nepthass_dev_sim_window_open` off | `binary_sensor.nepthass_dev_window` |
| Temperature above threshold | run `scenario_high_temperature` (31.2 °C) | `sensor.nepthass_dev_temperature`: `21.5` to `31.2` |
| Temperature hysteresis | set `nepthass_dev_sim_temperature` to values such as 29.5, 30.5, 28.0 | numeric changes around a threshold |
| Water leak | run `scenario_water_leak` | `binary_sensor.nepthass_dev_water_leak`: `off` to `on` |
| Low battery | run `scenario_low_battery` (12 %) | `sensor.nepthass_dev_battery`: `87` to `12` |
| Sensor unavailable | run `scenario_sensors_offline`, or turn one `nepthass_dev_offline_*` helper on | state becomes `unavailable` |
| Recovery from unavailable | turn the helper off again | `unavailable` to the real value |
| Home Assistant connection loss | `docker compose --env-file .env.dev -f compose.dev.yaml stop homeassistant`, later `start homeassistant` | REST API unreachable, then back |

Three details matter for the monitoring milestones:

- `unavailable` and `unknown` are never "safe" or "inactive"; the offline helpers give you a reliable way to exercise that rule.
- After an HA restart every helper returns to its baseline, which is a handy way to test reconciliation after reconnect without generating real alerts.
- Scenario scripts change state instantly. Time-based rules (for example "door open for 10 minutes") are tested by waiting; the scripts do not fake time.

## Changing or extending the simulation

The configuration is three small files you can read in a minute: `dev/homeassistant/configuration.yaml`, `dev/homeassistant/packages/*.yaml` (helpers, light and switch, sensors, scenarios) and `dev/homeassistant/dashboards/nepthass-dev.yaml`. They are mounted read-only. After editing, restart Home Assistant:

```bash
docker compose --env-file .env.dev -f compose.dev.yaml restart homeassistant
```

Keep IDs stable (`*_nepthass_dev_*`): the integration tests and these docs depend on them.
