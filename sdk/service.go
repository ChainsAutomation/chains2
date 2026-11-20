package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"github.com/google/jsonschema-go/jsonschema"
)

// Hooks run at the named service lifecycle stages. Callbacks must honor cancellation.
type Hooks struct {
	OnInit      func(context.Context, *Runtime) error
	OnStart     func(context.Context, *Runtime) error
	OnEvent     func(context.Context, *Runtime, chains.Message) error
	OnPoll      func(context.Context, *Runtime) error
	OnReconnect func(context.Context, *Runtime) error
	OnShutdown  func(context.Context, *Runtime) error
}

type ActionHandler func(context.Context, *Runtime, chains.Message, chains.Action) (any, error)

type Definition struct {
	Service chains.Service
	Actions map[string]map[string]ActionHandler
	Hooks   Hooks
	Log     *slog.Logger
	OpenBus func(context.Context, Config, bus.Subscription) (bus.Bus, error)
}

type Runtime struct {
	Config        Config // Read-only while the runtime is running.
	definition    Definition
	registration  chains.Service
	client        Client
	ctx           context.Context
	stop          context.CancelFunc
	log           *slog.Logger
	instance      string
	online        atomic.Bool
	subscriptions map[string]bool
	schemas       map[string]map[string]*jsonschema.Resolved
	actions       map[string]map[string]*jsonschema.Resolved
	sendMu        sync.Mutex
	workers       sync.WaitGroup
	wake          chan struct{}
	reset         atomic.Bool
	incoming      chan chains.Message
	callbacks     chan func(context.Context) error
	failure       chan error
	stateMu       sync.Mutex
	state         LocalState
	reports       []chains.Message
	queryMu       sync.Mutex
	queries       map[string]chan queryResult
	outgoing      chan queryRequest
}

var protocolTypes = map[string]bool{
	chains.Registration: true, chains.Report: true, chains.ObservationSubmitted: true, chains.Heartbeat: true, chains.Request: true,
	chains.StateQuery: true, chains.StateReply: true, chains.SnapshotRequest: true, chains.SnapshotResponse: true,
	"state.committed": true, "state.head": true,
}

func OpenTransport(ctx context.Context, c Config, sub bus.Subscription) (bus.Bus, error) {
	u, err := url.Parse(c.URI)
	if err != nil {
		return nil, errors.New("invalid service bus URI")
	}
	switch u.Scheme {
	case "mqtt", "mqtts", "mqtt+unix":
		return bus.OpenMQTT(ctx, c.URI, c.Namespace, sub)
	case "redis", "rediss":
		return bus.OpenRedis(ctx, c.URI, c.Namespace, sub)
	default:
		return nil, errors.New("unsupported service bus URI scheme")
	}
}

// RunService owns registration, workers, and transport shutdown.
func RunService(ctx context.Context, c Config, definition Definition) (result error) {
	if err := c.validate(); err != nil {
		return err
	}
	if !serviceName(definition.Service.ServiceType) {
		return errors.New("invalid service type")
	}
	if c.ServiceID == "" {
		id, err := persistedID(definition.Service.ServiceType, c.Instance)
		if err != nil {
			return err
		}
		c.ServiceID = id
	}
	if err := c.validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	log := definition.Log
	if log == nil {
		log = slog.Default()
	}
	r := &Runtime{Config: c, definition: definition, ctx: ctx, stop: cancel, log: log, instance: chains.ID(),
		subscriptions: map[string]bool{}, wake: make(chan struct{}, 1), incoming: make(chan chains.Message, 128),
		callbacks: make(chan func(context.Context) error, 256), failure: make(chan error, 1), state: LocalState{},
		queries: map[string]chan queryResult{}, outgoing: make(chan queryRequest, 128)}
	if err := r.prepare(); err != nil {
		return err
	}
	defer func() {
		cancel()
		r.workers.Wait()
		shutdown, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		if hook := definition.Hooks.OnShutdown; hook != nil {
			result = errors.Join(result, hook(shutdown, r))
		}
		if r.client.Bus != nil {
			if r.online.Load() {
				result = errors.Join(result, r.flushReports(shutdown, 0))
			}
			result = errors.Join(result, r.client.Bus.Close())
		}
	}()
	if hook := definition.Hooks.OnInit; hook != nil {
		if err := hook(ctx, r); err != nil {
			return err
		}
	}
	open := definition.OpenBus
	if open == nil {
		open = OpenTransport
	}
	connect, done := context.WithTimeout(ctx, 8*time.Second)
	transport, err := open(connect, c, r.subscription())
	done()
	if err != nil {
		return err
	}
	r.client = Client{Bus: transport, Encoding: c.Encoding, Source: c.ServiceID, ServiceType: r.registration.ServiceType}
	if err = r.register(ctx); err != nil {
		return err
	}
	r.online.Store(true)
	if r.reset.Swap(false) {
		r.queueSnapshot()
	}
	if err = r.flushReports(ctx, 0); err != nil {
		return err
	}
	if err = r.heartbeat(ctx); err != nil {
		return err
	}
	r.worker(r.receive)
	r.worker(r.publishLoop)
	r.worker(r.actionLoop)
	r.worker(r.callbackLoop)
	if hook := definition.Hooks.OnStart; hook != nil {
		if err := hook(ctx, r); err != nil {
			return err
		}
	}
	if definition.Hooks.OnPoll != nil && c.PollInterval > 0 {
		r.worker(r.poll)
	}
	select {
	case err := <-r.failure:
		return err
	case <-ctx.Done():
		return nil
	}
}

