package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/rules"
	"chains-automation/sdk"
	"chains-automation/state"
)

type connectFunc func(context.Context, string, bus.Subscription) (bus.Bus, error)

type memoryBroker struct {
	mu      sync.Mutex
	members map[*memoryBus]bool
}

type memoryBus struct {
	broker *memoryBroker
	out    chan bus.Delivery
	types  []string
}

func (b *memoryBroker) open(_ context.Context, _ string, sub bus.Subscription) (bus.Bus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	client := &memoryBus{broker: b, out: make(chan bus.Delivery, 8192), types: sub.Types}
	if b.members == nil {
		b.members = map[*memoryBus]bool{}
	}
	b.members[client] = true
	return client, nil
}

func (m *memoryBus) Publish(ctx context.Context, topic string, raw []byte) error {
	m.broker.mu.Lock()
	defer m.broker.mu.Unlock()
	v, err := chains.Decode(raw)
	if err != nil {
		return err
	}
	for target := range m.broker.members {
		if len(target.types) > 0 && !contains(target.types, v.Type) {
			continue
		}
		select {
		case target.out <- bus.Delivery{Topic: topic, Data: raw}:
		case <-ctx.Done():
			return ctx.Err()
		default:
			return fmt.Errorf("test broker queue full")
		}
	}
	return nil
}

func (m *memoryBus) Receive() <-chan bus.Delivery { return m.out }
func (m *memoryBus) Close() error {
	m.broker.mu.Lock()
	defer m.broker.mu.Unlock()
	if m.broker.members[m] {
		delete(m.broker.members, m)
		close(m.out)
	}
	return nil
}

func realConnector(env string) connectFunc {
	address := os.Getenv(env)
	if address == "" {
		return nil
	}
	if env == "CHAINS_TEST_MQTT" {
		return func(ctx context.Context, namespace string, sub bus.Subscription) (bus.Bus, error) {
			return bus.OpenMQTT(ctx, address, namespace, sub)
		}
	}
	return func(ctx context.Context, namespace string, sub bus.Subscription) (bus.Bus, error) {
		return bus.OpenRedis(ctx, address, namespace, sub)
	}
}

const readingKind = "humidity.reported"
const buttonKind = "button.pressed"
const sourceID = "sensor"
const authorityID = "test-authority"
const fanID = "sim-fan"
const fanName = "fan"

func sensorService() chains.Service {
	return chains.Service{
		UUID: sourceID, Name: "Sensor", ServiceType: "mqtt_sensor",
		Events: map[string]map[string]json.RawMessage{
			readingKind: {"1": json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"number"}}}`)},
			buttonKind:  {"1": json.RawMessage(`{"type":"object"}`)},
		},
		Devices: []chains.Device{{ID: "bathroom", Class: "sensor", Attributes: []string{"humidity"}}},
	}
}

func fanService(idempotent bool) chains.Service {
	s := chains.Service{UUID: fanID, Name: "Fan", ServiceType: "sim_fan",
		Actions: map[string]map[string]json.RawMessage{
			"set_power": {"1": json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"boolean"}}}`)},
		},
		Devices: []chains.Device{{ID: fanName, Name: "fan", Class: "fan", Actions: []string{"set_power"}, Attributes: []string{"power"}}},
	}
	if idempotent {
		s.IdempotentActions = []string{"set_power"}
	}
	return s
}

type fixture struct {
	connect   connectFunc
	namespace string
	ctx       context.Context
}

func newFixture(t testing.TB, connect connectFunc) fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return fixture{connect: connect, namespace: "test-" + chains.ID(), ctx: ctx}
}

func (f fixture) open(t testing.TB, types ...string) bus.Bus {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	b, err := f.connect(ctx, f.namespace, bus.Subscription{Types: types})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func (f fixture) start(t testing.TB, id, db, source string, kinds ...string) (*Engine, func()) {
	t.Helper()
	b := f.open(t, kinds...)
	ctx, cancel := context.WithCancel(f.ctx)
	var list []rules.Rule
	if source != "" {
		rule, err := rules.Parse(id, source)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		list = []rules.Rule{rule}
	}
	e, err := Open(ctx, b, Options{ID: id, AuthorityID: authorityID, Database: db, Rules: list, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ActionTimeout: 400 * time.Millisecond})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Run(ctx) }()
	stop := sync.OnceFunc(func() { cancel(); <-done; e.Close(); _ = b.Close() })
	t.Cleanup(stop)
	return e, stop
}

func eventually(t testing.TB, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

func readMessage(t testing.TB, b bus.Bus, condition func(chains.Message) bool) chains.Message {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case d, ok := <-b.Receive():
			if !ok {
				t.Fatal("observer closed")
			}
			if d.Reset {
				continue
			}
			m, err := chains.Decode(d.Data)
			if err != nil {
				t.Fatal(err)
			}
			if condition(m) {
				return m
			}
		case <-deadline:
			t.Fatal("timed out waiting for a message")
		}
	}
}

