import { createMemo, For, Show } from "solid-js";
import { attributeLabel, type Json, type Schema, type Service } from "./api";
import { Argument } from "./argument";
import { Button, options, Picker } from "./components";

export type RuleAction = { kind?: "delay"; seconds?: number; service_id: string; device_id: string; name: string; attribute?: string; wait?: string; version?: string; args: Record<string, Json> };
export type Builder = { trigger: "Event" | "Every" | "Time"; interval?: string; selectors: Record<string, Json>; actions: RuleAction[]; mode?: "drop" | "restart"; scope?: "rule" | "device" | "service"; concurrency?: number; timeout?: number };
export const blankAction = (): RuleAction => ({ service_id: "", device_id: "", name: "", args: {} });
export const blankBuilder = (): Builder => ({ trigger: "Event", selectors: {}, actions: [blankAction()] });

export function RuleForm(props: { value: Builder; services: Service[]; change: (value: Builder) => void; disabled: boolean }) {
  const selectors = () => props.value.selectors;
  const put = (key: string, value: Json | undefined) => {
    const next = { ...selectors() };
    if (value === undefined) delete next[key]; else next[key] = value;
    props.change({ ...props.value, selectors: next });
  };
  const pick = (key: string, label: string, values: () => { value: string; label?: string }[]) =>
    <Picker id={"rule-trigger-" + key} label={label} value={String(selectors()[key] ?? "")} choices={values()} onValue={v => put(key, v || undefined)} />;
  const services = () => props.services.filter(s => !selectors().service_id || s.uuid === selectors().service_id);
  const devices = () => services().flatMap(s => s.devices ?? []);
  const actionChange = (index: number, action: RuleAction) => props.change({ ...props.value,
    actions: props.value.actions.map((old, i) => i === index ? action : old) });
  return <fieldset disabled={props.disabled}>
    <label class="field"><span>Trigger</span><select value={props.value.trigger} onChange={e =>
      props.change({ ...props.value, trigger: e.currentTarget.value as Builder["trigger"], selectors: {} })}>
      <option value="Event">Event</option><option value="Every">Every interval</option><option value="Time">Calendar time</option>
    </select></label>
    <Show when={props.value.trigger === "Event"}>
      <div class="rule-fields">
        {pick("type", "Event type", () => [...new Map<string, { value: string; label?: string }>([
          ["device.observed", { value: "device.observed" }], ["state.changed", { value: "state.changed" }],
          ...services().flatMap(s => Object.entries(s.events ?? {}).map(([value, versions]) =>
            [value, { value, label: (versions["1"] ?? Object.values(versions)[0])?.title }] as const)),
        ]).values()])}
        {pick("service_id", "Source service", () => props.services.map(s => ({ value: s.uuid, label: s.name })))}
        {pick("device_id", "Source device", () => devices().map(d => ({ value: d.id, label: d.name })))}
        {pick("device_class", "Device class", () => options(devices().flatMap(d => d.classes)))}
        {pick("tag", "Device tag", () => options(devices().flatMap(d => d.tags ?? [])))}
        <Show when={!Array.isArray(selectors().location)}>
          {pick("location", "Location pattern", () => options(devices().filter(d => d.location?.length && d.location.every(segment => !segment.includes(".")))
            .map(d => d.location!.join("."))))}
        </Show>
        {pick("attribute", "Attribute", () => options(devices().filter(d => !selectors().device_id || d.id === selectors().device_id).flatMap(d => Object.keys(d.attributes ?? {}))))}
      </div>
      <details>
        <summary>More event filters</summary>
        <JSONInput label="Additional selectors as JSON" value={Object.fromEntries(Object.entries(selectors()).filter(([k]) => !eventKeys.has(k) && (k !== "location" || Array.isArray(selectors().location))))}
          onValue={v => {
            const main = Object.fromEntries(Object.entries(selectors()).filter(([k]) => eventKeys.has(k) || (k === "location" && !Array.isArray(selectors().location))));
            props.change({ ...props.value, selectors: { ...main, ...v } });
          }} />
      </details>
    </Show>
    <Show when={props.value.trigger === "Every"}>
      <label class="field"><span>Interval</span><input placeholder="5m" value={props.value.interval ?? ""} onInput={e =>
        props.change({ ...props.value, interval: e.currentTarget.value })} /></label>
    </Show>
    <Show when={props.value.trigger === "Time"}>
      <div class="rule-fields"><For each={["year", "month", "week", "day", "weekday", "hour", "minute", "second"]}>{key =>
        <label class="field"><span>{key}</span><input type="number" placeholder="Default" value={String(selectors()[key] ?? "")}
          onInput={e => put(key, e.currentTarget.value === "" ? undefined : e.currentTarget.valueAsNumber)} /></label>
      }</For></div>
    </Show>
    <div class="rule-fields">
      <label class="field"><span>Run mode</span><select value={props.value.mode ?? "drop"} onChange={e => {
        const next = e.currentTarget.value as "drop" | "restart";
        props.change({ ...props.value, mode: next === "drop" ? undefined : next, concurrency: next === "restart" ? undefined : props.value.concurrency });
      }}>
        <option value="drop">Skip while busy (drop)</option>
        <option value="restart">Cancel active run (restart)</option>
      </select></label>
      <label class="field"><span>Run scope</span><select value={props.value.scope ?? "rule"} onChange={e => {
        const next = e.currentTarget.value as "rule" | "device" | "service";
        props.change({ ...props.value, scope: next === "rule" ? undefined : next });
      }}>
        <option value="rule">Whole rule</option>
        <option value="device">Per device</option>
        <option value="service">Per service</option>
      </select></label>
      <label class="field"><span>Max concurrency</span><input type="number" min="1" max="1024" placeholder="1" disabled={props.value.mode === "restart"} value={props.value.concurrency ?? ""}
        onInput={e => props.change({ ...props.value, concurrency: e.currentTarget.valueAsNumber || undefined })} /></label>
      <label class="field"><span>Timeout in seconds</span><input type="number" min="0" step="any" placeholder="300" value={props.value.timeout ?? ""}
        onInput={e => props.change({ ...props.value, timeout: e.currentTarget.valueAsNumber || undefined })} /></label>
    </div>
    <For each={props.value.actions.map((_, i) => i)}>{index => {
      const step = () => props.value.actions[index];
      const remove = () => props.change({ ...props.value, actions: props.value.actions.filter((_, i) => i !== index) });
      return <Show when={step().kind === "delay"} fallback={
        <ActionFields index={index} action={step()} services={props.services} onChange={action => actionChange(index, action)}
          onRemove={props.value.actions.length > 1 ? remove : undefined} />
      }>
        <section class="rule-action">
          <div class="rule-toolbar"><h3>Delay</h3><Show when={props.value.actions.length > 1}><Button onClick={remove}>Remove delay</Button></Show></div>
          <label class="field"><span>Delay in seconds</span><input type="number" min="0" step="any" required value={step().seconds ?? ""}
            onInput={e => actionChange(index, { ...step(), seconds: e.currentTarget.value === "" ? undefined : e.currentTarget.valueAsNumber })} /></label>
        </section>
      </Show>;
    }}</For>
    <div class="toolbar">
      <Button onClick={() => props.change({ ...props.value, actions: [...props.value.actions, blankAction()] })}>Add action</Button>
      <Button onClick={() => props.change({ ...props.value, actions: [...props.value.actions, { ...blankAction(), kind: "delay" }] })}>Add delay</Button>
    </div>
  </fieldset>;
}

