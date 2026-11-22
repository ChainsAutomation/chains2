# Device locations and tags

**All Devices** groups cards by service, ordered by service name and then device name. Select a location or apply a location pattern to group matching devices by location across services. Cards show the service name alongside the device name and location. Onkyo receiver zones show power, input, volume, and mute where available. The same cards appear on [household dashboards](household-dashboards.md).

## Assign locations and tags

A location is an ordered array of segments. For example, `["House", "Bedroom", "Shelf.1"]` places a device on a shelf with a literal dot in its name. Tags are individual labels such as `guest` and `battery`. They can group devices across locations. The State path remains `service_id/device_id/attribute` when a device moves.

Integrations can supply `location` and `tags` in their device registration. An operator edit overrides the corresponding service default. Use **Clear** to set an empty value or **Reset** to inherit the current service default again. Overrides survive re-registration and authority restart. Names preserve case and may contain spaces. Each location or tag list supports up to 32 entries of up to 128 UTF-8 bytes each.

## Match by location and tag

| Pattern | Matches |
| --- | --- |
| `House.Bedroom` | Devices assigned to this exact location |
| `House.Bedroom.**` | Devices in the bedroom or anywhere below it |
| `House.*.Shelf` | Devices on a shelf under any one intermediate level |
| `**.Shelf` | Locations that end in `Shelf` |
| `**.Bedroom.**` | Any `Bedroom` segment and its descendants |
| `["House", "**", "Shelf.1"]` | Locations under `House` that end in the literal segment `Shelf.1` |

A single asterisk matches one whole segment. A double asterisk matches zero or more. Use a dotted string for simple patterns and an array where a segment contains a literal dot. A location pattern matches devices with an assigned location. Tag matching uses exact membership, and a list of tags requires every listed tag.

```text
ev = Event(type="device.observed", location="**.Bedroom.**", tag="guest")
for device in Devices(location=["House", "Bedroom", "**"], tags=["guest", "battery"]):
    power = State(device).power
```

In a batch, device selectors apply to the same patch. A committed observation or `state.changed` carries the effective location and tags at commit time. `Devices()` uses the current catalog. Editing placement changes the catalog revision and leaves reported values and their timestamps intact. [Events and State](events.md)

## Operator API

- `GET /operator/v1/devices` lists device descriptions and their reported attributes at a single revision. Use `location` for dotted syntax or repeat `location_segment` for an array. Repeat `tag` to require several tags. Use `device_class` for class membership or `unassigned=true` for devices without a location.
- `PATCH /operator/v1/devices/{service}/{device}/placement` accepts `location` and `tags` as arrays. Omit a field to keep its override, supply `[]` to clear it, or supply `null` to inherit the service default. Both endpoints require operator authentication.
