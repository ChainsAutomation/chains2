# Chains Go service SDK

The SDK uses explicit Go declarations for devices, event schemas, action schemas, and handlers. The managed runtime handles transport selection, registration, workers, heartbeats, and shutdown.

## Minimal service

```go
package main

import (
    "context"
    "encoding/json"
    "errors"
    "flag"
    "log"
    "os"
    "os/signal"
    "syscall"

    chains "chains-automation"
    "chains-automation/sdk"
)

func main() {
    cfg, err := sdk.LoadConfig("ping", os.Args[1:], nil)
    if errors.Is(err, flag.ErrHelp) {
        return
    }
    if err != nil {
        log.Fatal(err)
    }
    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer cancel()

    err = sdk.RunService(ctx, cfg, sdk.Definition{
        Service: chains.Service{
            ServiceType: "ping",
            Devices: []chains.Device{{ID: "system", Name: "System", Attributes: chains.Attributes("last_ping")}},
            Events: map[string]map[string]json.RawMessage{
                "ping": {"1": json.RawMessage(`{"type":"object"}`)},
            },
        },
        Hooks: sdk.Hooks{
            OnStart: func(ctx context.Context, service *sdk.Runtime) error {
                return service.Event(ctx, "ping", "system", "", map[string]any{}, map[string]any{"last_ping": true})
            },
        },
    })
    if err != nil {
        log.Fatal(err)
    }
}
```

The empty device and version select the default device and schema version `1`. With no devices declared, the SDK adds `system`. A one-shot producer can call `service.Stop()` after publishing.

## Configuration and lifecycle

`sdk.LoadConfig(serviceType, args, &settings)` uses the [same operator config as Python](sdk-python.md#configuration). Common fields go in `[service]`, custom fields in `[settings]`, and environments override flags and TOML. Use `-f` or `--config` to select a file. Without an explicit file, the SDK looks for `$XDG_CONFIG_HOME/chains/services/<instance>.toml` and matching system defaults. The instance comes from `CHAINS_INSTANCE`, `--instance`, the explicit filename, or the service type, in that order. The generated ID persists per service type and instance.

Initialize a settings struct with defaults before calling `LoadConfig`. Use `toml:"field_name"` on exported fields, `required:"true"` for required values, and `help:"…"` for generated help. Nested structs form sections. Supported leaf values include strings, booleans, integers, floating-point values, slices, string-keyed maps, and pointers to those types. For example, `[settings.metrics].flush_interval` maps to `--metrics-flush-interval` and `CHAINS_SETTINGS__METRICS__FLUSH_INTERVAL`. Pass strings as raw text and other overrides as TOML literals.

| Hook | Use |
|---|---|
| `OnInit` | Set up hardware, seed local state, or call `Subscribe(...)` before connecting. |
| `OnStart` | Start after registration and initial sync. Call `service.Go(worker)` here to attach a managed input loop. |
| `OnEvent` | Handle selected bus events. Committed observations carry device patches and payloads in `Message.Observations`. `Subscribe("*")` observes public events, and explicit types can include protocol messages. |
| `OnPoll` | Perform a single poll. It runs immediately after startup and then waits `poll_interval` seconds after each call. Zero disables polling. |
| `OnReconnect` | Run after re-registration and silent state sync. |
| `OnShutdown` | Release hardware after managed workers exit and before transport closure. |

Workers, callbacks, and actions should honor their contexts. Heartbeats are independent of polling and action handlers. A service can inject a test transport with `Definition.OpenBus`.

## Actions and state

Set `Device.Placement` to declare default location segments and tags. [Device placement](devices.md)

See the [event and State guide](events.md) for the shared wire format, batch constraints, and rule matching.

Declare typed attributes with `map[string]chains.Attribute` and optional setters. Use `chains.Attributes(...)` for open JSON schemas. [Device contracts](actions.md)

Declare schemas in `Definition.Service.Actions`, list supported action names on each device, and supply handlers under `Definition.Actions[actionName][schemaVersion]`. An `ActionHandler` receives the context, runtime, request envelope, and parsed action. Its return value goes into the action outcome. Declare idempotence through `Service.IdempotentActions` when retrying the action is safe.

- `Event(ctx, kind, device, version, data, values)` publishes a typed observation and updates local State. Pass `nil` for `data` to use the values map as the payload.
- `ObserveBatch(ctx, kind, version, observations)` publishes multiple devices of the same service and schema version in one revision. Build patches with `chains.NewObservation(device, values, data)`. The lower-level `Client.ObserveBatch` also takes an optional causal message.
- `SetState(ctx, device, values)` updates declared attributes locally and queues an observation of type `device.observed`.
- `SeedState(device, observedAt, values)` queues a silent observation. A zero timestamp uses the current time.
- `GetState(device, attribute)` reads the local subtree. The built-in `get_state` action exposes it remotely.
- `SaveState(path)` and `LoadState(path)` opt into disk persistence. Loading preserves timestamps and seeds silently.
- `QueryState(ctx, service, device, attribute)` reads central state, including from an action. Empty service selects the caller. It returns a found flag and a JSON subtree, plus availability for whole-service queries. The default deadline is five seconds when the caller supplies none.

## Bundled USB keyboard

On Linux, install the libmosquitto and libevdev development packages, then build the standalone launcher.

```sh
go build -buildvcs=false -o chains-go-service ./cmd/chains-go-service
./chains-go-service --list
./chains-go-service keyboard -f automation-keyboard.toml
```

```toml
[service]
uri = "mqtt://localhost:1883"

[settings]
device = "/dev/input/by-id/usb-example-event-kbd"
grab = false
reconnect_interval = 2
```

Grant the service user read access to the selected evdev node. A stable device path lets the service find the same keyboard after unplugging. Enable `grab` only for an automation keyboard that should be reserved for Chains. Use `--device`, `--grab true`, and `--reconnect-interval` or `CHAINS_SETTINGS__DEVICE`, `CHAINS_SETTINGS__GRAB`, and `CHAINS_SETTINGS__RECONNECT_INTERVAL` to override TOML.

`keyboard.key` contains `action` (`press` or `release`), Linux `code`, symbolic `key`, and the held `keys` and `mods`. The same observation records the transition in `last_key`. Auto-repeat and resynchronization do not trigger events.

```text
ev = Event(type="keyboard.key", action=press, key=KEY_F13)
```
