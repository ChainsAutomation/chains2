package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/rules"
	"chains-automation/sdk"
	"chains-automation/state"
)

func TestBatchObservation(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		broker := &memoryBroker{}
		testBatchObservation(t, broker.open)
	})
	for _, transport := range []struct{ name, env string }{{"mqtt", "CHAINS_TEST_MQTT"}, {"redis", "CHAINS_TEST_REDIS"}} {
		t.Run(transport.name, func(t *testing.T) {
			connect := realConnector(transport.env)
			if connect == nil {
				t.Skipf("set %s to run the broker test", transport.env)
			}
			testBatchObservation(t, connect)
		})
	}
}

func testBatchObservation(t *testing.T, connect connectFunc) {
	f := newFixture(t, connect)
	const kind = "amplifier.sample"
	authority, _ := f.start(t, authorityID, ":memory:", "")
	worker, _ := f.start(t, "worker", "", `
ev = Event(type="amplifier.sample", service_name=Amplifier, device_name=Main, attribute="volume", value=35)
return
`, kind, state.Updated, state.HeadType, chains.SnapshotResponse)
	observer := f.open(t, kind, chains.Changed)
	client := sdk.Client{Bus: f.open(t), Source: "amp"}
	service := chains.Service{UUID: client.Source, Name: "Amplifier", ServiceType: "amplifier",
		Events: map[string]map[string]json.RawMessage{kind: {"1": json.RawMessage(`{"type":"object"}`)}},
		Devices: []chains.Device{
			{ID: "zone1", Name: "Main", Class: "zone", Attributes: []string{"volume", "muted"}},
			{ID: "zone2", Name: "Main", Class: "zone", Attributes: []string{"volume", "muted"}},
		},
	}
	if err := client.Register(f.ctx, service); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both devices on the worker", func() bool {
		return worker.ready.Load() && len(worker.ResolveDevices(map[string]string{"service_name": "Amplifier", "device_name": "Main"})) == 2
	})
	if _, err := worker.ResolveDevice("Amplifier", "Main"); err == nil {
		t.Fatal("ambiguous name resolved as one device")
	}
	one, _ := chains.NewObservation("zone1", map[string]any{"volume": 35}, nil)
	two, _ := chains.NewObservation("zone2", map[string]any{"volume": 36, "muted": nil}, nil)
	send := func(message chains.Message) {
		t.Helper()
		if err := client.Send(f.ctx, message); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := chains.SubmitObservation(client.Source, kind, "1", []chains.Observation{one, two})
	invalid := two
	invalid.DeviceID = "missing"
	bad, _ := chains.SubmitObservation(client.Source, kind, "1", []chains.Observation{one, invalid})
	_, before := authority.view.Position()
	send(bad)
	send(first)
	send(first)
	event := readMessage(t, observer, func(m chains.Message) bool { return m.Type == kind })
	if event.Revision != before+1 || len(event.Observations) != 2 {
		t.Fatalf("expected one atomic batch after rejection: %+v", event)
	}
	eventually(t, "worker applied the complete batch", func() bool {
		snapshot := worker.view.Snapshot()
		return snapshot.Devices["amp/zone1"]["volume"].Revision == event.Revision &&
			snapshot.Devices["amp/zone2"]["volume"].Revision == event.Revision &&
			string(snapshot.Devices["amp/zone2"]["muted"].Value) == "null"
	})
	stale, _ := chains.SubmitObservation(client.Source, kind, "1", []chains.Observation{one, two})
	stale.ObservedAt = first.ObservedAt.Add(-time.Second)
	send(stale)
	next, _ := chains.SubmitObservation(client.Source, kind, "1", []chains.Observation{one, two})
	send(next)
	fresh := readMessage(t, observer, func(m chains.Message) bool { return m.Type == kind })
	if fresh.Revision != event.Revision+1 || fresh.ID != observationID(next) {
		t.Fatalf("stale or duplicate observation produced an event: %+v", fresh)
	}
	eventually(t, "equal reading refreshed its timestamp", func() bool {
		a := worker.view.Snapshot().Devices["amp/zone1"]["volume"]
		return a.Revision == fresh.Revision && a.ObservedAt.Equal(next.ObservedAt) && a.ChangedAt.Equal(first.ObservedAt)
	})
}

