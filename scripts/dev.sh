#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
root=$PWD
umask 077
dev="$root/.dev"
mqtt_port=${CHAINS_DEV_MQTT_PORT:-18883}
http_port=${CHAINS_DEV_HTTP_PORT:-8080}
url="http://127.0.0.1:$http_port"
bus="mqtt://127.0.0.1:$mqtt_port"

require() {
  for program in "$@"; do
    command -v "$program" >/dev/null || { printf 'Missing prerequisite: %s\n' "$program" >&2; exit 1; }
  done
}

build() {
  require go node npm pkg-config
  pkg-config --exists libmosquitto sqlite3 libevdev || {
    printf 'Install the libmosquitto, SQLite, and libevdev development packages.\n' >&2
    exit 1
  }
  mkdir -p "$dev/bin"
  if [[ ! -d web/node_modules || ! -f "$dev/web.lock" ]] || ! cmp -s web/package-lock.json "$dev/web.lock"; then
    npm ci --prefix web
    cp web/package-lock.json "$dev/web.lock"
  fi
  npm run build --prefix web
  CGO_ENABLED=1 go build -tags webui -o "$dev/bin/chains" ./cmd/chains
  CGO_ENABLED=1 go build -o "$dev/bin/chains-go-service" ./cmd/chains-go-service
  CGO_ENABLED=0 go build -o "$dev/bin/chainsctl" ./cmd/chainsctl
}

use_token() {
  if [[ -z "${CHAINS_TOKEN:-}" ]]; then
    if [[ ! -s "$dev/operator.token" ]]; then
      require openssl
      mkdir -p "$dev"
      (set -o noclobber; openssl rand -hex 32 >"$dev/operator.token")
    fi
    CHAINS_TOKEN=$(<"$dev/operator.token")
  fi
  export CHAINS_TOKEN
  export CHAINS_OPERATOR_TOKEN="$CHAINS_TOKEN" CHAINS_OPERATOR_URL="$url"
}

wait_until() {
  local label=$1 pid=$2
  shift 2
  for ((attempt=0; attempt<80; attempt++)); do
    kill -0 "$pid" 2>/dev/null || break
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep .25
  done
  printf 'Timed out waiting for %s. See %s/logs.\n' "$label" "$dev" >&2
  return 1
}

fan_state() {
  "$dev/bin/chainsctl" state --output json sim-fan/fan/power |
    jq -e --argjson expected "$1" '.rows | any(.attribute == "power" and .value == $expected)'
}

up() {
  require curl jq mosquitto mosquitto_pub
  # An existing demo may have its binaries open.
  require flock
  mkdir -p "$dev"
  exec 9>"$dev/launcher.lock"
  flock -n 9 || { printf 'A local Chains demo is already running.\n' >&2; exit 1; }
  build
  use_token
  mkdir -p "$dev/logs"
  # Keep the anonymous demo listener bound to host loopback.
  printf 'listener %s 127.0.0.1\nallow_anonymous true\npersistence false\n' "$mqtt_port" >"$dev/mosquitto.conf"
  printf '{}\n' >"$dev/integration.json"
  pids=()
  cleanup() {
    if (( ${#pids[@]} )); then
      kill "${pids[@]}" 2>/dev/null || true
      wait "${pids[@]}" 2>/dev/null || true
    fi
  }
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap 'cleanup' EXIT

  # CHAINS_* environment options take precedence over flags in the engine.
  # Keep this launcher isolated from any user-level runtime configuration.
  local -a config_env=(env -u CHAINS_CONFIG -u CHAINS_ID -u CHAINS_AUTHORITY -u CHAINS_DATABASE
    -u CHAINS_RULES -u CHAINS_LISTEN -u CHAINS_SENSOR_TOPIC -u CHAINS_SENSOR_BROKER -u CHAINS_TYPES
    CHAINS_BUS="$bus" CHAINS_NAMESPACE=chains-dev CHAINS_OPERATOR_API=true CHAINS_OPERATOR_WEB=true
    CHAINS_OPERATOR_TRACE=true CHAINS_OPERATOR_WEB_LOOPBACK_PROXY=false CHAINS_Z2M_ENABLED=false)

  local port
  for port in "$mqtt_port" "$http_port"; do
    if ! node -e '
      const port = Number(process.argv[1]);
      if (!Number.isInteger(port) || port < 1 || port > 65535) process.exit(1);
      const server = require("node:net").createServer();
      server.on("error", () => process.exit(1));
      server.listen(port, "127.0.0.1", () => server.close());
    ' "$port"; then
      printf 'Port %s is invalid or unavailable on loopback.\n' "$port" >&2
      return 1
    fi
  done
  mosquitto -c "$dev/mosquitto.conf" 9>&- >"$dev/logs/mosquitto.log" 2>&1 &
  local broker=$!
  pids+=("$broker")
  wait_until Mosquitto "$broker" timeout 2 mosquitto_pub -h 127.0.0.1 -p "$mqtt_port" -q 1 -t chains-dev/startup -n

  "${config_env[@]}" "$dev/bin/chains" engine -config "$root/examples/humidity.json" \
    -database "$dev/state.sqlite" -rules "$root/examples/humidity.chains" \
    -listen "127.0.0.1:$http_port" 9>&- >"$dev/logs/authority.log" 2>&1 &
  local authority=$!
  pids+=("$authority")
  wait_until authority "$authority" "$dev/bin/chainsctl" state --output json

  "${config_env[@]}" "$dev/bin/chains" fan -config "$dev/integration.json" -id sim-fan \
    9>&- >"$dev/logs/fan.log" 2>&1 &
  local fan=$!
  pids+=("$fan")
  "${config_env[@]}" "$dev/bin/chains" sensor -config "$dev/integration.json" -id humidity-sensor \
    9>&- >"$dev/logs/sensor.log" 2>&1 &
  local sensor=$!
  pids+=("$sensor")
  wait_until fan "$fan" fan_state false
  wait_until 'sensor registration' "$sensor" sensor_registered

  printf 'Web console: %s/ui/\nLogs: %s/logs\n' "$url" "$dev"
  if [[ -s "$dev/operator.token" && "$CHAINS_TOKEN" == "$(<"$dev/operator.token")" ]]; then
    printf 'Operator token: cat .dev/operator.token\n'
  else
    printf 'Sign in with the operator token from CHAINS_TOKEN.\n'
  fi
  printf 'From another terminal: ./scripts/dev.sh smoke\nPress Ctrl+C to stop.\n'
  wait -n "${pids[@]}" || true
  printf 'A demo process exited. See %s/logs.\n' "$dev" >&2
  exit 1
}

sensor_registered() {
  "$dev/bin/chainsctl" state --output json humidity-sensor/bathroom-sensor |
    jq -e '.rows | any(.kind == "device")'
}

smoke() {
  require jq mosquitto_pub
  use_token
  fan_state false >/dev/null || {
    printf 'The demo fan must start off. Restart the demo to repeat this check.\n' >&2
    exit 1
  }
  mosquitto_pub -h 127.0.0.1 -p "$mqtt_port" -q 1 -t sensors/bathroom/humidity -m 72.5
  wait_until 'fan action' "$$" fan_state true
  printf 'Humidity 72.5 triggered the simulated fan.\n'
}

case "${1:-up}" in
  up) up ;;
  build) build ;;
  smoke) smoke ;;
  *) printf 'Usage: %s [up|build|smoke]\n' "$0" >&2; exit 2 ;;
esac
