import { createEffect, createSignal, For, onCleanup, Show, Switch, Match } from "solid-js";
import { render } from "@solidjs/web";
import { type Activity, type Connection, HISTORY, HTTPError, jsonBody, type Profile, query, type Service, type SetReceipt, type Snapshot, type SetRequest, type Trace, request, watch } from "./api";
import { TraceView, WatchView } from "./activity";
import { Button, type Navigate } from "./components";
import { SendView, type Draft } from "./send";
import { RulesView } from "./rules";
import { DevicesView } from "./devices";
import { controlKey, DashboardView, deviceKey, type Control, type Home } from "./home";
import { DashboardBuilder } from "./dashboard-builder";
import { StateView } from "./state";
import "./styles.css";

const pages = [["State", "state", "▦"], ["Watch", "watch", "≋"], ["Send", "send", "＋"], ["Rules", "rules", "≡"], ["Trace Explorer", "trace", "⑂"]] as const;
const pageURL = () => new URL(window.location.href);
const pageName = (url: URL) => url.pathname.replace(/^\/ui\/?/, "").replace(/\/$/, "") || "";
function App() {
  const [location, setLocation] = createSignal(pageURL());
  const [auth, setAuth] = createSignal<"checking" | "signed-out" | "signed-in">("checking");
  const [role, setRole] = createSignal<"operator" | "household">();
  const [home, setHome] = createSignal<Home>();
  const [homeError, setHomeError] = createSignal("");
  const [controls, setControls] = createSignal<Record<string, Control>>({});
  let homeRequest: AbortController | undefined;
  let homeTimer: number | undefined;
  let homeAgain = false;
  let commands: AbortController | undefined;
  const controlTimers = new Set<number>();
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
  const isOperator = () => role() === "operator";
  const dashboardID = () => page().startsWith("dashboards/") ? page().slice(11) : "";
  function openLink(e: MouseEvent, destination: string) {
    if (e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
    e.preventDefault(); navigate(destination);
  }
  const [rulesVisited, setRulesVisited] = createSignal(false);
  createEffect(() => page() === "rules" && isOperator(), visited => { if (visited) setRulesVisited(true); });
  function stopWork() {
    stream?.abort(); catalogRequest?.abort(); clearTimeout(catalogTimer);
    homeRequest?.abort(); homeRequest = undefined; homeAgain = false;
    commands?.abort(); clearInterval(homeTimer);
    for (const timer of controlTimers) clearTimeout(timer);
    controlTimers.clear();
  }
  function signedOut() {
    stopWork(); setRulesVisited(false); setAuth("signed-out"); setConnection("Disconnected");
    setRole(undefined); setHome(undefined); setHomeError(""); setControls({});
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
  async function reloadHome() {
    if (auth() !== "signed-in") return;
    if (homeRequest) { homeAgain = true; return; }
    const request = new AbortController();
    homeRequest = request;
    try {
      const snapshot = await api<Home>("/home", { signal: request.signal });
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
      const update = (status: Control["status"], until = 0, message?: string) =>
        setControls(previous => ({ ...previous, [key]: { status, revision, until, desired: request.value, message } }));
      update("pending");
      try {
        const result = await api<{ status: Control["status"] }>("/home/actions", { ...jsonBody("POST", request), signal: lifetime?.signal });
        if (lifetime?.signal.aborted) return;
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
        if (!lifetime?.signal.aborted) update("failed", 0, (error as Error).message);
      } finally {
        if (!lifetime?.signal.aborted) void reloadHome();
      }
    },
  };
  function signedIn(session: { role: "operator" | "household" }) {
    setRole(session.role); setLoginError(""); setToken(""); setAuth("signed-in"); setConnection("Connecting");
    commands = new AbortController();
    void reloadHome();
    homeTimer = window.setInterval(() => {
      if (document.visibilityState === "visible" && (!page() || page() === "devices" || page() === "dashboard-builder" || dashboardID()))
        void reloadHome();
    }, 3000);
    if (session.role !== "operator") return;
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
    try { signedIn(await request<{ role: "operator" | "household" }>("/session", jsonBody("POST", { token: token() }))); }
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
  createEffect(() => [page(), home(), auth()] as const, ([current, snapshot]) => {
    document.title = (pages.find(item => item[1] === current)?.[0] ?? snapshot?.dashboards.find(d => d.id === (dashboardID() || snapshot.home_id))?.name ?? "Home") + " · Chains";
  });
  createEffect(() => page(), current => {
    if (!current || current === "devices" || current === "dashboard-builder" || current.startsWith("dashboards/")) void reloadHome();
  });
  return <Show when={auth() === "signed-in"} fallback={
    <div class="login-shell"><div class="login-brand"><span class="brand-icon">C</span> Chains</div>
      <form class="login-card panel" onSubmit={event => { event.preventDefault(); void login(); }}>
        <h1>Sign in to Chains</h1><p>Enter your household or operator token.</p>
        <label class="field"><span>Access token</span><input type="password" value={token()} autocomplete="current-password" onInput={event => setToken(event.currentTarget.value)} /></label>
        <Show when={loginError()}><p class="notice" role="alert">{loginError()}</p></Show>
        <Button primary type="submit" disabled={busy() || !token() || auth() === "checking"}>{auth() === "checking" ? "Checking session…" : busy() ? "Signing in…" : "Sign in"}</Button>
      </form>
    </div>}>
    <div class={isOperator() ? "shell home-shell" : "shell home-shell household-shell"}>
      <a class="skip" href="#content">Skip to content</a>
      <header class="home-nav">
        <a class="brand" href="/ui/" onClick={e => openLink(e, "")}><span class="brand-icon">C</span><span>Chains</span></a>
        <nav aria-label="Main navigation">
          <a href="/ui/" aria-current={!page() ? "page" : undefined} onClick={e => openLink(e, "")}>Home</a>
          <a href="/ui/devices" aria-current={page() === "devices" ? "page" : undefined} onClick={e => openLink(e, "devices")}>All Devices</a>
          <For each={(home()?.dashboards ?? []).filter(d => d.pinned).map(d => d.id)}>{id =>
            <a href={"/ui/dashboards/" + id} aria-current={dashboardID() === id ? "page" : undefined}
              onClick={e => openLink(e, "dashboards/" + id)}>{home()?.dashboards.find(board => board.id === id)?.name}</a>}
          </For>
        </nav>
        <button class="signout" onClick={() => void logout()}>Sign out</button>
      </header>
      <Show when={isOperator()}><aside class="sidebar home-operator">
        <nav aria-label="Operator tools">
          <a href="/ui/dashboard-builder" aria-current={page() === "dashboard-builder" ? "page" : undefined}
            onClick={e => openLink(e, "dashboard-builder")}>Dashboard builder</a>
          <For each={pages}>{([label, id, icon]) =>
            <a href={"/ui/" + id} aria-current={page() === id ? "page" : undefined} onClick={e => openLink(e, id)}>
              <span class="nav-icon" aria-hidden="true">{icon}</span>{label}</a>}
          </For>
        </nav>
        <div class="sidebar-footer"><div class={connection() === "Live" ? "connection online" : "connection"} role="status"><i />{connection()}</div></div>
      </aside></Show>
      <main id="content">
        <Show when={homeError()}><div class="notice" role="alert">{homeError()} <button class="quiet" onClick={() => void reloadHome()}>Retry</button></div></Show>
        <Show when={catalogError()}><div class="notice" role="alert">{catalogError()} <button class="quiet" onClick={() => void reloadCatalog()}>Retry</button></div></Show>
        <Show when={rulesVisited()}><div hidden={page() !== "rules"}><RulesView services={services()} api={api} /></div></Show>
        <Switch fallback={<div class="panel empty">Page unavailable. <Button onClick={() => navigate("")}>Open Home</Button></div>}>
          <Match when={!page() || dashboardID()}><DashboardView id={dashboardID()} home={home()} actions={actions} operator={isOperator()} /></Match>
          <Match when={page() === "devices"}><DevicesView api={api} entries={home()?.devices} operator={isOperator()} actions={actions} refreshCatalog={() => {
            void reloadHome(); if (isOperator()) void reloadCatalog();
          }} /></Match>
          <Match when={isOperator() && page() === "dashboard-builder"}><DashboardBuilder api={api} devices={home()?.devices ?? []} refresh={() => void reloadHome()} /></Match>
          <Match when={isOperator() && page() === "rules"}>{null}</Match>
          <Match when={isOperator() && page() === "state"}><StateView services={services()} load={signal => api<Snapshot>("/state", { signal })} refreshCatalog={reloadCatalog} setAttribute={data => api<SetReceipt>("/actions", jsonBody("POST", data))} /></Match>
          <Match when={isOperator() && page() === "watch"}><WatchView services={services()} records={records()} status={connection()} navigate={navigate} /></Match>
          <Match when={isOperator() && page() === "send"}><SendView services={services()} profiles={profiles()} drafts={drafts()} initialProfile={profile()} selectProfile={setProfile} navigate={navigate}
            loadState={() => api<Snapshot>("/state")} submit={data => api("/events", jsonBody("POST", data))}
            change={(key, value) => setDrafts({ ...drafts(), [key]: value })} /></Match>
          <Match when={isOperator() && page() === "trace"}><TraceView correlation={location().searchParams.get("correlation") ?? ""} recent={records()} navigate={navigate}
            load={(id, signal) => api<Trace>(query("/traces", { correlation: id }), { signal })} /></Match>
        </Switch>
      </main>
    </div>
  </Show>;
}

render(() => <App />, document.getElementById("app")!);
