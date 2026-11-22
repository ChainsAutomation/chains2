import { createMemo, createSignal, For, Match, Show, Switch } from "solid-js";
import type { JSX } from "@solidjs/web";
import { attributeLabel, type DeviceEntry, type Json, type SetRequest } from "./api";
import { Button, Empty, Heading, jsonText } from "./components";
import { Argument } from "./argument";

export type API = <T>(path: string, init?: RequestInit) => Promise<T>;
export type Block = { id: string; kind: "device" | "group" | "separator"; title?: string;
  service_id?: string; device_id?: string; location?: string[]; tags?: string[]; device_class?: string };
export type Board = { id: string; name: string; pinned: boolean; blocks: Block[] };
export type Catalog = { version: 1; revision: number; home_id: string; dashboards: Board[] };
export type Layout = Omit<Board, "blocks"> & { blocks: { id: string; kind: Block["kind"]; title?: string; devices: string[]; missing?: boolean }[] };
export type Home = { home_id: string; dashboards: Layout[]; devices: DeviceEntry[] };
export type Control = { status: "pending" | "completed" | "submitted" | "failed" | "timed_out"; revision: number; until: number; desired: Json; message?: string };
export type CardActions = { send: (request: SetRequest) => void; outcome: (request: Omit<SetRequest, "value">) => Control | undefined };
export const deviceKey = (entry: DeviceEntry) => entry.service_id + "/" + entry.device.id;
export const controlKey = (request: Omit<SetRequest, "value">) => [request.service_id, request.device_id, request.attribute].join("/");
export const lines = (value: string) => value.split("\n").map(v => v.trim()).filter(Boolean);

const receiverProfile = { icon: "receiver", primary: ["power", "input", "volume", "muted"] };

const cardProfiles = [
  { class: "dimmer", icon: "bulb", primary: ["power", "dimlevel"] },
  { class: "light", icon: "bulb", primary: ["power"] },
  { class: "toggle", icon: "power", primary: ["power"] },
];

function DeviceIcon(props: { icon: string }) {
  return <svg aria-hidden="true" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">
    <Switch fallback={<path d="M7 7h10v10H7zM4 9h3m-3 6h3m10-6h3m-3 6h3M9 4v3m6-3v3M9 17v3m6-3v3" />}>
      <Match when={props.icon === "bulb"}><path d="M9 18h6m-5 3h4m1-6c0-2 3-3 3-7a6 6 0 1 0-12 0c0 4 3 5 3 7z" /></Match>
      <Match when={props.icon === "receiver"}><path d="M3 6h18v12H3zM6 10h7v4H6zM6 20h2m8 0h2" /><circle cx="17" cy="12" r="2" /></Match>
      <Match when={props.icon === "power"}><path d="M12 3v8M6.3 6.3a8 8 0 1 0 11.4 0" /></Match>
    </Switch>
  </svg>;
}

function CardAttribute(props: { entry: DeviceEntry; name: string; actions?: CardActions }) {
  const [value, setValue] = createSignal<{ value: Json | undefined }>();
  const attr = () => props.entry.device.attributes![props.name];
  const reported = () => props.entry.state[props.name];
  const label = () => attributeLabel(props.name, attr());
  const address = () => ({ service_id: props.entry.service_id, device_id: props.entry.device.id, attribute: props.name });
  const outcome = () => props.actions?.outcome(address());
  const awaiting = () => {
    const result = outcome();
    return !!result && (result.status === "pending" ||
      ((result.status === "completed" || result.status === "submitted") &&
        (reported()?.revision ?? 0) <= result.revision && result.until > 0));
  };
  const status = () => {
    switch (outcome()?.status) {
      case "pending": return "Sending request…";
      case "failed": return outcome()?.message ?? "Device request failed.";
      case "timed_out": return "Device did not confirm the request.";
      case "completed":
      case "submitted":
        if ((reported()?.revision ?? 0) > (outcome()?.revision ?? 0)) {
          return jsonText(reported()?.value, false) === jsonText(outcome()?.desired, false) ? "" : "Device reported a different value.";
        }
        return awaiting() ? "Waiting for a device report…" : "Device has not reported a new value.";
      default: return "";
    }
  };
  const desired = () => value() ? value()?.value : reported()?.value;
  function send(next: Json | undefined) {
    if (next === undefined || awaiting()) return;
    props.actions?.send({ ...address(), value: next });
    setValue(undefined);
  }
  return <div class="widget-attribute">
    <div class="widget-reading"><span>{label()}</span><strong title={reported()?.observed_at}>
      {reported() ? typeof reported().value === "boolean" ? (reported().value ? "On" : "Off") : jsonText(reported().value, false) + (attr().unit ? " " + attr().unit : "") : "Unreported"}
    </strong></div>
    <Show when={props.actions && attr().setter}>
      <Show when={attr().schema.type === "boolean"} fallback={
        <div class="widget-control"><Argument label={attr().setter?.label || label()} schema={attr().schema} required
          value={desired()} change={next => setValue({ value: next })} slider={attr().unit === "%"} />
          <Button disabled={awaiting() || desired() === undefined} onClick={() => send(desired())}>Set</Button></div>
      }>
        <Show when={typeof reported()?.value === "boolean"} fallback={
          <div class="widget-control"><Button disabled={awaiting()} onClick={() => send(true)}>Turn on</Button><Button disabled={awaiting()} onClick={() => send(false)}>Turn off</Button></div>
        }>
          <button type="button" class="button widget-switch" role="switch" aria-checked={reported()?.value === true ? "true" : "false"}
            aria-label={label()} disabled={awaiting()} onClick={() => send(reported()?.value !== true)}>{reported()?.value ? "Turn off" : "Turn on"}</button>
        </Show>
      </Show>
    </Show>
    <Show when={status()}><small role="status">{status()}</small></Show>
  </div>;
}

