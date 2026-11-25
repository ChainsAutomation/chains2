import { createSignal, For, Show } from "solid-js";
import { attributeLabel, jsonBody, type Service } from "./api";
import { Button } from "./components";

type Input = { name: string; label: string; kind: "device" | "attribute"; device_class?: string;
  distinct_group?: string; device?: string; schema_type?: string; writable?: boolean };
export type RuleTemplate = { id: string; title: string; description?: string; inputs: Input[]; bundled: boolean };
type Choice = { service_id?: string; device_id?: string; attribute?: string };
type Candidate = { value: string; label: string; choice: Choice };

export function RuleTemplates(props: {
  templates: RuleTemplate[];
  services: Service[];
  writable: boolean;
  busy: boolean;
  close: () => void;
  upload: (file: File) => void;
  remove: (id: string) => void;
  generate: (id: string, body: RequestInit, name: string) => void;
}) {
  const [selected, setSelected] = createSignal("");
  const [name, setName] = createSignal("");
  const [choices, setChoices] = createSignal<Record<string, Choice>>({});
  const [confirmRemove, setConfirmRemove] = createSignal("");
  const template = () => props.templates.find(t => t.id === selected());
  const pick = (id: string) => {
    setSelected(id);
    setName(props.templates.find(t => t.id === id)?.title ?? "");
    setChoices({});
    setConfirmRemove("");
  };
  function candidates(input: Input): Candidate[] {
    if (input.kind === "attribute") {
      const selected = input.device ? choices()[input.device] : undefined;
      const device = props.services.find(s => s.uuid === selected?.service_id)?.devices?.find(d => d.id === selected?.device_id);
      return Object.entries(device?.attributes ?? {})
        .filter(([, attr]) => (!input.writable || !!attr.setter) && (!input.schema_type || attr.schema.type === input.schema_type))
        .map(([key, attr]) => ({ value: key, label: attributeLabel(key, attr), choice: { attribute: key } }));
    }
    return props.services.flatMap(s => (s.devices ?? []).flatMap(d => {
      if (input.device_class && !d.classes.includes(input.device_class)) return [];
      const used = input.distinct_group && template()?.inputs.some(other => other.name !== input.name &&
        other.distinct_group === input.distinct_group && choices()[other.name]?.service_id === s.uuid && choices()[other.name]?.device_id === d.id);
      if (used) return [];
      const label = [d.name, s.name, d.location?.join(" / "), s.uuid + "/" + d.id].filter(Boolean).join(" · ");
      return [{ value: JSON.stringify([s.uuid, d.id]), label, choice: { service_id: s.uuid, device_id: d.id } }];
    }));
  }
  const value = (input: Input) => {
    const choice = choices()[input.name];
    if (!choice) return "";
    return input.kind === "device" ? JSON.stringify([choice.service_id, choice.device_id]) : choice.attribute ?? "";
  };
  function choose(input: Input, key: string) {
    const choice = candidates(input).find(item => item.value === key)?.choice;
    setChoices(old => {
      const next = { ...old };
      if (choice) Object.defineProperty(next, input.name, { value: choice, enumerable: true, configurable: true, writable: true });
      else delete next[input.name];
      for (const child of template()?.inputs ?? []) if (child.device === input.name) delete next[child.name];
      return next;
    });
  }

  return <section class="panel composer">
    <div class="panel-title"><h2>Rule templates</h2><Button onClick={props.close} disabled={props.busy}>Close templates</Button></div>
    <div class="toolbar">
      <label class="button rule-upload">Upload template<input type="file" aria-label="Upload template" accept=".toml"
        disabled={props.busy || !props.writable} onChange={e => {
          const file = e.currentTarget.files?.[0];
          if (file) props.upload(file);
          e.currentTarget.value = "";
        }} /></label>
    </div>
    <div class="field"><label for="rule-template">Template</label><select id="rule-template" value={selected()} onChange={e => pick(e.currentTarget.value)} disabled={props.busy}>
      <option value="">Select a template</option>
      <For each={props.templates}>{item => <option value={item.id} selected={item.id === selected()}>{item.title}{item.bundled ? " (bundled)" : ""}</option>}</For>
    </select></div>
    <Show when={template()}>{current =>
      <form onSubmit={e => {
        e.preventDefault();
        const title = name().trim();
        props.generate(current().id, jsonBody("POST", { name: title, choices: choices() }), title);
      }}>
        <p class="subtle">{current().description}</p>
        <div class="toolbar">
          <a class="button" href={"/operator/v1/rules/templates/" + encodeURIComponent(current().id)} download>Download template</a>
          <Show when={!current().bundled && props.writable}>
            <Show when={confirmRemove() === current().id} fallback={<Button disabled={props.busy}
              onClick={() => setConfirmRemove(current().id)}>Remove template</Button>}>
              <Button disabled={props.busy} onClick={() => { props.remove(current().id); setConfirmRemove(""); }}>Confirm removal</Button>
              <Button disabled={props.busy} onClick={() => setConfirmRemove("")}>Keep template</Button>
            </Show>
          </Show>
        </div>
        <fieldset disabled={props.busy || !props.writable}>
          <label class="field"><span>Rule name</span><input required value={name()} onInput={e => setName(e.currentTarget.value)} /></label>
          <For each={current().inputs}>{input =>
            <div class="field"><label for={"template-input-" + input.name}>{input.label}</label>
              <select id={"template-input-" + input.name} required value={value(input)} onChange={e => choose(input, e.currentTarget.value)}>
              <option value="">{candidates(input).length ? "Select" : input.device && !choices()[input.device]?.device_id
                ? "Select the device first" : "No matching choices"}</option>
              <For each={candidates(input)}>{item => <option value={item.value} selected={item.value === value(input)}>{item.label}</option>}</For>
            </select></div>
          }</For>
          <Button type="submit" primary disabled={!name().trim() || current().inputs.some(input => !candidates(input).some(item => item.value === value(input)))}>
            Create draft
          </Button>
        </fieldset>
      </form>
    }</Show>
  </section>;
}
