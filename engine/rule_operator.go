package engine

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	chains "chains-automation"
	"chains-automation/operator"
	"chains-automation/rules"
)

const maxRuleTransfer = 8 << 20

type ruleChange struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
	Delete  bool   `json:"delete,omitempty"`
}

type ruleDiagnostic struct {
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

var rulePosition = regexp.MustCompile(`^([0-9]+):([0-9]+): (.*)$`)

func diagnostic(err error) *ruleDiagnostic {
	if err == nil {
		return nil
	}
	d := &ruleDiagnostic{Message: err.Error()}
	if parts := rulePosition.FindStringSubmatch(d.Message); parts != nil {
		d.Line, _ = strconv.Atoi(parts[1])
		d.Column, _ = strconv.Atoi(parts[2])
		d.Message = parts[3]
	}
	return d
}

func decodeRulesJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRuleTransfer))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid or oversized rules request", http.StatusBadRequest)
		return false
	}
	return true
}

func (e *Engine) ruleRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	c := e.ruleCatalog
	if c == nil {
		return
	}
	base := operator.Base + "/rules"
	mux.HandleFunc("GET "+base, auth(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		entries := slices.Clone(c.entries)
		for i := range entries {
			entries[i].Source = ""
		}
		writeJSON(w, 200, map[string]any{"revision": c.revision, "writable": c.dir != "", "rules": entries})
	}))
	mux.HandleFunc("GET "+base+"/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, item := range c.entries {
			if item.ID == r.PathValue("id") {
				writeJSON(w, 200, struct {
					ruleSource
					Builder *ruleBuilder `json:"builder,omitempty"`
				}{item, recognizeBuilder(item)})
				return
			}
		}
		http.NotFound(w, r)
	}))
	mux.HandleFunc("POST "+base+"/validate", auth(e.validateRuleDraft))
	mux.HandleFunc("POST "+base+"/publish", auth(e.publishRules))
	mux.HandleFunc("POST "+base+"/import", auth(e.previewRuleImport))
	mux.HandleFunc("GET "+base+"/export", auth(e.exportRules))
}

func (e *Engine) prepareWebRule(name, source string) (*rules.Prepared, error) {
	rule, err := parseWebRule(name, source)
	if err != nil {
		return nil, err
	}
	return e.runtime.Prepare(rule)
}

func (e *Engine) validateRuleDraft(w http.ResponseWriter, r *http.Request) {
	var draft struct {
		Name    string       `json:"name"`
		Source  string       `json:"source"`
		Builder *ruleBuilder `json:"builder,omitempty"`
	}
	if !decodeRulesJSON(w, r, &draft) {
		return
	}
	var err error
	if draft.Builder != nil {
		draft.Source, err = e.buildRule(draft.Name, *draft.Builder)
	}
	if err == nil {
		_, err = e.prepareWebRule(draft.Name, draft.Source)
	}
	writeJSON(w, 200, map[string]any{"source": draft.Source, "diagnostic": diagnostic(err)})
}

func (e *Engine) publishRules(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Revision string       `json:"revision"`
		Changes  []ruleChange `json:"changes"`
	}
	c := e.ruleCatalog
	if c.dir == "" {
		http.Error(w, "configure web_rules_dir to publish rules", http.StatusForbidden)
		return
	}
	if !decodeRulesJSON(w, r, &request) {
		return
	}
	if len(request.Changes) == 0 {
		http.Error(w, "select at least one change", 400)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if request.Revision != c.revision {
		http.Error(w, "rules changed since this draft was opened", 409)
		return
	}
	entries := slices.Clone(c.entries)
	var remove []string
	var add []*rules.Prepared
	touched := map[string]bool{}
	for _, change := range request.Changes {
		i := slices.IndexFunc(entries, func(item ruleSource) bool { return item.ID == change.ID })
		if change.ID != "" && (i < 0 || entries[i].Origin != "web" || touched[change.ID]) {
			http.Error(w, "change targets an unavailable, repeated, or deployment-managed rule", 400)
			return
		}
		if change.ID != "" {
			touched[change.ID] = true
		}
		if change.Delete {
			if i < 0 {
				http.Error(w, "select a web rule to delete", 400)
				return
			}
			if entries[i].Enabled {
				remove = append(remove, entries[i].Name)
			}
			entries = slices.Delete(entries, i, i+1)
			continue
		}
		item := ruleSource{ID: change.ID, Name: change.Name, Source: change.Source, Enabled: change.Enabled, Origin: "web"}
		if i >= 0 && entries[i] == item {
			continue
		}
		prepared, err := e.prepareWebRule(item.Name, item.Source)
		if err != nil {
			writeJSON(w, 422, diagnostic(err))
			return
		}
		if i >= 0 {
			if entries[i].Enabled {
				remove = append(remove, entries[i].Name)
			}
			entries[i] = item
		} else {
			item.ID = chains.ID()
			entries = append(entries, item)
		}
		if item.Enabled {
			add = append(add, prepared)
		}
	}
	names := map[string]bool{}
	for _, item := range entries {
		if names[item.Name] {
			http.Error(w, fmt.Sprintf("rule name %q is already used", item.Name), 409)
			return
		}
		names[item.Name] = true
	}
	slices.SortFunc(entries, func(a, b ruleSource) int { return strings.Compare(a.Name, b.Name) })
	revision := chains.ID()
	committed, err := c.persist(entries, revision)
	if committed {
		e.runtime.Apply(remove, add)
		c.entries, c.revision = entries, revision
	}
	if err != nil {
		e.log.Error("persist rules", "error", err)
		if committed {
			writeJSON(w, 200, map[string]any{"revision": revision, "warning": "Published, but filesystem durability could not be confirmed."})
			return
		}
		http.Error(w, "could not persist rules", 500)
		return
	}
	writeJSON(w, 200, map[string]string{"revision": revision})
}

