export type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
export interface Schema {
  type?: string | string[];
  title?: string;
  description?: string;
  enum?: Json[];
  const?: Json;
  properties?: Record<string, Schema>;
  items?: Schema;
  required?: string[];
  additionalProperties?: boolean;
  default?: Json;
  [keyword: string]: unknown;
}
export interface Attribute { schema: Schema; label?: string; description?: string; unit?: string;
  setter?: { action: string; schema_version: string; argument: string; label?: string } }
export const attributeLabel = (id: string, attr?: Attribute) => attr?.label || id.replaceAll("_", " ");
export interface Device { id: string; name: string; classes: string[]; location?: string[]; tags?: string[]; description?: string; attributes?: Record<string, Attribute>; actions?: string[] }
export type Reading = { value: Json; observed_at: string; changed_at: string; revision: number };
export type DeviceEntry = { service_id: string; service_name: string; service_type: string; device: Device; state: Record<string, Reading> };
export type DeviceList = { revision: number; devices: DeviceEntry[] };
export type SetRequest = { service_id: string; device_id: string; attribute: string; value: Json };
export type SetReceipt = { action_id: string; correlation_id: string; status: "sent" };
export interface Service {
  uuid: string; name: string; description?: string; service_type: string; devices?: Device[];
  events?: Record<string, Record<string, Schema>>; actions?: Record<string, Record<string, Schema>>;
}
export interface StateRow {
  kind: string; service_id: string; service_name: string; device_id?: string; device_name?: string;
  attribute?: string; value?: Json; observed_at?: string; changed_at?: string; revision?: number;
}
export interface Snapshot { epoch: string; revision: number; rows: StateRow[] }
export interface Profile {
  name: string; source: string; type: string; device_id?: string; schema_version: string; schema?: Schema; ready: boolean;
}
export interface Activity {
  id?: string; time: string; type: string; source?: string; service_id?: string; device_id?: string; device_ids?: string[];
  target?: string; engine_id?: string; rule?: string; run_id?: string; event_id?: string; action_id?: string;
  causation_id?: string; correlation_id?: string; summary?: string; data?: Json; truncated?: boolean;
}
export interface Trace { records: Activity[]; oldest_id?: string; oldest_at?: string }
export interface Receipt { id: string; type: string; source: string }
export type Connection = "Connecting" | "Live" | "Reconnecting" | "Disconnected";

export class HTTPError extends Error {
  constructor(readonly status: number, message: string) { super(message); }
}
const base = "/operator/v1";
export function requestAt(prefix: string) {
  return async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
    const response = await fetch(prefix + path, { cache: "no-store", ...options });
    if (!response.ok) throw new HTTPError(response.status, (await response.text()).trim() || response.statusText);
    return (response.status === 204 ? undefined : await response.json()) as T;
  };
}
export const request = requestAt(base);
export function query(path: string, fields: Record<string, string>) {
  return path + "?" + new URLSearchParams(Object.entries(fields).filter(([, value]) => value));
}
export function jsonBody(method: string, data: unknown): RequestInit {
  return { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) };
}
export const HISTORY = 2048;

export async function watch(signal: AbortSignal, onBatch: (records: Activity[]) => void,
                            onStatus: (status: Connection) => void, onUnauthorized: () => void) {
  let cursor = "";
  let delay = 500;
  while (!signal.aborted) {
    try {
      const response = await fetch(base + "/activity?recent=150", {
        headers: cursor ? { "Last-Event-ID": cursor } : {}, cache: "no-store", signal,
      });
      if (response.status === 401) { onUnauthorized(); return; }
      if (!response.ok || !response.body || !response.headers.get("Content-Type")?.startsWith("text/event-stream"))
        throw new Error("Activity stream unavailable");
      onStatus("Live");
      delay = 500;
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let pending = "";
      try {
        for (;;) {
          const chunk = await reader.read();
          if (chunk.done) break;
          pending += decoder.decode(chunk.value, { stream: true });
          if (pending.length > 2 * 1024 * 1024) throw new Error("Activity frame exceeds limit");
          const batch: Activity[] = [];
          for (;;) {
            const boundary = /\r?\n\r?\n/.exec(pending);
            if (!boundary) break;
            const lines = pending.slice(0, boundary.index).split(/\r?\n/);
            pending = pending.slice(boundary.index + boundary[0].length);
            const id = lines.find(line => line.startsWith("id:"))?.slice(3).trim();
            const data = lines.filter(line => line.startsWith("data:")).map(line => line.slice(5).trimStart()).join("\n");
            if (data) batch.push(JSON.parse(data) as Activity);
            if (id) cursor = id;
          }
          if (batch.length) onBatch(batch);
        }
      } finally {
        void reader.cancel().catch(() => {});
      }
    } catch {
      if (signal.aborted) return;
    }
    if (signal.aborted) return;
    onStatus("Reconnecting");
    await new Promise<void>(resolve => {
      const done = () => { clearTimeout(timer); signal.removeEventListener("abort", done); resolve(); };
      const timer = window.setTimeout(done, delay);
      signal.addEventListener("abort", done, { once: true });
    });
    delay = Math.min(10000, delay * 2);
  }
}
