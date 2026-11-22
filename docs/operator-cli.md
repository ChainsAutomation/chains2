# Operator CLI and TUI

`chainsctl` connects to the state authority's HTTP API. It can inspect the State tree, describe services, follow live activity, and publish manual events over either system bus. Install it from the checkout with Go 1.23 or newer. For browser access and correlated activity timelines, see the [web console guide](operator-web.md).

```sh
./scripts/install.sh
chainsctl describe
```

The installer defaults to `~/.local/bin`. Add it to your `PATH` if needed. See [Native installation](getting-started.md#native-installation) for other binaries and custom install directories.

Create `$XDG_CONFIG_HOME/chains/chainsctl.toml` (usually `~/.config/chains/chainsctl.toml`). Use the authority's HTTPS URL when connecting remotely.

```toml
url = "https://chains.example.test"
token_command = ["gopass", "show", "--password", "--nofuzzysearch", "chains/operator"]
```

Use `token = "…"` in place of `token_command` to store a literal bearer token. Give a file containing a literal token mode `0600`. The command runs once per invocation. Specify its executable and arguments as separate strings. It must succeed within 30 seconds and return a single nonempty token on stdout. For gopass, store the same token as the authority uses and substitute your entry name. [gopass show](https://github.com/gopasspw/gopass/blob/master/docs/commands/show.md)

`CHAINS_OPERATOR_TOKEN` overrides the file credential. URL precedence is `CHAINS_OPERATOR_URL`, `--url`, the file, then `http://127.0.0.1:8080`. Use `--config` or `CHAINS_OPERATOR_CONFIG` to select a file for another deployment. If you override the URL to a different endpoint, supply `CHAINS_OPERATOR_TOKEN` for that endpoint.

For an existing `chainsctl.json`, copy its URL into the TOML file and add your token source. If the installer finds a custom launcher at the destination, check the new config with an install in a separate directory before using `--force` to replace the launcher.

## Configure the authority

```sh
export CHAINS_TOKEN='<your bearer token>'
chains engine -rules examples/humidity.chains -listen 127.0.0.1:8080 -operator-api -operator-trace
```

For a remote authority, configure the HTTPS proxy to stream Server-Sent Events without buffering.

| Engine JSON key | Flag | Environment | Default |
| --- | --- | --- | --- |
| `operator_api` | `-operator-api` | `CHAINS_OPERATOR_API` | `false` |
| `operator_trace` | `-operator-trace` | `CHAINS_OPERATOR_TRACE` | `false` |
| `listen` | `-listen` | `CHAINS_LISTEN` | Disabled |
| `token` | `-token` | `CHAINS_TOKEN` | Required for HTTP unless a command supplies it |
| `token_command` | | | Optional array of executable and arguments |

In the engine JSON config, use `"token_command": ["gopass", "show", "--password", "--nofuzzysearch", "chains/operator"]` in place of `token`. An explicit environment value or flag overrides the configured source. The engine resolves the command once at startup when HTTP is enabled.

Enable `operator_api` only on the state authority. Set `operator_trace=true` on each engine whose rule runs you want to see. The authority collects activity from a separate all-types subscription even when its rules subscribe to a subset. Workers send trace over the bus as `operator.trace`, which the engine ignores in rule dispatch. With MQTT ACLs, grant the observer subscription access across the Chains namespace and grant each tracing worker permission to publish under its own ID on the trace route.

## Inspect state

```sh
chainsctl state
chainsctl state humidity-sensor/bathroom-sensor
chainsctl state --output json humidity-sensor/bathroom-sensor/humidity
chainsctl state --output csv
```

Use stable IDs in a path of `service[/device[/attribute]]`. The response identifies the state epoch and revision and includes service, device, and attribute rows within the selected subtree. An unmatched path returns zero rows. State lists declared attributes even before a device reports them. The table and TUI show **Unreported**. JSON omits the reading fields for these attributes, and CSV leaves the value and timestamps empty. An explicit `null` reading has its own timestamps. Reported records also include the last state revision. The default table shows the last change time. JSON and CSV include both timestamps when available.

## Describe registered services

```sh
chainsctl describe
chainsctl describe 'Onkyo TX-NR696'
chainsctl describe Radicale --output json
chainsctl describe --output csv
```

The default table lists every registered service and its capability counts. An exact service name selects all matching instances. A stable service ID selects that instance. Detailed output includes devices, locations, classes, attribute schemas and setters, and versioned event and action schemas. JSON returns full registrations as an array. CSV lists one row per service, device, attribute, or capability version. The catalog includes declared capabilities before any observation arrives. Services such as the Onkyo receiver can publish the common `device.observed` event without declaring a custom event schema.

## Watch activity

```sh
chainsctl watch --recent 25
chainsctl watch --type 'action.*,rule.*' --engine keyboard-worker
chainsctl watch --service humidity-sensor --device bathroom-sensor --output json
chainsctl watch --all --correlation <event-id> --output csv
```

Watch shows incoming events, state changes, registrations, service availability and restart transitions, action requests and outcomes, and rule lifecycle records when tracing is enabled. A restart marker appears when the authority observes a new process instance for a known service. Rule tracing covers starts, resumes after a subsequent event, finishes, failures, timeouts, cancellations, and triggers skipped at capacity. Local bridge action requests appear in the feed alongside broker-delivered requests. Engines collecting traces also report their own action publication failures and timeouts.

Heartbeats, raw state reports and commits, snapshot traffic, and state queries are hidden by default. Use `--all` to include them or select them explicitly with `--type`. Combine `--type`, `--service`, `--device`, `--engine`, `--rule`, `--correlation`, and `--search` to narrow the view. Type selectors accept comma-separated exact types and trailing `*` wildcards. A device filter includes any committed batch containing that device. Search is case-insensitive. Watch JSON is newline-delimited. CSV writes a single header.

The feed keeps the latest 2,048 activity records in the authority's memory. `--recent` replays that many records before filters are applied, then follows new activity. Watch reconnects using its latest cursor. A gap marker indicates that records may have been lost, for example during a broker interruption, authority restart, or trace queue overflow. Broker observations reflect traffic the observer received, regardless of whether an engine accepted an event. Large activity payloads appear as truncated previews. Snapshot chunks appear as summaries.

## Send an event

```sh
chainsctl send --profile humidity --data '72.5'
chainsctl send --name scene_evening --data '{"room":"living-room"}'
chainsctl send --name scene_evening --file event.json --output json
```

A profile is an entry in the authority's existing `http` configuration. Chains uses its configured source, device, event type, and schema version. The source must be registered. For mappings with `attribute`, supply the attribute value itself. The authority wraps it in the event payload before validating it. See the [HTTP humidity mapping](../README.md#configure-and-extend).

A custom name submits `manual.<name>` with a JSON object from the reserved `chains.operator` service. The same observation updates `system/last_manual_event` with the name and payload. A rule can subscribe with `Event(type="manual.scene_evening")`. Custom names may use ASCII letters, digits, underscores, dashes, and dots, with a letter, digit, or underscore first. An omitted payload defaults to `{}`. Use `--file -` to read JSON from standard input. The receipt contains the event ID, type, and originating service. Use the ID to correlate activity.

## Terminal UI

Run `chainsctl tui`. The tab line and status line each use one row. The remaining rows are available for state, activity, the event form, or details.

| View | Keys |
| --- | --- |
| Navigate | `F1` State, `F2` Watch, `F3` Send. Number shortcuts also work in State and Watch. `Ctrl+C` quits. |
| State | `/` filters by path, name, or value. Arrows or `j` and `k` move selection. `Space` folds a service or device, `Enter` opens details, and `r` refreshes. The active view also refreshes every three seconds. |
| Watch | `p` pauses or follows. Moving the selection pauses automatically. `a` shows low-level traffic. `/` accepts free text and selectors such as `type=action.* service=sim-fan`. `Enter` opens the selected record. |
| Send | Use arrows on the profile row to pick a profile, then `Tab` to move between fields. `Ctrl+P` previews the submission and `Ctrl+S` publishes it. `Ctrl+J` switches typed profiles between fields and the JSON editor. Drafts persist while changing tabs or profiles. |
| Details | Arrows or Page Up and Page Down scroll. `Esc` closes details. |
