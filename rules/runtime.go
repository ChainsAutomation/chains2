package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"reflect"
	"sync"
	"time"

	chains "chains-automation"
)

// DeviceRef is a stable device identity across events and rule runs.
type DeviceRef struct {
	ServiceID string `json:"service_id"`
	DeviceID  string `json:"device_id"`
}

func (d DeviceRef) address() string { return d.ServiceID + "/" + d.DeviceID }

type Host interface {
	ResolveDevice(serviceName, deviceName string) (DeviceRef, error)
	State(address string) map[string]any
	Act(context.Context, chains.Message, DeviceRef, string, string, map[string]any) (map[string]any, error)
}

type Trace struct {
	Type, Rule, Run, Event, Correlation, Summary string
	Source, Service, Device, Action, Cause       string
	At                                           time.Time
}

type Runtime struct {
	trace    func(Trace)
	host     Host
	calendar Calendar
	log      *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	wg       sync.WaitGroup
	closed   bool
	openings map[string][]*opening
	waits    map[string]map[*wait]bool
}

type opening struct {
	rule    Rule
	pattern map[string]any
	running int
	initial map[string]any
	binding string
	body    []Statement
	trigger string
	args    []any
}

type wait struct {
	pattern map[string]any
	event   chan chains.Message
}
type execution struct {
	runtime  *Runtime
	runID    string
	rule     Rule
	env      map[string]any
	last     chains.Message
	deadline time.Time
	budget   int
	ctx      context.Context
}

func New(ctx context.Context, host Host, calendar Calendar, log *slog.Logger, rules []Rule, traces ...func(Trace)) (*Runtime, error) {
	ctx, cancel := context.WithCancel(ctx)
	r := &Runtime{host: host, calendar: calendar, log: log, ctx: ctx, cancel: cancel, openings: map[string][]*opening{}, waits: map[string]map[*wait]bool{}}
	if len(traces) > 0 {
		r.trace = traces[0]
	}
	var schedules []*opening
	seen := map[string]bool{}
	startupError := func(rule Rule, err error) (*Runtime, error) {
		cancel()
		return nil, fmt.Errorf("rule %q: %w", rule.Name, err)
	}
	for _, rule := range rules {
		if err := Validate(rule); err != nil {
			return startupError(rule, err)
		}
		if seen[rule.Name] {
			return startupError(rule, errors.New("duplicate rule name"))
		}
		seen[rule.Name] = true
		if rule.Concurrency == 0 {
			rule.Concurrency = 1
		}
		if rule.Timeout == 0 {
			rule.Timeout = 5 * time.Minute
		}
		start := openingIndex(rule)
		e := &execution{runtime: r, env: map[string]any{}, budget: 10000, ctx: ctx}
		if err := e.block(rule.Body[:start]); err != nil {
			return startupError(rule, err)
		}
		first := rule.Body[start]
		o := &opening{rule: rule, initial: e.env, binding: first.Name, body: rule.Body[start+1:], trigger: first.Value.Text}
		args, params, err := e.arguments(first.Value)
		if err != nil {
			return startupError(rule, err)
		}
		if o.trigger == "Event" {
			o.pattern = params
			key, _ := params["type"].(string)
			r.openings[key] = append(r.openings[key], o)
		} else {
			o.args = calendarArgs(args, params)
			if _, err = r.calendar.next(o.trigger, o.args, time.Now()); err != nil {
				return startupError(rule, err)
			}
			schedules = append(schedules, o)
		}
	}
	for _, o := range schedules {
		if calendar.Disabled {
			continue
		}
		r.wg.Add(1)
		go r.schedule(o)
	}
	return r, nil
}

func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	r.cancel()
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *Runtime) Dispatch(m chains.Message) {
	fields := eventFields(m)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	for _, key := range []string{m.Type, ""} {
		for w := range r.waits[key] {
			if matches(w.pattern, fields) {
				w.event <- m
				delete(r.waits[key], w)
			}
		}
		if len(r.waits[key]) == 0 {
			delete(r.waits, key)
		}
		for _, o := range r.openings[key] {
			if matches(o.pattern, fields) {
				r.start(o, m)
			}
		}
	}
}

func matches(pattern, fields map[string]any) bool {
	for k, v := range pattern {
		candidate, ok := fields[k]
		if !ok {
			body, _ := fields["data"].(map[string]any)
			candidate, ok = body[k]
		}
		if !ok || !equal(candidate, v) {
			return false
		}
	}
	return true
}

