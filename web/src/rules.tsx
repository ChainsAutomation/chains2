import { createSignal, For, Show } from "solid-js";
import { HTTPError, jsonBody, type Service } from "./api";
import { Button, Empty, Heading } from "./components";
import { blankBuilder, RuleForm, type Builder } from "./rule-form";

type Diagnostic = { message: string; line?: number; column?: number };
type RuleEntry = { id: string; name: string; origin: "file" | "web"; enabled: boolean; source?: string; builder?: Builder };
type Changes = { id?: string; name: string; enabled: boolean; source: string; delete?: boolean };
type Catalog = { revision: string; writable: boolean; rules: RuleEntry[] };
type Editor = { id?: string; name: string; origin: RuleEntry["origin"]; enabled: boolean; source: string; builder?: Builder; revision: string };
type ImportEntry = RuleEntry & { originalName: string; conflict?: RuleEntry; diagnostic?: Diagnostic; choice: "add" | "rename" | "skip" | "replace" };
type ImportReview = { revision: string; rows: ImportEntry[] };
const errorText = (error: unknown) => {
  const message = (error as Error).message;
  try { return diagnosticText(JSON.parse(message) as Diagnostic); } catch { return message; }
};
const diagnosticText = (d: Diagnostic) => (d.line ? "Line " + d.line + ", column " + d.column + ": " : "") + d.message;