func (r *Runtime) prepare() error {
	// Registration is copied before adding the built-in get_state action.
	raw, err := json.Marshal(r.definition.Service)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &r.registration); err != nil {
		return err
	}
	r.registration.UUID, r.registration.Name = r.Config.ServiceID, r.Config.Name
	if r.registration.Name == "" {
		r.registration.Name = r.Config.Instance
	}
	if len(r.registration.Devices) == 0 {
		r.registration.Devices = []chains.Device{{ID: "system", Name: "System", Class: "system"}}
	}
	if r.registration.Actions == nil {
		r.registration.Actions = map[string]map[string]json.RawMessage{}
	}
	if _, reserved := r.registration.Actions["get_state"]; reserved {
		return errors.New("get_state is reserved")
	}
	r.registration.Actions["get_state"] = map[string]json.RawMessage{"1": json.RawMessage(
		`{"type":"object","properties":{"device_id":{"type":"string"},"attribute":{"type":"string"}},"dependentRequired":{"attribute":["device_id"]},"additionalProperties":false}`)}
	for _, name := range r.registration.IdempotentActions {
		if name == "get_state" || len(r.registration.Actions[name]) == 0 {
			return fmt.Errorf("invalid idempotent action declaration %s", name)
		}
	}
	r.registration.IdempotentActions = append(r.registration.IdempotentActions, "get_state")
	ids := map[string]bool{}
	for i, d := range r.registration.Devices {
		if !chains.Segment(d.ID) || !chains.Segment(d.Class) || ids[d.ID] {
			return errors.New("invalid or duplicate device")
		}
		ids[d.ID] = true
		for _, attribute := range d.Attributes {
			if !chains.Segment(attribute) || attribute == "_attributes" {
				return fmt.Errorf("invalid attribute on device %s", d.ID)
			}
		}
		for _, action := range d.Actions {
			if action == "get_state" || len(r.registration.Actions[action]) == 0 {
				return fmt.Errorf("invalid action declaration %s", action)
			}
		}
		r.registration.Devices[i].Actions = append(d.Actions, "get_state")
	}
	r.schemas, err = compileSchemas(r.registration.Events)
	if err != nil {
		return err
	}
	for event := range r.schemas {
		if protocolTypes[event] || event == chains.Changed || event == chains.AvailabilityChanged || event == chains.Outcome {
			return fmt.Errorf("reserved event name %s", event)
		}
	}
	r.actions, err = compileSchemas(r.registration.Actions)
	if err != nil {
		return err
	}
	for name, versions := range r.actions {
		if name == "get_state" {
			continue
		}
		for version := range versions {
			if r.definition.Actions[name][version] == nil {
				return fmt.Errorf("missing handler for %s/%s", name, version)
			}
		}
	}
	return nil
}

