package state

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	chains "chains-automation"
)

func TestReportAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	registration := chains.Service{UUID: "sensor", Name: "Sensor", ServiceType: "sensor", Devices: []chains.Device{{ID: "one", Attributes: chains.Attributes("value")}}}
	if _, err := a.Register(chains.Message{Source: "sensor", ID: "register"}, registration); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	report := func(id string, observed time.Time, value string) (Update, []Change) {
		m := chains.Message{ID: id, Source: "sensor", DeviceID: "one", ObservedAt: observed}
		u, c, err := a.Observe(m, []chains.Observation{{DeviceID: m.DeviceID, Values: map[string]json.RawMessage{"value": json.RawMessage(value)}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return u, c
	}
	_, first := report("first", now, "42")
	if len(first) != 1 || !first[0].Initial {
		t.Fatalf("first observation: %v", first)
	}
	later := now.Add(time.Second)
	u, same := report("same", later, "42")
	if len(same) != 0 || !u.Devices[Address("sensor", "one")]["value"].ChangedAt.Equal(now) || !u.Devices[Address("sensor", "one")]["value"].ObservedAt.Equal(later) {
		t.Fatalf("repeat: %v %v", u, same)
	}
	_, changed := report("changed", later.Add(time.Second), "43")
	if len(changed) != 1 || string(changed[0].Previous) != "42" || changed[0].Initial {
		t.Fatalf("transition: %v", changed)
	}
	a.Close()
	a, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if got := a.Lookup("sensor/one")["value"]; got != float64(43) {
		t.Fatalf("restored value: %v", got)
	}
	replay, events := report("changed", later.Add(2*time.Second), "99")
	if replay.Revision != 0 || len(events) != 0 {
		t.Fatalf("duplicate report accepted: %v %v", replay, events)
	}
}

func TestResolveDevice(t *testing.T) {
	view := NewView()
	s := view.Snapshot()
	s.Services["one"] = chains.Service{Name: "LEDPanel", Devices: []chains.Device{{ID: "a", Name: "panel0"}}}
	s.Services["two"] = chains.Service{Name: "different", Devices: []chains.Device{{ID: "a", Name: "panel0"}}}
	view.Replace(s)
	if service, device, err := view.ResolveDevice("LEDPanel", "panel0"); service != "one" || device != "a" || err != nil {
		t.Fatalf("resolved %s/%s: %v", service, device, err)
	}
	if _, _, err := view.ResolveDevice("LEDPanel", "missing"); err == nil {
		t.Fatal("missing resolution succeeded")
	}
	s.Services["two"] = chains.Service{Name: "LEDPanel", Devices: []chains.Device{{ID: "a", Name: "panel0"}}}
	view.Replace(s)
	if _, _, err := view.ResolveDevice("LEDPanel", "panel0"); err == nil {
		t.Fatal("ambiguous resolution succeeded")
	}
}

func TestUpgradePreservesReportedLeaves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	old := json.RawMessage(`{"uuid":"legacy","name":"Legacy","service_type":"sensor","devices":[{"id":"meter","class":"sensor","attributes":["watts"]}]}`)
	stamp := time.Now().UTC()
	leaf := Attribute{Value: json.RawMessage("18"), ObservedAt: stamp, ChangedAt: stamp, Revision: 4}
	if err := db.put("service", "legacy", "", old); err != nil {
		t.Fatal(err)
	}
	if err := db.put("attribute", "legacy/meter", "watts", leaf); err != nil {
		t.Fatal(err)
	}
	if err := db.put("revision", "", "", 4); err != nil {
		t.Fatal(err)
	}
	if err := db.exec("PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	db.close()
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, found := a.Service("legacy"); found {
		t.Fatal("old service description is active")
	}
	s := chains.Service{UUID: "legacy", Name: "Legacy", ServiceType: "sensor", Devices: []chains.Device{
		{ID: "meter", Attributes: chains.Attributes("watts")},
	}}
	if _, err := a.Register(chains.Message{Source: "legacy", ID: "registration"}, s); err != nil {
		t.Fatal(err)
	}
	got, found := a.Query("legacy", "meter", "watts")
	if !found {
		t.Fatal("re-registration lost the old reading")
	}
	value := got.(Attribute)
	if value.Revision != 4 || !value.ObservedAt.Equal(stamp) || string(value.Value) != "18" {
		t.Fatalf("unexpected restored reading: %+v", value)
	}
}

func TestObserveDoesNotHoldViewLockDuringPreview(t *testing.T) {
	a, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	reg := chains.Service{UUID: "sensor", Name: "Sensor", ServiceType: "sensor", Devices: []chains.Device{{ID: "one", Attributes: chains.Attributes("value")}}}
	if _, err := a.Register(chains.Message{Source: "sensor", ID: "reg"}, reg); err != nil {
		t.Fatal(err)
	}
	if err := a.EditDashboardCatalog(func(json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"version":1,"revision":1}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	m := chains.Message{ID: "obs1", Source: "sensor", DeviceID: "one", ObservedAt: now}
	_, _, err = a.Observe(m, []chains.Observation{{DeviceID: "one", Values: map[string]json.RawMessage{"value": json.RawMessage("12")}}}, func(Update, []Change) error {
		// Readers and DashboardCatalog must not deadlock while Observe is inside preview.
		done := make(chan struct{})
		go func() {
			_ = a.Lookup("sensor/one")
			if _, _, ok := a.ServiceDevice("sensor", "one"); !ok {
				t.Errorf("ServiceDevice did not find registered device")
			}
			raw, err := a.DashboardCatalog()
			if err != nil || string(raw) != `{"version":1,"revision":1}` {
				t.Errorf("DashboardCatalog = %q, %v", raw, err)
			}
			close(done)
		}()
		select {
		case <-done:
			return nil
		case <-time.After(time.Second):
			t.Fatal("View.mu or dashboard read blocked during Observe preview")
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Lookup("sensor/one")["value"]; got != float64(12) {
		t.Fatalf("value = %v", got)
	}
}