const eventKeys = new Set(["type", "service_id", "device_id", "device_class", "attribute", "tag"]);

function ActionFields(props: { index: number; action: RuleAction; services: Service[]; onChange: (action: RuleAction) => void; onRemove?: () => void }) {
  const a = () => props.action;
  const service = () => props.services.find(s => s.uuid === a().service_id);
  const device = () => service()?.devices?.find(d => d.id === a().device_id);
  const versions = () => service()?.actions?.[a().name] ?? {};
  const attr = () => a().attribute === undefined ? undefined : device()?.attributes?.[a().attribute!];
  const chooseKind = (isSetter: boolean) => props.onChange({ ...a(), name: "", version: undefined, attribute: isSetter ? "" : undefined, args: {} });
  const schema = () => versions()[a().version || "1"];
  const fields = createMemo(() => Object.entries(schema()?.properties ?? {}));
  const set = (key: string, value: Json | undefined) => {
    const args = { ...a().args };
    if (value === undefined) delete args[key]; else Object.defineProperty(args, key, { value, enumerable: true, configurable: true, writable: true });
    props.onChange({ ...a(), args });
  };
  const choice = (key: string, label: string, value: () => string, choices: () => { value: string; label?: string }[], onValue: (v: string) => void) =>
    <Picker id={"rule-action-" + props.index + "-" + key} label={label} value={value()} choices={choices()} placeholder="Select" onValue={onValue} />;
  return <section class="rule-action">
    <div class="rule-toolbar"><h3>Action {props.index + 1}</h3><Show when={props.onRemove}><Button onClick={props.onRemove}>Remove action</Button></Show></div>
    <div class="rule-fields">
      {choice("service", "Target service", () => a().service_id, () => props.services.map(s => ({ value: s.uuid, label: s.name })),
        v => props.onChange({ ...blankAction(), service_id: v, attribute: a().attribute === undefined ? undefined : "", wait: a().wait }))}
      {choice("device", "Target device", () => a().device_id, () => (service()?.devices ?? []).map(d => ({ value: d.id, label: d.name })),
        v => props.onChange({ ...blankAction(), service_id: a().service_id, device_id: v, attribute: a().attribute === undefined ? undefined : "", wait: a().wait }))}
      <label class="field"><span>Step</span><select value={a().attribute === undefined ? "action" : "setter"}
        onChange={e => chooseKind(e.currentTarget.value === "setter")}><option value="action">Named action</option><option value="setter">Set attribute</option></select></label>
      <label class="field"><span>Continue after</span><select value={a().wait ?? "outcome"} onChange={e => props.onChange({ ...a(), wait: e.currentTarget.value })}>
        <option value="outcome">Action outcome</option><option value="sent">Request sent</option>
      </select></label>
      <Show when={a().attribute !== undefined} fallback={
        choice("name", "Action", () => a().name, () => (device()?.actions ?? []).map(value => ({ value, label: service()?.actions?.[value]?.["1"]?.title })), v => {
          const available = service()?.actions?.[v] ?? {};
          const version = available["1"] ? "1" : Object.keys(available).sort()[0];
          props.onChange({ ...a(), name: v, version, args: {} });
        })
      }>
        {choice("attribute", "Writable attribute", () => a().attribute ?? "", () => Object.entries(device()?.attributes ?? {})
          .filter(([, descriptor]) => !!descriptor.setter).map(([value, descriptor]) => ({ value, label: descriptor.setter?.label || attributeLabel(value, descriptor) })),
        v => props.onChange({ ...a(), attribute: v, args: {} }))}
        <Show when={attr()}>{descriptor => <Argument label={descriptor().setter?.label || attributeLabel(a().attribute!, descriptor())}
          schema={descriptor().schema} required value={a().args.value} change={v => set("value", v)} slider={descriptor().unit === "%"} />}</Show>
      </Show>
      <Show when={Object.keys(versions()).length > 1}>
        {choice("version", "Schema version", () => a().version || "1", () => options(Object.keys(versions())), v => props.onChange({ ...a(), version: v, args: {} }))}
      </Show>
      <For each={fields()}>{([key, info]) => <Argument label={info.title ?? key} schema={info}
        required={!!schema()?.required?.includes(key)} value={a().args[key]} change={v => set(key, v)} />}</For>
    </div>
    <Show when={a().name && ((!fields().length && schema()?.additionalProperties !== false) || schema()?.additionalProperties)} >
      <JSONInput label="Action arguments as JSON" value={a().args} onValue={args => props.onChange({ ...a(), args })} />
    </Show>
  </section>;
}

function JSONInput(props: { label: string; value: Record<string, Json>; onValue: (v: Record<string, Json>) => void }) {
  return <label class="field"><span>{props.label}</span><textarea rows="3" value={JSON.stringify(props.value, null, 2)} onChange={e => {
    const field = e.currentTarget;
    try {
      const parsed = JSON.parse(field.value);
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error();
      field.setCustomValidity(""); props.onValue(parsed);
    } catch { field.setCustomValidity("Enter a JSON object."); field.reportValidity(); }
  }} /></label>;
}