func TestWorkerWaitsForObservationRevision(t *testing.T) {
	f := newFixture(t, (&memoryBroker{}).open)
	observer := f.open(t, chains.Request)
	rule, err := rules.Parse("revision", `
ev = Event(type="device.observed")
if State(ev.device).power == true:
    Action("set_power", device=ev.device, value=false)
`)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := Open(f.ctx, f.open(t), Options{
		ID: "worker", AuthorityID: authorityID, Rules: []rules.Rule{rule},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	var service = fanService(false)
	worker.view.Replace(state.Snapshot{
		Epoch: "epoch", Revision: 1, Devices: map[string]map[string]state.Attribute{},
		Services: map[string]chains.Service{fanID: service},
	})
	worker.ready.Store(true)
	o, _ := chains.NewObservation(fanName, map[string]any{"power": true}, nil)
	event, _ := chains.New(chains.DeviceObserved, authorityID, nil)
	event.Observations = []chains.Observation{o}
	event.ServiceID, event.Epoch, event.Revision = fanID, "epoch", 2
	if err = worker.handle(f.ctx, event); err != nil {
		t.Fatal(err)
	}
	if len(worker.pendingEvents) != 1 || worker.State(fanID + "/" + fanName)["power"] != nil {
		t.Fatal("event bypassed its pending state revision")
	}
	patch, _ := chains.New(state.Updated, authorityID, state.Update{
		Epoch: "epoch", Revision: 2, Devices: map[string]map[string]state.Attribute{
			state.Address(fanID, fanName): {"power": {Value: json.RawMessage("true"), Revision: 2}},
		},
	})
	if err = worker.handle(f.ctx, patch); err != nil {
		t.Fatal(err)
	}
	request := readMessage(t, observer, func(m chains.Message) bool { return m.Type == chains.Request })
	if request.CausationID != event.ID {
		t.Fatalf("unexpected action: %+v", request)
	}
}

func TestOversizedChangeRejectsWholeBatch(t *testing.T) {
	f := newFixture(t, (&memoryBroker{}).open)
	authority, err := Open(f.ctx, f.open(t), Options{ID: authorityID, AuthorityID: authorityID, Database: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	service := chains.Service{UUID: "source", Name: "Source", ServiceType: "fixture", Devices: []chains.Device{
		{ID: "one", Class: "device", Attributes: []string{"value"}},
		{ID: "two", Class: "device", Attributes: []string{"value"}},
	}}
	registration, _ := chains.New(chains.Registration, service.UUID, service)
	if err := authority.handle(f.ctx, registration); err != nil {
		t.Fatal(err)
	}
	send := func(text string, other int) error {
		first, _ := chains.NewObservation("one", map[string]any{"value": text}, map[string]any{})
		second, _ := chains.NewObservation("two", map[string]any{"value": other}, nil)
		m, _ := chains.SubmitObservation(service.UUID, chains.DeviceObserved, "1", []chains.Observation{first, second})
		return authority.handle(f.ctx, m)
	}
	value := strings.Repeat("a", 550<<10)
	if err := send(value, 1); err != nil {
		t.Fatal(err)
	}
	_, before := authority.view.Position()
	if err := send(strings.Repeat("b", 550<<10), 2); !errors.Is(err, bus.ErrTooLarge) {
		t.Fatalf("expected oversized previous/new value notification, got %v", err)
	}
	_, after := authority.view.Position()
	if before != after || authority.State("source/one")["value"] != value || authority.State("source/two")["value"] != float64(1) {
		t.Fatal("oversized notification committed part of the batch")
	}
}
