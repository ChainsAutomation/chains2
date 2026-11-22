#!/usr/bin/env bash
# Run from the repository root with Go, Node, and Mosquitto installed.
set -euo pipefail
temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/chains-web-test.XXXXXX")
pids=()
cleanup() {
  if (( ${#pids[@]} )); then
    kill "${pids[@]}" 2>/dev/null || true
    wait "${pids[@]}" 2>/dev/null || true
  fi
  if [[ "${CHAINS_WEB_KEEP_TEST_LOGS:-}" == 1 ]]; then
    printf 'Test logs: %s\n' "$temp_dir"
  else
    rm -rf "$temp_dir"
  fi
}
trap cleanup EXIT
mqtt_port=${CHAINS_WEB_TEST_MQTT_PORT:-18883}
web_port=${CHAINS_WEB_TEST_PORT:-18080}
export CHAINS_WEB_TEST_MQTT_PORT="$mqtt_port" CHAINS_WEB_TEST_URL="http://127.0.0.1:$web_port"
export CHAINS_WEB_TEST_TOKEN="web-fixture-token"
export CHAINS_HOUSEHOLD_TOKEN="web-fixture-household"
export CHAINS_TOKEN="$CHAINS_WEB_TEST_TOKEN"
export CHAINS_BUS="mqtt://127.0.0.1:$mqtt_port"
export CHAINS_NAMESPACE="web-fixture"
printf 'listener %s 127.0.0.1\nallow_anonymous true\n' "$mqtt_port" > "$temp_dir/mosquitto.conf"
cat > "$temp_dir/engine.json" <<'JSON'
{"http":{"humidity":{"source":"humidity-sensor","device":"bathroom-sensor","type":"humidity.reported","schema_version":"1","attribute":"value","state_attribute":"humidity"}}}
JSON
printf '{}\n' > "$temp_dir/service.json"
cat > "$temp_dir/manual.chains" <<'RULE'
Event(type="manual.web_test")
Action("set_power", device=Device(service_id="sim-fan", device_id="fan"), value=true)
RULE
npm run build --prefix web
go build -tags webui -o "$temp_dir/chains" ./cmd/chains
mosquitto -c "$temp_dir/mosquitto.conf" > "$temp_dir/mqtt.log" 2>&1 &
pids+=($!)
for ((i=0; i<50; i++)); do
  if mosquitto_pub -p "$mqtt_port" -t web-test/ready -m 1 2>/dev/null; then break; fi
  sleep .1
done
"$temp_dir/chains" engine -config "$temp_dir/engine.json" -database :memory: -web-rules-dir "$temp_dir/web-rules" -rules "$temp_dir/manual.chains" \
  -listen "127.0.0.1:$web_port" -operator-api -operator-trace -operator-web > "$temp_dir/engine.log" 2>&1 &
pids+=($!)
for ((i=0; i<50; i++)); do
  if curl -fsS "$CHAINS_WEB_TEST_URL/ui/" >/dev/null 2>&1; then break; fi
  sleep .1
done
if ! curl -fsS "$CHAINS_WEB_TEST_URL/ui/" >/dev/null; then cat "$temp_dir/engine.log"; exit 1; fi
"$temp_dir/chains" fan -config "$temp_dir/service.json" -id sim-fan > "$temp_dir/fan.log" 2>&1 &
pids+=($!)
"$temp_dir/chains" sensor -config "$temp_dir/service.json" -id humidity-sensor > "$temp_dir/sensor.log" 2>&1 &
pids+=($!)
if ! npm test --prefix web; then cat "$temp_dir/engine.log" "$temp_dir/fan.log" "$temp_dir/sensor.log"; exit 1; fi
