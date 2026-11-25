package engine

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	chains "chains-automation"
	"chains-automation/rules"
	"github.com/pelletier/go-toml/v2"
)

type cachedTemplate struct {
	bundled bool
	modTime time.Time
	size    int64
	parsed  ruleTemplate
	raw     []byte
}

//go:embed templates/*.toml
var bundledTemplates embed.FS

var templateID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var templateVariable = regexp.MustCompile(`^[A-Za-z_][A-Za-z_0-9]*$`)

type ruleTemplate struct {
	ID          string          `json:"id" toml:"id"`
	Title       string          `json:"title" toml:"title"`
	Description string          `json:"description,omitempty" toml:"description"`
	Inputs      []templateInput `json:"inputs" toml:"inputs"`
	Source      string          `json:"-" toml:"source"`
	Bundled     bool            `json:"bundled" toml:"-"`
}

type templateInput struct {
	Name          string `json:"name" toml:"name"`
	Label         string `json:"label" toml:"label"`
	Kind          string `json:"kind" toml:"kind"`
	DeviceClass   string `json:"device_class,omitempty" toml:"device_class"`
	DistinctGroup string `json:"distinct_group,omitempty" toml:"distinct_group"`
	Device        string `json:"device,omitempty" toml:"device"`
	SchemaType    string `json:"schema_type,omitempty" toml:"schema_type"`
	Writable      bool   `json:"writable,omitempty" toml:"writable"`
}

