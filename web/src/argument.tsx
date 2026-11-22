import { For, Show } from "solid-js";
import type { Json, Schema } from "./api";

export function Argument(props: { label: string; schema: Schema; required: boolean; value?: Json; slider?: boolean; change: (v: Json | undefined) => void }) {
  const choices = () => props.schema.enum ?? (props.schema.const !== undefined ? [props.schema.const] : props.schema.type === "boolean" ? [true, false] : undefined);
  const label = () => props.label + (props.required ? " *" : "");
  const scalar = () => ["string", "integer", "number"].includes(String(props.schema.type));
  if (props.slider && typeof props.schema.minimum === "number" && typeof props.schema.maximum === "number") {
    return <label class="field"><span>{label()} ({props.value === undefined ? "choose %" : String(props.value) + "%"})</span>
      <input type="range" min={props.schema.minimum} max={props.schema.maximum}
        value={typeof props.value === "number" ? props.value : props.schema.minimum}
        onInput={e => props.change(e.currentTarget.valueAsNumber)} />
      <input type="number" aria-label={props.label + " percentage"} min={props.schema.minimum} max={props.schema.maximum}
        value={typeof props.value === "number" ? props.value : ""}
        onInput={e => props.change(e.currentTarget.value === "" ? undefined : e.currentTarget.valueAsNumber)} />
    </label>;
  }
  return <Show when={choices()} fallback={<Show when={scalar()} fallback={<label class="field"><span>{label()} as JSON</span>
    <input value={props.value === undefined ? "" : JSON.stringify(props.value)} onChange={e => {
      const field = e.currentTarget;
      try { props.change(field.value === "" ? undefined : JSON.parse(field.value)); field.setCustomValidity(""); }
      catch { field.setCustomValidity("Enter valid JSON."); field.reportValidity(); }
    }} /></label>}>
    <label class="field"><span>{label()}</span><input type={props.schema.type === "string" ? "text" : "number"}
      step={props.schema.type === "integer" ? "1" : "any"} value={String(props.value ?? "")} onInput={e => {
        const field = e.currentTarget;
        props.change(field.value === "" ? undefined : props.schema.type === "string" ? field.value : field.valueAsNumber);
      }} /></label>
  </Show>}>{items => <label class="field"><span>{label()}</span><select value={props.value === undefined ? "" : JSON.stringify(props.value)}
    onChange={e => props.change(e.currentTarget.value === "" ? undefined : JSON.parse(e.currentTarget.value))}>
    <option value="">Select</option><For each={items()}>{item => <option value={JSON.stringify(item)}>{String(item)}</option>}</For>
  </select></label>}</Show>;
}
