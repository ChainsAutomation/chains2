# Chains Automation

Chains runs automation rules over a single MQTT 5 or Redis Streams bus. A Go engine accepts events, maintains a revisioned view of reported state, and sends actions to registered integrations. Each deployment has one state authority and can run additional engines with their own rules and subscriptions.

## Run the humidity example

Build on Linux with Go 1.23+, a C compiler, pkg-config, and the development headers for SQLite and libmosquitto. Start a local MQTT 5 broker, then run the engine, simulated fan, and sensor adapter in separate terminals.

```sh
go build -buildvcs=false -o chains ./cmd/chains
./chains engine -rules examples/humidity.chains -debug
./chains fan -id sim-fan
./chains sensor -id humidity-sensor
mosquitto_pub -q 1 -t sensors/bathroom/humidity -m 72.5
```

A reading of at least 70 turns the simulated fan on if its reported power is off. The sensor adapter also reports humidity to the state authority. It accepts raw numbers by default. A retained MQTT reading seeds reported state without emitting a live reading event. To map another device, set `sensor_topic` and the config file's `sensor` object. The object can override `device`, `class`, `event`, `attribute`, `field`, `format`, and `schema`. Supported input formats are `number`, `boolean` (including `ON`/`OFF`), `string`, and `json`. Supply a matching JSON Schema when changing the event payload.

For Redis, add `-bus redis://localhost:6379` to the engine, fan, and sensor commands. Also pass `-sensor-broker mqtt://localhost:1883` to the sensor adapter for its device input. Redis is the system bus in this deployment, while MQTT carries the raw sensor feed. The Redis adapter uses a standalone Redis server, one stream for all messages and one stream per message type, and a bounded diagnostic history. Each subscriber follows new messages independently.

## Operator console

Build `./cmd/chainsctl` to inspect state, watch live events and rule runs, or send manual events. The terminal UI combines all three in a keyboard-driven view. The [operator guide](docs/operator-cli.md) covers authority setup, table, JSON and CSV output, filters, and event profiles.

## Rules

A rule is one indentation-based `.chains` file. Pass a file or a directory containing rule files to `-rules`. Each filename supplies the rule's name.

See [rules engine concurrency and latency](docs/rules-engine-concurrency.md) for event routing, run limits, CPU contention, and multi-process deployment.

```text
Event(device_class=light)
Action('writestring', service_name=LEDPanel, device_name=panel0, value="Light is modified")
```

```text
rule.concurrency = 3
rule.timeout = 60

ev = Event(action=power_toggle)
if State(ev.device).power == false:
    Action('writestring', service_name=LEDPanel, device_name=panel0, value="Light is off!")
else:
    Action('writestring', service_name=LEDPanel, device_name=panel0, value="Light is on!")
```

- `Event()` waits for matching envelope or payload fields. A matched event exposes envelope fields such as `ev.device_id`, its payload as `ev.data`, and its stable device reference as `ev.device`. `state.changed` carries `previous`, `value`, `initial`, and `revision` under `ev.data`.
- An opening `Event()`, `Every()`, or `Time()` starts a run after optional pure local assignments. Further waits suspend the current run. `rule.concurrency` defaults to one. `rule.timeout` defaults to 300 seconds and starts at the opening match. The same deadline applies through later waits, actions, and computation. A run ends on expiry, `return`, or the end of the file.
- Read the latest reported attribute with `State(ev.device).power`. A missing attribute evaluates to `null`. Use `ev.data.power` for the value carried by the event itself. Read attribute timestamps under `State(ev.device)._attributes.power`. Both state values and event fields can be used directly in action arguments.
- Use `Action("set_power", device=ev.device, value=false)` for a known device, or supply a unique `service_name` and `device_name` pair. `Device(service_id="sim-fan", device_id=fan)` builds a reference from stable IDs. `Device()` also accepts the name pair. Unbound bare words such as `fan` are string shorthand in selectors and named target fields. Quote literal names containing spaces or punctuation. Other value expressions use explicit literals or declared variables.
- Action arguments are validated against the registered schema. Use `schema_version="2"` to select a schema version. The default is `"1"`. Actions return `completed`, `submitted`, `failed`, or `timed_out`. The engine traces `accepted` separately. Declared idempotent actions may retry while awaiting acceptance. A late device observation can still update state.
- Use `#` for comments. The language supports single- and double-quoted strings, lists, objects, arithmetic, comparisons, `if`/`elseif`/`else`, `switch`/`case`/`default`, `while`, and `for item in items`. Use `Has(object, key)` to check whether a field exists.
- `Every("1s")` uses elapsed time. `Time(month=12, day=25, hour=0, minute=0)` matches local Christmas midnight. Calendar selectors support year, month, week, day, weekday (Sunday is 0), hour, minute, and second. Seconds default to zero, an hour by itself defaults to minute zero, and a date without clock fields uses midnight. Nonexistent local times are skipped and repeated local times fire once.

The Go `rules` package exposes the structured rule model and `Parse`, `Validate`, and `Format` for tools that generate rules.

## Configure and extend

