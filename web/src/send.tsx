import { createMemo, createSignal, For, Show } from "solid-js";
import type { Json, Profile, Receipt, Service, Snapshot } from "./api";
import { Button, Empty, Heading, jsonText, type Navigate, type Option, options, Picker } from "./components";

export interface Draft {
  mode?: "fields" | "json";
  raw?: string;
  value?: Json;
  name?: string;
}
type Schema = NonNullable<Profile["schema"]>;
type Field = { key: string; schema: Schema; required: boolean };
type Submission = { name?: string; profile?: string; data: Json };
const scalar = (schema: Schema) => schema.enum?.length || schema.const !== undefined ||
  ["string", "boolean", "integer", "number"].includes(String(schema.type));
function fields(schema?: Schema): Field[] | undefined {
  if (!schema) return;
  const properties = schema.properties ?? {};
  if (schema.type === "object" && Object.keys(properties).length && Object.values(properties).every(scalar))
    return Object.entries(properties).map(([key, info]) => ({ key, schema: info, required: !!schema.required?.includes(key) }));
  if (scalar(schema)) return [{ key: "", schema, required: true }];
}
function initial(schema?: Schema): Json | undefined {
  if (!schema) return;
  if (schema.default !== undefined) return schema.default;
  if (schema.const !== undefined) return schema.const;
  if (schema.type !== "object") return;
  return Object.fromEntries(Object.entries(schema.properties ?? {}).filter(([, p]) => p.default !== undefined || p.const !== undefined)
    .map(([key, p]) => [key, p.default !== undefined ? p.default : p.const!]));
}
function conforms(schema: Schema, value: Json) {
  if (schema.enum) return schema.enum.some(item => JSON.stringify(item) === JSON.stringify(value));
  if (schema.const !== undefined) return JSON.stringify(schema.const) === JSON.stringify(value);
  switch (schema.type) {
    case "integer": return typeof value === "number" && Number.isInteger(value);
    case "number": return typeof value === "number" && Number.isFinite(value);
    case "string": case "boolean": return typeof value === schema.type;
    default: return true;
  }
}

