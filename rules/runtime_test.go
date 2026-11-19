package rules

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	chains "chains-automation"
)

type runtimeHost struct{ calls chan string }

func (h runtimeHost) ResolveDevice(service, device string) (DeviceRef, error) {
	return DeviceRef{service, device}, nil
}

func (h runtimeHost) State(string) map[string]any { return nil }
func (h runtimeHost) Act(ctx context.Context, _ chains.Message, _ DeviceRef, name string, _ string, _ map[string]any) (map[string]any, error) {
	select {
	case h.calls <- name:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestFanoutCapacityAndTimeout(t *testing.T) {
	sequence, err := Parse("sequence", `
rule.timeout = 0.25
start = Event(type=start)
Action("waiting", device=Device(service_id=test, device_id=test))
next = Event(type=next)
while start.data.n < next.data.n:
    start = next
for item in ["done"]:
    switch item:
        case "done":
            Action(item, device=Device(service_id=test, device_id=test))
`)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := Parse("observer", `
Event(type=start)
Action("observed", device=Device(service_id=test, device_id=test))
`)
	if err != nil {
		t.Fatal(err)
	}
	parsed := []Rule{sequence, observer}
	calls := make(chan string, 10)
	r, err := New(context.Background(), runtimeHost{calls}, Calendar{}, slog.New(slog.NewTextHandler(io.Discard, nil)), parsed)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dispatch := func(kind string, n int) { m, _ := chains.New(kind, "test", map[string]int{"n": n}); r.Dispatch(m) }
	get := func() string {
		t.Helper()
		select {
		case name := <-calls:
			return name
		case <-time.After(time.Second):
			t.Fatal("missing rule action")
			return ""
		}
	}
	dispatch("start", 1)
	first, second := get(), get()
	if first == second || !((first == "waiting" && second == "observed") || (first == "observed" && second == "waiting")) {
		t.Fatalf("fanout: %q %q", first, second)
	}
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		ready := len(r.waits["next"]) == 1 && r.openings["start"][1].running == 0
		r.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rule did not reach its second wait")
		}
		time.Sleep(time.Millisecond)
	}
	dispatch("start", 2)
	if actual := get(); actual != "observed" {
		t.Fatalf("capacity: %s", actual)
	}
	dispatch("next", 3)
	if actual := get(); actual != "done" {
		t.Fatalf("continuation: %s", actual)
	}
	deadline = time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		ready := r.openings["start"][0].running == 0 && r.openings["start"][1].running == 0
		r.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runs did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	dispatch("start", 1)
	get()
	get()
	deadline = time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		ready := r.openings["start"][0].running == 0
		r.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not expire")
		}
		time.Sleep(time.Millisecond)
	}
	dispatch("next", 3)
	select {
	case name := <-calls:
		t.Fatalf("expired wait ran %s", name)
	case <-time.After(30 * time.Millisecond):
	}
}

type invocation struct {
	ref      DeviceRef
	name     string
	args     map[string]any
	deadline time.Time
}
type captureHost struct {
	calls     chan invocation
	cancelled chan error
}

func (h captureHost) ResolveDevice(service, device string) (DeviceRef, error) {
	if service == "LEDPanel" && device == "panel0" {
		return DeviceRef{"led", "panel0"}, nil
	}
	return DeviceRef{}, errors.New("test target unavailable")
}

func (h captureHost) State(address string) map[string]any {
	return map[string]any{"power": address == "second/shared"}
}

func (h captureHost) Act(ctx context.Context, _ chains.Message, ref DeviceRef, name, _ string, args map[string]any) (map[string]any, error) {
	deadline, _ := ctx.Deadline()
	h.calls <- invocation{ref: ref, name: name, args: args, deadline: deadline}
	if name == "block" {
		<-ctx.Done()
		h.cancelled <- ctx.Err()
		return nil, ctx.Err()
	}
	return nil, nil
}

func captureRuntime(t *testing.T, source string) (*Runtime, captureHost) {
	t.Helper()
	rule, err := Parse("capture", source)
	if err != nil {
		t.Fatal(err)
	}
	host := captureHost{make(chan invocation, 10), make(chan error, 10)}
	r, err := New(context.Background(), host, Calendar{}, slog.New(slog.NewTextHandler(io.Discard, nil)), []Rule{rule})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, host
}

