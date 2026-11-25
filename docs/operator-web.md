# Chains web console

The authority can serve a bundled Solid 2 console for reported state, live activity, event submission, rule authoring, and traces. The web console uses the same operator API as `chainsctl`.

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

Open `http://127.0.0.1:8080/ui/` and sign in with the token. The console opens on State. The standalone household app uses `/` and an independent session. See [Household dashboards](household-dashboards.md) for dashboard editing and household sign-in. The equivalent configuration settings are `operator_web` and `CHAINS_OPERATOR_WEB=true`. The console defaults to disabled. The Go binary serves all runtime assets. Builds without the `webui` tag still provide the CLI and operator API.

For remote access, put the authority behind an HTTPS reverse proxy. Preserve the browser's original `Host` and disable buffering for `/operator/v1/activity`. HTTP sign-in is available over loopback. For a service reachable only through a loopback-bound Docker or Kubernetes forward, set `operator_web_loopback_proxy`, `-operator-web-loopback-proxy`, or `CHAINS_OPERATOR_WEB_LOOPBACK_PROXY=true`. This permits a matching loopback browser origin when the forwarding connection has a non-loopback peer address. The setting defaults to off and does not use forwarded headers. Browser sessions expire after eight hours and are cleared when the authority restarts. Sign out to revoke the current session. CLI clients continue to use the bearer token.

## Views

| View | Use |
| --- | --- |
| **State** | Browse registered services and devices, filter attributes, and inspect values, timestamps, and revisions. Devices in one observation share a revision. Declared attributes without an observation are marked **Unreported**. Select a writable attribute to send a command. [Device controls](actions.md) |
| **All Devices** | Browse device cards by location, edit placement and tags, and control writable attributes. [Device dashboard](devices.md) |
| **Watch** | Follow events, rule traces, and actions. Pause to inspect a stable list or resume to catch up. Type wildcards such as `action.*` work in the type filter. Enable low-level traffic to see heartbeats and synchronization. |
| **Send** | Choose a configured HTTP profile or write a `manual.<name>` event. Use fields when available, switch to JSON for complex data, or insert an observed state value. Preview then publish. Drafts survive tab and profile changes until reload or sign-out. |
| **Rules** | Create and validate rules, publish live, import source files, and export the authority catalog. |
| **Trace Explorer** | Open a publication receipt, inspect a Watch record, or paste a correlation ID. The timeline joins matching events, rule runs, actions, outcomes, and state changes. Parent links resolve to records Chains still has. |

Enable `operator_trace` on the authority or any worker whose rule lifecycle should appear. Trace Explorer uses the newest 2,048 activity records retained by the authority. An authority restart clears that buffer. See the [operator CLI guide](operator-cli.md) for profile configuration and watch retention.

The authenticated `GET /operator/v1/services` endpoint provides registered descriptions for the selectors. `GET /operator/v1/traces?correlation=<id>` returns the matching activity records and the oldest retained record, when present.

## Rules

Use **From template** to select a bundled or uploaded template, choose the required devices and attributes, and open an editable DSL draft. The guided editor also offers **Add delay** for inserting a pause between steps. [Rule templates and TOML format](rule-templates.md)

Open **Rules** and select **New rule**. The guided form supports an event trigger, elapsed interval, or calendar schedule followed by actions. It uses registered devices and action schemas to populate selectors and arguments. Use **Set attribute** for a typed setter and **Continue after** to select the action wait mode. Use the DSL editor for conditions, loops, expressions, or more waits. Select **Validate and preview** to see the exact source and any diagnostics, then **Publish**. Enable, disable, and edit web rules from the same editor. Publication cancels runs of the changed rule. Actions already submitted can still finish. Deployment-managed rules appear read-only on the authority.

The authority stores each web rule as a `.chains` file in the generation named by `CURRENT`. Files are keyed by rule ID and the manifest records their names and enabled status. Set `web_rules_dir`, `-web-rules-dir`, or `CHAINS_WEB_RULES_DIR` to change the directory. The default appends `.rules` to the configured SQLite path, for example `$XDG_STATE_HOME/chains/state.sqlite.rules`. With `-database :memory:`, provide an explicit directory to publish. The authority loads published rules on restart and honors its event subscriptions and scheduler settings.

**Download All** exports a ZIP containing the authority's deployment and web rules, including disabled rules. **Import file** previews one `.chains` file or an exported ZIP. Review the source and enabled status before publishing. For duplicate names, rename, skip, or replace a web rule. A deployment rule can be skipped or imported under a new name. The selected changes publish together. Individual sources are limited to 128 KiB and imports to 8 MiB. [Architecture decision](adr/0002-web-managed-rules.md)

## Development and verification

Run the authority with the bundled console enabled for browser sessions. Run `npm run dev --prefix web` and open the printed local address under `/ui/`. Vite serves both app entry points and proxies both APIs to `http://127.0.0.1:8080` by default. Set `CHAINS_WEB_DEV_TARGET` to use another local authority.

The browser suite starts a temporary authority, MQTT broker, and simulated integrations. The fixture uses ports 18080 and 18883 by default. Set `CHAINS_WEB_TEST_PORT` and `CHAINS_WEB_TEST_MQTT_PORT` if those ports are busy.

```sh
npm ci --prefix web
npm exec --prefix web -- playwright install chromium
bash web/tests/run-live.sh
```

Set `CHAINS_WEB_TEST_CHROMIUM` to the path of an installed Chromium to use it instead of downloading a browser.
