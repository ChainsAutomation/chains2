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

func (h runtimeHost) ResolveDevices(map[string]string, chains.PlacementFilter) []DeviceRef {
	return nil
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

func (h captureHost) ResolveDevices(selectors map[string]string, _ chains.PlacementFilter) []DeviceRef {
	if selectors["service_name"] == "Amplifier" {
		return []DeviceRef{{"amp", "zone1"}, {"amp", "zone2"}}
	}
	return nil
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
	m.ServiceID, m.DeviceID, m.DeviceClasses = service, "shared", []string{"light"}
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

func TestBatchDispatchAndNameSelectors(t *testing.T) {
	r, h := captureRuntime(t, `
ev = Event(service_name=Amplifier, device_name=Main, attribute=volume, value=35)
for match in ev.matches:
    Action("matched", device=match.device, value=match.values.volume)
for zone in Devices(service_name=Amplifier):
    Action("discovered", device=zone)
`)
	event, _ := chains.New("amp.sample", "engine", nil)
	event.ServiceID, event.ServiceName = "amp", "Amplifier"
	first, _ := chains.NewObservation("zone1", map[string]any{"volume": 35}, map[string]any{"volume": 65})
	first.DeviceName = "Aux"
	second, _ := chains.NewObservation("zone2", map[string]any{"volume": 36, "balance": 35}, nil)
	second.DeviceName = "Main"
	event.Observations = []chains.Observation{first, second}
	r.Dispatch(event)
	first.DeviceName = "Main"
	event.Observations[0] = first
	r.Dispatch(event)
	for i, want := range []invocation{
		{name: "matched", ref: DeviceRef{"amp", "zone1"}, args: map[string]any{"value": float64(35)}},
		{name: "discovered", ref: DeviceRef{"amp", "zone1"}},
		{name: "discovered", ref: DeviceRef{"amp", "zone2"}},
	} {
		got := receiveInvocation(t, h)
		if got.name != want.name || got.ref != want.ref || got.args["value"] != want.args["value"] {
			t.Fatalf("invocation %d = %+v, want %+v", i, got, want)
		}
	}
	r.Close()
	if len(h.calls) != 0 {
		t.Fatal("batch matched more than once")
	}
}

func TestGenericEventSkipsLeafChanges(t *testing.T) {
	r, h := captureRuntime(t, `
ev = Event(device_class=sensor)
Action("matched",device=ev.device,value=ev.type)
`)
	changed, _ := chains.New(chains.Changed, "engine", map[string]any{"attribute": "value", "value": 42})
	changed.ServiceID, changed.DeviceID, changed.DeviceClasses = "sensor", "one", []string{"sensor"}
	r.Dispatch(changed)
	observation, _ := chains.NewObservation("one", map[string]any{"value": 42}, nil)
	observation.DeviceClasses = []string{"sensor"}
	event, _ := chains.New("sensor.sample", "engine", nil)
	event.ServiceID = "sensor"
	event.Observations = []chains.Observation{observation}
	r.Dispatch(event)
	if got := receiveInvocation(t, h); got.args["value"] != "sensor.sample" {
		t.Fatalf("unexpected trigger %+v", got)
	}
}

func TestApplyCancelsOnlyReplacedRule(t *testing.T) {
	parse := func(name, src string) Rule {
		t.Helper()
		rule, err := Parse(name, src)
		if err != nil {
			t.Fatal(err)
		}
		return rule
	}
	old := parse("replace", `Event(type=opening)
Event(type=finish)
Action("old", device=Device(service_id=test, device_id=test))
`)
	keep := parse("keep", `Event(type=opening)
Event(type=finish)
Action("kept", device=Device(service_id=test, device_id=test))
`)
	h := captureHost{make(chan invocation, 10), make(chan error, 10)}
	r, err := New(context.Background(), h, Calendar{}, slog.New(slog.NewTextHandler(io.Discard, nil)), []Rule{old, keep})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dispatchEvent(r, "opening", "test", nil)
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		count := len(r.waits["finish"])
		r.mu.Unlock()
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rules did not reach their waits")
		}
		time.Sleep(time.Millisecond)
	}
	next, err := r.Prepare(parse("replace", `Event(type=finish)
Action("new", device=Device(service_id=test, device_id=test))
`))
	if err != nil {
		t.Fatal(err)
	}
	r.Apply([]string{"replace"}, []*Prepared{next})
	dispatchEvent(r, "finish", "test", nil)
	a, b := receiveInvocation(t, h), receiveInvocation(t, h)
	if a.name == b.name || (a.name != "new" && a.name != "kept") || (b.name != "new" && b.name != "kept") {
		t.Fatalf("unexpected actions %q and %q", a.name, b.name)
	}
}