func (r *Runtime) start(o *opening, m chains.Message) {
	if o.running >= o.rule.Concurrency {
		r.emitTrace("rule.skipped", o.rule.Name, "", m, "at capacity")
		r.log.Debug("rule at capacity", "rule", o.rule.Name, "event", m.ID)
		return
	}
	o.running++
	e := &execution{runtime: r, rule: o.rule, env: maps.Clone(o.initial), ctx: r.ctx}
	if r.trace != nil {
		e.runID = chains.ID()
	}
	e.match(m)
	if o.binding != "" {
		e.env[o.binding] = eventFields(m)
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() { r.mu.Lock(); o.running--; r.mu.Unlock() }()
		var started time.Time
		if r.trace != nil {
			started = time.Now()
			r.emitTrace("rule.started", o.rule.Name, e.runID, m, "")
		}
		r.log.Debug("rule matched", "rule", o.rule.Name, "event", m.ID)
		err := e.block(o.body)
		kind, summary := "rule.finished", ""
		if r.trace != nil {
			summary = time.Since(started).String()
			if errors.Is(err, context.DeadlineExceeded) {
				kind = "rule.timeout"
			} else if errors.Is(err, context.Canceled) {
				kind = "rule.cancelled"
			} else if err != nil && !errors.Is(err, errReturn) {
				kind = "rule.failed"
			}
			if err != nil && !errors.Is(err, errReturn) {
				summary += " · " + err.Error()
			}
			r.emitTrace(kind, o.rule.Name, e.runID, m, summary)
		}
		if err != nil && !errors.Is(err, errReturn) {
			r.log.Warn("rule exited", "rule", o.rule.Name, "event", m.ID, "error", err)
		}
	}()
}

func (r *Runtime) emitTrace(kind, rule, run string, m chains.Message, summary string) {
	if r.trace == nil {
		return
	}
	service := m.ServiceID
	if service == "" {
		service = m.Source
	}
	correlation := m.CorrelationID
	if correlation == "" {
		correlation = m.ID
	}
	r.trace(Trace{Type: kind, Rule: rule, Run: run, Event: m.ID, Correlation: correlation, Source: m.Source, Service: service, Device: m.DeviceID, Action: m.ActionID, Cause: m.CausationID, Summary: summary, At: time.Now().UTC()})
}

func (r *Runtime) schedule(o *opening) {
	defer r.wg.Done()
	for {
		next, err := r.calendar.next(o.trigger, o.args, time.Now())
		if err != nil {
			r.log.Error("schedule stopped", "rule", o.rule.Name, "error", err)
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			m, _ := chains.New("time.matched", "scheduler", map[string]any{"time": next})
			r.mu.Lock()
			if !r.closed {
				r.start(o, m)
			}
			r.mu.Unlock()
		}
	}
}

func (e *execution) match(m chains.Message) {
	if !e.deadline.IsZero() {
		e.runtime.emitTrace("rule.resumed", e.rule.Name, e.runID, m, "")
	}
	e.last = m
	if e.deadline.IsZero() {
		e.deadline = time.Now().Add(e.rule.Timeout)
	}
	e.budget = 10000
}

func (e *execution) tick() error {
	if err := e.ctx.Err(); err != nil {
		return err
	}
	if !e.deadline.IsZero() && !time.Now().Before(e.deadline) {
		return context.DeadlineExceeded
	}
	e.budget--
	if e.budget < 0 {
		return errors.New("instruction budget exhausted")
	}
	return nil
}

func eventFields(m chains.Message) map[string]any {
	fields := m.Fields()
	if m.ServiceID != "" && m.DeviceID != "" {
		fields["device"] = DeviceRef{ServiceID: m.ServiceID, DeviceID: m.DeviceID}
	} else {
		fields["device"] = nil
	}
	return fields
}

func (e *execution) until() (context.Context, context.CancelFunc) {
	if e.deadline.IsZero() {
		return context.WithCancel(e.ctx)
	}
	return context.WithDeadline(e.ctx, e.deadline)
}

func (e *execution) await(pattern map[string]any) (any, error) {
	r := e.runtime
	w := &wait{pattern: pattern, event: make(chan chains.Message, 1)}
	key, _ := pattern["type"].(string)
	r.mu.Lock()
	if r.waits[key] == nil {
		r.waits[key] = map[*wait]bool{}
	}
	r.waits[key][w] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.waits[key], w)
		if len(r.waits[key]) == 0 {
			delete(r.waits, key)
		}
	}()
	ctx, cancel := e.until()
	defer cancel()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case m := <-w.event:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e.match(m)
		return eventFields(m), nil
	}
}

