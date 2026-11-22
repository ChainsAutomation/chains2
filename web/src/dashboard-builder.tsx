import { createEffect, createSignal, For, onCleanup, Show } from "solid-js";
import { HTTPError, jsonBody, type DeviceEntry } from "./api";
import { Button, Empty, Heading, options, Picker } from "./components";
import { DashboardGrid, deviceKey, lines, type API, type Block, type Board, type Catalog, type Layout } from "./home";

const key = () => crypto.randomUUID();
const groupReady = (b: Block) => !!(b.location?.length || b.tags?.length || b.device_class);

function DevicePicker(props: { devices: DeviceEntry[]; used: string[]; add: (entry: DeviceEntry) => void; close: () => void }) {
  const [search, setSearch] = createSignal("");
  const [selected, setSelected] = createSignal("");
  const groups = () => {
    const result = new Map<string, { name: string; devices: DeviceEntry[] }>();
    const devices = props.devices.filter(d => !props.used.includes(deviceKey(d)) &&
      [d.device.name, d.device.id, d.service_name, ...(d.device.location ?? [])].join(" ").toLowerCase().includes(search().trim().toLowerCase()));
    devices.sort((a, b) => a.service_name.localeCompare(b.service_name) || a.device.name.localeCompare(b.device.name));
    for (const entry of devices) {
      const group = result.get(entry.service_id) ?? { name: entry.service_name, devices: [] };
      group.devices.push(entry); result.set(entry.service_id, group);
    }
    return [...result.values()];
  };
  return <section class="builder-block" aria-label="Choose a device">
    <label class="field"><span>Search devices</span><input type="search" value={search()} onInput={e => { setSearch(e.currentTarget.value); setSelected(""); }} /></label>
    <label class="field"><span>Device</span><select value={selected()} onChange={e => setSelected(e.currentTarget.value)}>
      <option value="">Choose a device</option>
      <For each={groups()}>{group => <optgroup label={group.name}>
        <For each={group.devices}>{entry => <option value={deviceKey(entry)}>{entry.device.name || entry.device.id}</option>}</For>
      </optgroup>}</For>
    </select></label>
    <div class="toolbar"><Button primary disabled={!selected()} onClick={() => {
      const entry = props.devices.find(d => deviceKey(d) === selected());
      if (entry) props.add(entry);
    }}>Add to dashboard</Button><Button onClick={props.close}>Cancel</Button></div>
  </section>;
}

