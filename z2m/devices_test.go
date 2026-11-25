package z2m

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	chains "chains-automation"
)

const lightInventory = `[{"ieee_address":"0x0011223344556677","friendly_name":"kitchen/light","supported":true,"definition":{"exposes":[{"type":"light","features":[{"property":"state","access":3,"value_on":"ON","value_off":"OFF"},{"property":"brightness","access":3,"value_min":1,"value_max":200}]},{"property":"action","access":1}]}}]`

func TestDiscoveryTranslationAndRename(t *testing.T) {
	b := New(DefaultConfig(), nil)
	var registered chains.Service
	submit := func(_ context.Context, m chains.Message, _ bool) error { return json.Unmarshal(m.Data, &registered) }
	devices, err := b.discover(context.Background(), []byte(lightInventory), submit)
	if err != nil {
		t.Fatal(err)
	}
	d := devices["kitchen/light"]
	if len(d.Classes) != 1 || d.Classes[0] != "dimmer" || len(d.Actions) != 2 || len(registered.Devices) != 1 {
		t.Fatalf("bad registration: %+v", registered)
	}
	raw := []byte(`{"state":"ON","brightness":30,"action":"single","unadvertised":true}`)
	live, err := b.translate(d, raw, false)
	if err != nil || len(live) != 1 || live[0].Type != chains.ObservationSubmitted {
		t.Fatalf("live: %+v %v", live, err)
	}
	var batch chains.ObservationBatch
	if json.Unmarshal(live[0].Data, &batch) != nil || batch.EventType != ActionEvent || len(batch.Observations) != 1 ||
		string(batch.Observations[0].Values["last_action"]) != `"single"` || string(batch.Observations[0].Values["dimlevel"]) != "15" {
		t.Fatalf("invalid batch: %+v", batch)
	}
	again, _ := b.translate(d, raw, false)
	if again[0].ID == live[0].ID {
		t.Fatal("identical press was collapsed")
	}
	retained, _ := b.translate(d, raw, true)
	if len(retained) != 1 || retained[0].Type != chains.Report {
		t.Fatalf("retained: %+v", retained)
	}
	if _, err := d.command(chains.Action{Name: "set_brightness", Args: map[string]any{"value": 101.0}}); err == nil {
		t.Fatal("accepted out-of-range brightness")
	}
	brightness, err := d.command(chains.Action{Name: "set_brightness", Args: map[string]any{"value": 50.0}})
	if err != nil || string(brightness) != `{"brightness":101}` {
		t.Fatalf("native brightness: %s: %v", brightness, err)
	}
	command, err := d.command(chains.Action{Name: "set_power", Args: map[string]any{"value": true}})
	if err != nil || string(command) != `{"state":"ON"}` {
		t.Fatalf("command %s: %v", command, err)
	}
	renamed, err := b.discover(context.Background(), []byte(strings.ReplaceAll(lightInventory, "kitchen/light", "hall/light")), submit)
	if err != nil || renamed["hall/light"].ID != d.ID || renamed["kitchen/light"].ID != "" {
		t.Fatalf("rename: %v", err)
	}
	if _, err = b.discover(context.Background(), []byte("[]"), submit); err != nil || len(b.devices) != 0 {
		t.Fatalf("remove: %v", err)
	}
}

func TestInvalidURLDoesNotExposeCredentials(t *testing.T) {
	c := DefaultConfig()
	c.Enabled = true
	c.MQTTURL = "mqtt://user:secret%GG@localhost"
	if err := c.Validate(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("bad validation error: %v", err)
	}
}

func TestDiscoveryDeclaresMotionClass(t *testing.T) {
	inventory := `[{"ieee_address":"0x0011223344556677","friendly_name":"hall motion","supported":true,"definition":{"exposes":[{"property":"occupancy","access":1}]}}]`
	b := New(DefaultConfig(), nil)
	devices, err := b.discover(context.Background(), []byte(inventory), func(context.Context, chains.Message, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	d := devices["hall motion"]
	if len(d.Classes) != 1 || d.Classes[0] != "motion" || string(d.Attributes["occupancy"].Schema) != chains.PowerSchema {
		t.Fatalf("unexpected motion capabilities: %+v", d)
	}
}
