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
reconnect_interval = 2
```

Run `chains-service frigate -f frigate.toml`. If Chains also uses MQTT on the same broker, omit `mqtt_uri`. Override the source with `--mqtt-uri` or `CHAINS_SETTINGS__MQTT_URI`. Set `topics` to a subset to receive only the feed you use.

`<prefix>/events` emits `frigate.object` and `<prefix>/reviews` emits `frigate.review`. Both carry `phase` (`new`, `update`, or `end`), `camera`, `native_id`, `label`, `severity`, `zones`, and the original native JSON under `payload`. Use `native_id` to correlate changes to the same object or review. Retained messages are skipped. The service bridges event metadata, and images or clips remain in Frigate.

```text
review = Event(type="frigate.review", phase=new, camera=front_door, severity=alert)
```