export function DashboardBuilder(props: {
  id: string; api: API; devices: DeviceEntry[]; saved: (id: string) => void; close: () => void;
  guard: (check: (() => boolean) | undefined) => void;
}) {
  const [draft, setDraft] = createSignal<Board>();
  const [base, setBase] = createSignal<Catalog>();
  const [asHome, setAsHome] = createSignal(false);
  const [dirty, setDirty] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const [previewError, setPreviewError] = createSignal("");
  const [preview, setPreview] = createSignal<Layout>();
  const [picking, setPicking] = createSignal(false);
  const lifetime = new AbortController();
  const valid = () => !!draft()?.name.trim() && draft()!.blocks.every(b => b.kind !== "group" || groupReady(b));
  const beforeUnload = (event: BeforeUnloadEvent) => { if (dirty()) { event.preventDefault(); event.returnValue = ""; } };
  props.guard(() => !dirty() || window.confirm("Discard unsaved dashboard changes?"));
  window.addEventListener("beforeunload", beforeUnload);
  onCleanup(() => { lifetime.abort(); props.guard(undefined); window.removeEventListener("beforeunload", beforeUnload); });

  async function load() {
    setBusy(true); setError("");
    try {
      const saved = await props.api<Catalog>("/dashboard/config", { signal: lifetime.signal });
      setBase(saved);
      const board = props.id ? saved.dashboards.find(d => d.id === props.id) : { id: key(), name: "", pinned: false, blocks: [] };
      setDraft(board); setAsHome(props.id ? saved.home_id === props.id : !saved.home_id); setDirty(false);
    } catch (err) { if (!lifetime.signal.aborted) setError((err as Error).message); }
    finally { setBusy(false); }
  }
  void load();
  function edit(change: Partial<Board>) { setDraft({ ...draft()!, ...change }); setDirty(true); }
  function add(block: Omit<Block, "id">) { edit({ blocks: [...draft()!.blocks, { ...block, id: key() }] }); }
  function patch(id: string, fields: Partial<Block>) { edit({ blocks: draft()!.blocks.map(b => b.id === id ? { ...b, ...fields } : b) }); }
  function move(from: number, offset: number) {
    const blocks = [...draft()!.blocks];
    [blocks[from], blocks[from + offset]] = [blocks[from + offset], blocks[from]];
    edit({ blocks });
  }

  createEffect(() => draft(), (board) => {
    if (!board) return;
    const controller = new AbortController();
    const timer = window.setTimeout(async () => {
      try {
        const layout = await props.api<Layout>("/dashboard/preview", {
          ...jsonBody("POST", { dashboard: { ...board, name: board.name.trim() || "Untitled", blocks: board.blocks.filter(b => b.kind !== "group" || groupReady(b)) } }),
          signal: controller.signal,
        });
        if (!controller.signal.aborted) { setPreview(layout); setPreviewError(""); }
      } catch (err) { if (!controller.signal.aborted) setPreviewError((err as Error).message); }
    }, 200);
    onCleanup(() => { clearTimeout(timer); controller.abort(); });
  });

  async function save(remove = false) {
    if (busy() || !base() || !draft() || (!remove && !valid())) return;
    const previous = base()!, board = draft()!;
    if (remove && !window.confirm("Delete this dashboard?")) return;
    const home_id = !remove && asHome() ? board.id : previous.home_id === board.id ? "" : previous.home_id;
    const next = { ...previous, home_id, dashboards: remove ? previous.dashboards.filter(d => d.id !== board.id) :
      props.id ? previous.dashboards.map(d => d.id === board.id ? { ...board, name: board.name.trim() } : d) :
        [...previous.dashboards, { ...board, name: board.name.trim() }] };
    setBusy(true); setError("");
    try {
      await props.api<Catalog>("/dashboard/config", { ...jsonBody("PUT", next), signal: lifetime.signal });
      setDirty(false);
      props.saved(remove ? "" : board.id);
    } catch (err) {
      if (!lifetime.signal.aborted) setError(err instanceof HTTPError && err.status === 409
        ? "Someone saved dashboard changes while you were editing. Your draft is still here." : (err as Error).message);
    } finally { setBusy(false); }
  }

  const locations = () => [...new Set(props.devices.flatMap(d => (d.device.location ?? []).map((_, i, path) => JSON.stringify(path.slice(0, i + 1)))))].sort();
  return <>
    <Heading title={props.id ? "Edit dashboard" : "Create dashboard"}>
      <Button onClick={props.close}>Cancel</Button>
      <Button primary disabled={!valid() || busy() || (!!props.id && !dirty())} onClick={() => void save()}>{busy() && draft() ? "Saving…" : "Save dashboard"}</Button>
    </Heading>
    <Show when={error()}><div role="alert" class="notice">{error()} <Show when={dirty()}><Button disabled={busy()} onClick={() => {
      if (window.confirm("Discard your draft and load the saved dashboard?")) void load();
    }}>Reload saved version</Button></Show></div></Show>
    <Show when={draft()} fallback={<Empty title={busy() ? "Loading dashboard…" : error() ? "Dashboard could not be loaded" : "Dashboard unavailable"} />}>
      {board => <div class="dashboard-workspace">
        <fieldset class="panel builder-editor" disabled={busy()}>
          <label class="field"><span>Dashboard name</span><input maxlength="100" placeholder="e.g. Downstairs" value={board().name} onInput={e => edit({ name: e.currentTarget.value })} /></label>
          <div class="toolbar">
            <label><input type="checkbox" checked={asHome()} onChange={e => { setAsHome(e.currentTarget.checked); setDirty(true); }} /> Use as home</label>
            <label><input type="checkbox" checked={board().pinned} onChange={e => edit({ pinned: e.currentTarget.checked })} /> Pin in navigation</label>
          </div>
          <div class="toolbar">
            <Button onClick={() => setPicking(true)}>Add device</Button>
            <Button onClick={() => add({ kind: "group" })}>Add live group</Button>
            <Button onClick={() => add({ kind: "separator" })}>Add separator</Button>
          </div>
          <Show when={picking()}><DevicePicker devices={props.devices} used={board().blocks.filter(b => b.kind === "device").map(b => b.service_id + "/" + b.device_id)}
            add={entry => { add({ kind: "device", service_id: entry.service_id, device_id: entry.device.id }); setPicking(false); }} close={() => setPicking(false)} /></Show>
          <For each={board().blocks.map(b => b.id)}>{(id, index) => {
            const block = () => board().blocks[index()];
            const device = () => props.devices.find(d => d.service_id === block().service_id && d.device.id === block().device_id);
            return <section class="builder-block">
              <div class="panel-title"><h3>{block().kind === "device" ? (device() ? device()!.device.name + " · " + device()!.service_name : "Unavailable device") :
                block().kind === "group" ? "Live group" : "Separator"}</h3>
                <div class="toolbar">
                  <Button aria-label={"Move block " + (index() + 1) + " up"} disabled={index() === 0} onClick={() => move(index(), -1)}>↑</Button>
                  <Button aria-label={"Move block " + (index() + 1) + " down"} disabled={index() === board().blocks.length - 1} onClick={() => move(index(), 1)}>↓</Button>
                  <Button aria-label={"Remove block " + (index() + 1)} onClick={() => edit({ blocks: board().blocks.filter(b => b.id !== id) })}>Remove</Button>
                </div>
              </div>
              <Show when={block().kind !== "device"}><label class="field"><span>Heading (optional)</span>
                <input maxlength="100" value={block().title ?? ""} onInput={e => patch(id, { title: e.currentTarget.value })} /></label></Show>
              <Show when={block().kind === "group"}>
                <label class="field"><span>Location</span><select onChange={e => patch(id, { location: e.currentTarget.value ? [...JSON.parse(e.currentTarget.value), "**"] : [] })}>
                  <option value="">Any location</option>
                  <For each={locations()}>{location => <option value={location} selected={JSON.stringify(block().location ?? []) === JSON.stringify([...JSON.parse(location), "**"])}>{JSON.parse(location).join(" › ")}</option>}</For>
                </select></label>
                <Picker id={"group-class-" + id} label="Device class" value={block().device_class ?? ""} onValue={v => patch(id, { device_class: v })}
                  choices={options(props.devices.flatMap(d => d.device.classes))} />
                <label class="field"><span>Tags (match all)</span>
                  <select value="" onChange={e => {
                    const tag = e.currentTarget.value;
                    if (tag) patch(id, { tags: [...(block().tags ?? []), tag] });
                    e.currentTarget.value = "";
                  }}><option value="">Add a tag</option>
                    <For each={options(props.devices.flatMap(d => d.device.tags ?? [])).map(t => t.value).filter(t => !block().tags?.includes(t))}>{tag =>
                      <option value={tag}>{tag}</option>}</For>
                  </select>
                </label>
                <Show when={block().tags?.length}><div class="toolbar"><For each={block().tags}>{tag =>
                  <Button aria-label={"Remove tag " + tag} onClick={() => patch(id, { tags: block().tags?.filter(t => t !== tag) })}>{tag} ×</Button>
                }</For></div></Show>
                <details><summary>Advanced location pattern</summary>
                  <label class="field"><span>Location pattern segments, one per line</span><textarea rows="3" value={(block().location ?? []).join("\n")}
                    onChange={e => patch(id, { location: lines(e.currentTarget.value) })} /></label>
                </details>
                <small>{groupReady(block()) ? (preview()?.blocks.find(b => b.id === id)?.devices.length ?? 0) + " devices in group" : "Choose at least one filter"}</small>
              </Show>
            </section>;
          }}</For>
          <Show when={props.id}><Button onClick={() => void save(true)}>Delete dashboard</Button></Show>
        </fieldset>
        <section class="dashboard-canvas" aria-label="Live preview">
          <h2>Live preview</h2>
          <Show when={previewError()}><p class="notice" role="alert">{previewError()}</p></Show>
          <Show when={preview()?.blocks.length} fallback={<Empty title="Add a widget to start" />}>
            <DashboardGrid layout={preview()!} devices={props.devices} preview />
          </Show>
        </section>
      </div>}
    </Show>
  </>;
}
