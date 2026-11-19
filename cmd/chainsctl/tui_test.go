package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"chains-automation/operator"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestTUICompactViews(t *testing.T) {
	m := newTUI(context.Background(), nil, nil)
	m.Update(stateResult{state: operator.State{Revision: 41, Rows: []operator.StateRow{
		{Kind: "service", Service: "sensor", ServiceName: "Sensors"},
		{Kind: "device", Service: "sensor", Device: "bathroom", DeviceName: "Bathroom"},
		{Kind: "attribute", Service: "sensor", Device: "bathroom", Attribute: "humidity", Value: json.RawMessage("72.5")},
	}}})
	m.appendRecord(operator.Record{Type: "humidity.reported", Service: "sensor", Device: "bathroom", Data: json.RawMessage(`{"value": 72.5}`)})
	m.profiles = []operator.Profile{{Name: "humidity", Ready: true, Type: "humidity.reported", Schema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"number"}},"required":["value"]}`)}}
	for _, size := range []tea.WindowSizeMsg{{Width: 60, Height: 18}, {Width: 80, Height: 24}, {Width: 120, Height: 40}} {
		t.Run(fmt.Sprintf("%dx%d", size.Width, size.Height), func(t *testing.T) {
			m.Update(size)
			assertView := func(expected string) {
				t.Helper()
				view := strings.Split(m.View(), "\n")
				if len(view) != size.Height {
					t.Fatalf("height %d, expected %d", len(view), size.Height)
				}
				for i, line := range view {
					if ansi.StringWidth(line) > size.Width {
						t.Fatalf("row %d too wide: %q", i, line)
					}
				}
				if !strings.Contains(ansi.Strip(view[1]), expected) {
					t.Fatalf("main pane does not start directly below tabs: %q", view[1])
				}
			}
			m.tab = stateTab
			assertView("sensor")
			m.tab = watchTab
			assertView("humidity.reported")
			m.tab = sendTab
			m.profileIndex = 0
			assertView("Custom")
			m.profileIndex = 1
			assertView("humidity")
		})
	}
}

func TestTUIFilterPauseAndDraft(t *testing.T) {
	m := newTUI(context.Background(), nil, nil)
	for _, device := range []string{"one", "two"} {
		m.appendRecord(operator.Record{Type: "sensor.read", Device: device})
	}
	m.tab = watchTab
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if m.follow || m.watchIndex != 1 {
		t.Fatal("pause did not hold selection")
	}
	m.filters[watchTab] = "type=sensor.* device=one"
	if rows := m.watchRows(); len(rows) != 1 || rows[0].Device != "one" {
		t.Fatalf("filtered rows: %+v", rows)
	}
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if m.detail == nil {
		t.Fatal("record did not open")
	}
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	m.profiles = []operator.Profile{{Name: "value", Ready: true, Schema: json.RawMessage(`{"type":"boolean"}`)}}
	m.tab = sendTab
	m.profileIndex = 1
	typed := m.draft()
	if _, err := typed.payload(); err == nil {
		t.Fatal("required field accepted without a value")
	}
	typed.fields[0].input.SetValue("false")
	if data, err := typed.payload(); err != nil || string(data) != "false" {
		t.Fatalf("payload %s: %v", data, err)
	}
	m.profileIndex = 0
	custom := m.draft()
	custom.name.SetValue("foo")
	custom.json.SetValue(`{"value":[1,2]}`)
	m.profileIndex = 1
	if m.draft() != typed {
		t.Fatal("lost typed draft")
	}
	m.profileIndex = 0
	if m.draft().name.Value() != "foo" || m.draft().json.Value() != `{"value":[1,2]}` {
		t.Fatal("lost custom draft")
	}
}

func TestStateFormats(t *testing.T) {
	state := operator.State{Rows: []operator.StateRow{{Kind: "attribute", Service: "a", Device: "b", Attribute: "value", Value: json.RawMessage(`{"a":1,"b":2}`)}}}
	for _, format := range []string{"table", "json", "csv"} {
		var output bytes.Buffer
		if err := printState(&output, format, state); err != nil {
			t.Fatal(err)
		}
		switch format {
		case "table":
			if !strings.Contains(output.String(), "a/b/value") {
				t.Fatal(output.String())
			}
		case "json":
			if !json.Valid(output.Bytes()) {
				t.Fatal(output.String())
			}
		case "csv":
			if !strings.Contains(output.String(), `"{""a"":1,""b"":2}"`) {
				t.Fatal(output.String())
			}
		}
	}
}
