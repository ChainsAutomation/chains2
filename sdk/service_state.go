package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
)

type Attribute struct {
	Value      json.RawMessage `json:"value"`
	ChangedAt  time.Time       `json:"changed_at"`
	ObservedAt time.Time       `json:"observed_at"`
}
type LocalState map[string]map[string]Attribute

type StateReply struct {
	Found        bool            `json:"found"`
	State        json.RawMessage `json:"state"`
	Availability *struct {
		Online     bool   `json:"online"`
		InstanceID string `json:"instance_id"`
	} `json:"availability"`
	Error string `json:"error"`
}

type queryRequest struct {
	ctx     context.Context
	message chains.Message
}
type queryResult struct {
	reply StateReply
	err   error
}

func (r *Runtime) SetState(ctx context.Context, device string, values map[string]any) error {
	return r.updateState(ctx, device, time.Now().UTC(), time.Time{}, false, values)
}

func (r *Runtime) SeedState(device string, observedAt time.Time, values map[string]any) error {
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	return r.updateState(r.ctx, device, observedAt, time.Time{}, true, values)
}

func (r *Runtime) updateState(ctx context.Context, device string, at, changed time.Time, silent bool, values map[string]any) error {
	if device == "" {
		device = r.defaultDevice()
	}
	d, ok := serviceDevice(r.registration, device)
	if !ok {
		return errors.New("unknown state device")
	}
	encoded := map[string]json.RawMessage{}
	for k, v := range values {
		if _, ok := d.Attributes[k]; !ok {
			return fmt.Errorf("undeclared state attribute %s/%s", device, k)
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := r.validateAttribute(device, k, raw); err != nil {
			return err
		}
		encoded[k] = raw
	}
	// Bound the complete update before applying it locally.
	if raw, err := json.Marshal(encoded); err != nil || len(raw) > bus.MaxPayload-2048 {
		return bus.ErrTooLarge
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	local := r.state[device]
	if local == nil {
		local = map[string]Attribute{}
		r.state[device] = local
	}
	for k, v := range encoded {
		current, exists := local[k]
		if exists && at.Before(current.ObservedAt) {
			delete(encoded, k)
			continue
		}
		changedAt := at
		if !changed.IsZero() && !changed.After(at) {
			changedAt = changed
		}
		if exists && bytes.Equal(current.Value, v) {
			changedAt = current.ChangedAt
		}
		local[k] = Attribute{v, changedAt, at}
	}
	if len(encoded) == 0 {
		return nil
	}
	if len(r.reports) >= 4096 {
		r.reset.Store(true)
	}
	if !r.reset.Load() {
		m := r.stateReport(device, at, silent, encoded)
		if cause, ok := ctx.Value(actionContextKey{}).(chains.Message); ok && !silent {
			m.CausedBy(cause)
			m.ActionID = cause.ActionID
		}
		r.reports = append(r.reports, m)
	}
	r.notify()
	return nil
}

func (r *Runtime) validateAttribute(device, name string, raw json.RawMessage) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	return r.attributeSchemas[device][name].Validate(value)
}

func (r *Runtime) defaultDevice() string {
	if _, ok := serviceDevice(r.registration, "system"); ok {
		return "system"
	}
	return r.registration.Devices[0].ID
}

func (r *Runtime) stateReport(device string, at time.Time, silent bool, values map[string]json.RawMessage) chains.Message {
	if !silent {
		m, _ := chains.SubmitObservation(r.Config.ServiceID, chains.DeviceObserved, "1", []chains.Observation{{DeviceID: device, Values: values}})
		m.ObservedAt = at
		return m
	}
	m, _ := chains.New(chains.Report, r.Config.ServiceID, values)
	m.DeviceID, m.ServiceType, m.ObservedAt, m.Silent = device, r.registration.ServiceType, at, silent
	return m
}

func (r *Runtime) queueSnapshot() {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.reports = nil
	for device, attributes := range r.state {
		for k, record := range attributes {
			r.reports = append(r.reports, r.stateReport(device, record.ObservedAt, true, map[string]json.RawMessage{k: record.Value}))
		}
	}
}

func (r *Runtime) reportsEmpty() bool {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return len(r.reports) == 0
}

func (r *Runtime) flushReports(ctx context.Context, limit int) error {
	for i := 0; limit == 0 || i < limit; i++ {
		r.stateMu.Lock()
		if len(r.reports) == 0 || r.reset.Load() {
			r.stateMu.Unlock()
			return nil
		}
		m := r.reports[0]
		r.stateMu.Unlock()
		if err := r.send(ctx, m); err != nil {
			return err
		}
		r.stateMu.Lock()
		r.reports[0] = chains.Message{}
		r.reports = r.reports[1:]
		r.stateMu.Unlock()
	}
	if !r.reportsEmpty() {
		r.notify()
	}
	return nil
}

func cloneAttributes(source map[string]Attribute) map[string]Attribute {
	dest := maps.Clone(source)
	if dest == nil {
		return map[string]Attribute{}
	}
	for k, a := range dest {
		a.Value = bytes.Clone(a.Value)
		dest[k] = a
	}
	return dest
}

// GetState returns a copy of a service, device, or attribute subtree.
func (r *Runtime) GetState(device, attribute string) (any, error) {
	if device == "" && attribute != "" {
		return nil, errors.New("an attribute lookup requires a device")
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if device == "" {
		result := LocalState{}
		for id, values := range r.state {
			result[id] = cloneAttributes(values)
		}
		return result, nil
	}
	if attribute == "" {
		return cloneAttributes(r.state[device]), nil
	}
	result, ok := r.state[device][attribute]
	if !ok {
		return nil, nil
	}
	result.Value = bytes.Clone(result.Value)
	return result, nil
}

func (r *Runtime) SaveState(path string) error {
	snapshot, _ := r.GetState("", "")
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, err = temp.Write(raw)
	if err = errors.Join(err, temp.Close()); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

func (r *Runtime) LoadState(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state LocalState
	if err = json.Unmarshal(raw, &state); err != nil {
		return err
	}
	for device, attributes := range state {
		for name, a := range attributes {
			if a.ObservedAt.IsZero() || !json.Valid(a.Value) {
				return errors.New("invalid persisted state record")
			}
			if err := r.updateState(r.ctx, device, a.ObservedAt, a.ChangedAt, true, map[string]any{name: a.Value}); err != nil {
				return err
			}
		}
	}
	return nil
}

// QueryState asks the authority for a subtree. The caller's context bounds the wait.
func (r *Runtime) QueryState(ctx context.Context, service, device, attribute string) (StateReply, error) {
	if r.client.Bus == nil || r.ctx.Err() != nil {
		return StateReply{}, errors.New("service is not connected")
	}
	if device == "" && attribute != "" {
		return StateReply{}, errors.New("an attribute lookup requires a device")
	}
	if service == "" {
		service = r.Config.ServiceID
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	m, _ := chains.New(chains.StateQuery, r.Config.ServiceID, map[string]string{"service_id": service, "device_id": device, "attribute": attribute})
	m.Target = r.Config.Authority
	deadline, _ := ctx.Deadline()
	m.ExpiresAt = &deadline
	reply := make(chan queryResult, 1)
	r.queryMu.Lock()
	r.queries[m.ID] = reply
	r.queryMu.Unlock()
	defer func() { r.queryMu.Lock(); delete(r.queries, m.ID); r.queryMu.Unlock() }()
	select {
	case r.outgoing <- queryRequest{ctx, m}:
		r.notify()
	default:
		return StateReply{}, errors.New("state query queue full")
	}
	select {
	case response := <-reply:
		return response.reply, response.err
	case <-ctx.Done():
		return StateReply{}, ctx.Err()
	case <-r.ctx.Done():
		return StateReply{}, r.ctx.Err()
	}
}

func (r *Runtime) respond(queryID string, result queryResult) {
	r.queryMu.Lock()
	defer r.queryMu.Unlock()
	if reply := r.queries[queryID]; reply != nil {
		select {
		case reply <- result:
		default:
		}
	}
}

func (r *Runtime) handleQueryResult(m chains.Message) {
	var result queryResult
	if err := json.Unmarshal(m.Data, &result.reply); err != nil {
		result.err = err
	} else if result.reply.Error != "" {
		result.err = errors.New(result.reply.Error)
	}
	r.respond(m.CausationID, result)
}

func (r *Runtime) publishQueries() {
	for range 8 {
		select {
		case request := <-r.outgoing:
			if request.ctx.Err() != nil {
				continue
			}
			if err := r.send(request.ctx, request.message); err != nil {
				r.respond(request.message.ID, queryResult{err: err})
			}
		default:
			return
		}
	}
	if len(r.outgoing) > 0 {
		r.notify()
	}
}

func (r *Runtime) cacheObservation(o chains.Observation, at time.Time) {
	attributes := r.state[o.DeviceID]
	if attributes == nil {
		attributes = map[string]Attribute{}
		r.state[o.DeviceID] = attributes
	}
	for key, value := range o.Values {
		changed := at
		if previous, ok := attributes[key]; ok && bytes.Equal(previous.Value, value) {
			changed = previous.ChangedAt
		}
		attributes[key] = Attribute{bytes.Clone(value), changed, at}
	}
}
