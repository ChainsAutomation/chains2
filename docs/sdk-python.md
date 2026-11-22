# Chains Python SDK

Requires Python 3.11+. From the repository root, run `uv sync --project python --locked`. Use `uv run --project python --locked chains-service --help` to see launcher options. In an activated SDK environment, run `chains-service` directly. `python -m chains_automation` invokes the same launcher.

## Quick start

Save this as `ping_service.py` in an importable directory.

```python
from chains_automation import Service


class Ping(Service):
    def event_ping(self):
        self._send_event()
```

```sh
CHAINS_BUS=mqtt://localhost:1883 chains-service ping_service:Ping --emit ping
```

The launcher registers the service, calls the declared `ping` event method, and shuts down. `--emit EVENT` requires a unique wire event name and a method with no required arguments. Omit it to run continuously. One-shot mode runs the startup and shutdown hooks and skips periodic polling.

## Configuration

Use `chains-service MODULE:CLASS -f west.toml` or set `CHAINS_SERVICE_CONFIG` to select a file. Without an explicit file, load `$XDG_CONFIG_HOME/chains/services/<instance>.toml` over matching system defaults from `$XDG_CONFIG_DIRS`. An explicit file replaces XDG lookup. Precedence is environment, CLI or constructor overrides, user TOML, system TOML, then defaults.

Resolve the instance name from `CHAINS_INSTANCE`, then `--instance` or `instance=`, then the explicit filename stem, then the service type. The service type defaults to the snake-case class name and can be overridden as a class attribute. Give concurrently running copies distinct instances or service IDs. Editing the display name leaves the identity intact.

| Key in `[service]` | CLI flag | Environment | Default |
|---|---|---|---|
| `uri` | `--bus` | `CHAINS_BUS` | `mqtt://localhost:1883` |
| `namespace` | `--namespace` | `CHAINS_NAMESPACE` | `chains` |
| `encoding` | `--encoding` | `CHAINS_ENCODING` | `json` |
| `authority` | `--authority` | `CHAINS_AUTHORITY` | `engine` |
| `heartbeat_interval` | `--heartbeat-interval` | `CHAINS_HEARTBEAT_INTERVAL` | 15 seconds |
| `poll_interval` | `--poll-interval` | `CHAINS_POLL_INTERVAL` | 1 second with an `onPoll()` override |
| `service_id` | `--service-id` | `CHAINS_SERVICE_ID` | Generated ID keyed by service type and instance under `$XDG_STATE_HOME/chains/services` |
| `name` | `--service-name` | `CHAINS_SERVICE_NAME` | Instance name |

For programmatic use, pass `config_path=` or supply common keys directly to the constructor.

Supported URI schemes are `mqtt`, `mqtts`, `mqtt+unix`, `redis`, and `rediss`. Both `json` and `cbor` are supported. The SDK sends heartbeats independently of callbacks and polls. The authority marks the service offline after three missed intervals and emits `service.availability.changed` on transitions.

### Service-specific settings

Declare `settings_type` as a dataclass. Nested dataclasses map to TOML sections, and fields can use `str`, `bool`, `int`, `float`, typed lists, string-keyed typed dictionaries, and optional values. Supply a dataclass default if a setting may be omitted. The validated result is available as `self.settings` in `onInit()`.

```python
from dataclasses import dataclass, field
from chains_automation import Service


@dataclass
class Metrics:
    url: str = field(metadata={"help": "Metrics endpoint"})
    flush_interval: int = 30


@dataclass
class MetricsSettings:
    metrics: Metrics


class MetricsService(Service):
    settings_type = MetricsSettings
```

```toml
[service]
uri = "mqtts://broker.example"
name = "West Metrics"

[settings.metrics]
url = "https://metrics.example"
flush_interval = 10
```

Save the class in `metrics_service.py` and the config as `west.toml`. `chains-service metrics_service:MetricsService -f west.toml --metrics-flush-interval 15` overrides the interval. `CHAINS_SETTINGS__METRICS__FLUSH_INTERVAL=20` overrides both. Use raw text for strings and TOML literals for other kinds, for example `true`, `["west", "east"]`, or `{west = 10}`. Nested sections merge by field. Arrays and dictionary values are replaced as a unit. A Python caller can pass a nested mapping such as `settings={"metrics": {"flush_interval": 15}}`.