export function SendView(props: {
  profiles: Profile[]; services: Service[]; drafts: Record<string, Draft>; change: (key: string, value: Draft) => void;
  initialProfile: string; selectProfile: (profile: string) => void; navigate: Navigate;
  loadState: () => Promise<Snapshot>; submit: (data: Submission) => Promise<Receipt>;
}) {
  const [service, setService] = createSignal("");
  const [device, setDevice] = createSignal("");
  const [type, setType] = createSignal("");
  const [problem, setProblem] = createSignal("");
  const [preview, setPreview] = createSignal<Submission>();
  const [receipt, setReceipt] = createSignal<Receipt>();
  const [pending, setPending] = createSignal(false);
  const [state, setState] = createSignal<Snapshot>();
  const [statePath, setStatePath] = createSignal("");
  const [loadingState, setLoadingState] = createSignal(false);
  const [fieldTarget, setFieldTarget] = createSignal("");
  const profile = createMemo(() => props.profiles.find(p => p.name === props.initialProfile));
  const fieldList = createMemo(() => fields(profile()?.schema));
  const draft = () => props.drafts[props.initialProfile] ?? {};
  const mode = () => draft().mode ?? (fieldList() ? "fields" : "json");
  const value = () => "value" in draft() ? draft().value : initial(profile()?.schema);
  const raw = () => draft().raw ?? jsonText(value() === undefined ? {} : value(), true);
  function edit(changes: Partial<Draft>) {
    props.change(props.initialProfile, { ...draft(), ...changes });
    setPreview(undefined); setProblem(""); setReceipt(undefined);
  }
  function choose(name: string) { props.selectProfile(name); setFieldTarget(""); setPreview(undefined); setProblem(""); setReceipt(undefined); }
  function getField(key: string) {
    const data = value();
    return key && data && typeof data === "object" && !Array.isArray(data) ? data[key] : key ? undefined : data;
  }
  function setField(key: string, field: Json | undefined) {
    if (key === "") { edit({ value: field, raw: undefined }); return; }
    const data = value();
    const next = data && typeof data === "object" && !Array.isArray(data) ? { ...data } : {};
    if (field === undefined) delete next[key]; else Object.defineProperty(next, key, { value: field, enumerable: true, writable: true, configurable: true });
    edit({ value: next, raw: undefined });
  }
  function fieldChoices(key: string): Option[] {
    const item = profile();
    const services = props.services;
    const parentId = getField("service_id");
    const parents = typeof parentId === "string" ? services.filter(s => s.uuid === parentId) : services;
    const devices = parents.flatMap(s => s.devices ?? []);
    switch (key) {
      case "service_id": return services.map(s => ({ value: s.uuid, label: s.name }));
      case "service_type": return options(services.map(s => s.service_type));
      case "device_id": return options(devices.map(d => d.id));
      case "device_class": return options(devices.map(d => d.class));
      case "attribute": return options(devices.filter(d => !item?.device_id || d.id === item.device_id).flatMap(d => d.attributes ?? []));
      default: return [];
    }
  }
  const visible = () => props.profiles.filter(p => (!service() || p.source === service()) &&
    (!device() || p.device_id === device()) && (!type() || p.type === type()));
  const stateRows = () => (state()?.rows ?? []).filter(r => r.kind === "attribute" && r.value !== undefined);
  const path = (row: Snapshot["rows"][number]) => [row.service_id, row.device_id, row.attribute].join("/");
  async function loadState() {
    setLoadingState(true);
    try { setState(await props.loadState()); }
    catch (error) { setProblem((error as Error).message); }
    finally { setLoadingState(false); }
  }
  function insertState() {
    const row = stateRows().find(r => path(r) === statePath());
    if (!row || row.value === undefined) return;
    if (mode() === "json") { edit({ raw: jsonText(row.value) }); return; }
    const field = fieldList()?.find(f => f.key === fieldTarget());
    if (!field || !conforms(field.schema, row.value)) { setProblem("Choose a field that accepts this state value."); return; }
    setField(field.key, row.value);
  }
  function changeMode(next: "fields" | "json") {
    if (next === mode()) return;
    if (next === "json") { edit({ mode: next, raw: jsonText(value() === undefined ? {} : value()) }); return; }
    try {
      const parsed = JSON.parse(raw()) as Json;
      if (profile()?.schema?.type === "object" && (!parsed || typeof parsed !== "object" || Array.isArray(parsed)))
        throw new Error("Fields require a JSON object.");
      if (profile()?.schema?.type !== "object" && profile()?.schema && !conforms(profile()!.schema!, parsed))
        throw new Error("The JSON value does not match this field.");
      edit({ mode: next, value: parsed });
    } catch (error) { setProblem((error as Error).message); }
  }
  function prepare() {
    try {
      if (props.initialProfile && !profile()?.ready) throw new Error("This event profile is unavailable.");
      const payload = mode() === "json" ? JSON.parse(raw()) as Json : value();
      if (payload === undefined) throw new Error("Enter a payload.");
      if (mode() === "fields") for (const field of fieldList() ?? []) {
        const data = getField(field.key);
        if (data === undefined && field.required) throw new Error(`Enter ${field.schema.title || field.key || "a value"}.`);
        if (data !== undefined && !conforms(field.schema, data)) throw new Error(`Check ${field.schema.title || field.key || "the value"}.`);
      }
      if (!props.initialProfile) {
        if (!/^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,119}$/.test(draft().name ?? "")) throw new Error("Enter a valid custom event name.");
        if (!payload || typeof payload !== "object" || Array.isArray(payload)) throw new Error("Custom events require a JSON object.");
      }
      setProblem("");
      setPreview(props.initialProfile ? { profile: props.initialProfile, data: payload } : { name: draft().name, data: payload });
    } catch (error) { setProblem((error as Error).message); }
  }
  async function publish() {
    const data = preview();
    if (!data || pending()) return;
    setPending(true); setProblem("");
    try { setReceipt(await props.submit(data)); setPreview(undefined); }
    catch (error) { setProblem((error as Error).message); }
    finally { setPending(false); }
  }
  return <>
    <Heading title="Send event" subtitle="Build and preview a manual event or use a configured profile." />
    <div class="send-layout">
      <form class="panel composer" onSubmit={e => { e.preventDefault(); prepare(); }}>
        <div class="panel-title"><h2>Compose event</h2></div>
        <fieldset disabled={pending()}>
          <div class="inline-grid">
            <Picker id="send-service" label="Profile service" value={service()} choices={props.services.map(s => ({ value: s.uuid, label: s.name }))} onValue={v => { setService(v); setDevice(""); }} />
            <Picker id="send-device" label="Profile device" value={device()} choices={options(props.profiles.filter(p => !service() || p.source === service()).map(p => p.device_id))} onValue={setDevice} />
            <Picker id="send-type" label="Profile event type" value={type()} choices={options(props.profiles.filter(p => !service() || p.source === service()).map(p => p.type))} onValue={setType} />
          </div>
          <label class="field"><span>Event profile</span><select value={props.initialProfile} onChange={e => choose(e.currentTarget.value)}>
            <option value="">Custom manual event</option>
            <For each={visible()}>{p => <option value={p.name}>{p.name}{p.ready ? "" : " (unavailable)"}</option>}</For>
            <Show when={profile() && !visible().includes(profile()!)}><option value={props.initialProfile}>{props.initialProfile}</option></Show>
          </select></label>
          <Show when={profile()} fallback={<label class="field"><span>Event name</span><div class="input-prefix"><span>manual.</span><input value={draft().name ?? ""} placeholder="scene_evening" onInput={e => edit({ name: e.currentTarget.value })} /></div></label>}>
            {item => <p class="profile-summary mono">{item().type} · {item().source}{item().device_id ? "/" + item().device_id : ""} · v{item().schema_version}</p>}
          </Show>
          <div class="editor-head"><h3>Payload</h3><Show when={fieldList()}>
            <div class="segmented"><button type="button" aria-pressed={mode() === "fields" ? "true" : "false"} onClick={() => changeMode("fields")}>Fields</button><button type="button" aria-pressed={mode() === "json" ? "true" : "false"} onClick={() => changeMode("json")}>JSON</button></div>
          </Show></div>
          <Show when={mode() === "fields"} fallback={<label class="field"><span class="sr-only">JSON payload</span><textarea rows={10} value={raw()} spellcheck={false} onInput={e => edit({ raw: e.currentTarget.value })} /></label>}>
            <For each={fieldList() ?? []}>{field => {
              const label = field.schema.title || field.key || "Value";
              const selectValues = field.schema.enum ?? (field.schema.const !== undefined ? [field.schema.const] : field.schema.type === "boolean" ? [true, false] : undefined);
              const choices = () => fieldChoices(field.key);
              return <div class="schema-field">
                <Show when={selectValues} fallback={
                  <Show when={choices().length && field.schema.type === "string"} fallback={
                    <label class="field"><span>{label}{field.required ? " *" : ""}</span>
                      <input type={field.schema.type === "number" || field.schema.type === "integer" ? "number" : "text"} step={field.schema.type === "number" ? "any" : "1"}
                        value={String(getField(field.key) ?? "")} onInput={e => {
                          const input = e.currentTarget.value;
                          setField(field.key, e.currentTarget.type === "number" ? input === "" ? undefined : Number(input) : input);
                        }} />
                    </label>}>
                    <Picker id={"payload-" + field.key} label={label} choices={choices()} value={String(getField(field.key) ?? "")} onValue={v => setField(field.key, v)} />
                  </Show>
                }>{items => <label class="field"><span>{label}{field.required ? " *" : ""}</span><select value={getField(field.key) === undefined ? "" : JSON.stringify(getField(field.key))}
                  onChange={e => setField(field.key, e.currentTarget.value === "" ? undefined : JSON.parse(e.currentTarget.value))}>
                  <option value="">Select a value</option><For each={items()}>{item => <option value={JSON.stringify(item)}>{typeof item === "string" ? item : JSON.stringify(item)}</option>}</For>
                </select></label>}</Show>
                <Show when={field.schema.description}><small>{field.schema.description}</small></Show>
              </div>;
            }}</For>
          </Show>
          <details class="state-insert" onToggle={e => { if (e.currentTarget.open && !state() && !loadingState()) void loadState(); }}>
            <summary>Insert a reported state value</summary>
            <div class="inline-grid">
              <Picker id="insert-state" label="State path" value={statePath()} choices={options(stateRows().map(path))} onValue={setStatePath} />
              <Show when={mode() === "fields" && fieldList()?.some(f => f.key)}><Picker id="insert-target" label="Field" value={fieldTarget()} choices={options(fieldList()?.map(f => f.key) ?? [])} onValue={setFieldTarget} /></Show>
              <Button disabled={!statePath() || loadingState()} onClick={insertState}>Insert value</Button>
            </div>
          </details>
          <Button type="submit" primary disabled={!!props.initialProfile && !profile()?.ready}>Preview event</Button>
        </fieldset>
        <Show when={problem()}><p class="notice" role="alert">{problem()}</p></Show>
      </form>
      <section class="panel data-panel preview">
        <div class="panel-title"><h2>Publication</h2></div>
        <Show when={preview()} fallback={<Show when={receipt()} fallback={<Empty title="Nothing to publish yet">Preview the event to check the payload and destination.</Empty>}>
          {result => <div class="receipt" role="status"><strong>Published</strong><p>{result().type}</p><p class="mono">{result().id}</p><Button onClick={() => props.navigate("trace", { correlation: result().id })}>Explore trace</Button></div>}
        </Show>}>
          {data => <div class="receipt"><strong>{profile()?.type ?? "manual." + data().name}</strong><pre>{jsonText(data().data)}</pre>
            <Button primary onClick={() => void publish()} disabled={pending()}>{pending() ? "Publishing…" : "Publish event"}</Button></div>}
        </Show>
      </section>
    </div>
  </>;
}
