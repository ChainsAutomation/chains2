# Writing Rules in Chains

Chains automations are written in a compact, indentation-based DSL stored in `.chains` files (or authored directly in the web console under **Rules**). Each file defines one rule, and its filename stem (or web rule name) is the rule's name.

Rules are **live, stateful workflows**: an opening trigger starts a run in its own lightweight execution context, and the body can inspect State, branch, loop, wait for follow-up events, pause with `Sleep()`, and send actions or attribute updates.

---

## Contents

- [Anatomy of a rule](#anatomy-of-a-rule)
- [Rule settings](#rule-settings)
- [Triggers and waits](#triggers-and-waits)
- [Devices, placement, and State](#devices-placement-and-state)
- [Controlling devices (`Set` and `Action`)](#controlling-devices-set-and-action)
- [Language reference](#language-reference)
- [Cookbook: real-world examples](#cookbook-real-world-examples)
- [Authoring, validation, and live publishing](#authoring-validation-and-live-publishing)

---

## Anatomy of a rule

Every rule follows the same top-to-bottom structure:

1. **Optional rule settings** (`rule.mode`, `rule.scope`, `rule.concurrency`, `rule.timeout`) at the very top.
2. **Optional pure setup assignments** (device aliases, constants, pure expressions).
3. **One opening wait** — `Event()`, `Every()`, `Time()`, or `select:` — that triggers each run.
4. **The rule body** — statements that execute sequentially once the opening wait matches.

```text
# 1. Optional settings
rule.mode = "restart"
rule.timeout = "10m"

# 2. Optional pure setup
sensor = Device(service_id="zigbee2mqtt", device_id="0x00158d0001a2b3c4")
lamp   = Device(service_name=Zigbee2MQTT, device_name="hall/light")

# 3. Opening trigger
Event(type="state.changed", device=sensor, attribute="occupancy", value=true, initial=false)

# 4. Body
Set(device=lamp, attribute="power", value=true)
Sleep("2m")
Set(device=lamp, attribute="power", value=false)
```

> [!NOTE]
> Indentation uses **spaces** (typically 4 spaces per level). Tabs are rejected. Blank lines and `#` comments are ignored anywhere in the file, and expressions inside `(...)`, `[...]`, or `{...}` may span multiple lines.

---

## Rule settings

Place settings before any assignments or triggers. Each setting may appear at most once.

| Setting | Type / Values | Default | Description |
| --- | --- | --- | --- |
| `rule.mode` | `"drop"` or `"restart"` | `"drop"` | `"drop"` skips new opening matches while all concurrency slots are busy. `"restart"` cancels the active run and starts fresh on a new opening match (requires single concurrency). |
| `rule.scope` | `"rule"`, `"device"`, or `"service"` | `"rule"` | Controls how `rule.concurrency` and `rule.mode = "restart"` are partitioned: across the whole rule, independently per triggering `ev.device`, or per `service_id`. |
| `rule.concurrency` | Integer `1`–`1024` | `1` | Maximum simultaneous runs per scope key. Cannot be `> 1` when `rule.mode = "restart"`. |
| `rule.timeout` | Positive number (seconds) or duration string (`"15m"`, `"500ms"`) | `300` | Overall run deadline starting from the opening match. Applies across later waits, `Sleep()`, actions, and computation. |

```text
rule.concurrency = 4
rule.timeout = 90
```

A run ends when it reaches the end of the file, executes `return`, hits its deadline, encounters a runtime error, or is cancelled (by `rule.mode = "restart"`, live rule replacement, or engine shutdown).

---

## Triggers and waits

### `Event(...)`

`Event()` matches live events on the bus. All arguments are named selectors; every supplied selector must match.

```text
# Match a specific event type and device
ev = Event(type="humidity.reported", device_id="bathroom-sensor")

# Match by device class (including inherited classes like dimmer -> light -> toggle)
ev = Event(device_class=light)

# Match by location pattern and tag
ev = Event(type="device.observed", location="House.Bedroom.**", tag=guest)

# Match a device alias declared in pure setup
ev = Event(type="zigbee2mqtt.action", device=button, action=single)
```

**Selector categories:**

- **Envelope fields:** `type`, `source`, `service_id`, `service_name`, `service_type`, `device_id`, `device_name`, `target`, `schema_version`.
- **Device alias:** `device=<alias>` (cannot be combined with `service_id`, `device_id`, `service_name`, or `device_name`).
- **Device class:** `device_class="light"` (matches devices whose expanded class list includes that class; e.g., a `dimmer` matches `light` and `toggle`).
- **Placement:** `location="House.Bedroom.**"` (or list `["House", "**", "Shelf.1"]`), `tag="guest"`, `tags=["guest", "battery"]`. See [Device locations and tags](devices.md).
- **State leaf selectors (on observations):** `attribute="volume"`, `value=35`. Both must match the same attribute of the same device patch in a batch.
- **Event payload fields:** Any other key matches a top-level key in `ev.data` (for example `action=single` or `label=person`).

> [!TIP]
> Include `type="..."` whenever you know the event type. Typed triggers are indexed in per-type buckets; untyped triggers (such as `Event(device_class=light)`) are checked against every non-`state.changed` event.

#### Leaf transitions: `Event(type="state.changed", ...)`

An ordinary `Event()` triggers on every fresh observation—even when the reported value repeats (such as pressing the same button twice)—and **skips** derived `state.changed` notifications unless you explicitly pass `type="state.changed"`.

When an attribute's value is reported for the first time or changes to a different value, Chains emits a `state.changed` notification with:

| Field | Description |
| --- | --- |
| `ev.device` | Concrete device reference (`service_id` + `device_id`) |
| `ev.data.attribute` | Name of the attribute that changed |
| `ev.data.value` | New value |
| `ev.data.previous` | Previous value (absent when `initial == true`) |
| `ev.data.initial` | `true` for the first recorded value on that path, `false` on transitions |
| `ev.data.revision` | State revision number |

```text
ev = Event(type="state.changed", device_class=motion, attribute="occupancy", value=true, initial=false)
```

Setting `initial=false` prevents the rule from firing when a newly discovered sensor reports its very first state.

#### Multi-device observation batches

A single observation can update several devices atomically in one State revision. Each matching rule trigger or wait receives the batch **once**:

- `ev.matches` — list of device patches that matched the selector.
- `ev.observations` — list of all device patches in the batch.
- Each patch has `device`, `device_id`, `device_name`, `device_classes`, `location`, `tags`, `values` (exact reported State attributes), and `data` (event payload).
- When **exactly one** patch matches, `ev.device`, `ev.device_id`, `ev.values`, `ev.data`, `ev.location`, `ev.tags`, and `ev.device_classes` are also exposed directly on `ev`.

#### Follow-up waits, `when` guards, and per-wait timeouts

Any `Event()` after the opening wait suspends only the current run until a matching event arrives.

- **Pre-admission `when` guards:** Append `when <condition>` to an opening or mid-rule `Event()` to filter events **before** admitting, restarting, or waking a run. Guards can inspect the bound event variable as well as read-only `State()`, `Devices()`, `LocalTime()`, and `Now()`:

  ```text
  rule.mode = "restart"
  ev = Event(type="humidity.reported", device=sensor) when ev.data.value >= 70 and State(fan).power == false
  ```

  Events whose guard evaluates to `false` do not cancel active `rule.mode = "restart"` runs or wake a mid-rule wait.

- **Per-wait timeouts:** Pass `timeout=<seconds or duration string>` on a non-opening `Event()` to bound that single wait without aborting the whole run:

```text
next = Event(type="state.changed", device=motion, attribute="occupancy", value=false, timeout="5m")
if next == null:
    # No occupancy=false arrived within 5 minutes
    Set(device=lamp, attribute="power", value=false)
```

- If the wait times out before the rule's overall `rule.timeout` deadline, `Event(...)` returns `null`.
- If a matching event arrives, `Event(...)` returns the matched event object, updates correlation to that event, and refreshes the 10,000-step instruction budget (while keeping the original run deadline).
- `timeout=` is only valid on non-opening `Event()` calls (and inside `select:`, use `case Sleep(...):` instead).

---

### Multi-pattern waits: `select:`

Use `select:` to listen for **multiple distinct triggers or waits at once**—both as the opening trigger of a rule and mid-rule. Each `case` arm takes one wait call (`Event()`, `Every()`, `Time()`, or `Sleep()` in mid-rule `select:`), an optional binding (`case ev = ...:`), an optional `when` guard on `Event()`, and its own branch body.

```text
select:
    case ev = Event(type="state.changed", device=door, attribute="contact", value=false, initial=false):
        Set(device=lamp, attribute="power", value=true)
    case ev = Event(type="state.changed", device=motion, attribute="occupancy", value=true, initial=false):
        Set(device=lamp, attribute="power", value=true)
    case Time(hour=23, minute=30):
        Set(device=lamp, attribute="power", value=false)
        return
```

- **Opening `select:`:** All `case` arms share the rule's `rule.mode`, `rule.scope`, and `rule.concurrency`. When an arm matches, its body executes and then continues to any statements after `select:` (unless that arm calls `return`). If one observation matches multiple arms, the **first matching arm in source order** wins.
- **Mid-rule `select:`:** Races all `Event()`, `Time()`, `Every()`, and `Sleep()` arms simultaneously. The first arm to fire wins, sibling waits and timers are immediately cleaned up, and only the winning arm's body runs. Use `break` inside a case to leave `select:` early.
- **Scoping:** Variables assigned in every non-returning `case` arm (including an identical binding name on every non-returning arm) are available after `select:`.

---

### `Every(duration)` and `Time(...)`

Schedule rules start on elapsed intervals or local wall-clock times (unless the engine was started with `-scheduler=false`). Both can also be used as later waits inside a run.

```text
# Elapsed interval (Go duration string, minimum "1ms")
Every("30s")

# Local calendar time
Time(hour=7, minute=30)                      # Every day at 07:30:00
Time(weekday=1, hour=8, minute=0)            # Mondays at 08:00:00 (Sunday is 0)
Time(month=12, day=25, hour=0, minute=0)     # Christmas midnight
Time(day=1)                                  # First of every month at 00:00:00
```

**Calendar selector fields:** `year` (`1`–`9999`), `month` (`1`–`12`), `week` (`0`–`53`), `day` (`1`–`31`), `weekday` (`0` = Sunday … `6` = Saturday), `hour` (`0`–`23`), `minute` (`0`–`59`), `second` (`0`–`59`).

- `second` defaults to `0`.
- An `hour` without `minute` or `second` defaults `minute` to `0`.
- A date without clock fields defaults to midnight (`00:00:00`).
- During daylight saving transitions, nonexistent local times are skipped and repeated local times fire once.

---

### `Sleep(duration)`

`Sleep()` pauses the current run for the given duration—either numeric seconds (fractional values like `0.25` are supported) or a duration string such as `"2m"` or `"500ms"`. It respects the run's overall deadline, cancellation, and `rule.mode = "restart"`.

```text
Set(device=first_light, attribute="power", value=true, wait="sent")
Sleep("2s")
Set(device=second_light, attribute="power", value=true, wait="sent")
```

---

## Devices, placement, and State

### Referencing a single device: `Device(...)`

Use `Device()` to build a reusable handle:

```text
# Stable IDs (recommended: survives display renames)
fan = Device(service_id="sim-fan", device_id="fan")

# Display names (resolved dynamically; fails if missing or ambiguous)
led = Device(service_name=LEDPanel, device_name=panel0)
```

Device handles support `.service_id` and `.device_id`, work in `Event(device=...)`, `State(...)`, `Set(...)`, and `Action(...)`, and can be compared directly with `==`, `!=`, and `switch`—including comparing a name-based `Device(...)` against `ev.device`.

### Querying multiple devices: `Devices(...)`

`Devices(...)` returns a list of concrete device references sorted by `service_id`, then `device_id` (or an empty list `[]` if none match). It can combine any of:

`service_id`, `service_name`, `device_id`, `device_name`, `device_class`, `location`, `tag`, `tags`.

```text
for light in Devices(location="House.Upstairs.**", device_class=light):
    Set(device=light, attribute="power", value=false, wait="sent")
```

> [!NOTE]
> `Devices(...)` queries the live device catalog, so it can only run **after** the opening trigger (not in the pure setup preamble).

### Reading State: `State(device)`

`State(device)` reads the latest committed State view for a device:

- `State(fan).power` returns the latest reported `power` value, or `null` if unreported.
- `State(fan)._attributes.power` exposes metadata: `changed_at` (RFC3339Nano), `observed_at` (RFC3339Nano), and `revision`.
- Use `ev.values` (or `match.values`) when you need the exact values carried by the triggering observation rather than the latest State.

```text
meta = State(sensor)._attributes.temperature
if meta != null and Now() - ParseTime(meta.observed_at) > 1800:
    Action("send", service_name=Notifications, device_name=system, message="Temperature sensor silent for 30m")
```

---

## Controlling devices (`Set` and `Action`)

### `Set(...)` — writable attributes

Use `Set()` to update a device's declared writable attribute (such as `power` or `dimlevel` on `toggle`/`light`/`dimmer` devices). Chains resolves the registered setter action and validates the value against the attribute's JSON Schema:

```text
result = Set(device=lamp, attribute="power", value=true)
Set(device=lamp, attribute="dimlevel", value=40, wait="sent")
Set(service_name=Zigbee2MQTT, device_name="hall/light", attribute="power", value=false)
```

### `Action(...)` — named operations

Use `Action()` for device actions with custom arguments or return values:

```text
res = Action("set_volume", device=amp, volume=30)
Action("writestring", service_name=LEDPanel, device_name=panel0, value="Hello")
Action("custom_op", device=target, schema_version="2", mode="fast", wait="sent")
```

### Wait modes and return values

| Mode | Behavior | Returned `status` values |
| --- | --- | --- |
| Default (`wait="outcome"`) | Waits for the integration to report an outcome (within the engine action timeout and rule deadline). | `"completed"`, `"submitted"`, `"failed"`, `"timed_out"` |
| `wait="sent"` | Returns immediately once the request enters the bus or local bridge queue. | `"sent"` |

Both `Set()` and `Action()` return an object containing `status` and `action_id`, plus `value` (if the action returned a payload) and `error` (if it failed). Declared idempotent actions may be retried automatically while awaiting acceptance. See [Device controls and actions](actions.md) for details.

---

## Language reference

### Values and literals

| Type | Examples |
| --- | --- |
| Numbers (64-bit float) | `0`, `42`, `-3.5`, `72.5`, `1e3` |
| Strings | `"hello\nworld"`, `'it\'s ready'` |
| Booleans & null | `true`, `false`, `null` |
| Lists | `[]`, `[1, "two", true]` |
| Objects | `{}`, `{"volume": 30, "muted": false}` |

#### Bareword shorthand

In `Event()` selectors (except `device=` and `timeout=`), `Devices()` selectors (`service_id`, `service_name`, `device_id`, `device_name`, `device_class`, `location`, `tag`), and target identity fields (`service_id`, `service_name`, `device_id`, `device_name`) of `Device()`, `Set()`, and `Action()`, an unbound identifier is accepted as string shorthand:

```text
Event(device_class=light, action=power_toggle)
Device(service_id="sim-fan", device_id=fan)
```

- If an identifier **is** a declared variable in scope, its variable value is used.
- String-only event selectors (`type`, `device_class`, `service_id`, `service_name`, `device_id`, `device_name`, `source`, `target`, `schema_version`) reject non-string values at prepare/runtime so a variable like `light = Device(...)` cannot silently break `Event(device_class=light)`.
- Quote any literal containing spaces, punctuation, or dots (e.g., `"hall/light"`, `"humidity.reported"`).

### Operators and precedence

Listed from lowest to highest precedence:

| Precedence | Operators | Notes |
| --- | --- | --- |
| 1 | `or` | Short-circuit boolean OR (operands must be boolean) |
| 2 | `and` | Short-circuit boolean AND (operands must be boolean) |
| 3 | `==`, `!=`, unary `not` | Structural equality; resolves named `Device()` handles |
| 4 | `<`, `<=`, `>`, `>=`, `in`, `not in` | `<`/`<=`/`>`/`>=` work on numbers and strings; `in`/`not in` check list elements, object keys, or substrings |
| 5 | `+`, `-` | Numeric add/subtract; `+` also concatenates strings |
| 6 | `*`, `/`, `%` | Numeric multiply, divide, floating-point remainder (`/` and `%` error on zero) |
| 7 | unary `+`, `-` | Numeric sign |
| 8 | `x.field`, `x[index]`, `Func(...)` | Field access on `null` evaluates to `null`; out-of-bounds list index raises an error |

### Control flow and scoping

```text
if temp >= 25:
    label = "warm"
elseif temp >= 18:
    label = "mild"
else:
    label = "cool"
```

```text
switch ev.data.action:
    case "single", "press":
        Set(device=lamp, attribute="power", value=true)
    case "double":
        Set(device=lamp, attribute="dimlevel", value=100)
    default:
        return
```

```text
count = 0
while count < 3:
    count = count + 1
    if count == 2:
        continue
```

```text
for d in Devices(device_class=light):
    if State(d).power == true:
        Set(device=d, attribute="power", value=false, wait="sent")
        break
```

- `if` and `while` conditions must evaluate to a boolean (`true` or `false`).
- `break` exits the nearest enclosing loop or `switch`; `continue` advances the nearest loop; `return` ends the rule run cleanly.
- Variables must be definitely assigned before use. Static validation accounts for branches that exit via `return` and exhaustive `switch` statements with a `default` case.
- A `for` loop variable is scoped to the loop and restores any outer variable of the same name when the loop finishes.

### Built-in functions

| Function | Pure? | Description |
| --- | --- | --- |
| `Has(object, key)` | Yes | `true` if `key` exists on `object` (even if its value is `null`). |
| `Exists(value)` | Yes | `true` if `value != null`. |
| `Len(x)` | Yes | Number of runes in a string, items in a list, or keys in an object. |
| `Now()` | No | Current UTC Unix timestamp in seconds (float). |
| `LocalTime()` | No | Current local clock object (`year`, `month`, `day`, `weekday` with `0` = Sunday, `hour`, `minute`, `second`) in the engine's configured timezone. |
| `ParseTime(rfc3339)` | Yes | Parses an RFC3339 / RFC3339Nano timestamp string into UTC Unix seconds (float). |
| `Duration(str)` | Yes | Parses a duration string (e.g., `"30m"`, `"1h30m"`) into seconds (float). |
| `Min(a, b)` / `Max(a, b)` | Yes | Smaller / larger of two numbers. |
| `Abs(x)` / `Round(x)` | Yes | Absolute value / nearest integer of a number. |
| `Device(...)` | Yes | Builds a single device reference by ID pair or name pair. |
| `Devices(...)` | No | Returns matching concrete device references in ID order. |
| `State(device)` | No | Reads the latest State map (plus `_attributes` metadata) for `device`. |
| `Event(...)` | Trigger/Wait | Matches an opening event or suspends for a follow-up event (with optional `timeout=`). |
| `Every(duration)` / `Time(...)` | Trigger/Wait | Schedules elapsed or local calendar triggers/waits. |
| `Sleep(seconds)` | No | Pauses the current run within its deadline. |
| `Set(...)` / `Action(...)` | No | Updates a writable attribute or invokes a named device action. |

> [!NOTE]
> Each run starts with a **10,000-step instruction budget** to stop runaway loops. Resuming from a subsequent `Event()`, `Every()`, or `Time()` wait resets the instruction budget while keeping the original `rule.timeout` deadline.

---

## Cookbook: real-world examples

### 1. Bathroom humidity extractor fan

Turns the fan on when humidity reaches 70% (if it isn't already on) and turns it back off once humidity drops below 60%.

```text
fan = Device(service_id="sim-fan", device_id="fan")
reading = Event(type="humidity.reported", device_id="bathroom-sensor")

if reading.data.value >= 70 and State(fan).power == false:
    Set(device=fan, attribute="power", value=true)
elseif reading.data.value < 60 and State(fan).power == true:
    Set(device=fan, attribute="power", value=false)
```

### 2. Retriggerable motion-activated light

Uses `rule.mode = "restart"` and a bounded follow-up wait: every new motion detection restarts the timer, turning the light off 2 minutes after occupancy clears (or after 10 minutes if the sensor never reports `occupancy=false`).

```text
rule.mode = "restart"
rule.timeout = 900

sensor = Device(service_id="zigbee2mqtt", device_id="0x00158d0001a2b3c4")
lamp   = Device(service_name=Zigbee2MQTT, device_name="hall/light")

Event(type="state.changed", device=sensor, attribute="occupancy", value=true, initial=false)
Set(device=lamp, attribute="power", value=true)

cleared = Event(type="state.changed", device=sensor, attribute="occupancy", value=false, timeout=600)
if cleared != null:
    Sleep(120)
Set(device=lamp, attribute="power", value=false)
```

### 3. Double-press button shortcut

Distinguishes a single press from a quick double press using a 0.6-second follow-up wait.

```text
rule.timeout = 10

button = Device(service_id="zigbee2mqtt", device_id="0x0011223344556677")
lamp   = Device(service_name=Zigbee2MQTT, device_name="living/ceiling")

Event(type="zigbee2mqtt.action", device=button, action=single)
second = Event(type="zigbee2mqtt.action", device=button, action=single, timeout=0.6)

if second != null:
    # Double press: full brightness
    Set(device=lamp, attribute="dimlevel", value=100)
else:
    # Single press: toggle power
    current = State(lamp).power
    Set(device=lamp, attribute="power", value=not (current == true))
```

### 4. Bedtime "all lights off" (except nightlights)

Triggered manually or on a schedule, turns off every light in the house except devices tagged `nightlight`.

```text
Time(hour=23, minute=30)

nightlights = Devices(tag=nightlight)
for light in Devices(location="House.**", device_class=light):
    if light not in nightlights and State(light).power == true:
        Set(device=light, attribute="power", value=false, wait="sent")
```

### 5. Multi-zone amplifier volume cap (batch observations)

Handles an observation batch that updates multiple zones atomically in one revision, checking each matching zone's reported volume.

```text
ev = Event(service_name=Amplifier, device_name=Main, attribute=volume)

for match in ev.matches:
    if match.values.volume > 30:
        Action("set_volume", device=match.device, volume=30)
```

### 6. Stale sensor watchdog using timestamps

Checks every 10 minutes whether a critical temperature sensor has gone quiet for more than 30 minutes.

```text
Every("10m")

freezer = Device(service_id="zigbee2mqtt", device_id="0x00158d0009998888")
meta = State(freezer)._attributes.temperature

if meta == null:
    return

age = Now() - ParseTime(meta.observed_at)
if age > 1800:
    Action("send", service_name=Notifications, device_name=system,
           title="Sensor offline",
           message="Freezer sensor has not reported for " + "30+ minutes")
```

### 7. Sequential welcome lights with `Sleep()`

Turns on three lights in sequence two seconds apart when motion is detected (the same pattern bundled in the **See 1–2–3** rule template).

```text
motion = Device(service_id="zigbee2mqtt", device_id="0x00158d0001a2b3c4")
first  = Device(service_id="zigbee2mqtt", device_id="0x00158d0000000001")
second = Device(service_id="zigbee2mqtt", device_id="0x00158d0000000002")
third  = Device(service_id="zigbee2mqtt", device_id="0x00158d0000000003")

Event(type="state.changed", device=motion, attribute="occupancy", value=true, initial=false)
Set(device=first, attribute="power", value=true, wait="sent")
Sleep(2)
Set(device=second, attribute="power", value=true, wait="sent")
Sleep(2)
Set(device=third, attribute="power", value=true, wait="sent")
```

### 8. Inspecting action outcomes and retrying/alerting on failure

```text
lamp = Device(service_name=Zigbee2MQTT, device_name="porch/light")

Time(hour=18, minute=0)
result = Set(device=lamp, attribute="power", value=true)

if result.status == "failed" or result.status == "timed_out":
    Action("send", service_name=Notifications, device_name=system,
           title="Porch light failed",
           message="Could not turn on porch light")
```

### 9. Multi-sensor hallway light with manual override (`select:` + `LocalTime()`)

Turns on the hallway light when either the door opens or motion is detected, adapts brightness by local time of day, and races occupancy clearing against a manual wall-switch off or a 10-minute fallback timeout.

```text
rule.mode = "restart"
rule.timeout = "15m"

door        = Device(service_id="zigbee2mqtt", device_id="0x00158d0001111111")
motion      = Device(service_id="zigbee2mqtt", device_id="0x00158d0002222222")
wall_switch = Device(service_id="zigbee2mqtt", device_id="0x00158d0003333333")
lamp        = Device(service_name=Zigbee2MQTT, device_name="hall/light")

select:
    case Event(type="state.changed", device=door, attribute="contact", value=false, initial=false):
        t = LocalTime()
        if t.hour >= 22 or t.hour < 7:
            Set(device=lamp, attribute="dimlevel", value=20)
        else:
            Set(device=lamp, attribute="dimlevel", value=100)
    case Event(type="state.changed", device=motion, attribute="occupancy", value=true, initial=false):
        t = LocalTime()
        if t.hour >= 22 or t.hour < 7:
            Set(device=lamp, attribute="dimlevel", value=20)
        else:
            Set(device=lamp, attribute="dimlevel", value=100)
    case Time(hour=23, minute=30):
        Set(device=lamp, attribute="power", value=false)
        return

select:
    case Event(type="state.changed", device=motion, attribute="occupancy", value=false):
        Sleep("2m")
        Set(device=lamp, attribute="power", value=false)
    case Event(type="zigbee2mqtt.action", device=wall_switch, action=off):
        Set(device=lamp, attribute="power", value=false)
        return
    case Sleep("10m"):
        Set(device=lamp, attribute="power", value=false)
```

### 10. Single-rule multi-room motion watchdog (`rule.scope = "device"` + `when`)

One rule tracks every motion sensor independently: motion on one sensor restarts only that sensor's timer without affecting other rooms.

```text
rule.mode = "restart"
rule.scope = "device"
rule.timeout = "10m"

ev = Event(type="state.changed", device_class=motion, attribute="occupancy", value=true, initial=false) when LocalTime().hour >= 22 or LocalTime().hour < 6
Sleep("5m")
if State(ev.device).occupancy == true:
    Action("send", service_name=Notifications, device_name=system,
           title="Sustained night motion",
           message=ev.device_name + " remained occupied for 5 minutes")
```

---

## Authoring, validation, and live publishing

- **File-based rules:** Pass a single `.chains` file or a directory of `.chains` files via `-rules` (or `rules` in `config.json`). Files are validated at engine startup and appear read-only in the web console.
- **Web console rules:** Open **Rules** in `/ui/` to create automations with the guided form, start from a [rule template](rule-templates.md), edit raw DSL with line/column diagnostics, export/import portable ZIP archives, and publish live without restarting Chains. Publishing replaces only modified rules and cancels only their affected runs.
- **Programmatic tooling (Go):** The Go `rules` package exposes the structured AST (`Rule`, `Statement`, `Expression`) along with `Parse`, `Validate`, `Format`, and `Bind` for tools and UIs that generate or transform rules.
- **Troubleshooting:** Enable `-operator-trace` and open **Watch** or **Trace Explorer** in the web console (or run `chainsctl watch`) to trace `rule.started`, `rule.resumed`, `rule.skipped`, `rule.timeout`, `rule.failed`, and correlated action outcomes end to end. See [Rules engine concurrency and latency](rules-engine-concurrency.md) for performance tuning and multi-worker deployments.