Name fields in snake case. Each section and field contributes a hyphen-separated component to its flag and an upper-case, double-underscore-separated component to its environment variable. Run `chains-service <service> --help` to see generated flags and help. Keep custom environment variables scoped to the applicable service process.

## Bundled services

Run `chains-service --list` for bundled names, then `chains-service <name> [options]` to launch one. Custom services use `MODULE:CLASS`. Both accept the same TOML, CLI, and environment settings.

To add a bundle, place its implementation in the SDK's `chains_automation.services` package and add a short-name-to-`MODULE:CLASS` entry in `BUNDLED_SERVICES`. The launcher imports the selected implementation on demand. See the [bundled service guide](services-python.md) for RFLink, Frigate, Radicale, and Tibber Pulse configuration and rule examples.

## Lifecycle and overrides

| Hook or setting | Behavior |
|---|---|
| `onInit()` | Runs before transport creation. Initialize hardware, seed state, or call `subscribe(*types)`. |
| `onStart()` | Runs after registration and initial state synchronization. Enable push-based hardware callbacks here. |
| `onPoll()` | Performs one poll. Runs immediately after `onStart()` on a dedicated worker, then waits the configured interval after each call. Polls never overlap. Exceptions are logged and retried on the next interval. Zero disables polling. |
| `onEvent(event)` | Receives a decoded message for a subscribed event type. For committed observations, read `event["observations"]` for the device patches and their payloads. `subscribe("*")` observes public events across the namespace. Subscribe by exact type to observe internal messages such as `service.heartbeat`. Actions addressed to the service are handled separately. |
| `onReconnect()` | Runs after re-registration and silent synchronization of current state. |
| `onShutdown()` | Runs after SDK workers stop and before the transport closes. Remove hardware callbacks and close hardware here. |
| `describe_devices()` | Override to return device dictionaries with `id`, `name`, `classes`, and `attributes`. The SDK derives each device's action list. |
| `transport_factory(config, types)` | Override in `onInit()`, override `open_transport(config, types)`, or inject a transport with `Service(transport=...)`. The SDK adds its action and query subscriptions. The factory receives an empty tuple for a wildcard subscription. |

Use `run(emit=None)` or a context manager when embedding a service. Each instance starts once. `stop()` requests shutdown, `stopping` lets blocking hooks check it, and `close()` joins workers and releases resources. In-flight polls finish before cleanup. Hardware callbacks can call event methods or state APIs. Serialize hardware access if polling, callbacks, and actions share a connection.

## Events, actions, and registration

Device descriptions can include `location` and `tags` lists. These provide defaults for the authority's catalog. [Device placement](devices.md)

See the [event and State guide](events.md) for the shared wire format, batch constraints, and rule matching.

- `event_<name>(...)` declares an event. Call `self._send_event(*, _device=None, _values=None, **fields)` inside it to publish the bound arguments and their defaults. Keyword fields override argument values and must fit the schema. `_device` selects a registered device. The default projection declares and updates `last_<method suffix>` with the reflected payload. Pass `_values={"attribute": value}` to project explicitly into declared attributes. A class can set `event_attributes = {"event_reading": None}` to skip automatic declaration when the method always supplies a projection.
- `observe_batch(kind, observations, *, schema_version="1")` publishes a single revision across multiple devices. Pass a list of dictionaries with `device_id`, `values`, and optional `data`. The SDK validates the devices and declared schema. Omitted `data` uses `values` as the event payload.
- `register_device(description)` adds a discovered device while the service is connected. Use a stable ID and register the device before emitting its first observation. Set `retain_devices = True` for dynamically discovered inventories that should remain registered across service restarts.
- `action_<name>(...)` declares an action. The SDK validates incoming arguments, sends acceptance, invokes the method, and sends a completion with its JSON-compatible return value under `value`. An exception produces a failed outcome. Action requests are handled serially and recent duplicates reuse their stored outcome.
- `@capability(name=None, version="1", schema=None, device=None, idempotent=False)` overrides discovery metadata. Mark an action idempotent only when retrying it is safe. Without a device override, capabilities use the device with ID `system`, or the first declared device.
- `describe_action_schema(name, version, schema)` can refine an action's reflected schema after reading configuration or return `None` to omit it. The SDK uses the resulting schema for registration and argument validation.
- `describe()` returns the registration offline, including identity, devices, versioned schemas, and idempotent actions. Schemas normally come from named parameters and type hints. Supported annotations include primitives, unions, `Literal`, lists, and string-keyed dictionaries. Unannotated parameters are unconstrained. Use an explicit JSON Schema for other shapes.
- A service with a durable publication queue can call `_prepare_event(method, data, device=..., values=..., event_id=..., cause=...)` to build a validated envelope, then publish it with `_publish_observation(envelope)`. Capture `_action_context()` inside an action to retain its causation and correlation when a background worker publishes later.