func TestApplyReplacesSchedule(t *testing.T) {
	old, err := Parse("schedule", `Every("250ms")
Action("old",device=Device(service_id=test, device_id=test))
`)
	if err != nil {
		t.Fatal(err)
	}
	next, err := Parse("schedule", `Every("70ms")
Action("new",device=Device(service_id=test, device_id=test))
`)
	if err != nil {
		t.Fatal(err)
	}
	h := captureHost{make(chan invocation, 10), make(chan error, 10)}
	r, err := New(context.Background(), h, Calendar{}, slog.New(slog.NewTextHandler(io.Discard, nil)), []Rule{old})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	prepared, err := r.Prepare(next)
	if err != nil {
		t.Fatal(err)
	}
	r.Apply([]string{"schedule"}, []*Prepared{prepared})
	for i := 0; i < 4; i++ {
		if got := receiveInvocation(t, h); got.name != "new" {
			t.Fatalf("retired schedule fired: %s", got.name)
		}
	}
	r.Apply([]string{"schedule"}, nil)
	select {
	case got := <-h.calls:
		t.Fatalf("disabled schedule fired: %s", got.name)
	case <-time.After(100 * time.Millisecond):
	}
}

func (h runtimeHost) SendAction(ctx context.Context, m chains.Message, d DeviceRef, name, version string, args map[string]any) (map[string]any, error) {
	return h.Act(ctx, m, d, name, version, args)
}
func (h runtimeHost) ResolveSetter(DeviceRef, string, any) (chains.Setter, map[string]any, error) {
	return chains.Setter{}, nil, errors.New("test setter unavailable")
}

func (h captureHost) SendAction(ctx context.Context, m chains.Message, d DeviceRef, name, version string, args map[string]any) (map[string]any, error) {
	return h.Act(ctx, m, d, name, version, args)
}
func (h captureHost) ResolveSetter(DeviceRef, string, any) (chains.Setter, map[string]any, error) {
	return chains.Setter{}, nil, errors.New("test setter unavailable")
}

func TestBatchPlacementSelectorsUseSameDevice(t *testing.T) {
	r, h := captureRuntime(t, `
ev = Event(type="sample", location=["**", "Shelf.1"], tags=["battery", "guest"])
for match in ev.matches:
    Action("matched", device=match.device)
`)
	m, _ := chains.New("sample", "engine", nil)
	m.ServiceID = "service"
	m.Observations = []chains.Observation{
		{DeviceID: "one", Placement: chains.Placement{Location: []string{"House", "Shelf.1"}, Tags: []string{"guest"}}},
		{DeviceID: "two", Placement: chains.Placement{Location: []string{"House", "Shelf"}, Tags: []string{"battery", "guest"}}},
	}
	r.Dispatch(m)
	m.Observations[0].Tags = append(m.Observations[0].Tags, "battery")
	r.Dispatch(m)
	if got := receiveInvocation(t, h); got.ref.DeviceID != "one" {
		t.Fatalf("unexpected match %+v", got)
	}
}

type aliasHost struct {
	captureHost
	registry chan DeviceRef
}

