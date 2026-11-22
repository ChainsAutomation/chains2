# Bundled Python services

Run `chains-service --list` for bundled names. Each service uses the [Python SDK configuration conventions](sdk-python.md#configuration). `--help` shows generated flags. These integrations publish live events to either Chains bus.

## RFLink USB

Install the serial extra with `uv sync --project python --locked --extra rflink`. Give the service user access to the serial device. On Linux, use a stable path under `/dev/serial/by-id/` when available.

```toml
# rflink.toml
[service]
uri = "mqtt://localhost:1883"

[settings]
port = "/dev/ttyACM0"
baudrate = 57600
reconnect_interval = 2
```

Run `chains-service rflink -f rflink.toml`. Override `port` with `--port` or `CHAINS_SETTINGS__PORT`. The gateway defaults to 57600 baud, 8 data bits, no parity, and one stop bit. The integration receives frames and reconnects when the port returns. [RFLink protocol reference](https://rflink.nl/protref.php)

Each received RF frame with an ID emits `rflink.signal` from the gateway device. The payload includes `protocol`, `sequence`, `rf_id`, `switch`, `command`, `fields`, `measurements`, and `raw`. The field map keeps native values as text, including hexadecimal IDs, commands, and unfamiliar tags. The raw line preserves original field order and repetitions. Common documented measurements such as temperature, humidity, and battery status are also decoded. Repeated frames are separate events.

The gateway also records the decoded frame in `system/last_signal`.

```text
signal = Event(type="rflink.signal", protocol="NewKaku", rf_id="000005", command=ON)
```

## Frigate

Enable MQTT in Frigate. This service subscribes to the native tracked-object and review change feeds. It runs as a standalone Python bridge, so Frigate events work with either Chains bus and can use a separate source broker. [Frigate MQTT topics](https://docs.frigate.video/integrations/mqtt/)

```toml
# frigate.toml
[service]
uri = "redis://localhost:6379"

[settings]
mqtt_uri = "mqtt://localhost:1883"
topic_prefix = "frigate"
topics = ["events", "reviews"]
cameras = ["front_door"]
reconnect_interval = 2
```

Run `chains-service frigate -f frigate.toml`. If Chains also uses MQTT on the same broker, omit `mqtt_uri`. Override the source with `--mqtt-uri` or `CHAINS_SETTINGS__MQTT_URI`. Set `topics` to a subset to receive only the feed you use.

`<prefix>/events` emits `frigate.object` and `<prefix>/reviews` emits `frigate.review`. Both carry `phase` (`new`, `update`, or `end`), `camera`, `native_id`, `label`, `severity`, `zones`, and the original native JSON under `payload`. Use `native_id` to correlate changes to the same object or review. Retained messages are skipped. The service bridges event metadata, and images or clips remain in Frigate.

Set `cameras` (CLI `--cameras` or environment `CHAINS_SETTINGS__CAMERAS`) to register known camera names at startup. Additional cameras are registered as live updates arrive. The authority keeps discovered cameras registered across Frigate restarts. The device stores `last_object` and `last_review` summaries with `phase`, `native_id`, `label`, `severity`, and `zones`. An `end` update remains visible as the last observation. The event payload also carries the complete native update.

```text
review = Event(type="frigate.review", phase=new, camera=front_door, severity=alert)
```


## Radicale

Run `chains-service radicale` with the `radicale` extra to sync CalDAV calendars and tasks and CardDAV address books. See the [Radicale guide](radicale.md) for collection aliases, alarms, write permissions, and rule examples.

## Tibber Pulse IR

Run `chains-service tibber-pulse` to read a local Pulse IR Bridge. Configure the Bridge's local web interface first and use its LAN address and Bridge password. The service uses authenticated HTTP polling and supports both the current `/node_data.json` and legacy `/data.json` endpoints. It connects directly to the Bridge and works with either Chains bus. [Bridge setup and node ID reference](https://pypi.org/project/tibber-local-lib/)

```toml
# home-meter.toml
[service]
name = "Home electricity meter"
uri = "mqtt://localhost:1883"
poll_interval = 3

[settings]
bridge_url = "http://tibber-bridge.local"
node_id = 1
```

Set `CHAINS_SETTINGS__PASSWORD` to your Bridge password, then run:

```sh
uv run --project python --locked chains-service tibber-pulse -f home-meter.toml
```

Use a separate instance and service ID for each node. Override settings with `CHAINS_SETTINGS__BRIDGE_URL`, `CHAINS_SETTINGS__NODE_ID`, `CHAINS_SETTINGS__USERNAME` (default `admin`), and `CHAINS_SETTINGS__REQUEST_TIMEOUT` (default five seconds). Polls are at least three seconds apart. Set `CHAINS_POLL_INTERVAL=0` to disable polling. Use HTTP on a trusted LAN since Basic authentication sends credentials without encryption over HTTP.

Each fresh SML telegram emits `tibber_pulse.reading` and writes the measurements to the `default` meter device in the same observation. A repeated cached telegram leaves State timestamps untouched. Fields absent from a sample stay out of the event, and their existing State values and timestamps remain available. Use the event payload for the current sample and State timestamps to check the age of previous measurements.

| Fields | Unit and meaning |
| --- | --- |
| `power_w` | Signed net active power in watts. Positive means import and negative means export. Derived from both directional values when the meter omits net power. |
| `power_import_w`, `power_export_w` | Directional active power in watts, as reported by the meter. |
| `energy_import_wh`, `energy_export_wh` | Cumulative energy in watt-hours. |
| `l1_power_w`, `l2_power_w`, `l3_power_w` | Per-phase active power in watts. |
| `l1_voltage_v`, `l2_voltage_v`, `l3_voltage_v` | Per-phase voltage in volts. |
| `l1_current_a`, `l2_current_a`, `l3_current_a` | Per-phase current in amperes. |

```text
ev = Event(type="tibber_pulse.reading")
if Has(ev.data, "power_w") and ev.data.power_w < -1000:
    Action("writestring", service_name=LEDPanel, device_name=panel0, value="Exporting over 1 kW")
```

The bundled Python container runs the same command with `tibber-pulse -f /etc/chains/services/home-meter.toml` as its arguments. See [Docker images](docker-compose.md) for the build command.
