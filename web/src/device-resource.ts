import { createSignal } from "solid-js";
import type { SetRequest } from "./api";
import { controlKey, deviceKey, type Control, type Home } from "./home";

export function deviceResource(ready: () => boolean, load: (signal: AbortSignal) => Promise<Home>,
  send: (request: SetRequest, signal: AbortSignal) => Promise<{ status: Control["status"] }>) {
  const [home, setHome] = createSignal<Home>();
  const [problem, setHomeError] = createSignal("");
  const [controls, setControls] = createSignal<Record<string, Control>>({});
  let homeRequest: AbortController | undefined;
  let homeTimer: number | undefined;
  let homeAgain = false;
  let commands: AbortController | undefined;
  const controlTimers = new Set<number>();
  async function reloadHome() {
    if (!ready()) return;
    if (homeRequest) { homeAgain = true; return; }
    const request = new AbortController();
    homeRequest = request;
    try {
      const snapshot = await load(request.signal);
      if (!request.signal.aborted) { setHome(snapshot); setHomeError(""); }
    } catch (error) {
      if (!request.signal.aborted) setHomeError((error as Error).message);
    } finally {
      if (homeRequest === request) {
        homeRequest = undefined;
        if (homeAgain) { homeAgain = false; void reloadHome(); }
      }
    }
  }
  const actions = {
    outcome: (request: Omit<SetRequest, "value">) => controls()[controlKey(request)],
    send: async (request: SetRequest) => {
      const key = controlKey(request);
      const current = controls()[key];
      const revision = home()?.devices.find(d => deviceKey(d) === request.service_id + "/" + request.device_id)?.state[request.attribute]?.revision ?? 0;
      if (current?.status === "pending" || (current?.until && revision <= current.revision)) return;
      const lifetime = commands;
      if (!lifetime || lifetime.signal.aborted) return;
      const update = (status: Control["status"], until = 0, message?: string) =>
        setControls(previous => ({ ...previous, [key]: { status, revision, until, desired: request.value, message } }));
      update("pending");
      try {
        const result = await send(request, lifetime.signal);
        if (lifetime.signal.aborted) return;
        const waits = result.status === "completed" || result.status === "submitted";
        const until = waits ? Date.now() + 10000 : 0;
        update(result.status, until);
        if (waits) {
          const timer = window.setTimeout(() => {
            controlTimers.delete(timer);
            if (controls()[key]?.until === until) update(result.status);
          }, 10000);
          controlTimers.add(timer);
        }
      } catch (error) {
        if (!lifetime.signal.aborted) update("failed", 0, (error as Error).message);
      } finally {
        if (!lifetime.signal.aborted) void reloadHome();
      }
    },
  };

  function start(poll: () => boolean) {
    commands = new AbortController();
    void reloadHome();
    homeTimer = window.setInterval(() => {
      if (document.visibilityState === "visible" && poll()) void reloadHome();
    }, 3000);
  }
  function stop() {
    homeRequest?.abort(); homeRequest = undefined; homeAgain = false;
    commands?.abort(); clearInterval(homeTimer);
    for (const timer of controlTimers) clearTimeout(timer);
    controlTimers.clear();
    setHome(undefined); setHomeError(""); setControls({});
  }
  return { home, problem, actions, reload: reloadHome, start, stop };
}