func (e *Engine) exportRules(w http.ResponseWriter, r *http.Request) {
	c := e.ruleCatalog
	c.mu.Lock()
	entries := slices.Clone(c.entries)
	c.mu.Unlock()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	manifest := ruleManifest{Version: 1, Rules: []ruleSource{}}
	for i, item := range entries {
		path := fmt.Sprintf("%06d.chains", i+1)
		file, err := z.Create(path)
		if err == nil {
			_, err = io.WriteString(file, item.Source)
		}
		if err != nil {
			http.Error(w, "could not export rules", 500)
			return
		}
		item.ID, item.Source = path, ""
		manifest.Rules = append(manifest.Rules, item)
	}
	manifestFile, err := z.Create("manifest.json")
	if err == nil {
		err = json.NewEncoder(manifestFile).Encode(manifest)
	}
	if err == nil {
		err = z.Close()
	}
	if err != nil {
		http.Error(w, "could not export rules", 500)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="chains-rules.zip"`)
	_, _ = w.Write(buf.Bytes())
}

func (e *Engine) previewRuleImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRuleTransfer)
	file, header, err := r.FormFile("file")
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		http.Error(w, "choose a .chains file or Chains ZIP within 8 MiB", 400)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	var entries []ruleSource
	if err == nil {
		entries, err = decodeRuleImport(header.Filename, data)
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	c := e.ruleCatalog
	c.mu.Lock()
	revision := c.revision
	existing := slices.Clone(c.entries)
	c.mu.Unlock()
	type candidate struct {
		ruleSource
		Conflict   *ruleSource     `json:"conflict,omitempty"`
		Diagnostic *ruleDiagnostic `json:"diagnostic,omitempty"`
	}
	result := make([]candidate, 0, len(entries))
	for _, item := range entries {
		_, err := e.prepareWebRule(item.Name, item.Source)
		row := candidate{ruleSource: item, Diagnostic: diagnostic(err)}
		for _, old := range existing {
			if old.Name == item.Name {
				old.Source = ""
				row.Conflict = &old
				break
			}
		}
		result = append(result, row)
	}
	writeJSON(w, 200, map[string]any{"revision": revision, "rules": result})
}

var archiveRuleName = regexp.MustCompile(`^[0-9]{6,}\.chains$`)

func decodeRuleImport(name string, data []byte) ([]ruleSource, error) {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".chains" && len(data) <= maxRuleSource {
		return []ruleSource{{Name: strings.TrimSuffix(name, filepath.Ext(name)), Source: string(data), Enabled: true, Origin: "web"}}, nil
	}
	if ext != ".zip" {
		return nil, errors.New("choose a .chains file within 128 KiB or a Chains ZIP")
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("invalid rule ZIP")
	}
	files := map[string]*zip.File{}
	var size uint64
	for _, file := range z.File {
		if !file.Mode().IsRegular() || files[file.Name] != nil || file.UncompressedSize64 > maxRuleTransfer-size ||
			(file.Name != "manifest.json" && (!archiveRuleName.MatchString(file.Name) || file.UncompressedSize64 > maxRuleSource)) {
			return nil, errors.New("invalid or oversized entry in rule ZIP")
		}
		size += file.UncompressedSize64
		files[file.Name] = file
	}
	read := func(file *zip.File, limit int64) ([]byte, error) {
		if file == nil {
			return nil, errors.New("missing archive entry")
		}
		reader, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
		if int64(len(raw)) > limit {
			return nil, errors.New("oversized archive entry")
		}
		return raw, err
	}
	raw, err := read(files["manifest.json"], maxRuleTransfer)
	var manifest ruleManifest
	if err != nil || json.Unmarshal(raw, &manifest) != nil || manifest.Version != 1 || len(files) != len(manifest.Rules)+1 {
		return nil, errors.New("invalid Chains rules manifest")
	}
	seen := map[string]bool{}
	for i := range manifest.Rules {
		item := &manifest.Rules[i]
		if seen[item.ID] || !archiveRuleName.MatchString(item.ID) {
			return nil, errors.New("invalid or repeated rule archive entry")
		}
		seen[item.ID] = true
		raw, err := read(files[item.ID], maxRuleSource)
		if err != nil {
			return nil, err
		}
		item.ID, item.Source = "", string(raw)
	}
	return manifest.Rules, nil
}
