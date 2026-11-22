import { createSignal, For, onCleanup, Show } from "solid-js";
import { jsonBody, type DeviceEntry } from "./api";
import { Button, Empty, Heading, options, Picker } from "./components";
import { DashboardGrid, deviceKey, lines, type API, type Block, type Board, type Catalog, type Layout } from "./home";

const key = () => crypto.randomUUID();
const makeBlock = (kind: Block["kind"]): Block => ({ id: key(), kind });
function move<T>(items: T[], index: number, offset: number) {
  const next = [...items];
  [next[index], next[index + offset]] = [next[index + offset], next[index]];
  return next;
}

export function DashboardBuilder(props: { api: API; devices: DeviceEntry[]; refresh: () => void }) {
  const [draft, setDraft] = createSignal<Catalog>();
  const [selected, setSelected] = createSignal("");
  const [dirty, setDirty] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const [household, setHousehold] = createSignal(false);
  const [preview, setPreview] = createSignal<Layout>();
  const [confirmDelete, setConfirmDelete] = createSignal(false);
  let previewRequest: AbortController | undefined;
  const lifetime = new AbortController();
  onCleanup(() => { lifetime.abort(); previewRequest?.abort(); });
  const board = () => draft()?.dashboards.find(d => d.id === selected());

  async function load() {
    setBusy(true); setError("");
    try {
      const saved = await props.api<Catalog>("/home/config", { signal: lifetime.signal });
      setDraft(saved); setDirty(false); setPreview(undefined);
      if (!saved.dashboards.some(d => d.id === selected())) setSelected(saved.dashboards[0]?.id ?? "");
    } catch (err) { if (!lifetime.signal.aborted) setError((err as Error).message); }
    finally { setBusy(false); }
  }
  void load();
  function change(next: Catalog) {
    previewRequest?.abort();
    setDraft(next); setDirty(true); setPreview(undefined);
  }
  function edit(next: Board) {
    const current = draft()!;
    change({ ...current, dashboards: current.dashboards.map(d => d.id === next.id ? next : d) });
  }
  function add() {
    const current = draft()!;
    const next: Board = { id: key(), name: "New dashboard", pinned: false, blocks: [] };
    change({ ...current, dashboards: [...current.dashboards, next] }); choose(next.id);
  }
  function choose(id: string) {
    previewRequest?.abort(); setSelected(id); setConfirmDelete(false); setPreview(undefined);
  }
  function remove() {
    const current = draft()!;
    const dashboards = current.dashboards.filter(d => d.id !== selected());
    change({ ...current, dashboards, home_id: current.home_id === selected() ? "" : current.home_id });
    choose(dashboards[0]?.id ?? "");
  }
  async function save() {
    setBusy(true); setError("");
    try {
      const saved = await props.api<Catalog>("/home/config", { ...jsonBody("PUT", draft()), signal: lifetime.signal });
      setDraft(saved); setDirty(false); props.refresh();
    } catch (err) { if (!lifetime.signal.aborted) setError((err as Error).message); }
    finally { setBusy(false); }
  }
  async function showPreview() {
    previewRequest?.abort();
    const controller = new AbortController();
    previewRequest = controller;
    setError("");
    try {
      const rendered = await props.api<Layout>("/home/preview", {
        ...jsonBody("POST", { dashboard: board(), household: household() }), signal: controller.signal,
      });
      if (!controller.signal.aborted) setPreview(rendered);
    } catch (err) { if (!controller.signal.aborted) setError((err as Error).message); }
  }

  return <><Heading title="Dashboard builder">
    <Button primary disabled={!dirty() || busy()} onClick={() => void save()}>Save dashboards</Button>
  </Heading>
  <Show when={error()}><p role="alert" class="notice">{error()}</p></Show>
  <Show when={draft()} fallback={<Empty title={busy() ? "Loading dashboards…" : "Dashboard catalog unavailable"}>
    <Button disabled={busy()} onClick={() => void load()}>Retry</Button></Empty>}>
    {catalog => <div class="builder-layout">
      <aside class="panel builder-pages"><h2>Dashboards</h2>
        <For each={catalog().dashboards.map(d => d.id)}>{(id, index) => {
          const item = () => catalog().dashboards[index()];
          return <div class="builder-page">
            <button class="tree-link" aria-pressed={selected() === id ? "true" : "false"} onClick={() => choose(id)}>{item().name || "Untitled"}</button>
            <Button aria-label={"Move " + item().name + " up"} disabled={busy() || index() === 0}
              onClick={() => change({ ...catalog(), dashboards: move(catalog().dashboards, index(), -1) })}>↑</Button>
            <Button aria-label={"Move " + item().name + " down"} disabled={busy() || index() === catalog().dashboards.length - 1}
              onClick={() => change({ ...catalog(), dashboards: move(catalog().dashboards, index(), 1) })}>↓</Button>
          </div>;
        }}</For>
        <Button disabled={busy()} onClick={add}>New dashboard</Button>
        <Show when={dirty()}><Button disabled={busy()} onClick={() => {
          if (window.confirm("Discard unsaved changes and reload the dashboard catalog?")) void load();
        }}>Discard and reload</Button></Show>
      </aside>
      <Show when={board()} fallback={<Empty title="Choose or create a dashboard" />}>{current =>
        <fieldset class="panel builder-editor" disabled={busy()}>
          <label class="field"><span>Dashboard name</span><input maxlength="100" value={current().name}
            onInput={e => edit({ ...current(), name: e.currentTarget.value })} /></label>
          <div class="toolbar">
            <label><input type="checkbox" checked={current().pinned} onChange={e => edit({ ...current(), pinned: e.currentTarget.checked })} /> Pin in navigation</label>
            <label><input type="checkbox" checked={catalog().home_id === selected()}
              onChange={e => change({ ...catalog(), home_id: e.currentTarget.checked ? current().id : "" })} /> Use as home</label>
            <Show when={confirmDelete()} fallback={<Button onClick={() => setConfirmDelete(true)}>Delete dashboard</Button>}>
              <Button onClick={remove}>Confirm delete</Button><Button onClick={() => setConfirmDelete(false)}>Cancel</Button>
            </Show>
          </div>
          <For each={current().blocks.map(b => b.id)}>{(id, index) => {
            const item = () => current().blocks[index()];
            const patch = (fields: Partial<Block>) => edit({ ...current(), blocks: current().blocks.map(b => b.id === id ? { ...b, ...fields } : b) });
            return <section class="builder-block">
              <div class="panel-title"><h3>{item().kind === "device" ? "Device card" : item().kind === "group" ? "Live group" : "Separator"}</h3>
                <div class="toolbar">
                  <Button aria-label={"Move block " + (index() + 1) + " up"} disabled={index() === 0} onClick={() => edit({ ...current(), blocks: move(current().blocks, index(), -1) })}>↑</Button>
                  <Button aria-label={"Move block " + (index() + 1) + " down"} disabled={index() === current().blocks.length - 1} onClick={() => edit({ ...current(), blocks: move(current().blocks, index(), 1) })}>↓</Button>
                  <Button aria-label={"Remove block " + (index() + 1)} onClick={() => edit({ ...current(), blocks: current().blocks.filter(b => b.id !== id) })}>Remove</Button>
                </div></div>
              <Show when={item().kind === "device"} fallback={
                <label class="field"><span>Heading (optional)</span><input value={item().title ?? ""} onInput={e => patch({ title: e.currentTarget.value })} /></label>
              }>
                <label class="field"><span>Device</span><select value={(item().service_id ?? "") + "/" + (item().device_id ?? "")} onChange={e => {
                  const d = props.devices.find(d => deviceKey(d) === e.currentTarget.value);
                  patch({ service_id: d?.service_id, device_id: d?.device.id });
                }}>
                  <option value="/">Choose a device</option>
                  <Show when={item().device_id && !props.devices.some(d => d.service_id === item().service_id && d.device.id === item().device_id)}>
                    <option value={item().service_id + "/" + item().device_id}>Unavailable device ({item().device_id})</option>
                  </Show>
                  <For each={props.devices.map(deviceKey)}>{address => {
                    const d = () => props.devices.find(d => deviceKey(d) === address)!;
                    return <option value={address}>{d().device.name} · {d().service_name}</option>;
                  }}</For>
                </select></label>
              </Show>
              <Show when={item().kind === "group"}>
                <label class="field"><span>Location pattern segments, one per line</span><textarea rows="3" placeholder={"House\nBedroom\n**"}
                  value={(item().location ?? []).join("\n")} onChange={e => patch({ location: lines(e.currentTarget.value) })} /></label>
                <Picker id={"group-class-" + id} label="Device class" value={item().device_class ?? ""} onValue={v => patch({ device_class: v })}
                  choices={options(props.devices.flatMap(d => d.device.classes))} />
                <label class="field"><span>Required tags, one per line</span><textarea rows="2" value={(item().tags ?? []).join("\n")}
                  onChange={e => patch({ tags: lines(e.currentTarget.value) })} /></label>
              </Show>
            </section>;
          }}</For>
          <div class="toolbar"><For each={["device", "group", "separator"] as const}>{kind =>
            <Button onClick={() => edit({ ...current(), blocks: [...current().blocks, makeBlock(kind)] })}>
              {kind === "device" ? "Add device" : kind === "group" ? "Add live group" : "Add separator"}
            </Button>}</For></div>
          <div class="toolbar">
            <label><input type="checkbox" checked={household()} onChange={e => {
              previewRequest?.abort(); setHousehold(e.currentTarget.checked); setPreview(undefined);
            }} /> Preview as household</label>
            <Button onClick={() => void showPreview()}>Preview dashboard</Button>
          </div>
          <Show when={preview()}>{layout => <DashboardGrid layout={layout()} devices={props.devices} preview />}</Show>
        </fieldset>
      }</Show>
    </div>}
  </Show></>;
}
