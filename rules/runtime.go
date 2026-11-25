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
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	chains "chains-automation"
)

// DeviceRef is a stable device identity across events and rule runs.
type DeviceRef struct {
	ServiceID string `json:"service_id"`
	DeviceID  string `json:"device_id"`
}

type namedDeviceRef struct{ service, device string }

func (d DeviceRef) address() string { return d.ServiceID + "/" + d.DeviceID }

type Host interface {
	ResolveDevice(serviceName, deviceName string) (DeviceRef, error)
	ResolveDevices(selectors map[string]string, placement chains.PlacementFilter) []DeviceRef
	State(address string) map[string]any
	Act(context.Context, chains.Message, DeviceRef, string, string, map[string]any) (map[string]any, error)
	SendAction(context.Context, chains.Message, DeviceRef, string, string, map[string]any) (map[string]any, error)
	ResolveSetter(DeviceRef, string, any) (chains.Setter, map[string]any, error)
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
	active   map[string]*opening
	openings map[string][]*opening
	waits    map[string]map[*wait]bool
}

type opening struct {
	placement chains.PlacementFilter
	ctx       context.Context
	cancel    context.CancelFunc
	rule      Rule
	pattern   map[string]any
	running   int
	runs      map[*execution]context.CancelFunc
	initial   map[string]any
	binding   string
	body      []Statement
	trigger   string
	args      []any
}

type wait struct {
	placement chains.PlacementFilter
	ctx       context.Context
	pattern   map[string]any
	event     chan matchedEvent
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
	r.active = map[string]*opening{}
	var prepared []*Prepared
	seen := map[string]bool{}
	for _, rule := range rules {
		if seen[rule.Name] {
			cancel()
			return nil, fmt.Errorf("duplicate rule name %q", rule.Name)
		}
		seen[rule.Name] = true
		entry, err := r.Prepare(rule)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("rule %q: %w", rule.Name, err)
		}
		prepared = append(prepared, entry)
	}
	r.Apply(nil, prepared)
	return r, nil
}

// Prepared is a validated rule that has yet to be activated.
type Prepared struct{ entry *opening }

func (r *Runtime) Prepare(rule Rule) (*Prepared, error) {
	if err := Validate(rule); err != nil {
		return nil, err
	}
	if rule.Concurrency == 0 {
		rule.Concurrency = 1
	}
	if rule.Timeout == 0 {
		rule.Timeout = 5 * time.Minute
	}
	start := openingIndex(rule)
	e := &execution{runtime: r, env: map[string]any{}, budget: 10000, ctx: r.ctx}
	if err := e.block(rule.Body[:start]); err != nil {
		return nil, err
	}
	first := rule.Body[start]
	o := &opening{rule: rule, initial: e.env, binding: first.Name, body: rule.Body[start+1:], trigger: first.Value.Text}
	args, params, err := e.arguments(first.Value)
	if err != nil {
		return nil, err
	}
	if o.trigger == "Event" {
		o.placement, err = extractPlacement(params)
		if err != nil {
			return nil, invalid(first.At, "%v", err)
		}
		if err := validateEventPattern(params); err != nil {
			return nil, invalid(first.At, "%v", err)
		}
		if err := validateEventDevice(params); err != nil {
			return nil, invalid(first.At, "%v", err)
		}
		o.pattern = params
	} else {
		o.args = calendarArgs(args, params)
		if _, err = r.calendar.next(o.trigger, o.args, time.Now()); err != nil {
			return nil, invalid(first.At, "%v", err)
		}
	}
	return &Prepared{o}, nil
}

// Apply installs prepared rules and cancels the rules they replace.
func (r *Runtime) Apply(remove []string, add []*Prepared) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	for _, name := range remove {
		o := r.active[name]
		if o == nil {
			continue
		}
		o.cancel()
		delete(r.active, name)
		if o.trigger == "Event" {
			key, _ := o.pattern["type"].(string)
			r.openings[key] = slices.DeleteFunc(r.openings[key], func(item *opening) bool { return item == o })
			if len(r.openings[key]) == 0 {
				delete(r.openings, key)
			}
		}
	}
	for _, item := range add {
		o := item.entry
		o.ctx, o.cancel = context.WithCancel(r.ctx)
		o.runs = map[*execution]context.CancelFunc{}
		r.active[o.rule.Name] = o
		if o.trigger == "Event" {
			key, _ := o.pattern["type"].(string)
			r.openings[key] = append(r.openings[key], o)
		} else if !r.calendar.Disabled {
			r.wg.Add(1)
			go r.schedule(o)
		}
	}
}