func (e *execution) arguments(x *Expression) ([]any, map[string]any, error) {
	var positional []any
	named := map[string]any{}
	for _, arg := range x.Arguments {
		var value any
		var err error
		if arg.Value.Kind == "name" && bareArgument(x.Text, arg.Name) {
			var present bool
			value, present = e.env[arg.Value.Text]
			if !present {
				value = arg.Value.Text
			}
		} else {
			value, err = e.expr(arg.Value)
		}
		if err != nil {
			return nil, nil, err
		}
		if arg.Name == "" {
			positional = append(positional, value)
		} else {
			named[arg.Name] = value
		}
	}
	return positional, named, nil
}

func calendarArgs(pos []any, named map[string]any) []any {
	for k, v := range named {
		pos = append(pos, k, v)
	}
	return pos
}

func pairs(args []any) (map[string]any, error) {
	if len(args)%2 != 0 {
		return nil, errors.New("expected named arguments")
	}
	m := map[string]any{}
	for i := 0; i < len(args); i += 2 {
		k, ok := args[i].(string)
		if !ok || k == "" {
			return nil, errors.New("expected argument name")
		}
		if _, ok = m[k]; ok {
			return nil, fmt.Errorf("duplicate argument %q", k)
		}
		m[k] = args[i+1]
	}
	return m, nil
}

func (e *execution) call(x *Expression) (any, error) {
	args, named, err := e.arguments(x)
	if err != nil {
		return nil, err
	}
	switch x.Text {
	case "Event":
		return e.await(named)
	case "Action":
		action, ok := args[0].(string)
		if !ok || !chains.Segment(action) {
			return nil, errors.New("Action needs a valid action name")
		}
		var ref DeviceRef
		if device, found := named["device"]; found {
			ref, err = deviceReference(device)
		} else {
			ref, err = e.resolve(named)
		}
		if err != nil {
			return nil, err
		}
		version := "1"
		if value, found := named["schema_version"]; found {
			if version, ok = value.(string); !ok || !chains.Segment(version) {
				return nil, errors.New("invalid action schema version")
			}
		}
		delete(named, "device")
		delete(named, "service_name")
		delete(named, "device_name")
		delete(named, "schema_version")
		ctx, cancel := e.until()
		defer cancel()
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		result, err := e.runtime.host.Act(ctx, e.last, ref, action, version, named)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return result, err
	case "State":
		ref, err := deviceReference(args[0])
		if err != nil {
			return nil, err
		}
		e.runtime.log.Debug("rule read state", "rule", e.rule.Name, "event", e.last.ID, "address", ref.address())
		return e.runtime.host.State(ref.address()), nil
	case "Device":
		if source, found := named["service_id"]; found {
			service, sok := source.(string)
			device, dok := named["device_id"].(string)
			if !sok || !dok {
				return nil, errors.New("device IDs must be strings")
			}
			return deviceReference(DeviceRef{service, device})
		}
		return e.resolve(named)
	case "Every", "Time":
		if e.runtime.calendar.Disabled {
			return nil, errors.New("scheduler is disabled")
		}
		next, err := e.runtime.calendar.next(x.Text, calendarArgs(args, named), time.Now())
		if err != nil {
			return nil, err
		}
		ctx, cancel := e.until()
		defer cancel()
		timer := time.NewTimer(time.Until(next))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			m, _ := chains.New("time.matched", "scheduler", map[string]any{"time": next})
			e.match(m)
			return eventFields(m), nil
		}
	case "Has":
		object, ok := args[0].(map[string]any)
		key, nameOK := args[1].(string)
		if !ok || !nameOK {
			return nil, errors.New("Has expects an object and a key")
		}
		_, present := object[key]
		return present, nil
	case "Exists":
		return args[0] != nil, nil
	}
	return nil, fmt.Errorf("unknown function %q", x.Text)
}

func deviceReference(value any) (DeviceRef, error) {
	if ref, ok := value.(DeviceRef); ok && chains.Segment(ref.ServiceID) && chains.Segment(ref.DeviceID) {
		return ref, nil
	}
	return DeviceRef{}, errors.New("expected a device reference")
}

