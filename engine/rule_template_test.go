package engine

import (
	"encoding/json"
	"strings"
	"testing"

	chains "chains-automation"
	"chains-automation/state"
)

const attributeTemplate = `id = "attribute-test"
title = "Attribute test"
source = """
rule.timeout = 20

Event(type="state.changed", device=light, attribute=power)
Set(device=light, attribute=power, value=true)
"""

[[inputs]]
name = "light"
kind = "device"
device_class = "light"

[[inputs]]
name = "power"
kind = "attribute"
device = "light"
writable = true
schema_type = "boolean"
`

func TestRuleTemplateBindingsAndStorage(t *testing.T) {
	raw := []byte(attributeTemplate)
	dir := t.TempDir()
	if err := saveTemplate(dir+"/templates", "attribute-test", raw); err != nil {
		t.Fatal(err)
	}
	if err := saveTemplate(dir+"/templates", "attribute-test", raw); err == nil {
		t.Fatal("replaced existing template")
	}
	c, err := openRuleCatalog(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	template, stored, err := c.readTemplate("attribute-test")
	if err != nil || string(stored) != attributeTemplate {
		t.Fatalf("cannot reopen template: %v", err)
	}
	if _, err := parseTemplate([]byte(strings.ReplaceAll(attributeTemplate, `device = "light"`, `device = "missing"`))); err == nil {
		t.Fatal("accepted missing device input")
	}
	if _, err := parseTemplate([]byte(strings.ReplaceAll(attributeTemplate, `attribute=power`, `attribute=undeclared.key`))); err == nil {
		t.Fatal("accepted unbound expression")
	}
	a, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	e := Engine{view: a.View}
	boolSchema := json.RawMessage(chains.PowerSchema)
	service := chains.Service{UUID: "room", Name: "Room", ServiceType: "fixture", Actions: map[string]map[string]json.RawMessage{
		"write": {"1": json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"boolean"}}}`)},
	}}
	d := chains.Device{ID: "lamp", Name: "Lamp", Classes: []string{"dimmer"}, Actions: []string{"write"}, Attributes: map[string]chains.Attribute{
		"power":     chains.Writable(chains.PowerSchema, "write"),
		"occupancy": {Schema: boolSchema},
		"dimlevel":  chains.Writable(chains.DimlevelSchema, "dim"),
	}}
	// The normalized catalog includes inherited classes.
	d.Classes = []string{"toggle", "light", "dimmer"}
	service.Devices = []chains.Device{d}
	msg, _ := chains.New(chains.Registration, service.UUID, service)
	if _, err := a.Register(msg, service); err != nil {
		t.Fatal(err)
	}
	choices := map[string]templateChoice{"light": {ServiceID: "room", DeviceID: "lamp"}, "power": {Attribute: "power"}}
	if err := e.validateTemplateChoices(template, choices); err != nil {
		t.Fatal(err)
	}
	source, err := template.bind("Bound rule", choices)
	if err != nil || !strings.Contains(source, "rule.timeout = 20") || !strings.Contains(source, `power = "power"`) ||
		!strings.Contains(source, `light = Device(device_id="lamp", service_id="room")`) {
		t.Fatalf("invalid generated source: %v\n%s", err, source)
	}
	for _, invalid := range []string{"occupancy", "dimlevel", "unknown"} {
		choices["power"] = templateChoice{Attribute: invalid}
		if err := e.validateTemplateChoices(template, choices); err == nil {
			t.Errorf("accepted attribute %s", invalid)
		}
	}
	bundled, _, err := c.readTemplate("see-1-2-3")
	if err != nil || !bundled.Bundled {
		t.Fatalf("cannot load bundled template: %v", err)
	}
	service.Devices = append(service.Devices, chains.Device{ID: "pir", Name: "Motion", Classes: []string{"motion"},
		Attributes: map[string]chains.Attribute{"occupancy": {Schema: boolSchema}}})
	msg, _ = chains.New(chains.Registration, service.UUID, service)
	if _, err := a.Register(msg, service); err != nil {
		t.Fatal(err)
	}
	lamp := templateChoice{ServiceID: "room", DeviceID: "lamp"}
	repeated := map[string]templateChoice{"motion": {ServiceID: "room", DeviceID: "pir"},
		"first_light": lamp, "second_light": lamp, "third_light": lamp}
	if err := e.validateTemplateChoices(bundled, repeated); err == nil || !strings.Contains(err.Error(), "different device") {
		t.Fatalf("expected duplicate device error, got %v", err)
	}
	delete(repeated, "first_light")
	if err := e.validateTemplateChoices(bundled, repeated); err == nil {
		t.Fatal("accepted missing required device")
	}
}
