import { createEffect, createSignal, For, onCleanup, Show, Switch, Match } from "solid-js";
import { render } from "@solidjs/web";
import { type Activity, type Connection, HISTORY, HTTPError, jsonBody, type Profile, query, type Service, type SetReceipt, type Snapshot, type DeviceList, type Trace, request, watch } from "./api";
import { TraceView, WatchView } from "./activity";
import { Button, type Navigate } from "./components";
import { SendView, type Draft } from "./send";
import { RulesView } from "./rules";
import { DevicesView } from "./devices";
import { deviceResource } from "./device-resource";
import { StateView } from "./state";
import "./styles.css";

const pages = [["State", "state", "▦"], ["Devices", "devices", "⌂"], ["Watch", "watch", "≋"], ["Send", "send", "＋"], ["Rules", "rules", "≡"], ["Trace Explorer", "trace", "⑂"]] as const;
const pageURL = () => new URL(window.location.href);
const pageName = (url: URL) => url.pathname.replace(/^\/ui\/?/, "").replace(/\/$/, "") || "state";
function App() {
  const [location, setLocation] = createSignal(pageURL());
  const [auth, setAuth] = createSignal<"checking" | "signed-out" | "signed-in">("checking");
  const [token, setToken] = createSignal("");
  const [loginError, setLoginError] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [services, setServices] = createSignal<Service[]>([]);
  const [profiles, setProfiles] = createSignal<Profile[]>([]);
  const [catalogError, setCatalogError] = createSignal("");
  const [records, setRecords] = createSignal<Activity[]>([]);
  const [connection, setConnection] = createSignal<Connection>("Connecting");
  const [drafts, setDrafts] = createSignal<Record<string, Draft>>({});
  const [profile, setProfile] = createSignal("");
  let catalogRequest: AbortController | undefined;
  let stream: AbortController | undefined;
  let catalogTimer: number | undefined;
  const pop = () => setLocation(pageURL());
  window.addEventListener("popstate", pop);
  onCleanup(() => { window.removeEventListener("popstate", pop); stopWork(); });
  const navigate: Navigate = (page, fields = {}) => {
    const target = "/ui/" + page + (Object.keys(fields).length ? "?" + new URLSearchParams(fields) : "");
    if (target === location().pathname + location().search) return;
    window.history.pushState({}, "", target);
    setLocation(pageURL());
    window.scrollTo(0, 0);
  };
  const page = () => pageName(location());
  function openLink(e: MouseEvent, destination: string) {
    if (e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
    e.preventDefault(); navigate(destination);
  }
  const devices = deviceResource(() => auth() === "signed-in",
    async signal => ({ ...await api<DeviceList>("/devices", { signal }), dashboards: [], home_id: "" }),
    async (data, signal) => {
      await api<SetReceipt>("/actions", { ...jsonBody("POST", data), signal });
      return { status: "submitted" };
    });
  const [rulesVisited, setRulesVisited] = createSignal(false);
  createEffect(() => page() === "rules" && auth() === "signed-in", visited => { if (visited) setRulesVisited(true); });
  function stopWork() {
    stream?.abort(); catalogRequest?.abort(); clearTimeout(catalogTimer); devices.stop();
  }
  function signedOut() {
    stopWork(); setRulesVisited(false); setAuth("signed-out"); setConnection("Disconnected");
    setRecords([]); setServices([]); setProfiles([]); setDrafts({}); setProfile(""); setCatalogError("");
  }
  async function api<T>(path: string, init?: RequestInit) {
    try { return await request<T>(path, init); }
    catch (error) {
      if (error instanceof HTTPError && error.status === 401 && auth() === "signed-in") {
        signedOut();
        setLoginError("Your session has ended. Sign in to continue.");
      }
      throw error;
    }
  }
  async function reloadCatalog() {
    catalogRequest?.abort();
    const pending = new AbortController();
    catalogRequest = pending;
    try {
      const [catalog, readyProfiles] = await Promise.all([api<Service[]>("/services", { signal: pending.signal }), api<Profile[]>("/profiles", { signal: pending.signal })]);
      if (!pending.signal.aborted) { setServices(catalog); setProfiles(readyProfiles); setCatalogError(""); }
    } catch (error) {
      if (!pending.signal.aborted) setCatalogError((error as Error).message);
    }
  }
  function receive(batch: Activity[]) {
    setRecords(previous => {
      const ids = new Set(previous.filter(r => !r.type.startsWith("watch.")).map(r => r.id));
      const arrived = batch.filter(r => {
        if (!r.id || r.type.startsWith("watch.")) return true;
        if (ids.has(r.id)) return false;
        ids.add(r.id); return true;
      });
      return [...previous, ...arrived].slice(-HISTORY);
    });
    if (batch.some(r => r.type === "service.registered" || r.type === "device.placement.changed" || r.type === "watch.gap")) {
      clearTimeout(catalogTimer);
      catalogTimer = window.setTimeout(reloadCatalog, 250);
    }
  }
  function signedIn() {
    setLoginError(""); setToken(""); setAuth("signed-in"); setConnection("Connecting");
    devices.start(() => page() === "devices");
    void reloadCatalog();
    stream = new AbortController();
    void watch(stream.signal, receive, setConnection, () => {
      signedOut();
      setLoginError("Your session has ended. Sign in to continue.");
    });
  }
  async function login() {
    if (busy()) return;
    setBusy(true); setLoginError("");
    try { await request("/session", jsonBody("POST", { token: token() })); signedIn(); }
    catch (error) { setLoginError(error instanceof HTTPError && error.status === 401 ? "The token was rejected." : (error as Error).message); }
    finally { setToken(""); setBusy(false); }
  }
  async function logout() {
    try { await request("/session", { method: "DELETE" }); signedOut(); }
    catch (error) { setCatalogError("Sign-out failed. " + (error as Error).message); }
  }
  void request<{ role: "operator" | "household" }>("/session").then(signedIn, (error: Error) => {
    signedOut();
    if (!(error instanceof HTTPError && error.status === 401)) setLoginError(error.message);
  });
  createEffect(() => page(), current => {
    document.title = (pages.find(item => item[1] === current)?.[0] ?? "Operator Console") + " · Chains";
    if (current === "devices") void devices.reload();
  });
  return <Show when={auth() === "signed-in"} fallback={
    <div class="login-shell"><div class="login-brand"><span class="brand-icon">C</span> Chains</div>
      <form class="login-card panel" onSubmit={event => { event.preventDefault(); void login(); }}>
        <h1>Operator sign-in</h1><p>Enter your operator token.</p>
        <label class="field"><span>Operator token</span><input type="password" value={token()} autocomplete="current-password" onInput={event => setToken(event.currentTarget.value)} /></label>
        <Show when={loginError()}><p class="notice" role="alert">{loginError()}</p></Show>
        <Button primary type="submit" disabled={busy() || !token() || auth() === "checking"}>{auth() === "checking" ? "Checking session…" : busy() ? "Signing in…" : "Sign in"}</Button>
      </form>
    </div>}>
    <div class="shell">
      <a class="skip" href="#content">Skip to content</a>
      <aside class="sidebar">
        <a class="brand" href="/ui/" onClick={e => openLink(e, "")}><span class="brand-icon">C</span><span>Chains<small>Operator console</small></span></a>
        <nav aria-label="Main navigation">
          <For each={pages}>{([label, id, icon]) =>
            <a href={"/ui/" + id} aria-current={page() === id ? "page" : undefined} onClick={e => openLink(e, id)}>
              <span class="nav-icon" aria-hidden="true">{icon}</span>{label}</a>}
          </For>
        </nav>
        <div class="sidebar-footer"><div class={connection() === "Live" ? "connection online" : "connection"} role="status"><i />{connection()}</div><button class="signout" onClick={() => void logout()}>Sign out</button></div>
      </aside>
      <main id="content">
        <Show when={page() === "devices" && devices.problem()}><p class="notice" role="alert">{devices.problem()}</p></Show>
        <Show when={catalogError()}><div class="notice" role="alert">{catalogError()} <button class="quiet" onClick={() => void reloadCatalog()}>Retry</button></div></Show>
        <Show when={rulesVisited()}><div hidden={page() !== "rules"}><RulesView services={services()} api={api} /></div></Show>
        <Switch fallback={<div class="panel empty">Page unavailable. <Button onClick={() => navigate("")}>Open State</Button></div>}>
          <Match when={page() === "devices"}><DevicesView api={api} entries={devices.home()?.devices} operator actions={devices.actions}
            refreshCatalog={() => { void devices.reload(); void reloadCatalog(); }} /></Match>
          <Match when={page() === "rules"}>{null}</Match>
          <Match when={page() === "state"}><StateView services={services()} load={signal => api<Snapshot>("/state", { signal })} refreshCatalog={reloadCatalog} setAttribute={data => api<SetReceipt>("/actions", jsonBody("POST", data))} /></Match>
          <Match when={page() === "watch"}><WatchView services={services()} records={records()} status={connection()} navigate={navigate} /></Match>
          <Match when={page() === "send"}><SendView services={services()} profiles={profiles()} drafts={drafts()} initialProfile={profile()} selectProfile={setProfile} navigate={navigate}
            loadState={() => api<Snapshot>("/state")} submit={data => api("/events", jsonBody("POST", data))}
            change={(key, value) => setDrafts({ ...drafts(), [key]: value })} /></Match>
          <Match when={page() === "trace"}><TraceView correlation={location().searchParams.get("correlation") ?? ""} recent={records()} navigate={navigate}
            load={(id, signal) => api<Trace>(query("/traces", { correlation: id }), { signal })} /></Match>
        </Switch>
      </main>
    </div>
  </Show>;
}

render(() => <App />, document.getElementById("app")!);
