import { createMemo, createSignal, For, onCleanup, Show } from "solid-js";
import { attributeLabel, type Service, type SetRequest, type SetReceipt, type Snapshot, type StateRow } from "./api";
import { AttributeControl } from "./attribute-control";
import { Button, Empty, formatTime, Heading, jsonText, Picker } from "./components";

export function StateView(props: { services: Service[]; load: (signal: AbortSignal) => Promise<Snapshot>; refreshCatalog: () => void; setAttribute: (request: SetRequest) => Promise<SetReceipt> }) {
  const [snapshot, setSnapshot] = createSignal<Snapshot>();
  const [problem, setProblem] = createSignal("");
  const [loading, setLoading] = createSignal(false);
  const [service, setService] = createSignal("");
  const [device, setDevice] = createSignal("");
  const [attribute, setAttribute] = createSignal("");
  const [search, setSearch] = createSignal("");
  const [active, setActive] = createSignal("");
  const [closed, setClosed] = createSignal(new Set<string>());
  const abort = new AbortController();
  async function refresh() {
    if (loading() || abort.signal.aborted) return;
    setLoading(true);
    try {
      setSnapshot(await props.load(abort.signal));
      setProblem("");
    } catch (error) {
      if (!abort.signal.aborted) setProblem(String((error as Error).message));
    } finally { setLoading(false); }
  }
  void refresh();
  const timer = window.setInterval(refresh, 3000);
  onCleanup(() => { abort.abort(); clearInterval(timer); });
  const serviceRecord = createMemo(() => props.services.find(item => item.uuid === service()));
  const deviceRecord = createMemo(() => serviceRecord()?.devices?.find(item => item.id === device()));
  const key = (row: StateRow) => [row.service_id, row.device_id, row.attribute].join("/");
  const rows = createMemo(() => {
    return (snapshot()?.rows ?? []).filter(row => row.kind === "attribute").filter(row =>
      (!service() || row.service_id === service()) && (!device() || row.device_id === device()) &&
      (!attribute() || row.attribute === attribute()) &&
      [key(row), row.service_name, row.device_name, jsonText(row.value, false)].join(" ").toLowerCase().includes(search().toLowerCase().trim()),
    ).sort((a, b) => key(a).localeCompare(key(b)));
  });
  const selected = createMemo(() => rows().find(row => key(row) === active()));
  const metadata = (row: StateRow) => props.services.find(s => s.uuid === row.service_id)?.devices
    ?.find(d => d.id === row.device_id)?.attributes?.[row.attribute ?? ""];
  function select(s: string, d = "") { setService(s); setDevice(d); setAttribute(""); setActive(""); }
  function fold(id: string) {
    const next = new Set(closed());
    if (next.has(id)) next.delete(id); else next.add(id);
    setClosed(next);
  }
  return <>
    <Heading title="State" subtitle="Browse the latest values reported by your devices.">
      <Show when={snapshot()}>{value => <span class="stamp">Revision {value().revision}</span>}</Show>
      <Button disabled={loading()} onClick={() => { void refresh(); props.refreshCatalog(); }}>Refresh</Button>
    </Heading>
    <section class="filters panel" aria-label="State filters">
      <Picker id="state-service" label="Service" value={service()} choices={props.services.map(s => ({ value: s.uuid, label: s.name }))} onValue={select} />
      <Picker id="state-device" label="Device" value={device()} disabled={!service()} choices={(serviceRecord()?.devices ?? []).map(d => ({ value: d.id, label: d.name || d.id }))} onValue={value => select(service(), value)} />
      <Picker id="state-attribute" label="Attribute" value={attribute()} disabled={!device()} choices={Object.entries(deviceRecord()?.attributes ?? {}).map(([id, descriptor]) => ({ value: id, label: attributeLabel(id, descriptor) }))} onValue={setAttribute} />
      <label class="field"><span>Search</span><input type="search" value={search()} placeholder="Names, IDs, or values" onInput={e => setSearch(e.currentTarget.value)} /></label>
    </section>
    <Show when={problem()}><p class="notice" role="status">{problem()}</p></Show>
    <div class="state-layout">
      <aside class="panel tree">
        <div class="panel-title"><h2>Services</h2><span class="subtle">{props.services.length}</span></div>
        <button class="tree-link" aria-pressed={!service() ? "true" : "false"} onClick={() => select("")}>All attributes</button>
        <For each={props.services}>{item => <div class="tree-branch">
          <div class="tree-row">
            <button class="fold" aria-label={`${closed().has(item.uuid) ? "Expand" : "Collapse"} ${item.name}`} aria-expanded={!closed().has(item.uuid) ? "true" : "false"} onClick={() => fold(item.uuid)}>{closed().has(item.uuid) ? "+" : "−"}</button>
            <button class="tree-link" aria-pressed={service() === item.uuid && !device() ? "true" : "false"} onClick={() => select(item.uuid)}>{item.name || item.uuid}<small>{item.service_type}</small></button>
          </div>
          <Show when={!closed().has(item.uuid)}>
            <For each={item.devices ?? []}>{dev => <button class="tree-link nested" aria-pressed={service() === item.uuid && device() === dev.id ? "true" : "false"}
              onClick={() => select(item.uuid, dev.id)}>{dev.name || dev.id}</button>}</For>
          </Show>
        </div>}</For>
      </aside>
      <section class="panel data-panel">
        <div class="panel-title"><h2>{deviceRecord()?.name || serviceRecord()?.name || "Attributes"}</h2><span class="subtle">{rows().length} attributes</span></div>
        <Show when={deviceRecord()?.description ?? serviceRecord()?.description}>{description => <p class="state-description">{description()}</p>}</Show>
        <Show when={rows().length} fallback={<Empty title={loading() && !snapshot() ? "Loading state" : "No matching attributes"}>Try a different service or filter.</Empty>}>
          <div class="table-scroll"><table><thead><tr><th>Attribute</th><th>Value</th><th>Changed</th></tr></thead><tbody>
            <For each={rows()}>{row => <tr class={active() === key(row) ? "chosen" : ""}>
              <td><button class="row-link" aria-pressed={active() === key(row) ? "true" : "false"} onClick={() => setActive(key(row))}>{attributeLabel(row.attribute ?? "",metadata(row))}</button>
                <small>{row.service_name || row.service_id} / {row.device_name || row.device_id}</small></td>
              <td><span class="sample mono" title={jsonText(row.value, false)}>{row.value === undefined ? "Unreported" : jsonText(row.value, false)}</span></td>
              <td class="nowrap">{formatTime(row.changed_at)}</td>
            </tr>}</For>
          </tbody></table></div>
        </Show>
        <Show when={selected()}>{item => <div class="state-detail">
          <div class="panel-title"><h2>{attributeLabel(item().attribute ?? "", metadata(item()))}</h2><button class="quiet" onClick={() => setActive("")}>Close</button></div>
          <Show when={metadata(item())?.description}><p>{metadata(item())?.description}</p></Show>
          <pre>{item().value === undefined ? "No reported value" : jsonText(item().value)}{metadata(item())?.unit ? " " + metadata(item())?.unit : ""}</pre>
          <Show when={metadata(item())?.setter && metadata(item())}>{descriptor =>
            <Show when={key(item())} keyed>{_ => <AttributeControl descriptor={descriptor()} service={item().service_id}
              device={item().device_id ?? ""} name={item().attribute ?? ""} value={item().value} revision={item().revision}
              send={props.setAttribute} refresh={() => void refresh()} />}</Show>
          }</Show>
          <dl><dt>Changed</dt><dd>{formatTime(item().changed_at)}</dd><dt>Observed</dt><dd>{formatTime(item().observed_at)}</dd>
            <dt>Revision</dt><dd>{item().revision ?? "—"}</dd></dl>
        </div>}</Show>
      </section>
    </div>
  </>;
}
