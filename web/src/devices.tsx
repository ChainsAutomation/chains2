import { createMemo, createSignal, For, onCleanup, Show } from "solid-js";
import { attributeLabel, jsonBody, type DeviceEntry, type DeviceList, type Service, type SetReceipt } from "./api";
import { AttributeControl } from "./attribute-control";
import { Button, Empty, formatTime, Heading, jsonText, options, Picker } from "./components";

type Scope = { path?: string[]; pattern?: string; unassigned?: boolean; tags: string[]; deviceClass: string };
type LocationNode = { name: string; path: string[]; children: Map<string, LocationNode> };
const key = (item: DeviceEntry) => item.service_id + "/" + item.device.id;
const locationKey = (path?: string[]) => JSON.stringify(path ?? []);
const splitLines = (value: string) => value.split("\n").map(part => part.trim()).filter(Boolean);

function treeFor(services: Service[]) {
  const roots = new Map<string, LocationNode>();
  for (const service of services) for (const device of service.devices ?? []) {
    let level = roots;
    const path: string[] = [];
    for (const segment of device.location ?? []) {
      path.push(segment);
      let node = level.get(segment);
      if (!node) {
        node = { name: segment, path: [...path], children: new Map() };
        level.set(segment, node);
      }
      level = node.children;
    }
  }
  return roots;
}

function LocationTree(props: { nodes: Map<string, LocationNode>; active?: string[]; select: (path: string[]) => void }) {
  return <ul><For each={[...props.nodes.keys()].sort()}>{name => {
    const node = () => props.nodes.get(name)!;
    return <li><button class="tree-link" aria-pressed={locationKey(node().path) === locationKey(props.active) ? "true" : "false"}
      onClick={() => props.select(node().path)}>{name}</button>
      <Show when={node().children.size}><LocationTree nodes={node().children} active={props.active} select={props.select} /></Show>
    </li>;
  }}</For></ul>;
}

