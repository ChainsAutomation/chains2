# Docker images and Compose

Build from the repository root.

```sh
docker build -f deploy/go.Dockerfile -t chains:dev .
docker build -f deploy/python.Dockerfile -t chains-python:dev .
```

The Go image includes `chains`, the embedded operator web console, `chainsctl`, and `chains-go-service`. The default command runs the authority. The Python image includes the bundled services and their locked RFLink and Radicale dependencies. Both images run as UID and GID 10001. Give that user access to any mounted state directory.

To publish images, substitute your registry and release tag below. Build for the architecture of your Kubernetes nodes. On mixed-architecture clusters, use Buildx with the relevant platforms.

```sh
docker tag chains:dev registry.example.org/chains/engine:0.1.0
docker tag chains-python:dev registry.example.org/chains/python:0.1.0
docker push registry.example.org/chains/engine:0.1.0
docker push registry.example.org/chains/python:0.1.0
```

## Run the MQTT demo

See `deploy/.env.example` for the local settings. The example starts Mosquitto, the authority, the simulated fan, and the sensor in separate containers. It publishes the authority only on host loopback and keeps the broker reachable inside the Compose network. Use the anonymous demo broker for local testing.

```sh
if [ ! -f deploy/.env ]; then
  (umask 077; printf 'CHAINS_TOKEN=%s\nCHAINS_HTTP_PORT=8080\n' "$(openssl rand -hex 32)" > deploy/.env)
fi
docker compose --env-file deploy/.env -f deploy/compose.yaml up --build --wait
```

Open `http://127.0.0.1:8080/ui/` and enter your token. Set `CHAINS_HTTP_PORT` in `deploy/.env` to change the host port. In the State view, wait for `sim-fan/fan/power` to report `false` and for the humidity sensor to register. Then publish a test reading.

```sh
docker compose --env-file deploy/.env -f deploy/compose.yaml exec -T mosquitto \
  mosquitto_pub -q 1 -t sensors/bathroom/humidity -m 72.5
```

The fan should report power on. `docker compose --env-file deploy/.env -f deploy/compose.yaml logs fan` shows the action. Run `down` with the same Compose arguments to stop the demo. The named authority volume retains SQLite state and the web rule catalog at `/var/lib/chains/state.sqlite.rules`.

## Use Redis Streams

Add `-f deploy/compose.redis.yaml` after the first `-f` for **every** Compose command.

```sh
docker compose --env-file deploy/.env -f deploy/compose.yaml -f deploy/compose.redis.yaml up --build --wait
docker compose --env-file deploy/.env -f deploy/compose.yaml -f deploy/compose.redis.yaml exec -T mosquitto \
  mosquitto_pub -q 1 -t sensors/bathroom/humidity -m 72.5
```

The authority, fan, and sensor publish Chains messages to Redis. The sensor still listens for raw humidity readings on Mosquitto. Redis has its own named data volume. Use different Compose project names and host ports to run both examples side by side.

## Run your integrations

For a Python service, run the image with arguments such as `rflink -f /etc/chains/services/rflink.toml` or `radicale -f /etc/chains/services/home-dav.toml`, and mount the corresponding configuration. Set `CHAINS_BUS` to the broker address reachable inside the container. Mount persistent storage under `/var/lib/chains` for Radicale checkpoints. For RFLink or a USB keyboard, pass the single serial or input device with Docker's `--device` option and grant the container's user access to it. See the [Python services guide](services-python.md) and [Go SDK guide](sdk-go.md) for configuration.

For Onkyo receivers, see the [standalone Go service example](onkyo.md#containers).

For remote browser access, use HTTPS and the normal sign-in policy. The demo sets `CHAINS_OPERATOR_WEB_LOOPBACK_PROXY=true` because publishing a host loopback port can give the authority a non-loopback TCP peer. Keep that setting scoped to local forwarding setups and see the [web console guide](operator-web.md) for proxy configuration.
