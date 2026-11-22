import { For, Show } from "solid-js";
import type { JSX } from "@solidjs/web";
import type { Activity, Json } from "./api";

export type Option = { value: string; label?: string };
export type Navigate = (page: string, data?: Record<string, string>) => void;
export function options(values: (string | undefined)[]): Option[] {
  return [...new Set(values.filter((s): s is string => !!s))].sort().map(value => ({ value }));
}
export function Picker(props: {
  id: string; label: string; value: string; choices: Option[]; placeholder?: string;
  onValue: (value: string) => void; disabled?: boolean;
}) {
  return <label class="field" for={props.id}>
    <span>{props.label}</span>
    <input id={props.id} list={props.id + "-choices"} value={props.value} placeholder={props.placeholder ?? "Any"}
      autocomplete="off" disabled={props.disabled} onInput={event => props.onValue(event.currentTarget.value)} />
    <datalist id={props.id + "-choices"}>
      <For each={props.choices}>{option => <option value={option.value} label={option.label ?? option.value} />}</For>
    </datalist>
  </label>;
}
export function Button(props: { children: JSX.Element; "aria-label"?: string; onClick?: () => void; primary?: boolean; disabled?: boolean; type?: "button" | "submit" }) {
  return <button type={props.type ?? "button"} class={props.primary ? "button primary" : "button"}
    aria-label={props["aria-label"]} onClick={() => props.onClick?.()} disabled={props.disabled}>{props.children}</button>;
}
export function Heading(props: { title: string; subtitle?: string; children?: JSX.Element }) {
  return <header class="heading"><div><h1>{props.title}</h1><Show when={props.subtitle}><p>{props.subtitle}</p></Show></div><div class="toolbar">{props.children}</div></header>;
}
export function Empty(props: { title: string; children?: JSX.Element }) {
  return <div class="empty"><strong>{props.title}</strong><Show when={props.children}><p>{props.children}</p></Show></div>;
}
export function formatTime(value?: string, compact = false) {
  if (!value) return "—";
  const time = new Date(value);
  if (Number.isNaN(time.valueOf())) return value;
  return compact ? time.toLocaleTimeString([], { hour12: false }) : time.toLocaleString();
}
export function jsonText(data: Json | undefined, pretty = true) {
  return data === undefined ? "—" : JSON.stringify(data, null, pretty ? 2 : 0);
}
export function Badge(props: { type: string }) {
  return <span class={`badge ${props.type.split(".")[0]}`}>{props.type}</span>;
}
export function RecordDetails(props: { record: Activity; navigate: Navigate; onClose: () => void }) {
  return <aside class="panel inspector" aria-label="Record details">
    <div class="panel-title"><h2>Record details</h2><button class="quiet" onClick={props.onClose}>Close</button></div>
    <Badge type={props.record.type} />
    <dl>
      <dt>Time</dt><dd>{formatTime(props.record.time)}</dd>
      <For each={([["Source", "source"], ["Service", "service_id"], ["Device", "device_id"], ["Engine", "engine_id"],
        ["Rule", "rule"], ["Run", "run_id"], ["Event", "event_id"], ["Action", "action_id"],
        ["Parent", "causation_id"], ["Correlation", "correlation_id"]] as const)}>
        {([label, key]) => <Show when={props.record[key]}><dt>{label}</dt><dd class="mono">{props.record[key]}</dd></Show>}
      </For>
      <Show when={props.record.device_ids?.length}><dt>Devices</dt><dd class="mono">{props.record.device_ids?.join(", ")}</dd></Show>
    </dl>
    <Show when={props.record.summary}><p>{props.record.summary}</p></Show>
    <Show when={props.record.data !== undefined}><h3>{props.record.truncated ? "Payload preview" : "Payload"}</h3><pre>{jsonText(props.record.data)}</pre></Show>
    <Show when={props.record.correlation_id}><Button onClick={() => props.navigate("trace", { correlation: props.record.correlation_id! })}>Explore trace</Button></Show>
  </aside>;
}
