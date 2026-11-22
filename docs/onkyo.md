# Onkyo receiver

The bundled Go service controls the TX-NR696 over local eISCP TCP. Run one instance per receiver. Give it network access to the receiver and your Chains bus. On the receiver, check **Setup → Hardware → Power Management → Network Standby** if you want to turn it on from standby. Find its IP address under **Setup → Hardware → Network**. [TX-NR696 manual](https://assets.onkyo-av.com/product-manuals/TX-NR696_En_2022-11-02-192920_laoa.pdf.pdf?v=1734914498#page=129)

## Run it

Build the launcher using the [Go SDK instructions](sdk-go.md#bundled-usb-keyboard). Copy [onkyo.toml](../examples/onkyo.toml), replace the hostname with the receiver's address, and run:

```sh
CHAINS_BUS=mqtt://localhost:1883 ./chains-go-service onkyo -f examples/onkyo.toml
```

| Setting | Default | CLI | Environment |
| --- | --- | --- | --- |
| `host` | Required | `--host` | `CHAINS_SETTINGS__HOST` |
| `port` | `60128` | `--port` | `CHAINS_SETTINGS__PORT` |
| `zone2_mode` | `disabled` | `--zone2-mode` | `CHAINS_SETTINGS__ZONE2_MODE` |

Use `[settings.main]` and `[settings.zone2]` for per-zone `name`, `location`, and `tags`. For example, `--zone2-name` or `CHAINS_SETTINGS__ZONE2__NAME` sets the Zone 2 display name. The SDK applies environment, CLI, TOML, and defaults in that order. It looks for `chains/services/<instance>.toml` under the XDG config directories when you omit `-f`. Use a separate instance and stable service ID for each receiver. [Go SDK configuration](sdk-go.md#configuration-and-lifecycle)

## Controls and Events

| Device | Controls |
| --- | --- |
| `main` | `power`, `volume`, `muted`, `input` |
| `zone2` in `speakers` mode | `power`, `volume`, `muted`, `input` |
| `zone2` in `external` mode | `power`, `input` |

Volume uses native receiver steps. Check the reported value at a comfortable listening level before using a numeric preset. Input choices use stable source names independent of the receiver display labels. Zone 2's analog and HDMI outputs have source and signal restrictions, and changing between NET and Bluetooth can also affect Main. Zone B is a shared-source output outside the service's scope. [Onkyo multi-zone setup](https://support.onkyousa.com/hc/en-us/articles/8924002040212-TX-NR696-Multi-zone-Connection), [TX-NR696 manual, p. 78](https://assets.onkyo-av.com/product-manuals/TX-NR696_En_2022-11-02-192920_laoa.pdf.pdf?v=1734914498#page=78)

The web console generates controls from the zone descriptions. A rule can react to a reported input change, then use the matched device reference to request another change:

```text
ev = Event(type="state.changed", service_type="onkyo", device_id="main", attribute="input", value="tv")
Set(device=ev.device, attribute="muted", value=false)
```

Use `device.observed` to watch individual reports, including confirmed setters that leave the value unchanged. Startup and receiver reconnect queries seed State silently. Receiver reports from the remote or front panel produce Events when values change. A matching setter report carries the Action's trace IDs. eISCP has no request ID, so a simultaneous manual change to exactly the requested value is indistinguishable from a command reply.

Watch `system.connected` for receiver reachability. The service heartbeat reflects the Chains process and bus connection. If the receiver drops, the zone values remain as last known values. A timed-out Action can leave the receiver changed if it acted before the response was lost. Chains refreshes State on reconnect and sends no repeat command.

## Containers

Build the [Go image](docker-compose.md#docker-images-and-compose). Add this service to your Compose configuration using the same broker and namespace as the authority:

```yaml
services:
  onkyo:
    image: chains:dev
    entrypoint: [chains-go-service]
    command: [onkyo, -f, /etc/chains/services/onkyo.toml]
    environment:
      CHAINS_BUS: mqtt://mosquitto:1883
      CHAINS_SERVICE_ID: living-room-onkyo
    volumes:
      - ./onkyo.toml:/etc/chains/services/onkyo.toml:ro
```

In Kubernetes, use the same image with `command: [chains-go-service]` and the same arguments in a separate single-replica Deployment. Mount the TOML from a ConfigMap, set a stable service ID, and use the `Recreate` strategy so updates keep one controller instance per receiver. Use the broker URL reachable from the pod and allow outbound TCP to the receiver. [Kubernetes deployment guide](kubernetes.md#use-an-existing-broker)
