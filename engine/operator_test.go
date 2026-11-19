package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/operator"
	"chains-automation/rules"
	"chains-automation/sdk"
	"chains-automation/state"
)

func TestOperator(t *testing.T) {
	for _, target := range []struct{ name, env string }{{"memory", ""}, {"mqtt", "CHAINS_TEST_MQTT"}, {"redis", "CHAINS_TEST_REDIS"}} {
		t.Run(target.name, func(t *testing.T) {
			var connect connectFunc
			if target.env == "" {
				connect = (&memoryBroker{}).open
			} else if connect = realConnector(target.env); connect == nil {
				t.Skip("set " + target.env)
			}
			testOperator(t, connect)
		})
	}
}

func testOperator(t *testing.T, connect connectFunc) {
	f := newFixture(t, connect)
	observed := make(chan struct{}, 1)
	opener := func(ctx context.Context, sub bus.Subscription) (bus.Bus, error) {
		b, err := f.connect(ctx, f.namespace, sub)
		if err == nil && len(sub.Types) == 0 {
			select {
			case observed <- struct{}{}:
			default:
			}
		}
		return b, err
	}
	start := func(b bus.Bus, o Options) *Engine {
		t.Helper()
		ctx, cancel := context.WithCancel(f.ctx)
		e, err := Open(ctx, b, o)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { defer close(done); _ = e.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done; e.Close() })
		return e
	}
	// The authority can omit the owner's application event types from its normal subscription.
	e := start(f.open(t, chains.Registration, chains.Report, chains.Heartbeat, chains.SnapshotRequest), Options{
		ID: authorityID, AuthorityID: authorityID, Database: ":memory:", OperatorAPI: true, OpenOperatorBus: opener,
	})
	workerRule, err := rules.Parse("manual-fan", `Event(type="manual.test")
Action("set_power", service_name=Fan, device_name=fan, value=true)
`)
	if err != nil {
		t.Fatal(err)
	}
	worker := start(f.open(t, "manual.test", chains.Outcome, chains.SnapshotResponse, state.Updated, state.HeadType), Options{
		ID: "operator-worker", AuthorityID: authorityID, Rules: []rules.Rule{workerRule}, OperatorTrace: true, OpenOperatorBus: opener,
	})
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not start")
	}
	handler := e.HTTP("token", map[string]HTTPSource{"humidity": {Source: sourceID, Device: "bathroom", Type: readingKind, SchemaVersion: "1", Attribute: "value"}})
	server := httptest.NewUnstartedServer(handler)
	server.Config.WriteTimeout = 10 * time.Second
	server.Start()
	defer server.Close()

	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	client, err := operator.NewClient(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := operator.NewClient(server.URL, "wrong")
	if _, err = wrong.State(ctx, nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong token: %v", err)
	}

	sensor := sdk.Client{Bus: f.open(t), Source: sourceID, Encoding: chains.CBOR}
	fan := sdk.Client{Bus: f.open(t, chains.Request), Source: fanID}
	if err = sensor.Register(ctx, sensorService()); err != nil {
		t.Fatal(err)
	}
	if err = fan.Register(ctx, fanService(false)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker registry", func() bool { _, ok := worker.view.Service(fanID); return worker.ready.Load() && ok })
	for _, instance := range []string{"boot-1", "boot-2"} {
		ping, _ := chains.New(chains.Heartbeat, sourceID, map[string]any{"instance_id": instance, "interval_seconds": 30})
		if err = sensor.Send(ctx, ping); err != nil {
			t.Fatal(err)
		}
	}
	if err = sensor.Report(ctx, "bathroom", map[string]any{"humidity": 72.5}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "state report", func() bool { return e.State("sensor/bathroom")["humidity"] == 72.5 })
	snapshot, err := client.State(ctx, []string{sourceID, "bathroom", "humidity"})
	if err != nil || len(snapshot.Rows) != 1 || string(snapshot.Rows[0].Value) != "72.5" || snapshot.Rows[0].Changed == nil || snapshot.Rows[0].Observed == nil {
		t.Fatalf("snapshot %+v, %v", snapshot, err)
	}
	profiles, err := client.Profiles(ctx)
	if err != nil || len(profiles) != 1 || !profiles[0].Ready || string(profiles[0].Schema) != `{"type":"number"}` {
		t.Fatalf("profiles %+v, %v", profiles, err)
	}
	if _, err = client.Send(ctx, operator.Submission{Profile: "humidity", Data: json.RawMessage(`"bad"`)}); err == nil {
		t.Fatal("invalid profile payload accepted")
	}
	if _, err = client.Send(ctx, operator.Submission{Name: "test", Data: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("invalid manual payload accepted")
	}
	profile, err := client.Send(ctx, operator.Submission{Profile: "humidity", Data: json.RawMessage(`73.1`)})
	if err != nil || profile.Source != sourceID || profile.Type != readingKind {
		t.Fatalf("profile receipt %+v, %v", profile, err)
	}
	manual, err := client.Send(ctx, operator.Submission{Name: "test", Data: json.RawMessage(`{"operator":true}`)})
	if err != nil || manual.Source != authorityID || manual.Type != "manual.test" {
		t.Fatalf("manual receipt %+v, %v", manual, err)
	}
	request := readMessage(t, fan.Bus, func(m chains.Message) bool { return m.Type == chains.Request })
	if request.CausationID != manual.ID || request.Source != "operator-worker" {
		t.Fatalf("action request %+v", request)
	}
	for _, status := range []string{chains.Accepted, chains.Completed} {
		if err = fan.Outcome(ctx, request, status, nil); err != nil {
			t.Fatal(err)
		}
	}

	complete := errors.New("observed all activity")
	needed := map[string]bool{readingKind: true, "manual.test": true, chains.Registration: true, chains.Heartbeat: true, "service.restarted": true,
		chains.Changed: true, chains.Request: true, chains.Outcome: true, "rule.started": true, "rule.finished": true}
	var run, finished string
	err = client.Watch(ctx, operator.HistorySize, func(record operator.Record) error {
		switch record.Type {
		case readingKind:
			if record.Event != profile.ID || string(record.Data) != `{"value":73.1}` {
				return nil
			}
		case "manual.test":
			if record.Event != manual.ID {
				return nil
			}
		case "rule.started", "rule.finished":
			if record.Engine != "operator-worker" || record.Rule != "manual-fan" || record.Correlation != manual.ID || record.Run == "" {
				t.Errorf("rule trace %+v", record)
			}
			if record.Type == "rule.started" {
				run = record.Run
			} else {
				finished = record.Run
			}
		case chains.Request:
			if record.Action != request.ActionID || record.Cause != manual.ID || record.Service != fanID {
				t.Errorf("watch action %+v", record)
			}
		case chains.Outcome:
			if record.Summary != chains.Completed {
				return nil
			}
		}
		delete(needed, record.Type)
		if len(needed) == 0 {
			return complete
		}
		return nil
	})
	if !errors.Is(err, complete) || run == "" || run != finished {
		t.Fatalf("watch missing %v, start %q finish %q: %v", needed, run, finished, err)
	}
}

func TestOperatorDisabled(t *testing.T) {
	broker := &memoryBroker{}
	f := newFixture(t, broker.open)
	e, _ := f.start(t, authorityID, ":memory:", "")
	req := httptest.NewRequest(http.MethodGet, operator.Base+"/state", nil)
	w := httptest.NewRecorder()
	e.HTTP("token", nil).ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled API returned %d", w.Code)
	}
}
