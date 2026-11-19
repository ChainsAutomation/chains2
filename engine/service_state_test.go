package engine

import (
	"encoding/json"
	"testing"
	"time"

	chains "chains-automation"
	"chains-automation/sdk"
	"chains-automation/state"
)

func TestSilentReportAndStateQuery(t *testing.T) {
	broker := &memoryBroker{}
	f := newFixture(t, broker.open)
	e, _ := f.start(t, authorityID, ":memory:", "")
	client := sdk.Client{Bus: f.open(t), Source: sourceID, ServiceType: "mqtt_sensor"}
	if err := client.Register(f.ctx, sensorService()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "registration", func() bool { _, ok := e.view.Service(client.Source); return ok })
	observed := f.open(t, state.Updated, chains.Changed, chains.StateReply)
	old := time.Now().Add(-time.Hour)
	report, _ := chains.New(chains.Report, client.Source, map[string]any{"humidity": 42})
	report.DeviceID, report.Silent, report.ObservedAt = "bathroom", true, old
	if err := client.Send(f.ctx, report); err != nil {
		t.Fatal(err)
	}
	readMessage(t, observed, func(m chains.Message) bool { return m.Type == state.Updated })
	request, _ := chains.New(chains.StateQuery, client.Source, map[string]any{"service_id": client.Source, "device_id": "bathroom", "attribute": "humidity"})
	request.Target = authorityID
	if err := client.Send(f.ctx, request); err != nil {
		t.Fatal(err)
	}
	reply := readMessage(t, observed, func(m chains.Message) bool {
		if m.Type == chains.Changed {
			t.Fatal("silent synchronization triggered state.changed")
		}
		return m.Type == chains.StateReply
	})
	var result struct {
		Found bool
		State state.Attribute
	}
	if err := json.Unmarshal(reply.Data, &result); err != nil || !result.Found || string(result.State.Value) != "42" || !result.State.ObservedAt.Equal(old) || reply.CausationID != request.ID {
		t.Fatalf("query: %s, %v", reply.Data, err)
	}
	if err := client.Report(f.ctx, "bathroom", map[string]any{"humidity": 43}, nil); err != nil {
		t.Fatal(err)
	}
	readMessage(t, observed, func(m chains.Message) bool { return m.Type == chains.Changed })
	report.ID = chains.ID()
	if err := client.Send(f.ctx, report); err != nil {
		t.Fatal(err)
	}
	request.ID = chains.ID()
	if err := client.Send(f.ctx, request); err != nil {
		t.Fatal(err)
	}
	reply = readMessage(t, observed, func(m chains.Message) bool { return m.Type == chains.StateReply })
	if err := json.Unmarshal(reply.Data, &result); err != nil || string(result.State.Value) != "43" {
		t.Fatalf("stale seed overwrote live state: %s", reply.Data)
	}
}

func TestHeartbeatAvailabilityTransitions(t *testing.T) {
	broker := &memoryBroker{}
	f := newFixture(t, broker.open)
	b := f.open(t)
	e, err := Open(f.ctx, b, Options{ID: authorityID, AuthorityID: authorityID, Database: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	source, _ := chains.New(chains.Registration, sourceID, sensorService())
	if err = e.handle(f.ctx, source); err != nil {
		t.Fatal(err)
	}
	observer := f.open(t, chains.AvailabilityChanged)
	now := time.Now()
	ping, _ := chains.New(chains.Heartbeat, sourceID, map[string]any{"instance_id": "boot-1", "interval_seconds": 1})
	if err = e.heartbeat(f.ctx, ping, now); err != nil {
		t.Fatal(err)
	}
	read := func(want bool) {
		m := readMessage(t, observer, func(m chains.Message) bool { return m.Type == chains.AvailabilityChanged })
		var status availability
		if json.Unmarshal(m.Data, &status) != nil || status.Online != want || m.ServiceID != sourceID {
			t.Fatalf("wrong availability: %+v", m)
		}
	}
	read(true)
	e.expirePresence(f.ctx, now.Add(3*time.Second-time.Nanosecond))
	select {
	case <-observer.Receive():
		t.Fatal("expired too early")
	default:
	}
	e.expirePresence(f.ctx, now.Add(3*time.Second))
	read(false)
	next, _ := chains.New(chains.Heartbeat, sourceID, map[string]any{"instance_id": "boot-2", "interval_seconds": 1})
	next.ObservedAt = ping.ObservedAt.Add(time.Second)
	if err = e.heartbeat(f.ctx, next, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	read(true)
	if err = e.heartbeat(f.ctx, ping, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if e.availability[sourceID].instance != "boot-2" {
		t.Fatal("late heartbeat displaced current instance")
	}
}
