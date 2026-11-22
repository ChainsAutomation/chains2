# Radicale service

The bundled Python service syncs named Radicale calendars, task lists, and address books. It publishes changes and reminder events on MQTT or Redis and can create or delete individual items. Radicale needs no server hook.

```sh
uv sync --project python --locked --extra radicale
export CHAINS_SETTINGS__PASSWORD='your Radicale password'
uv run --project python --locked --extra radicale chains-service radicale -f home-dav.toml
```

Use the DAV collection paths Radicale shows for each calendar or address book. This example assumes the server is mounted at `/dav/`. A path without a leading slash is relative to `radicale_url`.

```toml
[service]
name = "Home DAV"
uri = "mqtt://localhost:1883"

[settings]
radicale_url = "https://calendar.example.org/dav/"
username = "home"
timezone = "Europe/Oslo"
sync_interval = 30
writable_collections = ["family", "tasks", "people"]
deletable_collections = ["tasks"]

[settings.collections]
family = "home/family/"
tasks = "home/tasks/"
people = "home/addressbook/"
```

Use an account with the needed Radicale rights. Both permission lists default to empty and grant their operation independently. Each alias becomes a Chains device. Changes and alarms update its `last_entry`, `last_contact`, `last_todo`, or `last_alarm` attribute. Service actions are on the `system` device. Start another service instance with its own config and instance name to connect a different account. Settings follow the [Python SDK precedence](sdk-python.md#configuration). For example, `--sync-interval 10` overrides the file, and `CHAINS_SETTINGS__SYNC_INTERVAL=15` overrides both. Use HTTPS for connections carrying credentials.

## Watch for changes and alarms

The first successful sync builds a silent baseline. Later polls publish additions, edits, and deletions for the versions they observe. Changes that occur and are undone between polls can go unseen. Polling defaults to 30 seconds. The service keeps its sync position and queued events in `$XDG_STATE_HOME/chains/services/radicale.<instance>.sqlite` (normally under `~/.local/state`).

| Types | Payload |
| --- | --- |
| `calendar.entry.created`, `.updated`, `.deleted` | `collection`, `uid`, `title`, `start`, `end`, `status` |
| `calendar.todo.created`, `.updated`, `.deleted` | `collection`, `uid`, `title`, `due`, `status` |
| `contact.created`, `.updated`, `.deleted` | `collection`, `uid`, `name` |
| `calendar.alarm.due` | `collection`, `uid`, `title`, `alarm_id`, `occurrence_id`, `scheduled_at`, `late` |

Dates use `YYYY-MM-DD`. Timed entries preserve the reported offset if present. The configured timezone defaults to UTC and resolves floating times and all-day alarm dates. Deletion events carry the last synchronized fields. Raw calendars and contacts, descriptions, locations, attendees, email addresses, and phone numbers stay out of change events. The local SQLite cache contains calendar entries for alarm calculation, including fields excluded from the bus.

Alarms use the `VALARM` entries stored by Radicale and fire on a local timer between polls. After a restart, each collection must sync before its alarms can resume. Alarms missed by up to five minutes can fire late. Reminders already recorded as published are skipped. Edits, cancellations, and deletions affect reminders once the service sees them. The alarm event leaves notification or device actions to Chains rules.

```python
alarm = Event(type="calendar.alarm.due", collection="family")
Action("writestring", service_name="LEDPanel", device_name="panel0", value=alarm.data.title)
```

## Create and delete items

Target the service's `System` device and pass a collection alias. Create actions return the collection and UID. A subsequent sync publishes the change with the action's correlation ID.

| Action | Arguments after `collection` |
| --- | --- |
| `create_calendar_entry` | `title`, `start`, `end`, optional `reminders_minutes_before` list |
| `create_todo` | `title`, optional `due` |
| `create_contact` | `name`, optional `emails` and `phones` lists |
| `delete_calendar_entry`, `delete_todo`, `delete_contact` | `uid` of a synchronized item |

For dated actions, use an RFC3339 timestamp with an offset or a date such as `2030-02-12`. An all-day entry's end date is exclusive. Creation retries for the same Chains action use the same DAV resource identity. Deletes use the last observed ETag and fail if another client changed the item since sync. Deleting a recurring entry deletes the entire series.

```python
entry = Event(type="calendar.entry.created", collection="family")
Action("create_todo", service_name="Home DAV", device_name="System", collection="tasks", title=entry.data.title)
```

Names, titles, and any private values supplied as action arguments are visible to bus subscribers and operator watch. The change event field limits apply only to change and alarm publications.