func dispatchEvent(r *Runtime, kind, service string, data map[string]any) {
	m, _ := chains.New(kind, service, data)
	m.ServiceID, m.DeviceID, m.DeviceClass = service, "shared", "light"
	r.Dispatch(m)
}

func receiveInvocation(t *testing.T, h captureHost) invocation {
	t.Helper()
	select {
	case action := <-h.calls:
		return action
	case <-time.After(2 * time.Second):
		t.Fatal("missing action")
		return invocation{}
	}
}

func waitForWaiters(t *testing.T, r *Runtime, kind string, n int) {
	t.Helper()
	stop := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		count := len(r.waits[kind])
		r.mu.Unlock()
		if count == n {
			return
		}
		if time.Now().After(stop) {
			t.Fatalf("wanted %d waits for %s, got %d", n, kind, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSimpleNamedAction(t *testing.T) {
	r, h := captureRuntime(t, `
Event(device_class=light)
Action('writestring', service_name=LEDPanel, device_name=panel0, value="Light is modified")
`)
	dispatchEvent(r, "light.changed", "first", nil)
	action := receiveInvocation(t, h)
	if action.ref != (DeviceRef{"led", "panel0"}) || action.name != "writestring" || action.args["value"] != "Light is modified" || len(action.args) != 1 {
		t.Fatalf("unexpected invocation: %+v", action)
	}
}

func TestStateFromSeparateDeviceRuns(t *testing.T) {
	r, h := captureRuntime(t, `
rule.concurrency = 3
rule.timeout = 60
ev = Event(action=power_toggle)
next = Event(type=followup, key=ev.data.key)
if State(ev.device).power == false:
    Action('writestring', service_name=LEDPanel, device_name=panel0, value="Light is off! " + next.data.key)
else:
    Action('writestring', service_name=LEDPanel, device_name=panel0, value="Light is on! " + next.data.key)
`)
	dispatchEvent(r, "light.changed", "first", map[string]any{"action": "power_toggle", "key": "A"})
	dispatchEvent(r, "light.changed", "second", map[string]any{"action": "power_toggle", "key": "B"})
	waitForWaiters(t, r, "followup", 2)
	dispatchEvent(r, "followup", "second", map[string]any{"key": "B"})
	if action := receiveInvocation(t, h); action.args["value"] != "Light is on! B" {
		t.Fatalf("second device: %+v", action)
	}
	dispatchEvent(r, "followup", "first", map[string]any{"key": "A"})
	if action := receiveInvocation(t, h); action.args["value"] != "Light is off! A" {
		t.Fatalf("first device: %+v", action)
	}
}

func TestDeadlineDoesNotReset(t *testing.T) {
	r, h := captureRuntime(t, `
rule.timeout = 0.6
ev = Event(type=start)
Action('first', device=ev.device)
Event(type=followup)
Action('block', device=ev.device)
Action('too_late', device=ev.device)
`)
	dispatchEvent(r, "start", "first", nil)
	first := receiveInvocation(t, h)
	waitForWaiters(t, r, "followup", 1)
	dispatchEvent(r, "followup", "second", nil)
	second := receiveInvocation(t, h)
	if second.name != "block" || second.deadline != first.deadline || second.ref.ServiceID != "first" {
		t.Fatalf("resumed invocation: %+v after %+v", second, first)
	}
	select {
	case err := <-h.cancelled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("action scope was not cancelled")
	}
	r.Close()
	if len(h.calls) != 0 {
		t.Fatal("expired run called an additional action")
	}
}

func TestCommittedEventUsesReportingDevice(t *testing.T) {
	r, h := captureRuntime(t, `
ev = Event(type="state.changed")
Action('write', device=ev.device, value=State(ev.device).power)
`)
	m, _ := chains.New(chains.Changed, "authority", map[string]any{"attribute": "power", "value": false})
	m.ServiceID, m.DeviceID = "second", "shared"
	r.Dispatch(m)
	call := receiveInvocation(t, h)
	if call.ref != (DeviceRef{"second", "shared"}) || call.args["value"] != true {
		t.Fatalf("expected reported device's current state, got %+v", call)
	}
}