func TestAutomation(t *testing.T) {
	t.Run("memory", func(t *testing.T) { b := &memoryBroker{}; testAutomation(t, b.open) })
	for _, pair := range []struct{ name, env string }{{"mqtt", "CHAINS_TEST_MQTT"}, {"redis", "CHAINS_TEST_REDIS"}} {
		t.Run(pair.name, func(t *testing.T) {
			connect := realConnector(pair.env)
			if connect == nil {
				t.Skipf("set %s to run the broker test", pair.env)
			}
			testAutomation(t, connect)
		})
	}
}

func testAutomation(t *testing.T, connect connectFunc) {
	const automation = `
reading = Event(type="humidity.reported", service_type=mqtt_sensor)
fan = Device(service_id="sim-fan", device_id="fan")
if reading.data.value >= 70 and State(fan).power == false:
    Action("set_power", device=fan, value=true)
`
	f := newFixture(t, connect)
	db := filepath.Join(t.TempDir(), "authority.db")
	observer := f.open(t)
	authority, stopAuthority := f.start(t, authorityID, db, `
Event(type="button.pressed")
Action("set_power", service_name=Fan, device_name=fan, value=false)
`)
	worker, _ := f.start(t, "worker", "", ""+automation, readingKind, chains.Changed, state.Updated, state.HeadType, chains.SnapshotResponse, chains.Outcome)
	sensor := sdk.Client{Bus: f.open(t), Source: sourceID, ServiceType: "mqtt_sensor", Encoding: chains.CBOR}
	fan := sdk.Client{Bus: f.open(t, chains.Request), Source: fanID, ServiceType: "sim_fan"}
	if err := sensor.Register(f.ctx, sensorService()); err != nil {
		t.Fatal(err)
	}
	if err := fan.Register(f.ctx, fanService(true)); err != nil {
		t.Fatal(err)
	}
	if err := fan.Report(f.ctx, fanName, map[string]any{"power": false}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker state and registry", func() bool {
		_, ok := worker.view.Service(sourceID)
		return ok && worker.ready.Load() && worker.State("sim-fan/fan")["power"] == false
	})

	if err := sensor.Report(f.ctx, "bathroom", map[string]any{"humidity": 65}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first reading", func() bool { return authority.State("sensor/bathroom")["humidity"] == float64(65) })
	first := authority.view.Snapshot().Devices["sensor/bathroom"]["humidity"]
	if err := sensor.Report(f.ctx, "bathroom", map[string]any{"humidity": 65}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "observed timestamp refresh", func() bool {
		current := worker.view.Snapshot().Devices["sensor/bathroom"]["humidity"]
		return current.ObservedAt.After(first.ObservedAt) && current.ChangedAt.Equal(first.ChangedAt)
	})
	if err := sensor.Event(f.ctx, buttonKind, "bathroom", "1", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	_, beforePress := authority.view.Position()
	pressRequest := readMessage(t, fan.Bus, func(m chains.Message) bool { return m.Type == chains.Request })
	var pressAction chains.Action
	if err := json.Unmarshal(pressRequest.Data, &pressAction); err != nil || pressAction.Args["value"] != false {
		t.Fatalf("button action: %v, %v", pressAction, err)
	}
	if err := fan.Outcome(f.ctx, pressRequest, chains.Completed, nil); err != nil {
		t.Fatal(err)
	}
	if _, afterPress := authority.view.Position(); beforePress != afterPress {
		t.Fatal("button press mutated reported state")
	}
	if err := sensor.Event(f.ctx, readingKind, "bathroom", "1", map[string]any{"value": 75}); err != nil {
		t.Fatal(err)
	}
	request := readMessage(t, fan.Bus, func(m chains.Message) bool { return m.Type == chains.Request })
	if request.Target != fanID || request.ActionID == "" || request.CausationID == "" {
		t.Fatalf("missing action metadata: %+v", request)
	}
	if err := fan.Outcome(f.ctx, request, chains.Accepted, nil); err != nil {
		t.Fatal(err)
	}
	if err := fan.Outcome(f.ctx, request, chains.Completed, nil); err != nil {
		t.Fatal(err)
	}
	if got := authority.State("sim-fan/fan")["power"]; got != false {
		t.Fatalf("outcome modified reported power: %v", got)
	}
	if err := fan.Report(f.ctx, fanName, map[string]any{"power": true}, &request); err != nil {
		t.Fatal(err)
	}
	changed := readMessage(t, observer, func(m chains.Message) bool { return m.Type == chains.Changed && m.ActionID == request.ActionID })
	if changed.CausationID == request.ID || changed.CorrelationID != request.CorrelationID {
		t.Fatalf("invalid causal chain: %+v", changed)
	}
	eventually(t, "worker power update", func() bool { return worker.State("sim-fan/fan")["power"] == true })
	before, _ := worker.view.Position()
	stopAuthority()
	if err := sensor.Event(f.ctx, readingKind, "bathroom", "1", map[string]any{"value": 90}); err != nil {
		t.Fatal(err)
	}
	newAuthority, _ := f.start(t, authorityID, db, "")
	eventually(t, "worker recovery after authority restart", func() bool {
		epoch, _ := worker.view.Position()
		return epoch != before && worker.ready.Load() && worker.State("sim-fan/fan")["power"] == true
	})
	if got := newAuthority.State("sensor/bathroom")["humidity"]; got != float64(65) {
		t.Fatalf("restored humidity %v", got)
	}
}

func TestActionRetryAndLateResult(t *testing.T) {
	broker := &memoryBroker{}
	f := newFixture(t, broker.open)
	authority, _ := f.start(t, authorityID, ":memory:", "")
	fan := sdk.Client{Bus: f.open(t, chains.Request), Source: fanID}
	if err := fan.Register(f.ctx, fanService(true)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "fan registration", func() bool { _, ok := authority.view.Service(fanID); return ok })
	parent, _ := chains.New(buttonKind, sourceID, nil)
	result := make(chan map[string]any, 1)
	errResult := make(chan error, 1)
	go func() {
		r, err := authority.Act(f.ctx, parent, rules.DeviceRef{ServiceID: fanID, DeviceID: fanName}, "set_power", "1", map[string]any{"value": true})
		result <- r
		errResult <- err
	}()
	isRequest := func(m chains.Message) bool { return m.Type == chains.Request }
	first := readMessage(t, fan.Bus, isRequest)
	retry := readMessage(t, fan.Bus, isRequest)
	if retry.ID != first.ID || retry.ActionID != first.ActionID {
		t.Fatalf("retry changed action identity")
	}
	if err := fan.Outcome(f.ctx, first, chains.Accepted, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-result:
		if err := <-errResult; err != nil || r["status"] != chains.TimedOut {
			t.Fatalf("result = %v, %v", r, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("action did not time out")
	}
	if err := fan.Outcome(f.ctx, first, chains.Completed, nil); err != nil {
		t.Fatal(err)
	}
	if err := fan.Report(f.ctx, fanName, map[string]any{"power": true}, &first); err != nil {
		t.Fatal(err)
	}
	eventually(t, "late report", func() bool { return authority.State("sim-fan/fan")["power"] == true })
}

func BenchmarkSensorToAction(b *testing.B) {
	for _, pair := range []struct{ name, env string }{{"mqtt", "CHAINS_TEST_MQTT"}, {"redis", "CHAINS_TEST_REDIS"}} {
		b.Run(pair.name, func(b *testing.B) {
			connect := realConnector(pair.env)
			if connect == nil {
				b.Skipf("set %s", pair.env)
			}
			benchmarkSensorToAction(b, connect)
		})
	}
}

func benchmarkSensorToAction(b *testing.B, connect connectFunc) {
	const automation = `
rule.concurrency = 1024
Event(type="humidity.reported")
Action("set_power", device=Device(service_id="sim-fan", device_id=fan), value=true)
`
	f := newFixture(b, connect)
	authority, _ := f.start(b, authorityID, ":memory:", "")
	worker, _ := f.start(b, "worker", "", automation, readingKind, chains.Changed, state.Updated, state.HeadType, chains.SnapshotResponse, chains.Outcome)
	sensor := sdk.Client{Bus: f.open(b), Source: sourceID, ServiceType: "mqtt_sensor", Encoding: chains.CBOR}
	fan := sdk.Client{Bus: f.open(b, chains.Request), Source: fanID}
	if err := sensor.Register(f.ctx, sensorService()); err != nil {
		b.Fatal(err)
	}
	if err := fan.Register(f.ctx, fanService(false)); err != nil {
		b.Fatal(err)
	}
	eventually(b, "registration views", func() bool {
		_, a := authority.view.Service(fanID)
		_, w := worker.view.Service(fanID)
		_, s := worker.view.Service(sourceID)
		return a && w && s && worker.ready.Load()
	})
	timings := make([]time.Duration, 0, b.N)
	peakBacklog := 0
	b.ResetTimer()
	for start := 0; start < b.N; start += 32 {
		sent := map[string]time.Time{}
		for index := start; index < min(start+32, b.N); index++ {
			m, _ := chains.New(readingKind, sourceID, map[string]any{"value": 75})
			m.DeviceID, m.SchemaVersion = "bathroom", "1"
			sent[m.ID] = time.Now()
			if err := sensor.Send(f.ctx, m); err != nil {
				b.Fatal(err)
			}
			peakBacklog = max(peakBacklog, len(worker.node.Bus.Receive()))
		}
		for len(sent) > 0 {
			request := readMessage(b, fan.Bus, func(m chains.Message) bool { return m.Type == chains.Request })
			if at, ok := sent[request.CausationID]; ok {
				timings = append(timings, time.Since(at))
				delete(sent, request.CausationID)
			}
			if err := fan.Outcome(f.ctx, request, chains.Completed, nil); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	sort.Slice(timings, func(i, j int) bool { return timings[i] < timings[j] })
	b.ReportMetric(float64(peakBacklog), "peak-queued-deliveries")
	b.ReportMetric(float64(timings[len(timings)/2])/float64(time.Millisecond), "p50-ms")
	b.ReportMetric(float64(timings[(len(timings)-1)*99/100])/float64(time.Millisecond), "p99-ms")
}
