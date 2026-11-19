package sdk

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
)

type testTransport struct {
	inbound  chan bus.Delivery
	outbound chan chains.Message
}

func (b *testTransport) Publish(ctx context.Context, topic string, raw []byte) error {
	m, err := chains.Decode(raw)
	if err != nil {
		return err
	}
	select {
	case b.outbound <- m:
	case <-ctx.Done():
		return ctx.Err()
	}
	if m.Type == chains.StateQuery {
		reply, _ := chains.New(chains.StateReply, m.Target, map[string]any{"found": true, "state": map[string]any{"value": 42}})
		reply.Target, reply.CausationID = m.Source, m.ID
		b.inject(reply)
	}
	return nil
}
func (b *testTransport) Receive() <-chan bus.Delivery { return b.inbound }
func (b *testTransport) Close() error                 { return nil }
func (b *testTransport) inject(m chains.Message) {
	raw, _ := chains.Encode(m, chains.JSON)
	b.inbound <- bus.Delivery{Topic: bus.Route(m), Data: raw}
}
func (b *testTransport) expect(t *testing.T, kind string, match func(chains.Message) bool) chains.Message {
	t.Helper()
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	for {
		select {
		case m := <-b.outbound:
			if m.Type == kind && (match == nil || match(m)) {
				return m
			}
		case <-timer.C:
			t.Fatalf("missing %s", kind)
		}
	}
}

func TestManagedService(t *testing.T) {
	b := &testTransport{make(chan bus.Delivery, 128), make(chan chains.Message, 128)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := Config{URI: "mqtt://example", Namespace: "chains", Instance: "fixture", Name: "Fixture", Authority: "engine", Encoding: chains.CBOR,
		ServiceID: "fixture", HeartbeatInterval: 1, PollInterval: .01}
	var calls atomic.Int32
	polling := make(chan struct{}, 1)
	reconnect := make(chan struct{}, 1)
	lifecycle := make(chan string, 4)
	definition := Definition{
		Service: chains.Service{ServiceType: "fixture", Devices: []chains.Device{{ID: "default", Name: "Example", Class: "device", Attributes: []string{"power"}, Actions: []string{"inspect"}}},
			Actions: map[string]map[string]json.RawMessage{"inspect": {"1": json.RawMessage(`{"type":"object","additionalProperties":false}`)}}},
		Actions: map[string]map[string]ActionHandler{"inspect": {"1": func(ctx context.Context, r *Runtime, _ chains.Message, _ chains.Action) (any, error) {
			calls.Add(1)
			result, err := r.QueryState(ctx, "other", "default", "power")
			return result.State, err
		}}},
		OpenBus: func(_ context.Context, _ Config, sub bus.Subscription) (bus.Bus, error) {
			if len(sub.Types) != 2 || sub.Types[0] != chains.Request || sub.Types[1] != chains.StateReply {
				t.Errorf("subscriptions: %+v", sub)
			}
			return b, nil
		},
		Hooks: Hooks{
			OnInit: func(_ context.Context, r *Runtime) error {
				lifecycle <- "init"
				return r.SeedState("", time.Time{}, map[string]any{"power": true})
			},
			OnStart:     func(_ context.Context, _ *Runtime) error { lifecycle <- "start"; return nil },
			OnPoll:      func(ctx context.Context, _ *Runtime) error { polling <- struct{}{}; <-ctx.Done(); return nil },
			OnReconnect: func(_ context.Context, _ *Runtime) error { reconnect <- struct{}{}; return nil },
			OnShutdown:  func(_ context.Context, _ *Runtime) error { lifecycle <- "shutdown"; return nil },
		},
	}
	done := make(chan error, 1)
	go func() { done <- RunService(ctx, c, definition) }()
	if <-lifecycle != "init" || <-lifecycle != "start" {
		t.Fatal("lifecycle startup order")
	}
	select {
	case <-polling:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	b.expect(t, chains.Registration, nil)
	if m := b.expect(t, chains.Report, nil); !m.Silent {
		t.Fatal("seed entered live stream")
	}
	first := b.expect(t, chains.Heartbeat, nil)
	request, _ := chains.New(chains.Request, "engine", chains.Action{Name: "inspect", Args: map[string]any{}})
	request.Target, request.DeviceID, request.SchemaVersion, request.ActionID = "fixture", "default", "1", "a1"
	completed := func(m chains.Message) bool {
		var result chains.Result
		if json.Unmarshal(m.Data, &result) != nil || result.Status != chains.Completed {
			return false
		}
		return string(result.Value) == `{"value":42}`
	}
	b.inject(request)
	b.expect(t, chains.Outcome, completed)
	request.ID = chains.ID()
	b.inject(request)
	b.expect(t, chains.Outcome, completed)
	if calls.Load() != 1 {
		t.Fatal("retried request executed twice")
	}
	b.inbound <- bus.Delivery{Reset: true}
	b.expect(t, chains.Registration, nil)
	if m := b.expect(t, chains.Heartbeat, nil); string(m.Data) != string(first.Data) {
		t.Fatal("instance changed on reconnect")
	}
	if m := b.expect(t, chains.Report, nil); !m.Silent {
		t.Fatal("reconnect replay was live")
	}
	select {
	case <-reconnect:
	case <-time.After(time.Second):
		t.Fatal("missing reconnect hook")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown hung")
	}
	if <-lifecycle != "shutdown" {
		t.Fatal("missing cleanup")
	}
}
