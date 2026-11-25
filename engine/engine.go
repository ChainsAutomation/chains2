// Package engine joins protocol ingress, state projection, and rule execution.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/operator"
	"chains-automation/rules"
	"chains-automation/sdk"
	"chains-automation/state"
	"github.com/google/jsonschema-go/jsonschema"
)

type Options struct {
	ID              string
	AuthorityID     string
	Database        string
	Rules           []rules.Rule
	RuleSources     map[string]string
	WebRulesDir     string
	EventTypes      []string
	Calendar        rules.Calendar
	Encoding        chains.Encoding
	ActionTimeout   time.Duration
	Log             *slog.Logger
	LocalService    LocalService
	OperatorAPI     bool
	OperatorTrace   bool
	OpenOperatorBus func(context.Context, bus.Subscription) (bus.Bus, error)
}

type Engine struct {
	placementEdits  chan placementEdit
	node            sdk.Client
	authorityID     string
	authority       *state.Authority
	view            *state.View
	runtime         *rules.Runtime
	ruleCatalog     *ruleCatalog
	eventTypes      []string
	log             *slog.Logger
	mu              sync.Mutex
	actions         map[string]*pending
	seen            map[string]bool
	seenRing        [65536]string
	seenPos         int
	ready           atomic.Bool
	actionTimeout   time.Duration
	snapshotParts   [][]byte
	queuedUpdates   []state.Update
	snapshotPending string
	snapshotSent    time.Time
	snapshotSlots   chan struct{}
	snapshotWG      sync.WaitGroup
	pendingEvents   []chains.Message
	schemasMu       sync.RWMutex
	schemas         map[string]*jsonschema.Resolved
	local           *hostedService
	availability    map[string]presence
	operator        *operatorMonitor
}

type pending struct {
	target    string
	requestID string
	result    chan chains.Result
	accepted  chan struct{}
}

