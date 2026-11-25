# Local development and installation

The native demo runs Mosquitto, the Chains authority, the simulated fan, and the humidity sensor as separate processes on your machine. It also serves the web console at `http://127.0.0.1:8080/ui/`.

## Native installation

From a checkout, run:

```sh
./scripts/install.sh                     # chainsctl
./scripts/install.sh chains               # engine and demo adapters
./scripts/install.sh chains-go-service    # bundled Go services
./scripts/install.sh all --with-web       # all binaries, with both web apps embedded
```

The installer uses Go 1.23 or newer and puts binaries in `~/.local/bin`. Use `--bindir DIR` for another destination and ensure that directory is on your `PATH`. Run the same command from an updated checkout to upgrade. Use `--force` if you intend to replace an existing wrapper or unrelated binary.

The CLI build needs only Go. For the engine, install a C compiler, pkg-config, and the SQLite and libmosquitto development headers. For the Go services, also install the libevdev development headers. Web builds require Node.js 22.12 or newer and npm. The [operator CLI guide](operator-cli.md) covers connection settings and gopass setup.

## Prerequisites

Use Linux, Go 1.23 or newer, and Node.js 22.12 or newer. On Debian or Ubuntu, install the native build dependencies and demo tools.

```sh
sudo apt install build-essential pkg-config libmosquitto-dev libsqlite3-dev libevdev-dev \
  mosquitto mosquitto-clients curl jq openssl util-linux
```

Install Go and Node using your preferred package manager or version manager. Install [uv](https://docs.astral.sh/uv/getting-started/installation/) to develop Python integrations.

## Start the demo

From the repository root, run:

```sh
./scripts/dev.sh
```

Open the printed console URL. Sign in with the token in `.dev/operator.token`, or with `CHAINS_TOKEN` if you supplied one. In another terminal, run:

```sh
./scripts/dev.sh smoke
```

The smoke command publishes a humidity reading of 72.5 and checks that the simulated fan reports power on. Open **State** to inspect the reading and fan, or **Watch** to see the event and action. To repeat the check, stop and restart the demo. The fan starts off each time the simulator starts.

The launcher builds from the locked frontend dependencies and keeps the binaries, logs, token, and SQLite files in `.dev/`. Press Ctrl+C in the launcher terminal to stop the processes it started. Data remains in `.dev/` when you stop the demo. Use `./scripts/dev.sh build` to build the binaries without starting services.

The default ports are 8080 for HTTP and 18883 for MQTT. If either is busy, set `CHAINS_DEV_HTTP_PORT` and `CHAINS_DEV_MQTT_PORT` in both the launcher and smoke-test terminals. The launcher binds both listeners to loopback.

```sh
export CHAINS_OPERATOR_TOKEN="$(cat .dev/operator.token)"
export CHAINS_OPERATOR_URL=http://127.0.0.1:8080
.dev/bin/chainsctl state
.dev/bin/chainsctl watch
```

Rules published through the web console are stored under `.dev/state.sqlite.rules` and survive a demo restart. Edit `examples/humidity.chains` to change the demo rule and restart the launcher to reload it. The demo's HTTP profile is in `examples/humidity.json`. See [configuration and precedence](../README.md#configure-and-extend) for running a custom authority directly.

## Generic MQTT sensor adapter

The `chains sensor` subcommand maps a concrete raw MQTT topic into a Chains device observation:

- By default it subscribes to `sensors/bathroom/humidity`, accepts raw numbers, and writes `humidity` to State in the same observation that triggers the rule.
- A retained MQTT reading seeds reported State silently without emitting a live reading event.
- To map another device, set `sensor_topic` (or `-sensor-topic`) and the config file's `sensor` object. The object can override `device`, `event`, `attribute`, `field`, `format`, and `schema`.
- Supported input formats are `number`, `boolean` (including `ON`/`OFF`), `string`, and `json`. Supply a matching JSON Schema when changing the event payload.
- When running the system bus on Redis Streams (`-bus redis://localhost:6379`), pass `-sensor-broker mqtt://localhost:1883` so the adapter reads raw readings from MQTT and publishes observations to Redis.

## Frontend and SDK development

With the demo running, start Vite in another terminal.

```sh
npm run dev --prefix web
```

Open the printed address under `/ui/`. If you changed the authority port, set `CHAINS_WEB_DEV_TARGET` to its HTTP URL before starting Vite. The [web console guide](operator-web.md) covers its browser test suite.

Use `go test ./...` for the Go code and `uv sync --project python --locked` for the Python SDK. See the [Go SDK guide](sdk-go.md) and [Python SDK guide](sdk-python.md) to run integrations directly.

For container deployments, see [Docker and Compose](docker-compose.md) and [Kubernetes](kubernetes.md).
