package keyboard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/sdk"
)

var ctrl = Key{29, "KEY_LEFTCTRL"}
var f13 = Key{183, "KEY_F13"}

type readStep struct {
	input Input
	held  []Key
	err   error
}
type fixtureDevice struct {
	held   []Key
	steps  chan readStep
	closes atomic.Int32
}

func (d *fixtureDevice) Held() []Key  { return d.held }
func (d *fixtureDevice) Close() error { d.closes.Add(1); return nil }
func (d *fixtureDevice) Next(ctx context.Context) (Input, error) {
	select {
	case <-ctx.Done():
		return Input{}, ctx.Err()
	case step := <-d.steps:
		if step.input.Resync {
			d.held = step.held
		}
		return step.input, step.err
	}
}

func TestKeyboardTransitions(t *testing.T) {
	d := &fixtureDevice{held: []Key{ctrl}, steps: make(chan readStep, 10)}
	steps := []readStep{
		{input: Input{Key: f13, Value: 1}},
		{input: Input{Key: f13, Value: 2}},
		{input: Input{Key: f13, Value: 0}},
		{input: Input{Resync: true}, held: []Key{f13}},
		{input: Input{Key: ctrl, Value: 0}},
		{input: Input{Key: f13, Value: 0}},
		{err: errors.New("unplug")},
	}
	for _, step := range steps {
		d.steps <- step
	}
	var got []Transition
	if err := read(context.Background(), d, func(t Transition) error { got = append(got, t); return nil }); err == nil {
		t.Fatal("missing unplug")
	}
	want := []Transition{
		{"press", 183, "KEY_F13", []string{"KEY_F13"}, []string{"KEY_LEFTCTRL"}},
		{"release", 183, "KEY_F13", []string{}, []string{"KEY_LEFTCTRL"}},
		{"release", 183, "KEY_F13", []string{}, []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("transitions %+v; want %+v", got, want)
	}
}

func TestReopenAndCloseOnCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first := &fixtureDevice{steps: make(chan readStep, 1)}
	first.steps <- readStep{err: errors.New("unplug")}
	second := &fixtureDevice{steps: make(chan readStep, 1)}
	second.steps <- readStep{input: Input{Key: f13, Value: 1}}
	var opened int
	run(ctx, Settings{"/dev/input/automation", true, .01}, func(path string, grab bool) (Device, error) {
		if path != "/dev/input/automation" || !grab {
			t.Error("device path or grab changed")
		}
		opened++
		if opened == 1 {
			return first, nil
		}
		return second, nil
	}, func(Transition) error { cancel(); return nil })
	if opened != 2 || first.closes.Load() != 1 || second.closes.Load() != 1 {
		t.Fatal("missing reopen or cleanup")
	}
}

func TestManagedKeyboardOnBroker(t *testing.T) {
	for _, target := range []struct{ name, variable string }{{"mqtt", "CHAINS_TEST_MQTT"}, {"redis", "CHAINS_TEST_REDIS"}} {
		t.Run(target.name, func(t *testing.T) {
			address := os.Getenv(target.variable)
			if address == "" {
				t.Skip("set " + target.variable)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			c := sdk.Config{URI: address, Namespace: "keyboard-test-" + chains.ID(), Encoding: chains.CBOR, Instance: "fixture", Authority: "engine", Name: "Fixture keyboard", ServiceID: "keyboard-fixture", HeartbeatInterval: 1}
			observer, err := sdk.OpenTransport(ctx, c, bus.Subscription{Types: []string{chains.Registration, chains.ObservationSubmitted}})
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close()
			fake := &fixtureDevice{steps: make(chan readStep, 1)}
			def := definition(Settings{"/dev/input/fixture", false, .01}, func(string, bool) (Device, error) { return fake, nil })
			done := make(chan error, 1)
			go func() { done <- sdk.RunService(ctx, c, def) }()
			defer func() { cancel(); <-done }()
			var registered bool
			for {
				select {
				case <-ctx.Done():
					t.Fatal("fixture keyboard did not publish")
				case d := <-observer.Receive():
					m, err := chains.Decode(d.Data)
					if err != nil {
						t.Fatal(err)
					}
					switch m.Type {
					case chains.Registration:
						registered = true
						fake.steps <- readStep{input: Input{Key: f13, Value: 1}}
					case chains.ObservationSubmitted:
						var batch chains.ObservationBatch
						if err := json.Unmarshal(m.Data, &batch); err != nil || batch.EventType != Event || len(batch.Observations) != 1 {
							t.Fatalf("invalid batch %+v / %v", batch, err)
						}
						var value Transition
						if err := json.Unmarshal(batch.Observations[0].Values["last_key"], &value); err != nil || !registered || value.Key != "KEY_F13" || value.Action != "press" {
							t.Fatalf("invalid keyboard event %+v / %v", value, err)
						}
						return
					}
				}
			}
		})
	}
}
