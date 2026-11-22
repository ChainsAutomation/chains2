package chains

import "encoding/json"

type Attribute struct {
	Schema      json.RawMessage `json:"schema"`
	Label       string          `json:"label,omitempty"`
	Description string          `json:"description,omitempty"`
	Unit        string          `json:"unit,omitempty"`
	Setter      *Setter         `json:"setter,omitempty"`
}

type Setter struct {
	Action   string `json:"action"`
	Version  string `json:"schema_version"`
	Argument string `json:"argument"`
	Label    string `json:"label,omitempty"`
}

const PowerSchema = `{"type":"boolean"}`
const DimlevelSchema = `{"type":"integer","minimum":0,"maximum":100}`

// Attributes declares attributes whose values may be any JSON value.
func Attributes(names ...string) map[string]Attribute {
	attributes := make(map[string]Attribute, len(names))
	for _, name := range names {
		attributes[name] = Attribute{Schema: json.RawMessage("{}")}
	}
	return attributes
}

func Writable(schema, action string) Attribute {
	return Attribute{Schema: json.RawMessage(schema), Setter: &Setter{Action: action, Version: "1", Argument: "value"}}
}

func (d Device) MarshalJSON() ([]byte, error) {
	type wireDevice Device
	if d.Classes == nil {
		d.Classes = []string{}
	}
	return json.Marshal(wireDevice(d))
}
