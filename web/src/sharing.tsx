import { createSignal, For, Show } from "solid-js";
import { jsonBody, type DeviceEntry } from "./api";
import { Button } from "./components";
import { deviceKey, type API } from "./home";

export function Sharing(props: { api: API; devices: DeviceEntry[]; refresh: () => void }) {
  const [search, setSearch] = createSignal("");
  const [busy, setBusy] = createSignal("");
  const [error, setError] = createSignal("");
  const filtered = () => props.devices.filter(d => d.service_id !== "chains.operator" &&
    [d.device.name, d.service_name, ...(d.device.location ?? [])].join(" ").toLowerCase().includes(search().trim().toLowerCase()));
  async function toggle(entry: DeviceEntry) {
    const device = [entry.service_id, entry.device.id].map(encodeURIComponent).join("/");
    setBusy(deviceKey(entry)); setError("");
    try {
      await props.api("/devices/" + device + "/sharing", jsonBody("PATCH", { shared: !entry.device.tags?.includes("household") }));
      props.refresh();
    } catch (e) { setError((e as Error).message); }
    finally { setBusy(""); }
  }
  return <section class="panel household-sharing">
    <h2>Device sharing</h2>
    <label class="field"><span>Find devices to share</span><input type="search" value={search()} onInput={e => setSearch(e.currentTarget.value)} /></label>
    <Show when={error()}><p role="alert">{error()}</p></Show>
    <For each={filtered().map(deviceKey)}>{id => {
      const entry = () => props.devices.find(d => deviceKey(d) === id)!;
      return <div class="rule-toolbar">
        <span>{entry().device.name} · {entry().service_name}</span>
        <Button disabled={!!busy()} aria-label={(entry().device.tags?.includes("household") ? "Stop sharing " : "Share ") + entry().device.name}
          onClick={() => void toggle(entry())}>{entry().device.tags?.includes("household") ? "Stop sharing" : "Share"}</Button>
      </div>;
    }}</For>
  </section>;
}