func compileSchemas(schemas map[string]map[string]json.RawMessage) (map[string]map[string]*jsonschema.Resolved, error) {
	compiled := map[string]map[string]*jsonschema.Resolved{}
	for name, versions := range schemas {
		if !chains.Segment(name) || len(versions) == 0 {
			return nil, fmt.Errorf("invalid capability %s", name)
		}
		compiled[name] = map[string]*jsonschema.Resolved{}
		for version, raw := range versions {
			if !chains.Segment(version) {
				return nil, fmt.Errorf("invalid version for %s", name)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(raw, &schema); err != nil {
				return nil, fmt.Errorf("schema for %s: %w", name, err)
			}
			result, err := schema.Resolve(nil)
			if err != nil {
				return nil, fmt.Errorf("schema for %s: %w", name, err)
			}
			compiled[name][version] = result
		}
	}
	return compiled, nil
}

func (r *Runtime) Subscribe(types ...string) error {
	if r.client.Bus != nil {
		return errors.New("set subscriptions before connecting")
	}
	for _, kind := range types {
		if kind != "*" && !chains.Segment(kind) {
			return errors.New("invalid event subscription")
		}
		r.subscriptions[kind] = true
	}
	return nil
}

func (r *Runtime) subscription() bus.Subscription {
	if r.subscriptions["*"] {
		return bus.Subscription{}
	}
	types := []string{chains.Request, chains.StateReply}
	for kind := range r.subscriptions {
		if kind != chains.Request && kind != chains.StateReply {
			types = append(types, kind)
		}
	}
	sort.Strings(types)
	return bus.Subscription{Types: types}
}

func (r *Runtime) Stop() { r.stop() }

func (r *Runtime) worker(f func()) {
	r.workers.Add(1)
	go func() { defer r.workers.Done(); f() }()
}

func (r *Runtime) send(ctx context.Context, m chains.Message) error {
	if r.client.Bus == nil {
		return errors.New("service is not connected")
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	return r.client.Send(ctx, m)
}

func (r *Runtime) register(ctx context.Context) error {
	m, err := chains.New(chains.Registration, r.Config.ServiceID, r.registration)
	if err != nil {
		return err
	}
	return r.send(ctx, m)
}

func (r *Runtime) heartbeat(ctx context.Context) error {
	m, _ := chains.New(chains.Heartbeat, r.Config.ServiceID, map[string]any{"instance_id": r.instance, "interval_seconds": r.Config.HeartbeatInterval})
	deadline := m.ObservedAt.Add(time.Duration(3*r.Config.HeartbeatInterval) * time.Second)
	m.ExpiresAt = &deadline
	return r.send(ctx, m)
}

// Event publishes a declared event. An empty device selects the default device.
func (r *Runtime) Event(ctx context.Context, kind, device, version string, data any, values map[string]any) error {
	if device == "" {
		device = r.defaultDevice()
	}
	o, err := chains.NewObservation(device, values, data)
	if err != nil {
		return err
	}
	return r.ObserveBatch(ctx, kind, version, []chains.Observation{o})
}

func (r *Runtime) ObserveBatch(ctx context.Context, kind, version string, observations []chains.Observation) error {
	if !r.online.Load() {
		return errors.New("service bus is synchronizing")
	}
	if version == "" {
		version = "1"
	}
	schema := r.schemas[kind][version]
	if schema == nil && kind != chains.DeviceObserved {
		return errors.New("event schema is not declared")
	}
	seen := map[string]bool{}
	if len(observations) == 0 {
		return errors.New("empty observation batch")
	}
	for _, o := range observations {
		d, ok := serviceDevice(r.registration, o.DeviceID)
		if !ok || seen[o.DeviceID] || len(o.Values) == 0 {
			return errors.New("invalid observation device")
		}
		seen[o.DeviceID] = true
		for name := range o.Values {
			if !containsString(d.Attributes, name) {
				return fmt.Errorf("undeclared state attribute %s/%s", o.DeviceID, name)
			}
		}
		if schema != nil {
			data := o.Data
			if len(data) == 0 {
				data, _ = json.Marshal(o.Values)
			}
			var value any
			if err := json.Unmarshal(data, &value); err != nil {
				return err
			}
			if err := schema.Validate(value); err != nil {
				return err
			}
		}
	}
	m, err := chains.SubmitObservation(r.Config.ServiceID, kind, version, observations)
	if err != nil {
		return err
	}
	if cause, ok := ctx.Value(actionContextKey{}).(chains.Message); ok {
		m.CausedBy(cause)
		m.ActionID = cause.ActionID
	}
	if err = r.send(ctx, m); err != nil {
		return err
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	for _, o := range observations {
		for key := range o.Values {
			if m.ObservedAt.Before(r.state[o.DeviceID][key].ObservedAt) {
				return nil
			}
		}
	}
	for _, o := range observations {
		r.cacheObservation(o, m.ObservedAt)
	}
	return nil
}

func (r *Runtime) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runtime) offerCallback(f func(context.Context) error) {
	select {
	case r.callbacks <- f:
	default:
		r.log.Warn("service callback queue full")
	}
}

func (r *Runtime) receive() {
	for {
		select {
		case <-r.ctx.Done():
			return
		case d, ok := <-r.client.Bus.Receive():
			if !ok {
				r.failure <- errors.New("service bus closed")
				return
			}
			if d.Reset {
				r.online.Store(false)
				r.reset.Store(true)
				r.notify()
				continue
			}
			if d.Retained {
				continue
			}
			m, err := chains.Decode(d.Data)
			if err != nil || !bus.Match(d.Topic, m) || m.Validate(time.Now()) != nil {
				continue
			}
			switch {
			case m.Type == chains.StateReply && m.Target == r.Config.ServiceID && m.Source == r.Config.Authority:
				r.handleQueryResult(m)
			case m.Type == chains.Request && m.Target == r.Config.ServiceID && m.ActionID != "":
				select {
				case r.incoming <- m:
				default:
					r.log.Warn("service action queue full")
				}
			default:
				if hook := r.definition.Hooks.OnEvent; hook != nil && (r.subscriptions[m.Type] || (r.subscriptions["*"] && !protocolTypes[m.Type])) {
					r.offerCallback(func(ctx context.Context) error { return hook(ctx, r, m) })
				}
			}
		}
	}
}

func (r *Runtime) callbackLoop() {
	for {
		select {
		case <-r.ctx.Done():
			return
		case f := <-r.callbacks:
			if r.ctx.Err() != nil {
				return
			}
			if err := f(r.ctx); err != nil && r.ctx.Err() == nil {
				r.log.Warn("service callback failed", "error", err)
			}
		}
	}
}

func (r *Runtime) poll() {
	interval := max(time.Nanosecond, time.Duration(r.Config.PollInterval*float64(time.Second)))
	for r.ctx.Err() == nil {
		if err := r.definition.Hooks.OnPoll(r.ctx, r); err != nil && r.ctx.Err() == nil {
			r.log.Warn("service poll failed", "error", err)
		}
		if !waitFor(r.ctx, interval) {
			return
		}
	}
}

func waitFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *Runtime) publishLoop() {
	interval := time.Duration(r.Config.HeartbeatInterval) * time.Second
	next := time.Now().Add(interval)
	syncPending := false
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		case <-r.wake:
		}
		if r.reset.Swap(false) {
			r.online.Store(false)
			if err := r.register(r.ctx); err != nil {
				r.retryPublication(err)
				continue
			}
			r.queueSnapshot()
			syncPending = true
			next = time.Time{}
		}
		if !time.Now().Before(next) {
			if err := r.heartbeat(r.ctx); err != nil {
				r.retryPublication(err)
				continue
			}
			next = time.Now().Add(interval)
		}
		if err := r.flushReports(r.ctx, 32); err != nil {
			r.retryPublication(err)
			continue
		}
		if syncPending && r.reportsEmpty() && !r.reset.Load() {
			r.online.Store(true)
			syncPending = false
			if hook := r.definition.Hooks.OnReconnect; hook != nil {
				r.offerCallback(func(ctx context.Context) error { return hook(ctx, r) })
			}
		}
		r.publishQueries()
	}
}

func (r *Runtime) retryPublication(err error) {
	r.online.Store(false)
	r.reset.Store(true)
	if r.ctx.Err() == nil {
		r.log.Warn("service publication failed; will resynchronize", "error", err)
		waitFor(r.ctx, time.Second)
	}
}

// Go starts a managed worker. Call it from OnStart. Workers must honor cancellation.
func (r *Runtime) Go(work func(context.Context) error) {
	r.worker(func() {
		if err := work(r.ctx); err != nil && r.ctx.Err() == nil {
			select {
			case r.failure <- err:
			default:
			}
		}
	})
}
