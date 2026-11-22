import { createSignal, Show } from "solid-js";
import { attributeLabel, type Attribute, type Json, type SetRequest, type SetReceipt } from "./api";
import { Button, jsonText } from "./components";
import { Argument } from "./rule-form";

export function AttributeControl(props: {
  descriptor: Attribute; service: string; device: string; name: string;
  value?: Json; revision?: number; send: (request: SetRequest) => Promise<SetReceipt>; refresh: () => void;
}) {
  const [input, setInput] = createSignal<{ value: Json | undefined }>();
  const [request, setRequest] = createSignal<{ value: Json; revision: number; receipt: SetReceipt }>();
  const [busy, setBusy] = createSignal(false);
  const [problem, setProblem] = createSignal("");
  const desired = () => input() ? input()?.value : props.value;
  const pending = () => {
    const sent = request();
    return sent && (props.revision ?? 0) <= sent.revision ? sent : undefined;
  };
  const label = () => props.descriptor.setter?.label || attributeLabel(props.name, props.descriptor);
  async function write(value: Json | undefined) {
    if (value === undefined || busy()) return;
    const revision = props.revision ?? 0;
    setBusy(true); setProblem("");
    try {
      const receipt = await props.send({ service_id: props.service, device_id: props.device, attribute: props.name, value });
      setRequest({ value, revision, receipt });
      setInput(undefined);
      props.refresh();
    } catch (error) { setProblem((error as Error).message); }
    finally { setBusy(false); }
  }
  return <div class="attribute-control">
    <Show when={props.descriptor.schema.type === "boolean" && typeof props.value === "boolean"} fallback={
      <div class="toolbar"><Argument label={props.descriptor.setter?.label || "Requested value"} schema={props.descriptor.schema}
        required value={desired()} change={value => setInput({ value })} slider={props.descriptor.unit === "%"} />
        <Button primary disabled={busy() || desired() === undefined} onClick={() => void write(desired())}>Set value</Button>
      </div>
    }>
      <button class="button" role="switch" aria-label={label()} aria-checked={props.value === true ? "true" : "false"}
        disabled={busy()} onClick={() => void write(props.value !== true)}>{props.value === true ? "On" : "Off"}</button>
    </Show>
    <Show when={pending()}>{sent => <p role="status">Requested {jsonText(sent().value, false)}. Waiting for a device report.
      {" "}<a href={"/ui/trace?correlation=" + encodeURIComponent(sent().receipt.correlation_id)}>View trace</a></p>}</Show>
    <Show when={problem()}><p role="alert" class="notice">{problem()}</p></Show>
  </div>;
}