func Open(ctx context.Context, b bus.Bus, o Options) (*Engine, error) {
	if !chains.Segment(o.ID) || !chains.Segment(o.AuthorityID) {
		return nil, errors.New("invalid engine or authority ID")
	}
	if o.OperatorAPI && o.ID != o.AuthorityID {
		return nil, errors.New("operator API requires the authority")
	}
	if (o.OperatorAPI || o.OperatorTrace) && o.OpenOperatorBus == nil {
		return nil, errors.New("operator monitoring requires a separate bus connection")
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{node: sdk.Client{Bus: b, Encoding: o.Encoding, Source: o.ID}, authorityID: o.AuthorityID, eventTypes: o.EventTypes, view: state.NewView(), log: log, actions: map[string]*pending{}, seen: map[string]bool{}, snapshotSlots: make(chan struct{}, 4), schemas: map[string]*jsonschema.Resolved{}, availability: map[string]presence{}}
	if o.LocalService != nil {
		id := o.LocalService.ID()
		if o.ID != o.AuthorityID || id == o.ID || !chains.Segment(id) {
			return nil, errors.New("local service requires the authority and a distinct service ID")
		}
		e.local = newHosted(o.LocalService)
	}
	if o.ID == o.AuthorityID {
		a, err := state.Open(o.Database)
		if err != nil {
			return nil, err
		}
		e.authority, e.view = a, a.View
		e.placementEdits = make(chan placementEdit)
		if o.OperatorAPI {
			if err := e.registerOperator(); err != nil {
				a.Close()
				return nil, err
			}
		}
		e.ready.Store(true)
	}
	if e.node.Encoding == "" {
		e.node.Encoding = chains.JSON
	}
	e.actionTimeout = o.ActionTimeout
	if e.actionTimeout <= 0 {
		e.actionTimeout = 10 * time.Second
	}
	var trace func(rules.Trace)
	if o.OperatorAPI || o.OperatorTrace {
		e.operator = &operatorMonitor{open: o.OpenOperatorBus, trace: make(chan operator.Record, 512)}
		if o.OperatorAPI {
			e.operator.feed = operator.NewFeed()
		}
		if o.OperatorTrace {
			trace = e.ruleTrace
		}
	}
	if e.authority != nil {
		catalog, err := openRuleCatalog(o.WebRulesDir, o.RuleSources)
		if err != nil {
			e.authority.Close()
			return nil, fmt.Errorf("open web rules: %w", err)
		}
		e.ruleCatalog = catalog
		for _, item := range catalog.entries {
			if item.Origin != "web" {
				continue
			}
			rule, err := parseWebRule(item.Name, item.Source)
			if err != nil {
				e.authority.Close()
				return nil, fmt.Errorf("stored rule %q: %w", item.Name, err)
			}
			if item.Enabled {
				o.Rules = append(o.Rules, rule)
			}
		}
	}
	r, err := rules.New(ctx, e, o.Calendar, log, o.Rules, trace)
	if err != nil {
		if e.authority != nil {
			e.authority.Close()
		}
		return nil, err
	}
	e.runtime = r
	return e, nil
}

func (e *Engine) Close() {
	e.runtime.Close()
	if e.authority != nil {
		e.authority.Close()
	}
}

func (e *Engine) State(address string) map[string]any { return e.view.Lookup(address) }

func (e *Engine) ResolveDevice(serviceName, deviceName string) (rules.DeviceRef, error) {
	service, device, err := e.view.ResolveDevice(serviceName, deviceName)
	return rules.DeviceRef{ServiceID: service, DeviceID: device}, err
}

func (e *Engine) Run(ctx context.Context) error {
	stopOperator := e.startOperator(ctx)
	defer stopOperator()
	stopLocal := e.startLocal(ctx)
	defer stopLocal()
	defer e.snapshotWG.Wait()
	var local <-chan localInput
	if e.local != nil {
		local = e.local.input
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	e.requestSnapshot(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if e.authority != nil {
				e.expirePresence(ctx, time.Now())
				epoch, rev := e.view.Position()
				m, _ := chains.New(state.HeadType, e.node.Source, state.Head{Epoch: epoch, Revision: rev})
				if err := e.publishDerived(ctx, m); err != nil {
					e.log.Debug("announce state position", "error", err)
				}
			} else {
				e.requestSnapshot(ctx)
			}
		case edit := <-e.placementEdits:
			if err := edit.ctx.Err(); err != nil {
				edit.done <- err
			} else {
				edit.done <- e.editPlacement(ctx, edit)
			}
		case input := <-local:
			err := e.ingestLocal(ctx, input)
			if input.done != nil {
				input.done <- err
			} else if err != nil {
				e.log.Warn("local outcome rejected", "error", err)
			}
		case d, ok := <-e.node.Bus.Receive():
			if !ok {
				return errors.New("system bus closed")
			}
			if d.Reset {
				if e.authority == nil {
					e.ready.Store(false)
					e.pendingEvents = nil
					e.queuedUpdates = nil
					e.snapshotParts = nil
					e.snapshotPending = ""
					e.requestSnapshot(ctx)
				}
				continue
			}
			if d.Retained {
				continue
			}
			m, err := chains.Decode(d.Data)
			if err == nil && !bus.Match(d.Topic, m) {
				err = errors.New("message and route disagree")
			}
			if err == nil {
				err = m.Validate(time.Now())
			}
			if err == nil && e.local != nil && (m.Source == e.local.service.ID() || (m.Source == e.node.Source && m.Type == chains.Changed)) {
				continue
			}
			if err == nil && !e.duplicate(m) {
				err = e.handle(ctx, m)
			}
			if err != nil {
				e.log.Debug("message rejected", "topic", d.Topic, "id", m.ID, "error", err)
			}
		}
	}
}

func (e *Engine) duplicate(m chains.Message) bool {
	key := m.Source + "/" + m.ID
	if e.seen[key] {
		return true
	}
	if evicted := e.seenRing[e.seenPos]; evicted != "" {
		delete(e.seen, evicted)
	}
	e.seen[key] = true
	e.seenRing[e.seenPos] = key
	e.seenPos = (e.seenPos + 1) % len(e.seenRing)
	return false
}

func (e *Engine) requestSnapshot(ctx context.Context) {
	if e.authority != nil || e.ready.Load() || time.Since(e.snapshotSent) < 2*time.Second {
		return
	}
	m, _ := chains.New(chains.SnapshotRequest, e.node.Source, nil)
	m.Target = e.authorityID
	e.snapshotPending = m.ID
	e.snapshotParts = nil
	e.snapshotSent = time.Now()
	if err := e.node.Send(ctx, m); err != nil {
		e.log.Warn("request state snapshot", "error", err)
	}
}

func (e *Engine) handle(ctx context.Context, m chains.Message) error {
	switch m.Type {
	case operator.TraceType:
		return nil
	case chains.Heartbeat:
		if e.authority != nil {
			return e.heartbeat(ctx, m, time.Now())
		}
	case chains.StateQuery:
		if e.authority != nil && m.Target == e.node.Source {
			return e.queryState(ctx, m)
		}
	case chains.StateReply:
		return nil
	case chains.AvailabilityChanged:
		if m.Source == e.authorityID && e.ready.Load() {
			e.runtime.Dispatch(m)
		}
	case chains.Registration:
		if e.authority == nil {
			return nil
		}
		return e.register(ctx, m)
	case chains.ObservationSubmitted:
		if e.authority != nil {
			return e.observe(ctx, m)
		}
	case chains.Report:
		if e.authority == nil {
			return nil
		}
		return e.report(ctx, m)
	case state.HeadType:
		if e.authority != nil || m.Source != e.authorityID {
			return nil
		}
		var h state.Head
		if err := json.Unmarshal(m.Data, &h); err != nil {
			return err
		}
		epoch, rev := e.view.Position()
		if h.Epoch != epoch || h.Revision > rev {
			e.ready.Store(false)
			e.requestSnapshot(ctx)
		}
	case state.Updated:
		if e.authority != nil || m.Source != e.authorityID {
			return nil
		}
		var u state.Update
		if err := json.Unmarshal(m.Data, &u); err != nil {
			return err
		}
		if !e.view.Apply(u) {
			if len(e.queuedUpdates) < 4096 {
				e.queuedUpdates = append(e.queuedUpdates, u)
			}
			e.ready.Store(false)
			e.requestSnapshot(ctx)
		} else if e.ready.Load() {
			e.flushEvents()
		}
	case chains.SnapshotRequest:
		if e.authority != nil && m.Target == e.node.Source {
			return e.sendSnapshot(ctx, m)
		}
	case chains.SnapshotResponse:
		if e.authority != nil || m.Source != e.authorityID || m.Target != e.node.Source || m.CausationID != e.snapshotPending {
			return nil
		}
		return e.receiveSnapshot(ctx, m)
	case chains.Changed:
		return e.receiveObservation(ctx, m)
	case chains.Request:
		if e.local != nil && m.Target == e.local.service.ID() {
			return e.remoteLocalAction(ctx, m)
		}
		return nil
	case chains.Outcome:
		if e.local != nil && m.Source == e.local.service.ID() {
			e.rememberLocalOutcome(m)
		}
		if e.outcome(m) && e.ready.Load() {
			m.ServiceID = m.Source
			e.runtime.Dispatch(m)
		}
	default:
		if len(m.Observations) == 0 {
			return errors.New("expected committed observations")
		}
		return e.receiveObservation(ctx, m)
	}
	return nil
}

func (e *Engine) flushEvents() {
	epoch, rev := e.view.Position()
	pending := e.pendingEvents[:0]
	for _, m := range e.pendingEvents {
		if m.Validate(time.Now()) != nil || m.Epoch != epoch {
			continue
		}
		if m.Revision <= rev {
			e.runtime.Dispatch(m)
		} else {
			pending = append(pending, m)
		}
	}
	clear(e.pendingEvents[len(pending):])
	e.pendingEvents = pending
}

func (e *Engine) register(ctx context.Context, m chains.Message) error {
	var s chains.Service
	if err := json.Unmarshal(m.Data, &s); err != nil {
		return err
	}
	if s.UUID == chains.OperatorService {
		return errors.New("reserved service ID")
	}
	if e.local != nil && s.UUID == e.local.service.ID() {
		if previous, exists := e.view.Service(s.UUID); exists && previous.ServiceType != s.ServiceType {
			return errors.New("local service ID is already registered to a different service type")
		}
	}
	for name := range s.Events {
		if name == operator.TraceType || reservedEvent(name) {
			return errors.New("reserved event type")
		}
	}
	for _, set := range []map[string]map[string]json.RawMessage{s.Events, s.Actions} {
		for name, versions := range set {
			if !chains.Segment(name) || len(versions) == 0 {
				return errors.New("invalid capability registration")
			}
			for version, raw := range versions {
				if !chains.Segment(version) {
					return errors.New("invalid schema version")
				}
				if _, err := e.schema(raw); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range s.IdempotentActions {
		if len(s.Actions[name]) == 0 {
			return fmt.Errorf("idempotent action %q is not registered", name)
		}
	}
	ids := map[string]bool{}
	for _, d := range s.Devices {
		if !chains.Segment(d.ID) || ids[d.ID] {
			return errors.New("invalid or duplicate device registration")
		}
		ids[d.ID] = true
	}
	if s.RetainDevices {
		if previous, ok := e.authority.DefaultService(s.UUID); ok && previous.ServiceType == s.ServiceType {
			for _, d := range previous.Devices {
				if !ids[d.ID] {
					s.Devices = append(s.Devices, d)
				}
			}
		}
	}
	for i := range s.Devices {
		if err := s.Devices[i].Placement.Normalize(); err != nil {
			return fmt.Errorf("device %q: %w", s.Devices[i].ID, err)
		}
		if err := e.describeDevice(s, &s.Devices[i]); err != nil {
			return fmt.Errorf("device %q: %w", s.Devices[i].ID, err)
		}
	}
	u, err := e.authority.Register(m, s)
	if err != nil || u.Revision == 0 {
		return err
	}
	return e.publishUpdate(ctx, m, u)
}

func (e *Engine) publishUpdate(ctx context.Context, cause chains.Message, u state.Update) error {
	m, err := chains.New(state.Updated, e.node.Source, u)
	if err != nil {
		return err
	}
	m.CausedBy(cause)
	return e.publishDerived(ctx, m)
}

func (e *Engine) report(ctx context.Context, m chains.Message) error {
	s, d, known := e.view.ServiceDevice(m.Source, m.DeviceID)
	if !known {
		return errors.New("state report from unregistered device")
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(m.Data, &values); err != nil {
		return err
	}
	if err := e.validateAttributes(d, values); err != nil {
		return err
	}
	if !m.Silent {
		return errors.New("state reports are reserved for silent seeds")
	}
	return e.commitObservation(ctx, m, s, chains.ObservationBatch{Observations: []chains.Observation{{DeviceID: m.DeviceID, Values: values}}})
}

const maxCachedSchemas = 4096

func (e *Engine) schema(raw json.RawMessage) (*jsonschema.Resolved, error) {
	if len(raw) == 0 {
		return nil, errors.New("unknown schema")
	}
	e.schemasMu.RLock()
	if hit, ok := e.schemas[string(raw)]; ok {
		e.schemasMu.RUnlock()
		return hit, nil
	}
	e.schemasMu.RUnlock()
	var document jsonschema.Schema
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	resolved, err := document.Resolve(nil)
	if err != nil {
		return nil, err
	}
	key := string(raw)
	e.schemasMu.Lock()
	if e.schemas == nil || len(e.schemas) >= maxCachedSchemas {
		e.schemas = map[string]*jsonschema.Resolved{}
	}
	if existing, ok := e.schemas[key]; ok {
		e.schemasMu.Unlock()
		return existing, nil
	}
	e.schemas[key] = resolved
	e.schemasMu.Unlock()
	return resolved, nil
}

func (e *Engine) validate(raw, body json.RawMessage) error {
	schema, err := e.schema(raw)
	if err != nil {
		return err
	}
	var value any
	if err = json.Unmarshal(body, &value); err != nil {
		return err
	}
	return schema.Validate(value)
}

func device(s chains.Service, id string) (chains.Device, bool) {
	for _, d := range s.Devices {
		if d.ID == id {
			return d, true
		}
	}
	return chains.Device{}, false
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (e *Engine) Act(ctx context.Context, parent chains.Message, ref rules.DeviceRef, name, version string, args map[string]any) (map[string]any, error) {
	return e.callAction(ctx, parent, ref, name, version, args, false)
}

func (e *Engine) SendAction(ctx context.Context, parent chains.Message, ref rules.DeviceRef, name, version string, args map[string]any) (map[string]any, error) {
	return e.callAction(ctx, parent, ref, name, version, args, true)
}

func (e *Engine) callAction(ctx context.Context, parent chains.Message, ref rules.DeviceRef, name, version string, args map[string]any, sendOnly bool) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !e.ready.Load() {
		return nil, errors.New("state view is synchronizing")
	}
	target, id := ref.ServiceID, ref.DeviceID
	if !chains.Segment(target) || !chains.Segment(id) {
		return nil, errors.New("invalid device reference")
	}
	s, d, known := e.view.ServiceDevice(target, id)
	if !known || !contains(d.Actions, name) {
		return nil, errors.New("unknown device or action")
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	if err = e.validate(s.Actions[name][version], argsJSON); err != nil {
		return nil, err
	}
	m, err := chains.New(chains.Request, e.node.Source, chains.Action{Name: name, Args: args})
	if err != nil {
		return nil, err
	}
	m.ActionID, m.Target, m.DeviceID, m.SchemaVersion = m.ID, target, id, version
	m.CausedBy(parent)
	ctx, cancel := context.WithTimeout(ctx, e.actionTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	m.ExpiresAt = &deadline
	if sendOnly {
		if e.local != nil && target == e.local.service.ID() {
			select {
			case e.local.input <- localInput{message: m}:
				e.traceAction(m, chains.Request, name)
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				return nil, errors.New("local action queue full")
			}
		} else if err := e.node.Send(ctx, m); err != nil {
			e.traceAction(m, "action.publish_failed", err.Error())
			return nil, err
		}
		return map[string]any{"status": chains.Sent, "action_id": m.ActionID}, nil
	}
	if e.local != nil && target == e.local.service.ID() {
		e.traceAction(m, chains.Request, name)
		return e.actLocal(ctx, m), nil
	}
	p := &pending{target: target, requestID: m.ID, result: make(chan chains.Result, 1), accepted: make(chan struct{}, 1)}
	e.mu.Lock()
	e.actions[m.ActionID] = p
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.actions, m.ActionID); e.mu.Unlock() }()
	if err = e.node.Send(ctx, m); err != nil {
		e.traceAction(m, "action.publish_failed", err.Error())
		return nil, err
	}
	e.log.Debug("action requested", "action", m.ActionID, "cause", m.CausationID, "target", state.Address(target, id))
	var retry <-chan time.Time
	if contains(s.IdempotentActions, name) {
		delay := e.actionTimeout / 2
		if delay > time.Second {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		retry = timer.C
	}
	for {
		select {
		case result := <-p.result:
			response := map[string]any{"status": result.Status, "action_id": m.ActionID}
			if result.Error != "" {
				response["error"] = result.Error
			}
			if len(result.Value) > 0 {
				var value any
				if err := json.Unmarshal(result.Value, &value); err != nil {
					return nil, err
				}
				response["value"] = value
			}
			return response, nil
		case <-p.accepted:
			retry = nil
		case <-retry:
			retry = nil
			select {
			case <-p.accepted:
				continue
			default:
			}
			if err := e.node.Send(ctx, m); err != nil {
				e.log.Debug("action retry publication failed", "action", m.ActionID, "error", err)
			}
		case <-ctx.Done():
			kind := "action.timed_out"
			if errors.Is(ctx.Err(), context.Canceled) {
				kind = "action.cancelled"
			}
			e.traceAction(m, kind, ctx.Err().Error())
			e.log.Debug("action timed out", "action", m.ActionID, "cause", m.CausationID)
			return map[string]any{"status": chains.TimedOut, "action_id": m.ActionID}, nil
		}
	}
}

func (e *Engine) outcome(m chains.Message) bool {
	var result chains.Result
	if json.Unmarshal(m.Data, &result) != nil || m.ActionID == "" || m.Target == "" {
		return false
	}
	switch result.Status {
	case chains.Accepted, chains.Completed, chains.Failed, chains.Submitted, chains.TimedOut:
	default:
		return false
	}
	e.log.Debug("action outcome", "action", m.ActionID, "status", result.Status, "source", m.Source)
	if m.Target != e.node.Source {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.actions[m.ActionID]
	if p != nil && p.target == m.Source && p.requestID == m.CausationID {
		if result.Status == chains.Accepted {
			select {
			case p.accepted <- struct{}{}:
			default:
			}
			return true
		}
		select {
		case p.result <- result:
		default:
		}
	}
	return true
}