Configuration precedence is environment, explicit CLI flags, JSON config files, then defaults. Use `CHAINS_CONFIG` or `-config` to select a file. Otherwise the engine reads `chains/config.json` under the XDG config directories. The default rules file is `$XDG_CONFIG_HOME/chains/rules.chains` and the SQLite database is `$XDG_STATE_HOME/chains/state.sqlite`. Run `./chains engine -help` for scalar settings and matching `CHAINS_*` environment names. Set `-scheduler=false` to disable time triggers, or configure `timezone` and `week` (`system`, `iso`, or `sunday`).

Use one node whose `id` equals the deployment's `authority` ID. Other engines set a distinct `id` and the same `authority`, and can set `types` to a list of event types. Place each action-producing rule on a single engine. Workers restore their read view from the authority and resynchronize on gaps and reconnects. Rule runs and missed triggers are live-only, so an interruption can skip a trigger even if the state update itself was persisted. Keep the SQLite files on persistent storage and arrange manual recovery if the authority host fails.

To enable HTTP ingress, set `listen`, a bearer token (for example `CHAINS_TOKEN`), and an `http` mapping in the engine's JSON config:

```json
{
  "listen": "127.0.0.1:8080",
  "http": {
    "humidity": {
      "source": "humidity-sensor",
      "device": "bathroom-sensor",
      "type": "humidity.reported",
      "schema_version": "1",
      "attribute": "value"
    }
  }
}
```

With that source registered, a POST to `/events/humidity` bearing the token and a body of `72.5` submits a normal event. Use a TLS proxy for remote HTTP ingress. MQTT brokers should restrict publish/subscribe access by namespace and service identity. Redis clients with stream write permission are trusted participants in the deployment. Use a private broker and TLS URLs (`mqtts://`, `rediss://`) for remote links.

Go integrations use the `sdk` package with a `bus.Bus`. Python integrations can run `uv sync --project python --locked` and import `chains_automation`. Both SDKs support registration, publishing, reported state, and handling actions. Register stable service UUIDs and schema versions. Use JSON for readable messages or CBOR for compact structured data. The transport and codec boundaries are independent of the DSL and SQLite.

## Verification

```sh
go test ./...
uv run --project python --locked python -m unittest discover -s python/tests
CHAINS_TEST_MQTT=mqtt://127.0.0.1:1883 CHAINS_TEST_REDIS=redis://127.0.0.1:6379 \
  go test ./engine -run TestAutomation -bench BenchmarkSensorToAction -benchtime=256x
```

The broker-backed tests exercise a narrowly subscribed worker, registration, observed-state updates, causal action reports, and authority restart. The benchmark reports p50 and p99 from publishing a structured sensor event to receiving the action request, in bursts of 32, along with the peak sampled backlog in the worker’s delivery queue.

## Zigbee2MQTT bridge

Enable the bridge on the state-authority engine. The MQTT URL connects to Zigbee2MQTT's broker and can point to a different broker from the Chains bus.

```sh
CHAINS_Z2M_ENABLED=true \
CHAINS_Z2M_MQTT_URL=mqtt://localhost:1883 \
  ./chains engine -rules examples/z2m/button-to-light.chains
```

Replace the sample IEEE address and light name in the rule with values from your Zigbee2MQTT inventory. Each setting can also be set under `z2m` in the JSON config or via its flag.

| JSON key | Flag | Environment | Default |
|---|---|---|---|
| `enabled` | `-z2m-enabled` | `CHAINS_Z2M_ENABLED` | `false` |
| `mqtt_url` | `-z2m-mqtt-url` | `CHAINS_Z2M_MQTT_URL` | Required when enabled |
| `base_topic` | `-z2m-base-topic` | `CHAINS_Z2M_BASE_TOPIC` | `zigbee2mqtt` |
| `service_id` | `-z2m-service-id` | `CHAINS_Z2M_SERVICE_ID` | `zigbee2mqtt` |
| `service_name` | `-z2m-service-name` | `CHAINS_Z2M_SERVICE_NAME` | `Zigbee2MQTT` |
| `ca_file` | `-z2m-ca-file` | `CHAINS_Z2M_CA_FILE` | System trust store |

Use `mqtts://` for TLS and include percent-encoded credentials in the URL if required. Give the bridge permission to subscribe to its configured base topic and publish device commands below it. Keep Zigbee2MQTT configured to publish JSON reports.

Retained device reports seed state without starting rules. Live button actions carry the original action name under `data.action`. Actions support power and brightness on devices with writable capabilities. A `submitted` action result means the MQTT broker acknowledged a command. A later device report supplies the observed state. The authority dispatches local rules directly and mirrors events onto the Chains bus. Remote subscribers can miss live events if the publication queue overflows or the system bus is unavailable.

## Author a Python service

Start a service with `chains-service MODULE:CLASS`. The [Python SDK reference](docs/sdk-python.md) covers configuration, lifecycle hooks, events, actions, state, and runnable examples. See [bundled services](docs/services-python.md) for RFLink and Frigate.

## Author a Go service

The [Go SDK guide](docs/sdk-go.md) covers the managed runtime, configuration, and the bundled USB keyboard service.
