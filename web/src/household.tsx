import { createEffect, createSignal, For, onCleanup, Show, Switch, Match } from "solid-js";
import { render } from "@solidjs/web";
import { HTTPError, jsonBody, requestAt } from "./api";
import { Button, type Navigate } from "./components";
import { DevicesView } from "./devices";
import { DashboardCollection, DashboardView, type Control, type Home } from "./home";
import { deviceResource } from "./device-resource";
import { DashboardBuilder } from "./dashboard-builder";
import "./styles.css";

const request = requestAt("/household/v1");
const pageURL = () => new URL(window.location.href);
const pageName = (url: URL) => url.pathname.replace(/^\//, "");
function App() {
  const [location, setLocation] = createSignal(pageURL());
  const [auth, setAuth] = createSignal<"checking" | "signed-out" | "signed-in">("checking");
  const [token, setToken] = createSignal("");
  const [loginError, setLoginError] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  let allowLeave: (() => boolean) | undefined;
  const pop = () => {
    if (allowLeave?.() === false) { window.history.pushState({}, "", location().href); return; }
    setLocation(pageURL());
  };
  window.addEventListener("popstate", pop);
  onCleanup(() => { window.removeEventListener("popstate", pop); stopWork(); });
  const navigate: Navigate = (page, fields = {}) => {
    const target = "/" + page + (Object.keys(fields).length ? "?" + new URLSearchParams(fields) : "");
    if (target === location().pathname + location().search) return;
    if (allowLeave?.() === false) return;
    window.history.pushState({}, "", target);
    setLocation(pageURL());
    window.scrollTo(0, 0);
  };
  const page = () => pageName(location());
  const dashboardID = () => page().startsWith("dashboards/") ? page().slice(11) : "";
  const editorRoute = () => page() === "dashboard-editor" || page().startsWith("dashboard-editor/") ? page() : "";
  function openLink(e: MouseEvent, destination: string) {
    if (e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
    e.preventDefault(); navigate(destination);
  }
  const { home, problem: homeError, actions, reload: reloadHome, start, stop: stopWork } = deviceResource(
    () => auth() === "signed-in",
    signal => api<Home>("/dashboard", { signal }),
    (request, signal) => api<{ status: Control["status"] }>("/actions", { ...jsonBody("POST", request), signal }),
  );
  function signedOut() {
    stopWork(); setAuth("signed-out");
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
  function signedIn() {
    setLoginError(""); setToken(""); setAuth("signed-in");
    start(() => true);
  }
  async function login() {
    if (busy()) return;
    setBusy(true); setLoginError("");
    try { await request("/session", jsonBody("POST", { token: token() })); signedIn(); }
    catch (error) { setLoginError(error instanceof HTTPError && error.status === 401 ? "The token was rejected." : (error as Error).message); }
    finally { setToken(""); setBusy(false); }
  }
  async function logout() {
    if (allowLeave?.() === false) return;
    try { await request("/session", { method: "DELETE" }); signedOut(); }
    catch (error) { setLoginError("Sign-out failed. " + (error as Error).message); }
  }
  void request<{ role: "operator" | "household" }>("/session").then(signedIn, (error: Error) => {
    signedOut();
    if (!(error instanceof HTTPError && error.status === 401)) setLoginError(error.message);
  });
  createEffect(() => [page(), home(), auth()] as const, ([current, snapshot]) => {
    document.title = ((current === "dashboards" ? "Dashboards" : current.startsWith("dashboard-editor") ? "Dashboard editor" : current === "devices" ? "All Devices" : undefined) ?? snapshot?.dashboards.find(d => d.id === (dashboardID() || snapshot.home_id))?.name ?? "Home") + " · Chains";
  });
  createEffect(() => page(), () => {
    void reloadHome();
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
    <div class="shell home-shell household-shell">
      <a class="skip" href="#content">Skip to content</a>
      <header class="home-nav">
        <a class="brand" href="/" onClick={e => openLink(e, "")}><span class="brand-icon">C</span><span>Chains</span></a>
        <nav aria-label="Main navigation">
          <a href="/" aria-current={!page() ? "page" : undefined} onClick={e => openLink(e, "")}>Home</a>
          <a href="/devices" aria-current={page() === "devices" ? "page" : undefined} onClick={e => openLink(e, "devices")}>All Devices</a>
          <a href="/dashboards" aria-current={page() === "dashboards" ? "page" : undefined} onClick={e => openLink(e, "dashboards")}>Dashboards</a>
          <For each={(home()?.dashboards ?? []).filter(d => d.pinned).map(d => d.id)}>{id =>
            <a href={"/dashboards/" + id} aria-current={dashboardID() === id ? "page" : undefined}
              onClick={e => openLink(e, "dashboards/" + id)}>{home()?.dashboards.find(board => board.id === id)?.name}</a>}
          </For>
        </nav>
        <button class="signout" onClick={() => void logout()}>Sign out</button>
      </header>
      <main id="content">
        <Show when={homeError()}><div class="notice" role="alert">{homeError()} <button class="quiet" onClick={() => void reloadHome()}>Retry</button></div></Show>
        <Show when={loginError()}><p class="notice" role="alert">{loginError()}</p></Show>
        <Switch fallback={<div class="panel empty">Page unavailable. <Button onClick={() => navigate("")}>Open Home</Button></div>}>
          <Match when={!page() || dashboardID()}><DashboardView id={dashboardID()} home={home()} actions={actions} navigate={navigate} /></Match>
          <Match when={page() === "devices"}><DevicesView api={api} entries={home()?.devices} operator={false} actions={actions} refreshCatalog={() => void reloadHome()} /></Match>
          <Match when={page() === "dashboards"}>
            <DashboardCollection home={home()} navigate={navigate} />
          </Match>
          <Match when={editorRoute()} keyed>{route =>
            <DashboardBuilder id={route.split("/")[1] ?? ""} api={api} devices={home()?.devices ?? []}
              guard={check => { allowLeave = check; }} close={() => navigate("dashboards")}
              saved={id => { allowLeave = undefined; void reloadHome(); navigate(id ? "dashboards/" + id : "dashboards"); }} />
          }</Match>
        </Switch>
      </main>
    </div>
  </Show>;
}

render(() => <App />, document.getElementById("app")!);
