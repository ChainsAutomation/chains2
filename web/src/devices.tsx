import { createMemo, createSignal, For, Show } from "solid-js";
import { jsonBody, type DeviceEntry } from "./api";
import { DeviceCard, type CardActions } from "./home";
import { Button, Empty, Heading, options, Picker } from "./components";

type Scope = { path?: string[]; pattern?: string; unassigned?: boolean; tags: string[]; deviceClass: string };
type LocationNode = { name: string; path: string[]; children: Map<string, LocationNode> };
const key = (item: DeviceEntry) => item.service_id + "/" + item.device.id;
const locationKey = (path?: string[]) => JSON.stringify(path ?? []);
const splitLines = (value: string) => value.split("\n").map(part => part.trim()).filter(Boolean);

function matchLocation(pattern: string[], location: string[]) {
  let p = 0, l = 0, star = -1, retry = 0;
  while (l < location.length) {
    if (pattern[p] === "**") { star = p++; retry = l; }
    else if (pattern[p] === "*" || pattern[p] === location[l]) { p++; l++; }
    else if (star >= 0) { p = star + 1; l = ++retry; }
    else return false;
  }
  while (pattern[p] === "**") p++;
  return p === pattern.length;
}

function treeFor(entries: DeviceEntry[]) {
  const roots = new Map<string, LocationNode>();
  for (const { device } of entries) {
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

export function DevicesView(props: { entries?: DeviceEntry[]; operator: boolean; actions: CardActions; refreshCatalog: () => void; api: <T>(path: string, init?: RequestInit) => Promise<T> }) {
  const [search, setSearch] = createSignal("");
  const [scope, setScope] = createSignal<Scope>({ tags: [], deviceClass: "" });
  const [pattern, setPattern] = createSignal("");
  const [tags, setTags] = createSignal("");
  const [deviceClass, setDeviceClass] = createSignal("");
  const [editing, setEditing] = createSignal("");
  const [placementLocation, setPlacementLocation] = createSignal("");
  const [placementTags, setPlacementTags] = createSignal("");
  const [initial, setInitial] = createSignal<{ location: string; tags: string }>();
  const [saving, setSaving] = createSignal(false);
  const [editError, setEditError] = createSignal("");
  const tree = createMemo(() => treeFor(props.entries ?? []));
  const catalogDevices = () => (props.entries ?? []).map(entry => entry.device);
  const visible = createMemo(() => {
    const { pattern, path, unassigned, tags, deviceClass } = scope();
    const parts = pattern ? pattern.split(".") : path ? [...path, "**"] : undefined;
    return (props.entries ?? []).filter(({ device, service_name }) => {
      const location = device.location ?? [];
      return (!unassigned || !location.length) && (!parts || !!location.length && matchLocation(parts, location)) &&
        (!deviceClass || device.classes.includes(deviceClass)) && tags.every(tag => device.tags?.includes(tag)) &&
        [device.name, device.id, service_name, ...location, ...(device.tags ?? [])].join(" ").toLowerCase().includes(search().trim().toLowerCase());
    });
  });
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

  function choose(path?: string[], unassigned = false) {
    setPattern("");
    setScope({ ...scope(), path, unassigned, pattern: undefined });
  }
  function apply() {
    const location = pattern().trim();
    setScope({ pattern: location || undefined, path: location ? undefined : scope().path,
      unassigned: location ? undefined : scope().unassigned, tags: [...new Set(tags().split(",").map(t => t.trim()).filter(Boolean))], deviceClass: deviceClass() });
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
    } catch (error) { setEditError((error as Error).message); }
    finally { setSaving(false); }
  }

  return <>
    <Heading title="All Devices"><Button onClick={props.refreshCatalog}>Refresh</Button></Heading>
    <form class="filters panel" onSubmit={event => { event.preventDefault(); apply(); }}>
      <label class="field"><span>Search devices</span><input type="search" value={search()} onInput={e => setSearch(e.currentTarget.value)} /></label>
      <label class="field"><span>Location pattern</span><input value={pattern()} onInput={e => setPattern(e.currentTarget.value)} placeholder="**.Bedroom.**" /></label>
      <Picker id="devices-tags" label="Tags" value={tags()} onValue={setTags} choices={options(catalogDevices().flatMap(device => device.tags ?? []))} placeholder="Tag or comma-separated tags" />
      <Picker id="devices-class" label="Device class" value={deviceClass()} onValue={setDeviceClass} choices={options(catalogDevices().flatMap(device => device.classes))} />
      <Button type="submit">Apply filters</Button>
    </form>
    <div class="state-layout">
      <aside class="panel tree device-locations">
        <div class="panel-title"><h2>Locations</h2></div>
        <button class="tree-link" aria-pressed={!scope().path && !scope().pattern && !scope().unassigned ? "true" : "false"} onClick={() => choose()}>All locations</button>
        <LocationTree nodes={tree()} active={scope().path} select={choose} />
        <button class="tree-link" aria-pressed={scope().unassigned ? "true" : "false"} onClick={() => choose(undefined, true)}>Unassigned</button>
      </aside>
      <div class="device-groups">
        <Show when={visible().length} fallback={<Empty title={props.entries ? "No matching devices" : "Loading devices…"} />}>
          <For each={[...groups().keys()]}>{group => {
            const path = JSON.parse(group) as string[];
            return <section>
              <h2>{path.join(" › ") || "Unassigned"}</h2>
              <div class="widget-grid">
                <For each={groups().get(group) ?? []}>{id => {
                  const entry = () => byID().get(id)!;
                  return <DeviceCard entry={entry()} actions={props.actions} edit={props.operator ? () => edit(entry()) : undefined}>
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
                  </DeviceCard>;
                }}</For>
              </div>
            </section>;
          }}</For>
        </Show>
      </div>
    </div>
  </>;
}
