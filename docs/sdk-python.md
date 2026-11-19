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

Environment variables override CLI flags or constructor options, then XDG config, then SDK defaults. JSON config is loaded from `$XDG_CONFIG_HOME/chains/services/<service_type>.<instance>.json`, with system defaults from `$XDG_CONFIG_DIRS`. Select an explicit file with `--config`, `config_path=`, or `CHAINS_SERVICE_CONFIG`.

| JSON key / constructor option | CLI flag | Environment | Default |
|---|---|---|---|
| `uri` | `--bus` | `CHAINS_BUS` | `mqtt://localhost:1883` |
| `namespace` | `--namespace` | `CHAINS_NAMESPACE` | `chains` |
| `encoding` | `--encoding` | `CHAINS_ENCODING` | `json` |
| `authority` | `--authority` | `CHAINS_AUTHORITY` | `engine` |
| `heartbeat_interval` | `--heartbeat-interval` | `CHAINS_HEARTBEAT_INTERVAL` | 15 seconds |
| `poll_interval` | `--poll-interval` | `CHAINS_POLL_INTERVAL` | 1 second with an `onPoll()` override |
| `service_id` | `--service-id` | `CHAINS_SERVICE_ID` | Generated ID persisted under `$XDG_STATE_HOME/chains/services` |
| `name` | `--service-name` | `CHAINS_SERVICE_NAME` | Class name |
| `instance` (CLI/constructor) | `--instance` | `CHAINS_INSTANCE` | `default` |

`service_type` defaults to the snake-case class name and can be overridden as a class attribute. The service type and instance select the config file and persisted identity. Give concurrently running copies distinct instances or distinct service IDs.

Supported URI schemes are `mqtt`, `mqtts`, `mqtt+unix`, `redis`, and `rediss`. Both `json` and `cbor` are supported. The SDK sends heartbeats independently of callbacks and polls. The authority marks the service offline after three missed intervals and emits `service.availability.changed` on transitions.

## Lifecycle and overrides

| Hook or setting | Behavior |
|---|---|
| `onInit()` | Runs before transport creation. Initialize hardware, seed state, or call `subscribe(*types)`. |
| `onStart()` | Runs after registration and initial state synchronization. Enable push-based hardware callbacks here. |
| `onPoll()` | Performs one poll. Runs immediately after `onStart()` on a dedicated worker, then waits the configured interval after each call. Polls never overlap. Exceptions are logged and retried on the next interval. Zero disables polling. |
| `onEvent(event)` | Receives a decoded message for a subscribed event type. `subscribe("*")` observes public events across the namespace. Subscribe by exact type to observe internal messages such as `service.heartbeat`. Actions addressed to the service are handled separately. |
| `onReconnect()` | Runs after re-registration and silent synchronization of current state. |
| `onShutdown()` | Runs after SDK workers stop and before the transport closes. Remove hardware callbacks and close hardware here. |
| `describe_devices()` | Override to return device dictionaries with `id`, `name`, `class`, and `attributes`. The SDK derives each device's action list. |
| `transport_factory(config, types)` | Override in `onInit()`, override `open_transport(config, types)`, or inject a transport with `Service(transport=...)`. The SDK adds its action and query subscriptions. The factory receives an empty tuple for a wildcard subscription. |

Use `run(emit=None)` or a context manager when embedding a service. Each instance starts once. `stop()` requests shutdown, `stopping` lets blocking hooks check it, and `close()` joins workers and releases resources. In-flight polls finish before cleanup. Hardware callbacks can call event methods or state APIs. Serialize hardware access if polling, callbacks, and actions share a connection.

## Events, actions, and registration

- `event_<name>(...)` declares an event. Call `self._send_event(*, _device=None, **fields)` inside it to publish the bound arguments and their defaults. Keyword fields override argument values and must fit the schema. `_device` selects a registered device for that message.
- `action_<name>(...)` declares an action. The SDK validates incoming arguments, sends acceptance, invokes the method, and sends a completion with its JSON-compatible return value under `value`. An exception produces a failed outcome. Action requests are handled serially and recent duplicates reuse their stored outcome.
- `@capability(name=None, version="1", schema=None, device=None, idempotent=False)` overrides discovery metadata. Mark an action idempotent only when retrying it is safe. Without a device override, capabilities use the device with ID `default`, or the first declared device.
- `describe()` returns the registration offline, including identity, devices, versioned schemas, and idempotent actions. Schemas normally come from named parameters and type hints. Supported annotations include primitives, unions, `Literal`, lists, and string-keyed dictionaries. Unannotated parameters are unconstrained. Use an explicit JSON Schema for other shapes.

## Reported state

The default device has ID `default`. Declare its state keys in the `attributes` class tuple, or declare attributes per device in `describe_devices()`. Local state starts empty. An attribute record contains `value`, `changed_at`, and `observed_at`. Central records also carry a revision.

| API | Behavior |
|---|---|
| `set_state(device="default", /, **attributes)` | Updates locally and queues a live report. Central value transitions can emit `state.changed`. |
| `seed_state(device="default", /, *, observed_at=None, **attributes)` | Updates locally and synchronizes silently. Supply a timezone-aware observation time for cached data. A stale seed cannot replace a newer central observation. |
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

Use `Client(transport, source, service_type, encoding="json")` to manage the protocol lifecycle yourself. It exposes `send(message)`, `register(name, *, devices, events=None, actions=None, idempotent_actions=None)`, `event(kind, data, *, device="", schema_version="1")`, `report(device, values, *, cause=None)`, and `outcome(request, status, error="", **result)`. `serve(stop, registration, handler)` registers and dispatches actions to `handler(request, name, args)` until the stop event is set. Use `Service` for reflected return values, state, and heartbeats.

`MQTT(address, namespace="chains", types=())` and `RedisStreams(...)` implement the `Transport` protocol with `publish(topic, data)`, `receive(timeout)`, and `close()`. Receives return `Delivery(topic, data, reset, retained)` or `None` on timeout. A reset signals a subscription discontinuity. Helpers `message(kind, source, data, *, observed_at=None, **fields)`, `route(message)`, `encode(message, encoding="json")`, and `decode(bytes)` build, route, and serialize envelopes.