type templateChoice struct {
	ServiceID string `json:"service_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	Attribute string `json:"attribute,omitempty"`
}

func parseTemplate(raw []byte) (ruleTemplate, error) {
	var t ruleTemplate
	if len(raw) > maxRuleSource {
		return t, errors.New("template must be at most 128 KiB")
	}
	if err := toml.NewDecoder(bytes.NewReader(raw)).DisallowUnknownFields().Decode(&t); err != nil {
		return t, err
	}
	if !templateID.MatchString(t.ID) || strings.TrimSpace(t.Title) == "" || len(t.Inputs) == 0 {
		return t, errors.New("template needs a lowercase slug ID, title, and at least one input")
	}
	kinds := make(map[string]string, len(t.Inputs))
	samples := make(map[string]templateChoice, len(t.Inputs))
	for i := range t.Inputs {
		input := &t.Inputs[i]
		if !templateVariable.MatchString(input.Name) || kinds[input.Name] != "" {
			return t, fmt.Errorf("invalid or duplicate input name %q", input.Name)
		}
		kinds[input.Name] = input.Kind
		if input.Label == "" {
			input.Label = input.Name
		}
		switch input.Kind {
		case "device":
			if input.Device != "" || input.SchemaType != "" || input.Writable {
				return t, fmt.Errorf("device input %s has attribute filters", input.Name)
			}
			samples[input.Name] = templateChoice{ServiceID: "template", DeviceID: input.Name}
		case "attribute":
			if input.Device == "" || input.DeviceClass != "" || input.DistinctGroup != "" {
				return t, fmt.Errorf("attribute input %s needs a device input and attribute filters only", input.Name)
			}
			switch input.SchemaType {
			case "", "array", "boolean", "integer", "null", "number", "object", "string":
			default:
				return t, fmt.Errorf("invalid schema_type for input %s", input.Name)
			}
			samples[input.Name] = templateChoice{Attribute: "example"}
		default:
			return t, fmt.Errorf("invalid kind for input %s", input.Name)
		}
	}
	for _, input := range t.Inputs {
		if input.Kind == "attribute" && kinds[input.Device] != "device" {
			return t, fmt.Errorf("attribute input %s refers to an unknown device input %s", input.Name, input.Device)
		}
	}
	_, err := t.bind(t.ID, samples)
	return t, err
}

func (t ruleTemplate) bind(name string, choices map[string]templateChoice) (string, error) {
	prefix := make([]rules.Statement, 0, len(t.Inputs))
	for _, input := range t.Inputs {
		selected := choices[input.Name]
		value := builderLiteral(selected.Attribute)
		if input.Kind == "device" {
			value = &rules.Expression{Kind: "call", Text: "Device", Arguments: builderArgs(map[string]any{
				"service_id": selected.ServiceID, "device_id": selected.DeviceID,
			})}
		}
		prefix = append(prefix, rules.Statement{Kind: "assign", Name: input.Name, Value: value})
	}
	source, err := rules.Bind(name, t.Source, prefix)
	if len(source) > maxRuleSource {
		return "", errors.New("generated rule exceeds 128 KiB")
	}
	return source, err
}

func (e *Engine) validateTemplateChoices(t ruleTemplate, choices map[string]templateChoice) error {
	if len(choices) != len(t.Inputs) {
		return errors.New("select a value for every template input")
	}
	devices := make(map[string]chains.Device, len(t.Inputs))
	groups := map[string]map[rules.DeviceRef]bool{}
	for _, input := range t.Inputs {
		selected, ok := choices[input.Name]
		if !ok {
			return fmt.Errorf("select a value for %s", input.Label)
		}
		if input.Kind != "device" {
			continue
		}
		service, known := e.view.Service(selected.ServiceID)
		d, found := device(service, selected.DeviceID)
		if !known || !found || selected.Attribute != "" || (input.DeviceClass != "" && !slices.Contains(d.Classes, input.DeviceClass)) {
			return fmt.Errorf("select a matching device for %s", input.Label)
		}
		devices[input.Name] = d
		if group := input.DistinctGroup; group != "" {
			if groups[group] == nil {
				groups[group] = map[rules.DeviceRef]bool{}
			}
			ref := rules.DeviceRef{ServiceID: selected.ServiceID, DeviceID: selected.DeviceID}
			if groups[group][ref] {
				return fmt.Errorf("select a different device for %s", input.Label)
			}
			groups[group][ref] = true
		}
	}
	for _, input := range t.Inputs {
		if input.Kind != "attribute" {
			continue
		}
		selected := choices[input.Name]
		attr, found := devices[input.Device].Attributes[selected.Attribute]
		if !found || selected.ServiceID != "" || selected.DeviceID != "" || (input.Writable && attr.Setter == nil) {
			return fmt.Errorf("select a matching attribute for %s", input.Label)
		}
		if input.SchemaType != "" {
			var shape struct{ Type string }
			if json.Unmarshal(attr.Schema, &shape) != nil || shape.Type != input.SchemaType {
				return fmt.Errorf("select a %s attribute for %s", input.SchemaType, input.Label)
			}
		}
	}
	return nil
}

func isBundledTemplate(id string) bool {
	_, err := fs.Stat(bundledTemplates, "templates/"+id+".toml")
	return err == nil
}

func (c *ruleCatalog) readTemplate(id string) (ruleTemplate, []byte, error) {
	if !templateID.MatchString(id) {
		return ruleTemplate{}, nil, fs.ErrNotExist
	}
	path := "templates/" + id + ".toml"
	c.mu.Lock()
	cached, hit := c.templates[id]
	c.mu.Unlock()
	if hit && cached.bundled {
		return cached.parsed, bytes.Clone(cached.raw), nil
	}
	raw, err := bundledTemplates.ReadFile(path)
	bundled := err == nil
	var modTime time.Time
	var size int64
	if errors.Is(err, fs.ErrNotExist) && c.dir != "" {
		full := filepath.Join(c.dir, path)
		info, statErr := os.Stat(full)
		if statErr != nil {
			if hit {
				c.mu.Lock()
				delete(c.templates, id)
				c.mu.Unlock()
			}
			return ruleTemplate{}, nil, statErr
		}
		modTime, size = info.ModTime(), info.Size()
		if hit && !cached.bundled && cached.modTime.Equal(modTime) && cached.size == size {
			return cached.parsed, bytes.Clone(cached.raw), nil
		}
		raw, err = os.ReadFile(full)
	}
	if err != nil {
		return ruleTemplate{}, nil, err
	}
	t, err := parseTemplate(raw)
	t.Bundled = bundled
	if err == nil && t.ID != id {
		err = errors.New("template ID differs from filename")
	}
	if err == nil {
		c.mu.Lock()
		if c.templates == nil {
			c.templates = map[string]cachedTemplate{}
		}
		c.templates[id] = cachedTemplate{bundled: bundled, modTime: modTime, size: size, parsed: t, raw: bytes.Clone(raw)}
		c.mu.Unlock()
	}
	return t, raw, err
}

func (e *Engine) templateRoutes(mux *http.ServeMux, base string, auth func(http.HandlerFunc) http.HandlerFunc) {
	c := e.ruleCatalog
	base += "/templates"
	mux.HandleFunc("GET "+base, auth(func(w http.ResponseWriter, _ *http.Request) {
		entries, err := bundledTemplates.ReadDir("templates")
		if err != nil {
			http.Error(w, "cannot list bundled templates", 500)
			return
		}
		list := make([]ruleTemplate, 0, len(entries))
		for _, entry := range entries {
			t, _, err := c.readTemplate(strings.TrimSuffix(entry.Name(), ".toml"))
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			list = append(list, t)
		}
		if c.dir != "" {
			entries, err := os.ReadDir(filepath.Join(c.dir, "templates"))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				http.Error(w, "cannot list templates", 500)
				return
			}
			for _, entry := range entries {
				id, ok := strings.CutSuffix(entry.Name(), ".toml")
				if !ok || !entry.Type().IsRegular() || !templateID.MatchString(id) || isBundledTemplate(id) {
					continue
				}
				t, _, err := c.readTemplate(id)
				if errors.Is(err, fs.ErrNotExist) {
					continue // Removed after listing.
				}
				if err != nil {
					http.Error(w, fmt.Sprintf("cannot read template %s: %v", id, err), 500)
					return
				}
				list = append(list, t)
			}
		}
		writeJSON(w, 200, list)
	}))
	mux.HandleFunc("POST "+base, auth(func(w http.ResponseWriter, r *http.Request) {
		if c.dir == "" {
			http.Error(w, "configure a writable web rules directory", 403)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRuleSource))
		if err != nil {
			http.Error(w, "cannot read template or template exceeds 128 KiB", 400)
			return
		}
		t, err := parseTemplate(raw)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if isBundledTemplate(t.ID) {
			http.Error(w, "template ID is reserved", 409)
			return
		}
		if err := saveTemplate(filepath.Join(c.dir, "templates"), t.ID, raw); err != nil {
			if errors.Is(err, fs.ErrExist) {
				http.Error(w, "template ID already exists", 409)
			} else {
				http.Error(w, "cannot save template", 500)
			}
			return
		}
		writeJSON(w, 201, t)
	}))
	mux.HandleFunc("GET "+base+"/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		t, raw, err := c.readTemplate(r.PathValue("id"))
		if templateReadError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/toml")
		w.Header().Set("Content-Disposition", `attachment; filename="`+t.ID+`.toml"`)
		_, _ = w.Write(raw)
	}))
	mux.HandleFunc("DELETE "+base+"/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if c.dir == "" || isBundledTemplate(id) {
			http.Error(w, "template is read-only", 403)
			return
		}
		if !templateID.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		dir := filepath.Join(c.dir, "templates")
		if templateReadError(w, os.Remove(filepath.Join(dir, id+".toml"))) {
			return
		}
		if templateReadError(w, syncRuleDir(dir)) {
			return
		}
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST "+base+"/{id}/instantiate", auth(func(w http.ResponseWriter, r *http.Request) {
		var draft struct {
			Name    string                    `json:"name"`
			Choices map[string]templateChoice `json:"choices"`
		}
		if !decodeRulesJSON(w, r, &draft) {
			return
		}
		t, _, err := c.readTemplate(r.PathValue("id"))
		if templateReadError(w, err) {
			return
		}
		if err = e.validateTemplateChoices(t, draft.Choices); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		source, err := t.bind(draft.Name, draft.Choices)
		if err == nil {
			_, err = e.prepareWebRule(draft.Name, source)
		}
		c.mu.Lock()
		revision := c.revision
		c.mu.Unlock()
		writeJSON(w, 200, map[string]any{"source": source, "diagnostic": diagnostic(err), "revision": revision})
	}))
}

func templateReadError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "template not found", 404)
	default:
		http.Error(w, "cannot access template", 500)
	}
	return true
}

func saveTemplate(dir, id string, raw []byte) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+chains.ID())
	defer os.Remove(tmp)
	if err := writeRuleFile(tmp, raw); err != nil {
		return err
	}
	if err := os.Link(tmp, filepath.Join(dir, id+".toml")); err != nil {
		return err
	}
	return syncRuleDir(dir)
}
