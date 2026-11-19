package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"chains-automation/operator"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type fieldSchema struct {
	Type string            `json:"type"`
	Enum []json.RawMessage `json:"enum"`
}
type eventField struct {
	name     string
	schema   fieldSchema
	required bool
	input    textinput.Model
}

type eventDraft struct {
	name           textinput.Model
	json           textarea.Model
	fields         []eventField
	scalar         bool
	raw            bool
	schema         string
	initialized    bool
	rawInitialized bool
	focus          int
}

func newInput(placeholder string) textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = placeholder
	input.CharLimit = 8192
	return input
}

func newDraft(profile operator.Profile) *eventDraft {
	editor := textarea.New()
	editor.Prompt = ""
	editor.ShowLineNumbers = false
	editor.CharLimit = 0
	editor.MaxHeight = 0
	editor.MaxWidth = 0
	editor.SetValue("{}")
	editor.Blur()
	draft := &eventDraft{name: newInput("event name"), json: editor, raw: true, schema: string(profile.Schema)}
	if profile.Name == "" {
		return draft
	}
	var schema struct {
		fieldSchema
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if json.Unmarshal(profile.Schema, &schema) != nil {
		return draft
	}
	simple := func(s fieldSchema) bool {
		return len(s.Enum) > 0 || slices.Contains([]string{"string", "number", "integer", "boolean", "null"}, s.Type)
	}
	if simple(schema.fieldSchema) {
		draft.fields = []eventField{{name: "value", schema: schema.fieldSchema, required: true, input: newInput(schema.Type)}}
		draft.scalar, draft.raw = true, false
		return draft
	}
	if schema.Type != "object" || len(schema.Properties) == 0 {
		return draft
	}
	keys := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		keys = append(keys, name)
	}
	slices.Sort(keys)
	for _, name := range keys {
		var field fieldSchema
		if json.Unmarshal(schema.Properties[name], &field) != nil || !simple(field) {
			draft.fields = nil
			return draft
		}
		draft.fields = append(draft.fields, eventField{name: name, schema: field, required: slices.Contains(schema.Required, name), input: newInput(field.Type)})
	}
	for _, name := range schema.Required {
		if _, ok := schema.Properties[name]; !ok {
			draft.fields = nil
			return draft
		}
	}
	draft.raw = false
	return draft
}

func (m *tui) profile() operator.Profile {
	if m.profileIndex > 0 && m.profileIndex <= len(m.profiles) {
		return m.profiles[m.profileIndex-1]
	}
	return operator.Profile{}
}
func (m *tui) draft() *eventDraft {
	profile := m.profile()
	draft := m.drafts[profile.Name]
	if draft == nil || (draft.schema != string(profile.Schema) && !draft.initialized) {
		draft = newDraft(profile)
		m.drafts[profile.Name] = draft
	}
	return draft
}
func (d *eventDraft) focusField(custom bool) tea.Cmd {
	d.name.Blur()
	d.json.Blur()
	for i := range d.fields {
		d.fields[i].input.Blur()
	}
	if d.focus == 0 {
		return nil
	}
	if custom && d.focus == 1 {
		return d.name.Focus()
	}
	if d.raw {
		return d.json.Focus()
	}
	return d.fields[d.focus-1].input.Focus()
}
func (d *eventDraft) edit(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch {
	case d.focus == 0:
	case d.name.Focused():
		d.name, cmd = d.name.Update(msg)
	case d.raw:
		d.json, cmd = d.json.Update(msg)
	default:
		field := &d.fields[d.focus-1]
		field.input, cmd = field.input.Update(msg)
	}
	return cmd
}
func (d *eventDraft) payload() (json.RawMessage, error) {
	if d.raw {
		raw := json.RawMessage(strings.TrimSpace(d.json.Value()))
		if !json.Valid(raw) {
			return nil, fmt.Errorf("enter valid JSON")
		}
		return raw, nil
	}
	values := map[string]json.RawMessage{}
	for _, field := range d.fields {
		value := field.input.Value()
		if value == "" {
			if field.required {
				return nil, fmt.Errorf("enter %s", field.name)
			}
			continue
		}
		var raw json.RawMessage
		var err error
		if field.schema.Type == "string" {
			if value == `""` {
				value = ""
			}
			raw, err = json.Marshal(value)
		} else {
			raw = json.RawMessage(strings.TrimSpace(value))
			if !json.Valid(raw) {
				return nil, fmt.Errorf("%s needs a JSON %s", field.name, field.schema.Type)
			}
			switch field.schema.Type {
			case "boolean":
				if value != "true" && value != "false" {
					err = fmt.Errorf("expected true or false")
				}
			case "null":
				if value != "null" {
					err = fmt.Errorf("expected null")
				}
			case "number":
				_, err = strconv.ParseFloat(value, 64)
			case "integer":
				_, err = strconv.ParseInt(value, 10, 64)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field.name, err)
		}
		if d.scalar {
			return raw, nil
		}
		values[field.name] = raw
	}
	return json.Marshal(values)
}

