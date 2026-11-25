# Device controls and actions

A device declares an array of `classes` and a dictionary of attributes. The authority confirms that a device meets each declared class contract when it registers. The resulting classes can drive UI presentation and rule selection.

| Class | Required capabilities |
| --- | --- |
| `toggle` | Readable and writable boolean `power` |
| `light` | All `toggle` capabilities |
| `dimmer` | All `light` capabilities and readable and writable integer `dimlevel` from 0 to 100 percent |
| `motion` | Readable boolean `occupancy` |

A dimmer matches `Event(device_class="light")` and appears in `Devices(device_class="toggle")`. A toggle device can expose an additional native toggle action. Zigbee2MQTT declares `motion` for devices that report `occupancy`. Devices with an empty class list still get schema-driven controls.

## Attribute descriptions

The schema governs reported values and setter input. A setter maps one attribute to a registered action and names the argument that receives the value. An attribute without a setter is read-only, such as a temperature measurement. Labels and descriptions are optional. Use a setter label when the control needs different wording from the State readout. Service and device descriptions can supply additional context, and versioned event and action schemas can use JSON Schema `title` and `description`.

```json
{
  "power": {
    "schema": {"type": "boolean"},
    "label": "Power",
    "setter": {"action": "set_power", "schema_version": "1", "argument": "value"}
  },
  "temperature": {
    "schema": {"type": "number"},
    "unit": "°C",
    "description": "Measured temperature at the device"
  }
}
```

The authority validates every attribute in an observation before committing its batch. The [events guide](events.md) describes the commit and delivery guarantees.

## Calling setters and actions

```text
result = Set(device=lamp, attribute="power", value=true)
Set(device=lamp, attribute="dimlevel", value=40, wait="sent")
Action("create_calendar_entry", device=calendar, collection="work", title="Meeting", start="2026-12-01", end="2026-12-02")
```

Declare `lamp = Device(service_name="Lighting", device_name="Desk")` before or after the opening match. Chains looks up the name pair when the reference is used. Use the ID pair to keep targeting the same device through display-name changes.

Use `Set()` for a declared writable attribute and `Action()` for a named operation. Calls wait for the outcome by default. The returned object contains `status` and `action_id`, with the handler's optional `value` and an error when available. `wait="sent"` returns the action ID and status `sent` after the request enters the transport or hosted queue. Later outcomes are available in activity.

An outcome describes the command. The service reports what the device actually did in a separate observation. A device can change additional attributes, report a rounded value, or report after the call returns. The State console shows the requested value separately while it waits for a new device report.

**State** uses the authenticated `POST /operator/v1/actions` endpoint with `service_id`, `device_id`, `attribute`, and `value`. The guided **Rules** editor offers **Set attribute** for writable values and **Named action** for other operations. [Web console](operator-web.md)

## Upgrading from wire v2

Upgrade engines and publishers together. Services re-register their capabilities on wire v3. SQLite preserves leaf values and timestamps for unchanged paths. Frigate starts its camera paths from new observations, and Radicale discards any queued envelopes from the previous wire version. Zigbee2MQTT publishes percentage brightness under `dimlevel` on the next device report. Update rules that previously read native `brightness` or passed native values to `set_brightness`.
