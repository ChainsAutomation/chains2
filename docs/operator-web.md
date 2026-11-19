# Chains web console

The authority can serve a bundled Solid 2 console for reported state, live activity, event submission, and traces. The web console uses the same operator API as `chainsctl`.

## Build and run

Build the frontend with Node.js 22.12 or newer, then embed its output in the Go binary.

```sh
npm ci --prefix web
npm run build --prefix web
go build -tags webui -o chains ./cmd/chains
```

Enable the console on the state authority. Set `CHAINS_TOKEN` to the operator token for the deployment.

```sh
./chains engine -listen 127.0.0.1:8080 -operator-api -operator-web -operator-trace
```

Open `http://127.0.0.1:8080/ui/` and sign in with the token. The equivalent configuration settings are `operator_web` and `CHAINS_OPERATOR_WEB=true`. The console defaults to disabled. The Go binary serves all runtime assets. Builds without the `webui` tag still provide the CLI and operator API.

For remote access, put the authority behind an HTTPS reverse proxy. Preserve the browser's original `Host` and disable buffering for `/operator/v1/activity`. HTTP sign-in is available over loopback. Browser sessions expire after eight hours and are cleared when the authority restarts. Sign out to revoke the current session. CLI clients continue to use the bearer token.

## Views

| View | Use |
| --- | --- |
| **State** | Browse registered services and devices, filter attributes, and inspect values, timestamps, and revisions. Declared attributes without an observation are marked **Unreported**. |
| **Watch** | Follow events, rule traces, and actions. Pause to inspect a stable list or resume to catch up. Type wildcards such as `action.*` work in the type filter. Enable low-level traffic to see heartbeats and synchronization. |
| **Send** | Choose a configured HTTP profile or write a `manual.<name>` event. Use fields when available, switch to JSON for complex data, or insert an observed state value. Preview then publish. Drafts survive tab and profile changes until reload or sign-out. |
| **Trace Explorer** | Open a publication receipt, inspect a Watch record, or paste a correlation ID. The timeline joins matching events, rule runs, actions, outcomes, and state changes. Parent links resolve to records Chains still has. |

Enable `operator_trace` on the authority or any worker whose rule lifecycle should appear. Trace Explorer uses the newest 2,048 activity records retained by the authority. An authority restart clears that buffer. See the [operator CLI guide](operator-cli.md) for profile configuration and watch retention.

The authenticated `GET /operator/v1/services` endpoint provides registered descriptions for the selectors. `GET /operator/v1/traces?correlation=<id>` returns the matching activity records and the oldest retained record, when present.

## Development and verification

Run the authority with the bundled console enabled for browser sessions. Run `npm run dev --prefix web` and open the printed local address under `/ui/`. Vite proxies the operator API to `http://127.0.0.1:8080` by default. Set `CHAINS_WEB_DEV_TARGET` to use another local authority.

The browser suite starts a temporary authority, MQTT broker, and simulated integrations. The fixture uses ports 18080 and 18883 by default. Set `CHAINS_WEB_TEST_PORT` and `CHAINS_WEB_TEST_MQTT_PORT` if those ports are busy.

```sh
npm ci --prefix web
npm exec --prefix web -- playwright install chromium
bash web/tests/run-live.sh
```

Set `CHAINS_WEB_TEST_CHROMIUM` to the path of an installed Chromium to use it instead of downloading a browser.
