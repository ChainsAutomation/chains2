import { createEffect, createMemo, createSignal, For, onCleanup, Show } from "solid-js";
import { type Activity, type Connection, type Service, type Trace } from "./api";
import { Badge, Button, Empty, formatTime, Heading, jsonText, type Navigate, options, Picker, RecordDetails } from "./components";

const hidden = new Set(["service.heartbeat", "state.committed", "state.reported", "state.head",
  "state.query.requested", "state.query.responded", "state.snapshot.requested", "state.snapshot.responded"]);
const rowsPerPage = 150;
const subject = (record: Activity) => record.rule
  ? [record.engine_id, record.rule].filter(Boolean).join(" / ")
  : [record.service_id || record.source || record.engine_id, record.device_id].filter(Boolean).join(" / ");
const filters = [["Event type", "type"], ["Service", "service"], ["Device", "device"],
  ["Engine", "engine"], ["Rule", "rule"], ["Correlation ID", "correlation"]] as const;
type FilterKey = typeof filters[number][1];

export function WatchView(props: { services: Service[]; records: Activity[]; status: Connection; navigate: Navigate }) {
  const [selected, setSelected] = createSignal<Activity>();
  const [pinned, setPinned] = createSignal<Activity[]>();
  const [lowLevel, setLowLevel] = createSignal(false);
  const [search, setSearch] = createSignal("");
  const [page, setPage] = createSignal(0);
  const [filter, setFilter] = createSignal<Record<FilterKey, string>>({ type: "", service: "", device: "", engine: "", rule: "", correlation: "" });
  const choices = createMemo(() => {
    const known = props.services.find(s => s.uuid === filter().service);
    const records = props.records;
    return {
      type: options([...records.map(r => r.type), ...props.services.flatMap(s => Object.keys(s.events ?? {})), "action.*", "rule.*", "state.*", "service.*", "manual.*"]),
      service: [...props.services.map(s => ({ value: s.uuid, label: s.name })), ...options(records.map(r => r.service_id || r.source)).filter(x => !props.services.some(s => s.uuid === x.value))],
      device: options(known ? (known.devices ?? []).map(d => d.id) : [...props.services.flatMap(s => (s.devices ?? []).map(d => d.id)), ...records.map(r => r.device_id)]),
      engine: options(records.map(r => r.engine_id)), rule: options(records.map(r => r.rule)), correlation: options(records.map(r => r.correlation_id)),
    };
  });
  const matched = createMemo(() => (pinned() ?? props.records).filter(r => {
    if (r.type === "watch.gap" || r.type === "watch.error") return true;
    const f = filter();
    if (!f.type && !lowLevel() && hidden.has(r.type)) return false;
    if (f.type && !f.type.split(",").map(x => x.trim()).some(x => x.endsWith("*") ? r.type.startsWith(x.slice(0, -1)) : r.type === x)) return false;
    if (f.service && f.service !== r.service_id && f.service !== r.source) return false;
    if (f.device && f.device !== r.device_id || f.engine && f.engine !== r.engine_id ||
      f.rule && f.rule !== r.rule || f.correlation && f.correlation !== r.correlation_id) return false;
    return [r.type, r.source, subject(r), r.summary, jsonText(r.data, false)].join(" ").toLowerCase().includes(search().toLowerCase().trim());
  }).reverse());
  const visible = () => matched().slice(page() * rowsPerPage, (page() + 1) * rowsPerPage);
  function toggleFollow() { if (pinned()) { setPinned(undefined); setPage(0); } else setPinned(props.records); }
  return <>
    <Heading title="Watch" subtitle="Follow events, rule runs, and actions across Chains.">
      <Button onClick={toggleFollow}>{pinned() ? "Resume live" : "Pause"}</Button>
    </Heading>
    <section class="filters panel watch-filters" aria-label="Activity filters">
      <For each={filters}>{([label, field]) =>
        <Picker id={"watch-" + field} label={label} value={filter()[field]} choices={choices()[field]}
          onValue={value => { setFilter({ ...filter(), [field]: value, ...(field === "service" ? { device: "" } : {}) }); setPage(0); }} />
      }</For>
      <label class="field"><span>Search</span><input type="search" value={search()} placeholder="Type, subject, or payload" onInput={e => { setSearch(e.currentTarget.value); setPage(0); }} /></label>
      <label class="check"><input type="checkbox" checked={lowLevel()} onChange={event => { setLowLevel(event.currentTarget.checked); setPage(0); }} /> Include low-level traffic</label>
    </section>
    <div class={selected() ? "inspect-layout" : ""}>
      <section class="panel data-panel">
        <div class="panel-title"><h2>Activity</h2><span class="subtle">{pinned() ? "Paused" : props.status} · {matched().length} matching records</span></div>
        <Show when={visible().length} fallback={<Empty title="No matching activity">New records appear here as they reach the authority.</Empty>}>
          <div class="table-scroll"><table><thead><tr><th>Time</th><th>Type</th><th>Subject</th><th>Details</th></tr></thead><tbody>
            <For each={visible()}>{record => <tr class={(record === selected() ? "chosen " : "") + (record.type.startsWith("watch.") ? "gap" : "")}>
              <td class="mono nowrap">{formatTime(record.time, true)}</td>
              <td><button class="row-link" aria-pressed={record === selected() ? "true" : "false"} aria-label={`Inspect ${record.type}`} onClick={() => setSelected(record)}><Badge type={record.type} /></button></td>
              <td>{subject(record) || "—"}</td>
              <td><span class="sample">{record.summary ?? jsonText(record.data, false)}</span></td>
            </tr>}</For>
          </tbody></table></div>
        </Show>
        <Show when={matched().length > rowsPerPage}><div class="pager">
          <Button disabled={!page()} onClick={() => setPage(page() - 1)}>Newer</Button>
          <Button disabled={(page() + 1) * rowsPerPage >= matched().length} onClick={() => { if (!pinned()) setPinned(props.records); setPage(page() + 1); }}>Older</Button>
        </div></Show>
      </section>
      <Show when={selected()}>{record => <RecordDetails record={record()} navigate={props.navigate} onClose={() => setSelected(undefined)} />}</Show>
    </div>
  </>;
}

