package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	chains "chains-automation"
	"chains-automation/operator"
)

func TestDescribeService(t *testing.T) {
	services := []chains.Service{
		{UUID: "b", Name: "Fixture", ServiceType: "fixture"},
		{UUID: "a", Name: "Fixture", ServiceType: "fixture", Devices: []chains.Device{{ID: "one",
			Attributes: map[string]chains.Attribute{"power": chains.Writable(chains.PowerSchema, "set_power")}}},
			Events: map[string]map[string]json.RawMessage{"fixture.event": {"1": json.RawMessage(`{}`)}}},
	}
	var out bytes.Buffer
	if err := printDescriptions(&out, "table", "", services); err != nil || !strings.Contains(out.String(), "ATTRIBUTES") {
		t.Fatalf("summary: %s, %v", &out, err)
	}
	out.Reset()
	if err := printDescriptions(&out, "table", "Fixture", services); err != nil ||
		!strings.Contains(out.String(), "setter set_power@1") || !strings.Contains(out.String(), "fixture.event") {
		t.Fatalf("detail: %s, %v", &out, err)
	}
	out.Reset()
	if err := printDescriptions(&out, "json", "Fixture", services); err != nil {
		t.Fatal(err)
	}
	var decoded []chains.Service
	if err := json.NewDecoder(&out).Decode(&decoded); err != nil || len(decoded) != 2 || decoded[0].UUID != "a" {
		t.Fatalf("name should match both services: %+v, %v", decoded, err)
	}
	out.Reset()
	if err := printDescriptions(&out, "csv", "a", services); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&out).ReadAll()
	if err != nil || len(rows) != 5 || rows[3][0] != "attribute" || rows[3][7] != chains.PowerSchema {
		t.Fatalf("invalid CSV: %v, %v", rows, err)
	}
	if err := printDescriptions(&out, "table", "missing", services); err == nil {
		t.Fatal("accepted missing service")
	}
}

func TestStateDisplayDistinguishesUnreportedAndNull(t *testing.T) {
	row := operator.StateRow{Kind: "attribute"}
	if stateValueText(row) != "Unreported" {
		t.Fatal("unreported attribute has no readable status")
	}
	row.Value = json.RawMessage("null")
	if stateValueText(row) != "null" {
		t.Fatal("explicit null was treated as unreported")
	}
}