func (e *execution) resolve(named map[string]any) (DeviceRef, error) {
	service, sok := named["service_name"].(string)
	device, dok := named["device_name"].(string)
	if !sok || !dok || service == "" || device == "" {
		return DeviceRef{}, errors.New("service_name and device_name must be nonempty strings")
	}
	return e.runtime.host.ResolveDevice(service, device)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func equal(a, b any) bool {
	x, ok := number(a)
	y, yes := number(b)
	if ok && yes {
		return x == y
	}
	return reflect.DeepEqual(a, b)
}

func access(v, k any) (any, error) {
	if ref, ok := v.(DeviceRef); ok {
		key, _ := k.(string)
		switch key {
		case "service_id":
			return ref.ServiceID, nil
		case "device_id":
			return ref.DeviceID, nil
		}
	}
	if m, ok := v.(map[string]any); ok {
		if key, ok := k.(string); ok {
			return m[key], nil
		}
	}
	if list, ok := v.([]any); ok {
		if index, ok := number(k); ok && index >= 0 && index < float64(len(list)) && index == math.Trunc(index) {
			return list[int(index)], nil
		}
	}
	if v == nil {
		return nil, nil
	}
	return nil, errors.New("invalid field or index access")
}

func (e *execution) expr(x *Expression) (any, error) {
	if err := e.tick(); err != nil {
		return nil, err
	}
	switch x.Kind {
	case "literal":
		return x.Literal, nil
	case "name":
		value, ok := e.env[x.Text]
		if !ok {
			return nil, fmt.Errorf("unknown variable %q", x.Text)
		}
		return value, nil
	case "call":
		return e.call(x)
	case "list", "object":
		list := make([]any, 0, len(x.Arguments))
		object := map[string]any{}
		for _, item := range x.Arguments {
			value, err := e.expr(item.Value)
			if err != nil {
				return nil, err
			}
			if x.Kind == "list" {
				list = append(list, value)
			} else {
				object[item.Name] = value
			}
		}
		if x.Kind == "list" {
			return list, nil
		}
		return object, nil
	case "field", "index":
		value, err := e.expr(x.Left)
		if err != nil {
			return nil, err
		}
		var key any = x.Text
		if x.Kind == "index" {
			key, err = e.expr(x.Right)
			if err != nil {
				return nil, err
			}
		}
		return access(value, key)
	case "unary":
		value, err := e.expr(x.Left)
		if err != nil {
			return nil, err
		}
		if b, ok := value.(bool); ok && x.Text == "not" {
			return !b, nil
		}
		if n, ok := number(value); ok {
			if x.Text == "-" {
				return -n, nil
			}
			if x.Text == "+" {
				return n, nil
			}
		}
		return nil, fmt.Errorf("invalid operand for %s", x.Text)
	case "binary":
		left, err := e.expr(x.Left)
		if err != nil {
			return nil, err
		}
		if x.Text == "and" || x.Text == "or" {
			b, ok := left.(bool)
			if !ok {
				return nil, errors.New("expected boolean operand")
			}
			if (x.Text == "and" && !b) || (x.Text == "or" && b) {
				return b, nil
			}
		}
		right, err := e.expr(x.Right)
		if err != nil {
			return nil, err
		}
		return binary(x.Text, left, right)
	}
	return nil, fmt.Errorf("unsupported expression %q", x.Kind)
}

func binary(op string, left, right any) (any, error) {
	switch op {
	case "==":
		return equal(left, right), nil
	case "!=":
		return !equal(left, right), nil
	case "and", "or":
		if b, ok := right.(bool); ok {
			return b, nil
		}
	}
	if l, ok := left.(string); ok && op == "+" {
		if r, ok := right.(string); ok {
			return l + r, nil
		}
	}
	a, x := number(left)
	b, y := number(right)
	if x && y {
		switch op {
		case "+":
			return a + b, nil
		case "-":
			return a - b, nil
		case "*":
			return a * b, nil
		case "/", "%":
			if b == 0 {
				return nil, errors.New("division by zero")
			}
			if op == "/" {
				return a / b, nil
			}
			return math.Mod(a, b), nil
		case "<":
			return a < b, nil
		case "<=":
			return a <= b, nil
		case ">":
			return a > b, nil
		case ">=":
			return a >= b, nil
		}
	}
	return nil, fmt.Errorf("invalid operands for %s", op)
}