export function DeviceCard(props: { entry: DeviceEntry; actions?: CardActions; edit?: () => void; children?: JSX.Element }) {
  const isReceiver = () => props.entry.service_type === "onkyo" && !!props.entry.device.attributes?.power;
  const profile = () => isReceiver() ? receiverProfile : cardProfiles.find(p => props.entry.device.classes.includes(p.class));
  const names = () => Object.keys(props.entry.device.attributes ?? {}).sort();
  const primary = () => profile()?.primary.filter(name => names().includes(name)) ??
    names().filter(name => ["boolean", "number", "integer", "string"].includes(String(props.entry.device.attributes![name].schema.type))).slice(0, 2);
  const secondary = () => names().filter(name => !primary().includes(name));
  const name = () => props.entry.device.name || props.entry.device.id;
  return <article class="panel widget-card" aria-label={name()}>
    <header class="widget-header"><span class={profile() && props.entry.state.power?.value === true ? "widget-icon widget-active" : "widget-icon"}><DeviceIcon icon={profile()?.icon ?? "device"} /></span>
      <div><h3>{name()}</h3>
        <small>{props.entry.service_name}{isReceiver() ? " · AV receiver" : ""}</small>
        <Show when={props.entry.device.location?.length}><small class="widget-location">{props.entry.device.location?.join(" / ")}</small></Show>
      </div></header>
    <For each={primary()}>{attribute => <CardAttribute entry={props.entry} name={attribute} actions={props.actions} />}</For>
    <Show when={secondary().length}><details><summary>More details</summary>
      <For each={secondary()}>{attribute => <CardAttribute entry={props.entry} name={attribute} actions={props.actions} />}</For>
    </details></Show>
    <Show when={props.edit}><Button onClick={() => props.edit?.()}>Edit placement</Button></Show>
    {props.children}
  </article>;
}

export function DashboardGrid(props: { layout: Layout; devices: DeviceEntry[]; actions?: CardActions; preview?: boolean }) {
  const entries = createMemo(() => new Map(props.devices.map(d => [deviceKey(d), d])));
  return <div class="widget-grid">
    <For each={props.layout.blocks.map(block => block.id)}>{(_id, index) => {
      const block = () => props.layout.blocks[index()];
      return <>
        <Show when={block().kind === "separator"}><div class="widget-separator">{block().title ? <h2>{block().title}</h2> : <hr />}</div></Show>
        <Show when={block().kind === "group" && block().title && (block().devices.length || props.preview)}><h2 class="widget-separator">{block().title}</h2></Show>
        <For each={block().devices}>{address => <Show when={entries().get(address)}>{entry => <DeviceCard entry={entry()} actions={props.actions} />}</Show>}</For>
        <Show when={props.preview && block().missing}><p class="notice">Device is unavailable.</p></Show>
        <Show when={props.preview && block().kind === "group" && !block().devices.length}><p class="notice">No matching devices.</p></Show>
      </>;
    }}</For>
  </div>;
}

export function DashboardView(props: { id: string; home?: Home; actions: CardActions; operator: boolean }) {
  const board = () => props.home?.dashboards.find(b => b.id === (props.id || props.home?.home_id));
  return <Show when={props.home} fallback={<Empty title="Loading dashboards…" />}>
    <Show when={board()} fallback={<Empty title={props.id ? "Dashboard unavailable" : "Home"}>
      <div class="toolbar"><a class="button" href="/devices">Browse devices</a>
        <Show when={props.operator}><a class="button" href="/admin">Open dashboard builder</a></Show></div>
    </Empty>}>
      {layout => <><Heading title={layout().name} /><Show when={layout().blocks.some(b => b.devices.length || b.kind === "separator")} fallback={<Empty title="No cards yet" />}>
        <DashboardGrid layout={layout()} devices={props.home?.devices ?? []} actions={props.actions} />
      </Show></>}
    </Show>
  </Show>;
}