export function DevicesView(props: { services: Service[]; refreshCatalog: () => void; api: <T>(path: string, init?: RequestInit) => Promise<T> }) {
  const [visible, setVisible] = createSignal<DeviceEntry[]>([]);
  const [scope, setScope] = createSignal<Scope>({ tags: [], deviceClass: "" });
  const [pattern, setPattern] = createSignal("");
  const [tags, setTags] = createSignal("");
  const [deviceClass, setDeviceClass] = createSignal("");
  const [loaded, setLoaded] = createSignal(false);
  const [problem, setProblem] = createSignal("");
  const [editing, setEditing] = createSignal("");
  const [placementLocation, setPlacementLocation] = createSignal("");
  const [placementTags, setPlacementTags] = createSignal("");
  const [initial, setInitial] = createSignal<{ location: string; tags: string }>();
  const [saving, setSaving] = createSignal(false);
  const [editError, setEditError] = createSignal("");
  let request: AbortController | undefined;
  const tree = createMemo(() => treeFor(props.services));
  const catalogDevices = () => props.services.flatMap(service => service.devices ?? []);
  const byID = createMemo(() => new Map(visible().map(entry => [key(entry), entry])));
  const groups = createMemo(() => {
    const grouped = new Map<string, string[]>();
    for (const entry of visible()) {
      const location = locationKey(entry.device.location);
      const group = grouped.get(location) ?? [];
      group.push(key(entry)); grouped.set(location, group);
    }
    return grouped;
  });

  async function refresh() {
    request?.abort();
    const current = new AbortController();
    request = current;
    const filter = scope();
    const search = new URLSearchParams();
    if (filter.pattern) search.set("location", filter.pattern);
    if (filter.path) for (const segment of [...filter.path, "**"]) search.append("location_segment", segment);
    if (filter.unassigned) search.set("unassigned", "true");
    if (filter.deviceClass) search.set("device_class", filter.deviceClass);
    for (const tag of filter.tags) search.append("tag", tag);
    try {
      const matches = await props.api<DeviceList>("/devices" + (search.size ? "?" + search : ""), { signal: current.signal });
      if (current.signal.aborted) return;
      setVisible(matches.devices); setLoaded(true); setProblem("");
    } catch (error) {
      if (!current.signal.aborted) setProblem((error as Error).message);
    }
  }
  void refresh();
  const timer = window.setInterval(refresh, 3000);
  onCleanup(() => { request?.abort(); clearInterval(timer); });
  function choose(path?: string[], unassigned = false) {
    setPattern("");
    setScope({ ...scope(), path, unassigned, pattern: undefined });
    void refresh();
  }
  function apply() {
    const location = pattern().trim();
    setScope({ pattern: location || undefined, path: location ? undefined : scope().path,
      unassigned: location ? undefined : scope().unassigned, tags: [...new Set(tags().split(",").map(t => t.trim()).filter(Boolean))], deviceClass: deviceClass() });
    void refresh();
  }
  function edit(entry: DeviceEntry) {
    setEditing(key(entry));
    const values = { location: (entry.device.location ?? []).join("\n"), tags: (entry.device.tags ?? []).join("\n") };
    setInitial(values);
    setPlacementLocation(values.location);
    setPlacementTags(values.tags);
    setEditError("");
  }
  async function place(entry: DeviceEntry, body: { location?: string[] | null; tags?: string[] | null }) {
    if (saving()) return;
    setSaving(true); setEditError("");
    try {
      await props.api("/devices/" + encodeURIComponent(entry.service_id) + "/" + encodeURIComponent(entry.device.id) + "/placement", jsonBody("PATCH", body));
      setEditing("");
      props.refreshCatalog();
      void refresh();
    } catch (error) { setEditError((error as Error).message); }
    finally { setSaving(false); }
  }

  return <>
    <Heading title="Devices" subtitle="Browse devices by location and control writable attributes."><Button onClick={() => { props.refreshCatalog(); void refresh(); }}>Refresh</Button></Heading>
    <form class="filters panel" onSubmit={event => { event.preventDefault(); apply(); }}>
      <label class="field"><span>Location pattern</span><input value={pattern()} onInput={e => setPattern(e.currentTarget.value)} placeholder="**.Bedroom.**" /></label>
      <Picker id="devices-tags" label="Tags" value={tags()} onValue={setTags} choices={options(catalogDevices().flatMap(device => device.tags ?? []))} placeholder="Tag or comma-separated tags" />
      <Picker id="devices-class" label="Device class" value={deviceClass()} onValue={setDeviceClass} choices={options(catalogDevices().flatMap(device => device.classes))} />
      <Button type="submit">Apply filters</Button>
    </form>
    <Show when={problem()}><p role="alert" class="notice">{problem()}</p></Show>
    <div class="state-layout">
      <aside class="panel tree device-locations">
        <div class="panel-title"><h2>Locations</h2></div>
        <button class="tree-link" aria-pressed={!scope().path && !scope().pattern && !scope().unassigned ? "true" : "false"} onClick={() => choose()}>All locations</button>
        <LocationTree nodes={tree()} active={scope().path} select={choose} />
        <button class="tree-link" aria-pressed={scope().unassigned ? "true" : "false"} onClick={() => choose(undefined, true)}>Unassigned</button>
      </aside>
      <div class="device-groups">
        <Show when={visible().length} fallback={<Empty title={loaded() ? "No matching devices" : "Loading devices…"} />}>
          <For each={[...groups().keys()]}>{group => {
            const path = JSON.parse(group) as string[];
            return <section>
              <h2>{path.join(" › ") || "Unassigned"}</h2>
              <div class="device-cards">
                <For each={groups().get(group) ?? []}>{id => {
                  const entry = () => byID().get(id)!;
                  return <article class="panel device-card" aria-label={entry().device.name || entry().device.id}>
                    <div class="rule-toolbar"><div><h3>{entry().device.name || entry().device.id}</h3><span class="subtle">{entry().service_name}</span></div>
                      <Button onClick={() => edit(entry())}>Edit placement</Button></div>
                    <Show when={entry().device.description}><p>{entry().device.description}</p></Show>
                    <Show when={entry().device.tags?.length}><div class="device-tags"><For each={entry().device.tags}>{tag => <span class="stamp">{tag}</span>}</For></div></Show>
                    <Show when={editing() === id}>
                      <form class="device-placement" onSubmit={event => { event.preventDefault(); const body = {
                        ...(placementLocation() !== initial()?.location ? { location: splitLines(placementLocation()) } : {}),
                        ...(placementTags() !== initial()?.tags ? { tags: splitLines(placementTags()) } : {}),
                      };
                      if (Object.keys(body).length) void place(entry(), body); }}>
                        <label class="field"><span>Location segments</span><textarea rows="3" value={placementLocation()} placeholder={"House\nBedroom\nShelf"}
                          onInput={e => setPlacementLocation(e.currentTarget.value)} /></label>
                        <label class="field"><span>Tags, one per line</span><textarea rows="2" value={placementTags()} onInput={e => setPlacementTags(e.currentTarget.value)} /></label>
                        <Show when={editError()}><p class="notice" role="alert">{editError()}</p></Show>
                        <div class="toolbar"><Button primary type="submit" disabled={saving() || (placementLocation() === initial()?.location && placementTags() === initial()?.tags)}>Save placement</Button>
                          <Button disabled={saving()} onClick={() => void place(entry(), { location: [] })}>Clear location</Button>
                          <Button disabled={saving()} onClick={() => void place(entry(), { tags: [] })}>Clear tags</Button>
                          <Button disabled={saving()} onClick={() => void place(entry(), { location: null })}>Reset location</Button>
                          <Button disabled={saving()} onClick={() => void place(entry(), { tags: null })}>Reset tags</Button>
                          <Button disabled={saving()} onClick={() => setEditing("")}>Cancel</Button>
                        </div>
                      </form>
                    </Show>
                    <For each={Object.keys(entry().device.attributes ?? {}).sort()}>{name => {
                      const attr = () => entry().device.attributes![name];
                      const state = () => entry().state[name];
                      return <section class="device-reading">
                        <div class="rule-toolbar"><strong>{attributeLabel(name, attr())}</strong>
                          <span class="mono">{state() ? jsonText(state().value, false) + (attr().unit ? " " + attr().unit : "") : "Unreported"}</span></div>
                        <Show when={state()}>{reading => <small class="subtle">Observed {formatTime(reading().observed_at)}</small>}</Show>
                        <Show when={attr().setter}><AttributeControl descriptor={attr()} service={entry().service_id} device={entry().device.id} name={name}
                          value={state()?.value} revision={state()?.revision} refresh={() => void refresh()} send={body => props.api<SetReceipt>("/actions", jsonBody("POST", body))} /></Show>
                      </section>;
                    }}</For>
                  </article>;
                }}</For>
              </div>
            </section>;
          }}</For>
        </Show>
      </div>
    </div>
  </>;
}