export function RulesView(props: { services: Service[]; api: <T>(path: string, init?: RequestInit) => Promise<T> }) {
  const [catalog, setCatalog] = createSignal<Catalog>();
  const [draft, setDraft] = createSignal<Editor>();
  const [mode, setMode] = createSignal<"form" | "dsl">("form");
  const [preview, setPreview] = createSignal<{ source: string; diagnostic: Diagnostic | null }>();
  const [review, setReview] = createSignal<ImportReview>();
  const [problem, setProblem] = createSignal("");
  const [status, setStatus] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [confirmDelete, setConfirmDelete] = createSignal(false);
  const readOnly = () => draft()?.origin === "file" || !catalog()?.writable;
  const active = () => !!draft() || !!review();

  async function run(work: () => Promise<void>) {
    if (busy()) return;
    setBusy(true); setProblem(""); setStatus("");
    try { await work(); }
    catch (error) {
      setProblem(error instanceof HTTPError && error.status === 409
        ? error.message + ". Your draft is still open. Refresh the catalog before retrying." : errorText(error));
    }
    finally { setBusy(false); }
  }
  async function refresh() { const current = await props.api<Catalog>("/rules"); setCatalog(current); return current; }
  void run(async () => { await refresh(); });
  function select(editor: Editor) {
    setDraft(editor); setMode(editor.builder && editor.origin === "web" ? "form" : "dsl");
    setReview(undefined); setPreview(undefined); setConfirmDelete(false);
  }
  function create() {
    const current = catalog();
    if (!current) return;
    select({ name: "", origin: "web", source: "", enabled: true, revision: current.revision, builder: blankBuilder() });
  }
  function open(entry: RuleEntry) {
    void run(async () => {
      const revision = catalog()?.revision ?? "";
      const detail = await props.api<RuleEntry>("/rules/" + encodeURIComponent(entry.id));
      select({ ...detail, source: detail.source ?? "", revision });
    });
  }
  function edit(fields: Partial<Editor>) {
    setDraft(previous => previous && ({ ...previous, ...fields }));
    setPreview(undefined); setConfirmDelete(false); setStatus(""); setProblem("");
  }
  async function validate() {
    const d = draft();
    if (!d) return;
    await run(async () => {
      setPreview(await props.api("/rules/validate", jsonBody("POST",
        { name: d.name, ...(mode() === "form" ? { builder: d.builder } : { source: d.source }) })));
    });
  }
  async function publish(revision: string, changes: Changes[]) {
    const result = await props.api<{ revision: string; warning?: string }>("/rules/publish", jsonBody("POST", { revision, changes }));
    setReview(undefined); setDraft(undefined); setPreview(undefined); setConfirmDelete(false);
    setStatus(result.warning ?? "Rules published");
    await refresh();
  }
  function publishDraft(remove = false) {
    const d = draft(), validation = preview();
    if (!d || (!remove && (!validation || validation.diagnostic))) return;
    void run(() => publish(d.revision, [{ id: d.id, name: d.name, enabled: d.enabled,
      source: validation?.source ?? d.source, ...(remove ? { delete: true } : {}) }]));
  }
  function importFile(file?: File) {
    if (!file) return;
    void run(async () => {
      const body = new FormData(); body.append("file", file);
      const imported = await props.api<{ revision: string; rules: Omit<ImportEntry, "choice">[] }>("/rules/import", { method: "POST", body });
      setReview({ revision: imported.revision, rows: imported.rules.map<ImportEntry>(row => ({ ...row, originalName: row.name, choice: row.conflict || row.diagnostic ? "skip" : "add" })) });
      setDraft(undefined);
    });
  }
  function setRow(index: number, values: Partial<ImportEntry>) {
    setReview(previous => previous && ({ ...previous, rows: previous.rows.map((row, i) => i === index ? { ...row, ...values } : row) }));
  }
  function publishImport() {
    const imported = review();
    if (!imported) return;
    const changes = imported.rows.filter(row => row.choice !== "skip").map(row => ({
      name: row.name, source: row.source ?? "", enabled: row.enabled,
      ...(row.choice === "replace" ? { id: row.conflict?.id, name: row.conflict?.name ?? row.name } : {}),
    }));
    if (changes.length) void run(() => publish(imported.revision, changes));
  }
  function refreshDraft() {
    void run(async () => {
      const current = await refresh();
      const d = draft();
      const imported = review();
      if (imported && imported.revision !== current.revision) {
        setReview({ revision: current.revision, rows: imported.rows.map(row => {
          const conflict = current.rules.find(item => item.name === row.originalName);
          return { ...row, conflict, choice: row.choice === "replace" || (conflict && row.choice === "add") ? "skip" : row.choice };
        }) });
        setStatus("Import conflicts refreshed. Review the choices before publishing.");
      }
      if (d && d.revision !== current.revision) {
        if (d.id && !current.rules.some(row => row.id === d.id)) {
          setStatus("The original rule was deleted. Publish this draft as a new rule.");
          setDraft({ ...d, id: undefined, revision: current.revision });
        } else {
          setDraft({ ...d, revision: current.revision });
          setStatus("Catalog refreshed. Validate the draft again before publishing.");
          setPreview(undefined);
        }
      }
    });
  }

  return <>
    <Heading title="Rules" subtitle="Create, validate, and publish automations on this authority.">
      <Button onClick={refreshDraft} disabled={busy()}>Refresh catalog</Button>
      <a class="button" href="/operator/v1/rules/export" download="chains-rules.zip">Download All</a>
      <label class="button rule-upload">Import file<input type="file" aria-label="Import rules file" accept=".chains,.zip"
        disabled={busy() || !catalog()?.writable} onChange={e => { importFile(e.currentTarget.files?.[0]); e.currentTarget.value = ""; }} /></label>
      <Button primary onClick={create} disabled={busy() || !catalog()?.writable}>New rule</Button>
    </Heading>
    <Show when={problem()}><p class="notice" role="alert">{problem()}</p></Show>
    <Show when={status()}><p class="notice" role="status">{status()}</p></Show>
    <Show when={catalog()?.writable === false}><p class="notice">Configure a writable web rules directory to publish rules.</p></Show>
    <div class={active() ? "rules-layout has-editor" : "rules-layout"}>
      <section class="panel data-panel">
        <div class="panel-title"><h2>Rule catalog</h2><span class="subtle">{catalog()?.rules.length ?? 0} rules</span></div>
        <Show when={catalog()?.rules.length} fallback={<Empty title={catalog() ? "No rules loaded" : "Loading rules…"} />}>
          <div class="table-scroll"><table><thead><tr><th>Rule</th><th>Status</th><th>Source</th></tr></thead><tbody>
            <For each={catalog()?.rules}>{entry => <tr class={draft()?.id === entry.id ? "chosen" : ""}>
              <td><button class="row-link" onClick={() => open(entry)} disabled={busy()}>{entry.name}</button></td>
              <td>{entry.enabled ? "Enabled" : "Disabled"}</td><td>{entry.origin === "file" ? "Deployment" : "Web"}</td>
            </tr>}</For>
          </tbody></table></div>
        </Show>
      </section>
      <Show when={draft()}>{d =>
        <section class="panel composer">
          <div class="panel-title"><h2>{d().id ? "Edit rule" : "New rule"}</h2>
            <Button onClick={() => { setDraft(undefined); setPreview(undefined); }} disabled={busy()}>Close editor</Button>
          </div>
          <form onSubmit={event => { event.preventDefault(); void validate(); }}>
            <fieldset disabled={busy() || readOnly()}>
              <div class="rule-fields">
                <label class="field"><span>Rule name</span><input required value={d().name} onInput={e => edit({ name: e.currentTarget.value })} /></label>
                <label class="check"><input type="checkbox" checked={d().enabled} onChange={e => edit({ enabled: e.currentTarget.checked })} /> Enabled when published</label>
              </div>
              <Show when={d().builder}>
                <div class="toolbar">
                  <Button disabled={mode() === "form"} onClick={() => { edit({ source: preview()?.source || d().source }); setMode("form"); }}>Guided form</Button>
                  <Button disabled={mode() === "dsl"} onClick={() => { edit({ source: preview()?.source || d().source }); setMode("dsl"); }}>DSL editor</Button>
                </div>
              </Show>
            </fieldset>
            <Show when={mode() === "form" && d().builder} fallback={
              <label class="field rule-source"><span>Rule source</span><textarea rows="14" spellcheck={false} readonly={readOnly() || busy()}
                value={d().source} onInput={e => edit({ source: e.currentTarget.value, builder: undefined })} /></label>
            }>{builder => <RuleForm value={builder()} services={props.services} change={value => edit({ builder: value })} disabled={busy() || readOnly()} />}</Show>
            <Show when={preview()}>{result =>
              <Show when={!result().diagnostic} fallback={<p class="notice" role="alert">{diagnosticText(result().diagnostic!)}</p>}>
                <div class="preview"><h3>Validated source</h3><pre>{result().source}</pre></div>
              </Show>
            }</Show>
            <Show when={!readOnly()}><div class="toolbar rule-footer">
              <Button type="submit" disabled={busy()}>Validate and preview</Button>
              <Button primary onClick={() => publishDraft()} disabled={busy() || !preview() || !!preview()?.diagnostic}>Publish</Button>
              <Show when={d().id}><Button disabled={busy()} onClick={() => setConfirmDelete(true)}>Delete rule</Button></Show>
            </div></Show>
            <Show when={confirmDelete()}><div class="notice rule-toolbar"><span>Delete this published rule and cancel its active runs?</span>
              <Button onClick={() => publishDraft(true)} disabled={busy()}>Confirm deletion</Button>
              <Button onClick={() => setConfirmDelete(false)} disabled={busy()}>Keep rule</Button>
            </div></Show>
          </form>
        </section>
      }</Show>
      <Show when={review()}>{imported =>
        <section class="panel composer">
          <div class="panel-title"><h2>Import preview</h2><Button disabled={busy()} onClick={() => setReview(undefined)}>Close import</Button></div>
          <For each={imported().rows.map((_, i) => i)}>{index => {
            const item = () => imported().rows[index];
            return <fieldset class="rule-action" disabled={busy()}>
              <div class="rule-toolbar"><h3>{item().name}</h3><span class="subtle">{item().conflict ? "Name already in use" : "New rule"}</span></div>
              <Show when={item().diagnostic}>{d => <p class="notice">{diagnosticText(d())}</p>}</Show>
              <div class="rule-fields">
                <label class="field"><span>Import choice</span><select value={item().choice} onChange={e => setRow(index, { choice: e.currentTarget.value as ImportEntry["choice"] })}>
                  <Show when={!item().conflict}><option value="add">Add</option></Show><option value="rename">Rename</option><option value="skip">Skip</option>
                  <Show when={item().conflict?.origin === "web"}><option value="replace">Replace web rule</option></Show>
                </select></label>
                <Show when={item().choice === "rename"}><label class="field"><span>New name</span><input value={item().name}
                  onChange={e => setRow(index, { name: e.currentTarget.value })} /></label></Show>
                <label class="check"><input type="checkbox" checked={item().enabled} onChange={e => setRow(index, { enabled: e.currentTarget.checked })} /> Enabled when published</label>
              </div>
              <details><summary>Rule source</summary><pre>{item().source}</pre></details>
            </fieldset>;
          }}</For>
          <div class="toolbar rule-footer"><Button primary onClick={publishImport} disabled={busy() || imported().rows.every(row => row.choice === "skip")}>Publish import</Button></div>
        </section>
      }</Show>
    </div>
  </>;
}