func (m *tui) sendKey(msg tea.KeyMsg) tea.Cmd {
	profile, d := m.profile(), m.draft()
	custom := profile.Name == ""
	switch msg.String() {
	case "tab", "shift+tab":
		last := max(1, len(d.fields))
		if d.raw {
			last = 1
		}
		if custom {
			last++
		}
		if msg.String() == "tab" {
			d.focus = (d.focus + 1) % (last + 1)
		} else {
			d.focus = (d.focus + last) % (last + 1)
		}
		return d.focusField(custom)
	case "left", "right":
		if d.focus == 0 {
			delta := 1
			if msg.String() == "left" {
				delta = -1
			}
			m.profileIndex = (m.profileIndex + delta + len(m.profiles) + 1) % (len(m.profiles) + 1)
			m.status[sendTab] = ""
			next := m.draft()
			next.focus = 0
			return next.focusField(m.profile().Name == "")
		}
	case "ctrl+j":
		if len(d.fields) > 0 {
			if !d.raw && !d.rawInitialized {
				if raw, err := d.payload(); err == nil {
					var pretty bytes.Buffer
					_ = json.Indent(&pretty, raw, "", "  ")
					d.json.SetValue(pretty.String())
					d.rawInitialized = true
				}
			}
			d.raw = !d.raw
			d.focus = 1
			d.initialized = true
			return d.focusField(custom)
		}
	case "ctrl+p", "ctrl+s":
		if m.pendingSend {
			return nil
		}
		if !custom && !profile.Ready {
			m.status[sendTab] = "Profile source is unavailable"
			return nil
		}
		name := strings.TrimSpace(d.name.Value())
		if custom && name == "" {
			m.status[sendTab] = "Enter a custom event name"
			return nil
		}
		raw, err := d.payload()
		if err != nil {
			m.status[sendTab] = err.Error()
			return nil
		}
		request := operator.Submission{Profile: profile.Name, Name: name, Data: raw}
		if msg.String() == "ctrl+p" {
			m.inspect(request)
			return nil
		}
		m.pendingSend = true
		m.status[sendTab] = "Publishing…"
		return func() tea.Msg { receipt, err := m.client.Send(m.ctx, request); return sendResult{receipt, err} }
	}
	if d.focus > 0 {
		d.initialized = true
		if d.raw && !d.name.Focused() {
			d.rawInitialized = true
		}
		m.status[sendTab] = ""
	}
	return d.edit(msg)
}

func (m *tui) sendView(height int) ([]string, string) {
	p, d := m.profile(), m.draft()
	label := "Custom · manual.<name>"
	if p.Name != "" {
		label = p.Name + " · " + p.Type
		if !p.Ready {
			label += " · unavailable"
		}
	}
	profileLine := "Profile  ‹ " + clean(label) + " ›"
	if d.focus == 0 {
		profileLine = selected.Render(cell(profileLine, m.width))
	}
	rows := []string{profileLine}
	if p.Name == "" {
		d.name.Width = max(6, m.width-12)
		rows = append(rows, "Name     "+d.name.View())
	}
	if d.raw {
		d.json.SetWidth(max(10, m.width))
		d.json.SetHeight(max(1, height-len(rows)-1))
		rows = append(rows, muted.Render("Payload · JSON"))
		rows = append(rows, strings.Split(d.json.View(), "\n")...)
	} else {
		start, end := window(len(d.fields), max(0, d.focus-1), max(1, height-1))
		for i := start; i < end; i++ {
			f := &d.fields[i]
			label := f.name
			if f.required {
				label += "*"
			}
			column := min(24, max(9, m.width/3))
			f.input.Width = max(6, m.width-column-2)
			input := f.input.View()
			if len(f.schema.Enum) > 0 && f.input.Value() == "" && !f.input.Focused() {
				input = string(f.schema.Enum[0]) + " …"
			}
			rows = append(rows, cell(label, column)+"  "+input)
		}
	}
	footer := "Tab field   ←→ profile   ^P preview   ^S send"
	if len(d.fields) > 0 {
		footer += "   ^J mode"
	}
	return rows, footer
}
