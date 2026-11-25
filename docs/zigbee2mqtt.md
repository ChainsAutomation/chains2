# Zigbee2MQTT bridge

Chains includes a built-in Zigbee2MQTT bridge hosted directly inside the state-authority engine. Its MQTT URL connects to Zigbee2MQTT's broker and can point to a different broker from the Chains system bus.

## Enable the bridge

```sh
CHAINS_Z2M_ENABLED=true \
CHAINS_Z2M_MQTT_URL=mqtt://localhost:1883 \
  ./chains engine -rules examples/z2m/button-to-light.chains
```

Replace the sample IEEE address and light name in `examples/z2m/button-to-light.chains` with values from your Zigbee2MQTT inventory. Each setting can also be set under `z2m` in the engine's JSON config or via its CLI flag.

| JSON key (`z2m.*`) | Flag | Environment | Default |
| --- | --- | --- | --- |
| `enabled` | `-z2m-enabled` | `CHAINS_Z2M_ENABLED` | `false` |
| `mqtt_url` | `-z2m-mqtt-url` | `CHAINS_Z2M_MQTT_URL` | Required when enabled |
| `base_topic` | `-z2m-base-topic` | `CHAINS_Z2M_BASE_TOPIC` | `zigbee2mqtt` |
| `service_id` | `-z2m-service-id` | `CHAINS_Z2M_SERVICE_ID` | `zigbee2mqtt` |
| `service_name` | `-z2m-service-name` | `CHAINS_Z2M_SERVICE_NAME` | `Zigbee2MQTT` |
| `ca_file` | `-z2m-ca-file` | `CHAINS_Z2M_CA_FILE` | System trust store |

## Broker and Zigbee2MQTT requirements

- Use `mqtts://` for TLS and include percent-encoded credentials in the URL if required (for example `mqtts://user:p%40ss@broker.local:8883`).
- Grant the bridge MQTT ACL permission to subscribe to `<base_topic>/#` and publish device commands under `<base_topic>/<device>/set`.
- Keep Zigbee2MQTT configured to publish JSON reports.

## Discovery, state, and actions

- **Automatic discovery:** The bridge reads `<base_topic>/bridge/devices`, registers each supported device under its IEEE address (`device_id`) and friendly name (`device_name`), and maps standard capabilities (`toggle`, `light`, `dimmer`, `motion`).
- **Retained seeds:** Retained device reports seed State silently on startup or reconnect without triggering rules.
- **Button actions:** Live button presses publish a `zigbee2mqtt.action` event with the original Zigbee2MQTT action name under `data.action` and record it in `last_action`.
- **Standardized setters:** Power (`power` boolean) and percentage brightness (`dimlevel` 0–100 integer) are exposed on devices that advertise writable `state` and `brightness` exposes.
- **Action outcomes:** A `submitted` action result means the Zigbee2MQTT MQTT broker acknowledged the command (QoS 1 `PUBACK`). A later device report supplies the observed State update.
- **Local fast path and bus mirroring:** The authority dispatches local rules directly and mirrors events onto the Chains bus through a background publisher. Remote workers or observers can miss live events if the mirror queue overflows or the system bus is unavailable. See [Rules engine concurrency and latency](rules-engine-concurrency.md).

## Example rule

```text
Event(type="zigbee2mqtt.action", device_id="0x0011223344556677", action=single)
Set(service_name=Zigbee2MQTT, device_name="hall/light", attribute="power", value=true)
```