func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	r.cancel()
	r.mu.Unlock()
	r.wg.Wait()
}

type matchedEvent struct {
	message chains.Message
	fields  map[string]any
}

type resolvedNamedDevice struct {
	ref DeviceRef
	ok  bool
}

func stringSliceAny(values []string) []any {
	if len(values) == 0 {
		return nil
	}
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func (r *Runtime) Dispatch(m chains.Message) {
	keys := []string{m.Type}
	if m.Type != chains.Changed {
		keys = append(keys, "")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	hasCandidates := false
	var namedToResolve []namedDeviceRef
	for _, key := range keys {
		waits := r.waits[key]
		openings := r.openings[key]
		if len(waits) == 0 && len(openings) == 0 {
			continue
		}
		hasCandidates = true
		for w := range waits {
			if name, ok := w.pattern["device"].(namedDeviceRef); ok && (m.ServiceName == "" || name.service == m.ServiceName) {
				if !slices.Contains(namedToResolve, name) {
					namedToResolve = append(namedToResolve, name)
				}
			}
		}
		for _, o := range openings {
			if name, ok := o.pattern["device"].(namedDeviceRef); ok && (m.ServiceName == "" || name.service == m.ServiceName) {
				if !slices.Contains(namedToResolve, name) {
					namedToResolve = append(namedToResolve, name)
				}
			}
		}
	}
	r.mu.Unlock()
	if !hasCandidates {
		return
	}

	envelope := m
	envelope.Observations = nil
	fields := eventFields(envelope)
	patches := make([]map[string]any, 0, len(m.Observations))
	allPatches := make([]any, 0, len(m.Observations))
	for _, observation := range m.Observations {
		var data any
		if len(observation.Data) > 0 {
			_ = json.Unmarshal(observation.Data, &data)
		}
		values := make(map[string]any, len(observation.Values))
		for key, raw := range observation.Values {
			var value any
			_ = json.Unmarshal(raw, &value)
			values[key] = value
		}
		patch := map[string]any{
			"device":         DeviceRef{m.ServiceID, observation.DeviceID},
			"device_id":      observation.DeviceID,
			"device_name":    observation.DeviceName,
			"device_classes": stringSliceAny(observation.DeviceClasses),
			"location":       stringSliceAny(observation.Location),
			"tags":           stringSliceAny(observation.Tags),
			"values":         values,
			"data":           data,
		}
		patches = append(patches, patch)
		allPatches = append(allPatches, patch)
	}
	var resolved map[namedDeviceRef]resolvedNamedDevice
	if len(namedToResolve) > 0 {
		resolved = make(map[namedDeviceRef]resolvedNamedDevice, len(namedToResolve))
		for _, name := range namedToResolve {
			target, err := r.host.ResolveDevice(name.service, name.device)
			resolved[name] = resolvedNamedDevice{ref: target, ok: err == nil}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	for _, key := range keys {
		for w := range r.waits[key] {
			if w.ctx.Err() != nil {
				delete(r.waits[key], w)
				continue
			}
			if matched := r.eventMatch(w.pattern, w.placement, fields, patches, allPatches, resolved); matched != nil {
				w.event <- matchedEvent{m, matched}
				delete(r.waits[key], w)
			}
		}
		if len(r.waits[key]) == 0 {
			delete(r.waits, key)
		}
		for _, o := range r.openings[key] {
			if matched := r.eventMatch(o.pattern, o.placement, fields, patches, allPatches, resolved); matched != nil {
				r.start(o, m, matched)
			}
		}
	}
}

func validateEventPattern(pattern map[string]any) error {
	for _, key := range []string{"type", "device_class", "service_id", "service_name", "device_id", "device_name", "source", "target", "schema_version"} {
		if value, ok := pattern[key]; ok {
			if _, isString := value.(string); !isString {
				return fmt.Errorf("event selector %q must be a string", key)
			}
		}
	}
	return nil
}

func validateEventDevice(pattern map[string]any) error {
	value, ok := pattern["device"]
	if !ok {
		return nil
	}
	if _, ok := value.(namedDeviceRef); ok {
		return nil
	}
	_, err := deviceReference(value)
	return err
}

func (r *Runtime) eventMatch(pattern map[string]any, placement chains.PlacementFilter, fields map[string]any, patches []map[string]any, allPatches []any, resolved map[namedDeviceRef]resolvedNamedDevice) map[string]any {
	var overrideDevice *DeviceRef
	if name, ok := pattern["device"].(namedDeviceRef); ok {
		if fields["service_name"] != name.service {
			return nil
		}
		if len(patches) == 0 && fields["device_name"] != name.device {
			return nil
		}
		if len(patches) > 0 && !slices.ContainsFunc(patches, func(patch map[string]any) bool {
			return patch["device_name"] == name.device
		}) {
			return nil
		}
		if hit, checked := resolved[name]; checked {
			if !hit.ok {
				return nil
			}
			overrideDevice = &hit.ref
		} else {
			target, err := r.host.ResolveDevice(name.service, name.device)
			if err != nil {
				return nil
			}
			overrideDevice = &target
		}
	}
	return observationMatch(pattern, overrideDevice, placement, fields, patches, allPatches)
}

func observationMatch(pattern map[string]any, overrideDevice *DeviceRef, placement chains.PlacementFilter, fields map[string]any, patches []map[string]any, allPatches []any) map[string]any {
	if len(patches) == 0 {
		if matchesPlacement(placement, fields) && matches(pattern, overrideDevice, false, fields, nil) {
			return fields
		}
		return nil
	}
	attribute, hasAttribute := pattern["attribute"]
	value, hasValue := pattern["value"]
	var selected []any
	for _, patch := range patches {
		if !matchesPlacement(placement, patch) || !matches(pattern, overrideDevice, true, fields, patch) {
			continue
		}
		if hasAttribute || hasValue {
			values, _ := patch["values"].(map[string]any)
			found := false
			for key, item := range values {
				if (!hasAttribute || equal(key, attribute)) && (!hasValue || equal(item, value)) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		selected = append(selected, patch)
	}
	if len(selected) == 0 {
		return nil
	}
	result := maps.Clone(fields)
	result["observations"], result["matches"] = allPatches, selected
	if len(selected) == 1 {
		maps.Copy(result, selected[0].(map[string]any))
	}
	return result
}

func extractPlacement(selectors map[string]any) (chains.PlacementFilter, error) {
	filter, err := chains.ParsePlacementFilter(selectors)
	for _, field := range []string{"location", "tags", "tag"} {
		delete(selectors, field)
	}
	return filter, err
}

func matchesPlacement(filter chains.PlacementFilter, fields map[string]any) bool {
	if !filter.Active() {
		return true
	}
	location, _ := chains.StringParts(fields["location"])
	tags, _ := chains.StringParts(fields["tags"])
	return filter.Match(chains.Placement{Location: location, Tags: tags})
}

func matches(pattern map[string]any, overrideDevice *DeviceRef, skipAttrValue bool, fields, patch map[string]any) bool {
	lookup := func(k string) (any, bool) {
		if patch != nil {
			if v, ok := patch[k]; ok {
				return v, true
			}
		}
		if v, ok := fields[k]; ok {
			return v, true
		}
		var data any
		if patch != nil {
			data = patch["data"]
		} else {
			data = fields["data"]
		}
		body, _ := data.(map[string]any)
		v, ok := body[k]
		return v, ok
	}
	for k, v := range pattern {
		if skipAttrValue && (k == "attribute" || k == "value") {
			continue
		}
		if k == "device" && overrideDevice != nil {
			v = *overrideDevice
		}
		if k == "device_class" {
			class, ok := v.(string)
			if !ok {
				return false
			}
			found := false
			rawClasses, _ := lookup("device_classes")
			switch classes := rawClasses.(type) {
			case []string:
				found = slices.Contains(classes, class)
			case []any:
				found = slices.Contains(classes, any(class))
			}
			if !found {
				return false
			}
			continue
		}
		candidate, ok := lookup(k)
		if !ok || !equal(candidate, v) {
			return false
		}
	}
	return true
}

func (r *Runtime) start(o *opening, m chains.Message, fields map[string]any) {
	if o.rule.Mode == "restart" {
		for prev, cancel := range o.runs {
			cancel()
			delete(o.runs, prev)
		}
		o.running = 0
	} else if o.running >= o.rule.Concurrency {
		r.emitTrace("rule.skipped", o.rule.Name, "", m, "at capacity")
		r.log.Debug("rule at capacity", "rule", o.rule.Name, "event", m.ID)
		return
	}
	runCtx, runCancel := context.WithCancel(o.ctx)
	e := &execution{runtime: r, rule: o.rule, env: maps.Clone(o.initial), ctx: runCtx}
	if o.runs == nil {
		o.runs = map[*execution]context.CancelFunc{}
	}
	o.runs[e] = runCancel
	o.running = len(o.runs)
	if r.trace != nil {
		e.runID = chains.ID()
	}
	e.match(m)
	if o.binding != "" {
		e.env[o.binding] = fields
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(o.runs, e)
			o.running = len(o.runs)
			r.mu.Unlock()
			runCancel()
		}()
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
	after := time.Now()
	for {
		next, err := r.calendar.next(o.trigger, o.args, after)
		if err != nil {
			r.log.Error("schedule stopped", "rule", o.rule.Name, "error", err)
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-o.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			m, _ := chains.New("time.matched", "scheduler", map[string]any{"time": next})
			r.mu.Lock()
			if !r.closed && o.ctx.Err() == nil {
				r.start(o, m, eventFields(m))
			}
			r.mu.Unlock()
			after = time.Now()
			if after.Before(next) {
				after = next
			}
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
	e.budget--
	if e.budget < 0 {
		return errors.New("instruction budget exhausted")
	}
	if e.budget&63 == 0 {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		if !e.deadline.IsZero() && !time.Now().Before(e.deadline) {
			return context.DeadlineExceeded
		}
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

func (e *execution) await(pattern map[string]any, waitTimeout time.Duration, hasTimeout bool) (any, error) {
	r := e.runtime
	placement, err := extractPlacement(pattern)
	if err != nil {
		return nil, err
	}
	w := &wait{placement: placement, ctx: e.ctx, pattern: pattern, event: make(chan matchedEvent, 1)}
	key, _ := pattern["type"].(string)
	r.mu.Lock()
	if err := e.ctx.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
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
	waitCtx := ctx
	if hasTimeout {
		var waitCancel context.CancelFunc
		waitCtx, waitCancel = context.WithTimeout(ctx, waitTimeout)
		defer waitCancel()
	}
	select {
	case <-waitCtx.Done():
		if hasTimeout && ctx.Err() == nil {
			return nil, nil
		}
		return nil, ctx.Err()
	case matched := <-w.event:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e.match(matched.message)
		return matched.fields, nil
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
		var waitTimeout time.Duration
		var hasTimeout bool
		if rawTimeout, ok := named["timeout"]; ok {
			seconds, numOK := number(rawTimeout)
			d, durOK := secondsDuration(seconds)
			if !numOK || !durOK {
				return nil, errors.New("Event timeout expects nonnegative seconds within the supported duration")
			}
			waitTimeout, hasTimeout = d, true
			delete(named, "timeout")
		}
		if err := validateEventPattern(named); err != nil {
			return nil, err
		}
		if err := validateEventDevice(named); err != nil {
			return nil, err
		}
		return e.await(named, waitTimeout, hasTimeout)
	case "Sleep":
		seconds, ok := number(args[0])
		d, durOK := secondsDuration(seconds)
		if !ok || !durOK {
			return nil, errors.New("Sleep expects nonnegative seconds within the supported duration")
		}
		ctx, cancel := e.until()
		defer cancel()
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		return nil, ctx.Err()
	case "Action", "Set":
		var ref DeviceRef
		if target, found := named["device"]; found {
			ref, err = e.resolveReference(target)
		} else {
			ref, err = e.resolve(named)
		}
		if err != nil {
			return nil, err
		}
		act := e.runtime.host.Act
		if wait, ok := named["wait"]; ok {
			mode, _ := wait.(string)
			switch mode {
			case "outcome":
			case "sent":
				act = e.runtime.host.SendAction
			default:
				return nil, errors.New("wait must be outcome or sent")
			}
		}
		version := "1"
		var action string
		if x.Text == "Set" {
			attribute, ok := named["attribute"].(string)
			if !ok || !chains.Segment(attribute) {
				return nil, errors.New("Set needs a valid attribute name")
			}
			setter, values, err := e.runtime.host.ResolveSetter(ref, attribute, named["value"])
			if err != nil {
				return nil, err
			}
			action, version, named = setter.Action, setter.Version, values
		} else {
			var ok bool
			action, ok = args[0].(string)
			if !ok || !chains.Segment(action) {
				return nil, errors.New("Action needs a valid action name")
			}
			if v, found := named["schema_version"]; found {
				version, ok = v.(string)
				if !ok || !chains.Segment(version) {
					return nil, errors.New("invalid action schema version")
				}
			}
			for _, key := range []string{"device", "service_name", "device_name", "schema_version", "wait"} {
				delete(named, key)
			}
		}
		ctx, cancel := e.until()
		defer cancel()
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		result, err := act(ctx, e.last, ref, action, version, named)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return result, err
	case "State":
		ref, err := e.resolveReference(args[0])
		if err != nil {
			return nil, err
		}
		e.runtime.log.Debug("rule read state", "rule", e.rule.Name, "event", e.last.ID, "address", ref.address())
		return e.runtime.host.State(ref.address()), nil
	case "Devices":
		placement, err := extractPlacement(named)
		if err != nil {
			return nil, err
		}
		selectors := make(map[string]string, len(named))
		for key, value := range named {
			name, ok := value.(string)
			if !ok || name == "" {
				return nil, errors.New("device selectors must be nonempty strings")
			}
			selectors[key] = name
		}
		devices := e.runtime.host.ResolveDevices(selectors, placement)
		result := make([]any, len(devices))
		for i, device := range devices {
			result[i] = device
		}
		return result, nil

	case "Device":
		if source, found := named["service_id"]; found {
			service, sok := source.(string)
			device, dok := named["device_id"].(string)
			if !sok || !dok {
				return nil, errors.New("device IDs must be strings")
			}
			return deviceReference(DeviceRef{service, device})
		}
		return namedReference(named)
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
	case "Len":
		switch v := args[0].(type) {
		case string:
			return float64(utf8.RuneCountInString(v)), nil
		case []any:
			return float64(len(v)), nil
		case []string:
			return float64(len(v)), nil
		case map[string]any:
			return float64(len(v)), nil
		default:
			return nil, errors.New("Len expects a string, list, or object")
		}
	case "Now":
		return float64(time.Now().UTC().UnixNano()) / float64(time.Second), nil
	case "ParseTime":
		raw, ok := args[0].(string)
		if !ok {
			return nil, errors.New("ParseTime expects an RFC3339 timestamp string")
		}
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return nil, errors.New("ParseTime expects an RFC3339 timestamp string")
		}
		return float64(t.UTC().UnixNano()) / float64(time.Second), nil
	case "Abs":
		n, ok := number(args[0])
		if !ok {
			return nil, errors.New("Abs expects a number")
		}
		return math.Abs(n), nil
	case "Round":
		n, ok := number(args[0])
		if !ok {
			return nil, errors.New("Round expects a number")
		}
		return math.Round(n), nil
	case "Min", "Max":
		a, aok := number(args[0])
		b, bok := number(args[1])
		if !aok || !bok {
			return nil, fmt.Errorf("%s expects two numbers", x.Text)
		}
		if x.Text == "Min" {
			return math.Min(a, b), nil
		}
		return math.Max(a, b), nil
	}
	return nil, fmt.Errorf("unknown function %q", x.Text)
}

func deviceReference(value any) (DeviceRef, error) {
	if ref, ok := value.(DeviceRef); ok && chains.Segment(ref.ServiceID) && chains.Segment(ref.DeviceID) {
		return ref, nil
	}
	return DeviceRef{}, errors.New("expected a device reference")
}

func (e *execution) resolveReference(value any) (DeviceRef, error) {
	if named, ok := value.(namedDeviceRef); ok {
		return e.runtime.host.ResolveDevice(named.service, named.device)
	}
	return deviceReference(value)
}

func (e *execution) resolve(named map[string]any) (DeviceRef, error) {
	ref, err := namedReference(named)
	if err != nil {
		return DeviceRef{}, err
	}
	return e.resolveReference(ref)
}

func namedReference(named map[string]any) (namedDeviceRef, error) {
	service, sok := named["service_name"].(string)
	device, dok := named["device_name"].(string)
	if !sok || !dok || service == "" || device == "" {
		return namedDeviceRef{}, errors.New("service_name and device_name must be nonempty strings")
	}
	return namedDeviceRef{service, device}, nil
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
	if la, aok := asAnySlice(a); aok {
		if lb, bok := asAnySlice(b); bok {
			if len(la) != len(lb) {
				return false
			}
			for i := range la {
				if !equal(la[i], lb[i]) {
					return false
				}
			}
			return true
		}
	}
	return reflect.DeepEqual(a, b)
}

func asAnySlice(v any) ([]any, bool) {
	switch s := v.(type) {
	case []any:
		return s, true
	case []string:
		out := make([]any, len(s))
		for i, item := range s {
			out[i] = item
		}
		return out, true
	default:
		return nil, false
	}
}

func (e *execution) equalValues(a, b any) bool {
	if na, ok := a.(namedDeviceRef); ok {
		if ref, err := e.resolveReference(na); err == nil {
			a = ref
		}
	}
	if nb, ok := b.(namedDeviceRef); ok {
		if ref, err := e.resolveReference(nb); err == nil {
			b = ref
		}
	}
	return equal(a, b)
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
	if list, ok := v.([]string); ok {
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
		if named, ok := value.(namedDeviceRef); ok {
			value, err = e.resolveReference(named)
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
		return e.binary(x.Text, left, right)
	}
	return nil, fmt.Errorf("unsupported expression %q", x.Kind)
}

func (e *execution) binary(op string, left, right any) (any, error) {
	switch op {
	case "==":
		return e.equalValues(left, right), nil
	case "!=":
		return !e.equalValues(left, right), nil
	case "and", "or":
		if b, ok := right.(bool); ok {
			return b, nil
		}
	case "in", "not in":
		found, err := e.containsValue(left, right)
		if err != nil {
			return nil, err
		}
		if op == "not in" {
			return !found, nil
		}
		return found, nil
	}
	if l, ok := left.(string); ok {
		if r, ok := right.(string); ok {
			switch op {
			case "+":
				return l + r, nil
			case "<":
				return l < r, nil
			case "<=":
				return l <= r, nil
			case ">":
				return l > r, nil
			case ">=":
				return l >= r, nil
			}
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

func (e *execution) containsValue(needle, haystack any) (bool, error) {
	switch h := haystack.(type) {
	case []any:
		for _, item := range h {
			if e.equalValues(needle, item) {
				return true, nil
			}
		}
		return false, nil
	case []string:
		for _, item := range h {
			if e.equalValues(needle, item) {
				return true, nil
			}
		}
		return false, nil
	case map[string]any:
		key, ok := needle.(string)
		if !ok {
			return false, errors.New("in expects a string key for object membership")
		}
		_, present := h[key]
		return present, nil
	case string:
		sub, ok := needle.(string)
		if !ok {
			return false, errors.New("in expects a string substring")
		}
		return strings.Contains(h, sub), nil
	default:
		return false, errors.New("invalid operands for in")
	}
}
