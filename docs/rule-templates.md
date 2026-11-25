# Rule templates

In the operator console, open **Rules → From template**. Choose a template, fill in all inputs, and select **Create draft**. Review the generated DSL, edit it if needed, and publish it like any other rule. The generated source is stored as a regular rule and included in **Download All**. Template uploads and downloads are separate from rule imports and exports.

**See 1–2–3** asks for one motion sensor and three different lights, including dimmers. After the sensor's first reading, an occupancy change to `true` sends three power-on requests in the selected order, two seconds apart. The delay is between sent requests. Devices respond at their own pace.

## Write and upload a template

Save a UTF-8 `.toml` file of up to 128 KiB. Give it a unique ID of up to 64 lowercase letters, digits, and hyphens that starts with a letter or digit. The title is shown in the console. Add an optional description and declare each input in an `[[inputs]]` block.

```toml
id = "turn-on-selected-light"
title = "Turn on a selected light"
description = "Choose a light and a writable boolean attribute."
source = '''
Event(type="manual.turn_on_selected")
Set(device=lamp, attribute=setting, value=true)
'''

[[inputs]]
name = "lamp"
label = "Light"
kind = "device"
device_class = "light"

[[inputs]]
name = "setting"
label = "Setting"
kind = "attribute"
device = "lamp"
schema_type = "boolean"
writable = true
```

- Input names are DSL variable names. The optional label appears above the picker. Every input must be selected before a draft can be created.
- A `device` input can require a `device_class`. Use the same `distinct_group` on several device inputs to require different devices.
- An `attribute` input references a named device input through `device`. An optional `schema_type` matches a single type declared directly in the attribute's JSON Schema. Set `writable = true` to require a setter.
- The source uses ordinary DSL, including optional `rule.concurrency` or `rule.timeout` settings. Bindings become local assignments after those settings. Device inputs become stable-ID references and attribute inputs become strings.

**Upload template** saves the file under `templates/` in the persistent web rules directory. The console can download the original file and remove an uploaded template. Use a distinct ID to keep two versions or remove and upload again to replace one. Published rules keep their existing source when a template is removed or replaced. [Web rules setup](operator-web.md#rules)