## Reported state

The default device has ID `system`. Declare typed attributes as a dictionary of descriptors with `schema` and optional `setter`, `label`, `description`, and `unit`. The class-level `attributes` tuple declares open JSON schemas. Use `describe_devices()` for per-device attributes and `classes`. [Device contracts](actions.md) Local state starts empty. An attribute record contains `value`, `changed_at`, and `observed_at`. Central records also carry a revision.

| API | Behavior |
|---|---|
| `set_state(device=None, /, **attributes)` | Updates locally and queues a `device.observed` observation. Central value transitions emit `state.changed`. |
| `seed_state(device=None, /, *, observed_at=None, **attributes)` | Updates locally and synchronizes silently. Supply a timezone-aware observation time for cached data. A stale seed cannot replace a newer central observation. |
| `get_state(device_id=None, attribute=None)` | Returns a copy of the local service tree, device tree, or attribute record. Missing attributes return `None`. The built-in `get_state` action exposes the same lookup through its outcome's `value`. |
| `save_state(path)` / `load_state(path)` | Optionally persist local state. Loading seeds silently and preserves timestamps. Call these explicitly where restoration and persistence fit the service. |
| `query_state(service_id=None, device_id=None, attribute=None, *, timeout=5)` | Queries the authority, including from inside an action. Defaults to the caller's service. The reply has `found` and, when found, `state`. Whole-service replies also include `availability`. A missing reply raises `TimeoutError`. Registered services can query the shared tree. |

## Example services

**Polling and actions** (`thermostat.py`). Replace the fixed reading with a hardware read.

```python
from chains_automation import Service, capability


class Thermostat(Service):
    attributes = ("temperature", "target")

    def onInit(self):
        self.seed_state(target=20.0)

    def onPoll(self):
        celsius = 21.5
        self.set_state(temperature=celsius)
        self.event_temperature(celsius)

    def event_temperature(self, celsius: float):
        self._send_event()

    @capability(idempotent=True)
    def action_set_target(self, celsius: float):
        self.set_state(target=celsius)
        return celsius
```

Run `chains-service thermostat:Thermostat --poll-interval 5`.

**Event observer** (`observer.py`). Replace `print` with the metrics sink of your choice.

```python
from chains_automation import Service


class Observer(Service):
    def onInit(self):
        self.subscribe("*")

    def onEvent(self, event):
        print(event["source"], event["type"], event["data"])
```

Run `chains-service observer:Observer`.

## Low-level SDK

Use `Client(transport, source, service_type, encoding="json")` to manage the protocol lifecycle yourself. It exposes `send(message)`, `register(name, *, devices, events=None, actions=None, idempotent_actions=None)`, `event(kind, data, *, device, values, schema_version="1")`, `report(device, values, *, cause=None)`, `observe_batch(kind, observations, *, schema_version="1", cause=None)`, and `outcome(request, status, error="", **result)`. `serve(stop, registration, handler)` registers and dispatches actions to `handler(request, name, args)` until the stop event is set. Use `Service` for reflected return values, state, and heartbeats.

`MQTT(address, namespace="chains", types=())` and `RedisStreams(...)` implement the `Transport` protocol with `publish(topic, data)`, `receive(timeout)`, and `close()`. Receives return `Delivery(topic, data, reset, retained)` or `None` on timeout. A reset signals a subscription discontinuity. With `MQTT(..., raw_topics=(...))`, the adapter subscribes to exact upstream topics and delivers their original names. Use this mode with a separate connection from the Chains bus. Helpers `message(kind, source, data, *, observed_at=None, **fields)`, `route(message)`, `encode(message, encoding="json")`, and `decode(bytes)` build, route, and serialize envelopes.