func (h aliasHost) ResolveDevice(_, _ string) (DeviceRef, error) {
	select {
	case ref := <-h.registry:
		h.registry <- ref
		return ref, nil
	default:
		return DeviceRef{}, errors.New("ambiguous or unavailable device")
	}
}

func TestNamedAliasBeforeRegistrationAndScopedMatch(t *testing.T) {
	rule, err := Parse("alias", `
led = Device(service_name=LEDPanel, device_name=panel0)
Event(type="lamp", device=led)
Action("seen", device=led)
`)
	if err != nil {
		t.Fatal(err)
	}
	h := aliasHost{captureHost: captureHost{calls: make(chan invocation, 4)}, registry: make(chan DeviceRef, 1)}
	r, err := New(context.Background(), h, Calendar{}, slog.New(slog.NewTextHandler(io.Discard, nil)), []Rule{rule})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	emit := func(service string) {
		m, _ := chains.New("lamp", service, nil)
		m.ServiceID, m.ServiceName, m.DeviceID, m.DeviceName = service, "LEDPanel", "one", "panel0"
		r.Dispatch(m)
	}
	emit("first") // It was not registered when the rule was prepared.
	ref := DeviceRef{"first", "one"}
	h.registry <- ref
	emit("other") // A stale event with matching names has a different identity.
	emit("first")
	if got := receiveInvocation(t, h.captureHost); got.ref != ref || got.name != "seen" {
		t.Fatalf("unexpected action: %+v", got)
	}
	<-h.registry // Lookup now reports ambiguity.
	emit("first")
	select {
	case got := <-h.calls:
		t.Fatalf("unexpected action: %+v", got)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestSleepTimeoutReplacementAndIndependentProgress(t *testing.T) {
	parse := func(name, source string) Rule {
		t.Helper()
		rule, err := Parse(name, source)
		if err != nil {
			t.Fatal(err)
		}
		return rule
	}
	delay := parse("delay", `
rule.timeout = 0.2
led = Device(service_id=test, device_id=test)
Event(type="delay")
Action("before", device=led)
Sleep(30)
Action("after", device=led)
`)
	other := parse("other", `
Event(type="other")
Action("independent", device=Device(service_id=test, device_id=test))
`)
	host := captureHost{calls: make(chan invocation, 8)}
	traces := make(chan Trace, 16)
	r, err := New(context.Background(), host, Calendar{}, slog.New(slog.NewTextHandler(io.Discard, nil)), []Rule{delay, other}, func(trace Trace) { traces <- trace })
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	awaitTrace := func(kind string) {
		t.Helper()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		for {
			select {
			case trace := <-traces:
				if trace.Rule == "delay" && trace.Type == kind {
					return
				}
			case <-timer.C:
				t.Fatal("missing trace", kind)
			}
		}
	}
	dispatchEvent(r, "delay", "test", nil)
	if got := receiveInvocation(t, host); got.name != "before" {
		t.Fatal(got)
	}
	dispatchEvent(r, "other", "test", nil)
	if got := receiveInvocation(t, host); got.name != "independent" {
		t.Fatal(got)
	}
	awaitTrace("rule.timeout")
	delay.Timeout = 30 * time.Second
	prepared, err := r.Prepare(delay)
	if err != nil {
		t.Fatal(err)
	}
	r.Apply([]string{"delay"}, []*Prepared{prepared})
	dispatchEvent(r, "delay", "test", nil)
	if got := receiveInvocation(t, host); got.name != "before" {
		t.Fatal(got)
	}
	r.Apply([]string{"delay"}, nil)
	awaitTrace("rule.cancelled")
	if len(host.calls) != 0 {
		t.Fatal("action ran after the sleep was cancelled")
	}
	for _, source := range []string{"Sleep(2)\nEvent()", "Event()\nSleep()", "Event()\nSleep(seconds=2)"} {
		if _, err := Parse("bad", source); err == nil {
			t.Errorf("accepted %q", source)
		}
	}
}

func TestObservationListsNamedEqualityAndPrimitives(t *testing.T) {
	r, h := captureRuntime(t, `
led = Device(service_name=LEDPanel, device_name=panel0)
ev = Event(type="sample")
x = "outer"
for tag in ev.tags:
    x = tag
if x != "bedroom":
    return
if ev.location[0] != "House" or ev.tags != ["bedroom"] or "bedroom" not in ev.tags or "light" not in ev.device_classes:
    return
if ev.device != led:
    return
switch ev.device:
    case led:
        age = Now() - ParseTime("2020-01-01T00:00:00Z")
        if age > 0 and "2020-01-01T00:00:00Z" < "2025-01-01T00:00:00Z" and Len(ev.location) == 2 and "room" in "bedroom":
            Action("ok", device=led, value=Min(Max(Round(Abs(-2.4)), 1), 5))
`)
	obs, _ := chains.NewObservation("panel0", map[string]any{"power": true}, nil)
	obs.DeviceName = "panel0"
	obs.DeviceClasses = []string{"light"}
	obs.Placement = chains.Placement{Location: []string{"House", "Bedroom"}, Tags: []string{"bedroom"}}
	m, _ := chains.New("sample", "engine", nil)
	m.ServiceID, m.ServiceName = "led", "LEDPanel"
	m.Observations = []chains.Observation{obs}
	r.Dispatch(m)
	got := receiveInvocation(t, h)
	if got.name != "ok" || got.args["value"] != float64(2) {
		t.Fatalf("unexpected invocation: %+v", got)
	}
}

func TestForLoopPreservesOuterVariable(t *testing.T) {
	r, h := captureRuntime(t, `
Event(type="go")
item = "outer"
for item in ["a", "b"]:
    Action("step", device=Device(service_id=test, device_id=test), value=item)
Action("after", device=Device(service_id=test, device_id=test), value=item)
`)
	dispatchEvent(r, "go", "test", nil)
	for _, want := range []string{"a", "b", "outer"} {
		if got := receiveInvocation(t, h); got.args["value"] != want {
			t.Fatalf("got %v, want %q", got.args["value"], want)
		}
	}
}

func TestEventWaitTimeoutReturnsNull(t *testing.T) {
	r, h := captureRuntime(t, `
rule.timeout = 5
Event(type="start")
next = Event(type="never", timeout=0.05)
if next == null:
    Action("timed_out_gracefully", device=Device(service_id=test, device_id=test))
`)
	dispatchEvent(r, "start", "test", nil)
	if got := receiveInvocation(t, h); got.name != "timed_out_gracefully" {
		t.Fatalf("got %+v", got)
	}
}

func TestRuleModeRestartCancelsPreviousRun(t *testing.T) {
	r, h := captureRuntime(t, `
rule.mode = "restart"
rule.timeout = 5
ev = Event(type="motion")
Sleep(0.08)
Action("off", device=Device(service_id=test, device_id=test), value=ev.data.seq)
`)
	dispatchEvent(r, "motion", "test", map[string]any{"seq": 1})
	time.Sleep(30 * time.Millisecond)
	dispatchEvent(r, "motion", "test", map[string]any{"seq": 2})
	got := receiveInvocation(t, h)
	if got.name != "off" || got.args["value"] != float64(2) {
		t.Fatalf("expected restarted run seq=2, got %+v", got)
	}
	select {
	case extra := <-h.calls:
		t.Fatalf("cancelled run unexpectedly fired: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRejectCapturedNonStringEventSelector(t *testing.T) {
	rule, err := Parse("captured", `
light = Device(service_name=LEDPanel, device_name=panel0)
Event(device_class=light)
Action("seen", device=light)
`)
	if err != nil {
		t.Fatal(err)
	}
	host := captureHost{calls: make(chan invocation, 1)}
	if _, err := New(context.Background(), host, Calendar{}, slog.New(slog.NewTextHandler(io.Discard, nil)), []Rule{rule}); err == nil {
		t.Fatal("expected Prepare to reject DeviceRef captured in device_class selector")
	}
}