export function TraceView(props: {
  correlation: string; recent: Activity[]; navigate: Navigate;
  load: (id: string, signal: AbortSignal) => Promise<Trace>;
}) {
  const [input, setInput] = createSignal(props.correlation);
  const [trace, setTrace] = createSignal<Trace>();
  const [active, setActive] = createSignal<Activity>();
  const [problem, setProblem] = createSignal("");
  const [loading, setLoading] = createSignal(false);
  const [refresh, setRefresh] = createSignal(0);
  let request: AbortController | undefined;
  function load(id: string) {
    request?.abort();
    setInput(id); setActive(undefined); setTrace(undefined); setProblem(""); setLoading(!!id);
    if (!id) return;
    const abort = new AbortController();
    request = abort;
    void props.load(id, abort.signal).then(result => {
      if (!abort.signal.aborted) { setTrace(result); setLoading(false); }
    }).catch((error: Error) => {
      if (!abort.signal.aborted) { setProblem(error.message); setLoading(false); }
    });
  }
  createEffect(() => [props.correlation, refresh()] as const, ([id]) => load(id));
  onCleanup(() => request?.abort());
  const ids = createMemo(() => options(props.recent.map(r => r.correlation_id)));
  const records = createMemo(() => {
    const combined = new Map(trace()?.records.map(record => [record.id, record]));
    for (const record of props.recent) {
      if (record.correlation_id !== props.correlation || !record.id) continue;
      combined.set(record.id, record);
    }
    return [...combined.values()];
  });
  const eventParents = createMemo(() => {
    const result = new Map<string, Activity>();
    for (const record of records()) if (record.event_id && !record.run_id) result.set(record.event_id, record);
    return result;
  });
  return <>
    <Heading title="Trace Explorer" subtitle="Follow a single event from its origin to its effects.">
      <Button disabled={!props.correlation || loading()} onClick={() => setRefresh(refresh() + 1)}>Refresh trace</Button>
    </Heading>
    <form class="panel filters trace-search" onSubmit={event => { event.preventDefault(); const id = input().trim(); if (id === props.correlation) setRefresh(refresh() + 1); else props.navigate("trace", { correlation: id }); }}>
      <Picker id="trace-correlation" label="Correlation ID" value={input()} choices={ids()} placeholder="Select or paste a correlation ID" onValue={setInput} />
      <Button primary type="submit" disabled={!input().trim()}>Find trace</Button>
    </form>
    <Show when={problem()}><p class="notice" role="alert">{problem()}</p></Show>
    <div class={active() ? "inspect-layout" : ""}>
      <section class="panel data-panel">
        <div class="panel-title"><h2>Timeline</h2><span class="subtle">{records().length} records</span></div>
        <Show when={trace()?.oldest_at}><p class="history-note">Oldest retained activity · {formatTime(trace()?.oldest_at)}. Earlier records or unreported steps may be missing.</p></Show>
        <Show when={records().length} fallback={<Empty title={loading() ? "Loading trace" : props.correlation ? "No retained records" : "Find a trace to begin"}>Use a correlation ID from Watch or an event receipt.</Empty>}>
          <ol class="timeline"><For each={records()}>{record => <li class={record.type.split(".")[0]}>
            <div class="timeline-main">
              <time>{formatTime(record.time, true)}</time>
              <button class="row-link" aria-label={`Inspect trace step ${record.type}`} onClick={() => setActive(record)}><Badge type={record.type} /></button>
              <span>{subject(record)}</span>
            </div>
            <Show when={record.summary}><p>{record.summary}</p></Show>
            <Show when={record.run_id}><small class="mono">Run {record.run_id}</small></Show>
            <Show when={record.causation_id}>
              {id => <Show when={eventParents().get(id())} fallback={<small>Parent {id()} unavailable</small>}>
                {parent => <button class="quiet" onClick={() => setActive(parent())}>Parent · {parent().type}</button>}
              </Show>}
            </Show>
          </li>}</For></ol>
        </Show>
      </section>
      <Show when={active()}>{record => <RecordDetails record={record()} navigate={props.navigate} onClose={() => setActive(undefined)} />}</Show>
    </div>
  </>;
}
