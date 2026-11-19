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
	"chains-automation/rules"
	"chains-automation/sdk"
	"chains-automation/state"
	"github.com/google/jsonschema-go/jsonschema"
)

type Options struct {
	ID            string
	AuthorityID   string
	Database      string
	Rules         []rules.Rule
	Calendar      rules.Calendar
	Encoding      chains.Encoding
	ActionTimeout time.Duration
	Log           *slog.Logger
	LocalService  LocalService
}

type Engine struct {
	node            sdk.Client
	authorityID     string
	authority       *state.Authority
	view            *state.View
	runtime         *rules.Runtime
	log             *slog.Logger
	mu              sync.Mutex
	actions         map[string]*pending
	seen            map[string]bool
	seenOrder       []string
	ready           atomic.Bool
	actionTimeout   time.Duration
	snapshotParts   [][]byte
	queuedUpdates   []state.Update
	snapshotPending string
	snapshotSent    time.Time
	pendingChanges  []chains.Message
	schemas         sync.Map
	local           *hostedService
	availability    map[string]presence
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
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{node: sdk.Client{Bus: b, Encoding: o.Encoding, Source: o.ID}, authorityID: o.AuthorityID, view: state.NewView(), log: log, actions: map[string]*pending{}, seen: map[string]bool{}, availability: map[string]presence{}}
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
		e.ready.Store(true)
	}
	e.actionTimeout = o.ActionTimeout
	if e.actionTimeout <= 0 {
		e.actionTimeout = 10 * time.Second
	}
	r, err := rules.New(ctx, e, o.Calendar, log, o.Rules)
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
	stopLocal := e.startLocal(ctx)
	defer stopLocal()
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
					e.pendingChanges = nil
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
	e.seen[key] = true
	e.seenOrder = append(e.seenOrder, key)
	if len(e.seenOrder) > 65536 {
		delete(e.seen, e.seenOrder[0])
		e.seenOrder = e.seenOrder[1:]
	}
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
			e.flushChanges()
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
		if m.Source != e.authorityID {
			return errors.New("untrusted state change")
		}
		var c state.Change
		if err := json.Unmarshal(m.Data, &c); err != nil {
			return err
		}
		epoch, rev := e.view.Position()
		if !e.ready.Load() || epoch != c.Epoch || rev < c.Revision {
			if len(e.pendingChanges) < 1024 {
				e.pendingChanges = append(e.pendingChanges, m)
			}
			return nil
		}
		e.runtime.Dispatch(m)
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
		if !e.ready.Load() {
			return nil
		}
		normalized, err := e.validateEvent(m)
		if err != nil {
			return err
		}
		e.runtime.Dispatch(normalized)
	}
	return nil
}

func (e *Engine) flushChanges() {
	epoch, rev := e.view.Position()
	pending := e.pendingChanges[:0]
	for _, m := range e.pendingChanges {
		var c state.Change
		if json.Unmarshal(m.Data, &c) != nil || m.Validate(time.Now()) != nil || c.Epoch != epoch {
			continue
		}
		if c.Revision <= rev {
			e.runtime.Dispatch(m)
		} else {
			pending = append(pending, m)
		}
	}
	clear(e.pendingChanges[len(pending):])
	e.pendingChanges = pending
}

func (e *Engine) register(ctx context.Context, m chains.Message) error {
	var s chains.Service
	if err := json.Unmarshal(m.Data, &s); err != nil {
		return err
	}
	if e.local != nil && s.UUID == e.local.service.ID() {
		if previous, exists := e.view.Service(s.UUID); exists && previous.ServiceType != s.ServiceType {
			return errors.New("local service ID is already registered to a different service type")
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
		if !chains.Segment(d.ID) || ids[d.ID] || !chains.Segment(d.Class) {
			return errors.New("invalid or duplicate device registration")
		}
		ids[d.ID] = true
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
	s, ok := e.view.Service(m.Source)
	d, known := device(s, m.DeviceID)
	if !ok || !known {
		return errors.New("state report from unregistered device")
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(m.Data, &values); err != nil {
		return err
	}
	for name := range values {
		if !contains(d.Attributes, name) {
			return fmt.Errorf("unknown attribute %q", name)
		}
	}
	u, changes, err := e.authority.Report(m, values)
	if err != nil || u.Revision == 0 {
		return err
	}
	if err := e.publishUpdate(ctx, m, u); err != nil {
		return err
	}
	if m.Silent {
		return nil
	}
	for _, change := range changes {
		event, err := chains.New(chains.Changed, e.node.Source, change)
		if err != nil {
			return err
		}
		event.DeviceID, event.DeviceClass, event.ServiceType, event.ActionID = m.DeviceID, d.Class, s.ServiceType, m.ActionID
		event.ServiceID = m.Source
		event.ObservedAt = m.ObservedAt
		event.CausedBy(m)
		expires := time.Now().Add(time.Minute)
		event.ExpiresAt = &expires
		if err = e.publishDerived(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) schema(raw json.RawMessage) (*jsonschema.Resolved, error) {
	if len(raw) == 0 {
		return nil, errors.New("unknown schema")
	}
	if hit, ok := e.schemas.Load(string(raw)); ok {
		return hit.(*jsonschema.Resolved), nil
	}
	var document jsonschema.Schema
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	resolved, err := document.Resolve(nil)
	if err != nil {
		return nil, err
	}
	result, _ := e.schemas.LoadOrStore(string(raw), resolved)
	return result.(*jsonschema.Resolved), nil
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

func (e *Engine) validateEvent(m chains.Message) (chains.Message, error) {
	s, ok := e.view.Service(m.Source)
	if !ok {
		return m, errors.New("event source is unregistered")
	}
	m.ServiceID = m.Source
	m.ServiceType = s.ServiceType
	m.DeviceClass = ""
	if m.DeviceID != "" {
		d, ok := device(s, m.DeviceID)
		if !ok {
			return m, errors.New("event device is unregistered")
		}
		m.DeviceClass = d.Class
	}
	return m, e.validate(s.Events[m.Type][m.SchemaVersion], m.Data)
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
	if !e.ready.Load() {
		return nil, errors.New("state view is synchronizing")
	}
	target, id := ref.ServiceID, ref.DeviceID
	if !chains.Segment(target) || !chains.Segment(id) {
		return nil, errors.New("invalid device reference")
	}
	s, ok := e.view.Service(target)
	d, known := device(s, id)
	if !ok || !known || !contains(d.Actions, name) {
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
	if e.local != nil && target == e.local.service.ID() {
		return e.actLocal(ctx, m), nil
	}
	p := &pending{target: target, requestID: m.ID, result: make(chan chains.Result, 1), accepted: make(chan struct{}, 1)}
	e.mu.Lock()
	e.actions[m.ActionID] = p
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.actions, m.ActionID); e.mu.Unlock() }()
	if err = e.node.Send(ctx, m); err != nil {
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
			response := map[string]any{"status": result.Status, "error": result.Error, "action_id": m.ActionID}
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
