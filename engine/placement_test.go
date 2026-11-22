package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	chains "chains-automation"
	"chains-automation/sdk"
	"chains-automation/state"
)

func TestPlacementOverridesAndWorkerCatalog(t *testing.T) {
	f := newFixture(t, (&memoryBroker{}).open)
	path := filepath.Join(t.TempDir(), "state.db")
	e, stop := f.start(t, authorityID, path, "")
	w, _ := f.start(t, "worker", "", "")
	client := sdk.Client{Bus: f.open(t), Source: "fixture"}
	observer := f.open(t, chains.DeviceObserved, chains.Changed)
	s := chains.Service{UUID: client.Source, Name: "Fixture", ServiceType: "fixture", RetainDevices: true, Devices: []chains.Device{
		{ID: "one", Placement: chains.Placement{Location: []string{"House", "Bedroom"}, Tags: []string{"battery"}}, Attributes: chains.Attributes("value")},
	}}
	if err := client.Register(f.ctx, s); err != nil {
		t.Fatal(err)
	}
	eventually(t, "initial worker default", func() bool { s, ok := w.view.Service(client.Source); return ok && len(s.Devices) == 1 })
	if err := client.Report(f.ctx, "one", map[string]any{"value": 2}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "initial observation", func() bool { return e.State("fixture/one")["value"] == 2.0 })
	previous := readMessage(t, observer, func(m chains.Message) bool { return m.Type == chains.DeviceObserved })
	if !slices.Equal(previous.Observations[0].Location, []string{"House", "Bedroom"}) {
		t.Fatalf("original observation has wrong location: %+v", previous)
	}
	original := e.view.Snapshot().Devices["fixture/one"]["value"]
	apply := func(location, tags string) {
		t.Helper()
		editor := placementEdit{context.Background(), "fixture", "one", json.RawMessage(location), json.RawMessage(tags), nil, make(chan error, 1)}
		e.placementEdits <- editor
		if err := <-editor.done; err != nil {
			t.Fatal(err)
		}
	}
	apply(`["House","Office","Shelf.1"]`, `["guest","guest"]`)
	eventually(t, "worker received placement", func() bool {
		s, ok := w.view.Service(client.Source)
		return ok && len(s.Devices) == 1 && slices.Equal(s.Devices[0].Location, []string{"House", "Office", "Shelf.1"}) && slices.Equal(s.Devices[0].Tags, []string{"guest"})
	})
	filter, _ := chains.ParsePlacementFilter(map[string]any{"location": []string{"**", "Shelf.1"}, "tag": "guest"})
	if len(w.ResolveDevices(nil, filter)) != 1 {
		t.Fatal("worker filter missed device")
	}
	if after := e.view.Snapshot().Devices["fixture/one"]["value"]; after.Revision != original.Revision || !after.ObservedAt.Equal(original.ObservedAt) {
		t.Fatal("placement affected observation")
	}
	if err := client.Report(f.ctx, "one", map[string]any{"value": 3}, nil); err != nil {
		t.Fatal(err)
	}
	next := readMessage(t, observer, func(m chains.Message) bool { return m.Type == chains.DeviceObserved })
	leaf := readMessage(t, observer, func(m chains.Message) bool { return m.Type == chains.Changed && m.Revision == next.Revision })
	if !filter.Match(next.Observations[0].Placement) || !filter.Match(leaf.Placement) || filter.Match(previous.Observations[0].Placement) {
		t.Fatal("committed observations did not retain their placement at commit")
	}
	s.Devices[0].Location = []string{"House", "Bedroom", "Drawer"}
	if err := client.Register(f.ctx, s); err != nil {
		t.Fatal(err)
	}
	eventually(t, "updated default", func() bool {
		s, _ := e.authority.DefaultService(client.Source)
		return len(s.Devices) == 1 && len(s.Devices[0].Location) == 3
	})
	refresh := s
	refresh.Devices = []chains.Device{{ID: "two"}}
	if err := client.Register(f.ctx, refresh); err != nil {
		t.Fatal(err)
	}
	eventually(t, "retained device default", func() bool {
		s, _ := e.authority.DefaultService(client.Source)
		return len(s.Devices) == 2
	})
	stop()
	a, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	got, _ := a.Service(client.Source)
	retained, _ := device(got, "one")
	if !slices.Equal(retained.Tags, []string{"guest"}) {
		t.Fatal("lost override")
	}
	update, err := a.EditPlacement("fixture", "one", func(state.PlacementOverride) (state.PlacementOverride, error) {
		empty := []string{}
		return state.PlacementOverride{Tags: &empty}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reset, _ := device(*update.Service, "one")
	if !slices.Equal(reset.Location, s.Devices[0].Location) || len(reset.Tags) != 0 {
		t.Fatalf("reset defaults or explicit empty failed: %+v %v", update.Service, err)
	}
}
