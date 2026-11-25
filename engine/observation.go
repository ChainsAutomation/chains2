package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/operator"
	"chains-automation/rules"
	"chains-automation/state"
)

func (e *Engine) validateObservation(m chains.Message, batch *chains.ObservationBatch) (chains.Service, error) {
	s, ok := e.view.Service(m.Source)
	manual := m.Source == chains.OperatorService && strings.HasPrefix(batch.EventType, "manual.") &&
		manualName.MatchString(strings.TrimPrefix(batch.EventType, "manual."))
	if !ok || m.Target != "" || len(batch.Observations) == 0 || !chains.Segment(m.SchemaVersion) || !chains.Segment(batch.EventType) ||
		(!manual && batch.EventType != chains.DeviceObserved && reservedEvent(batch.EventType)) {
		return s, errors.New("invalid observation source or event type")
	}
	devices := make(map[string]bool, len(batch.Observations))
	for i := range batch.Observations {
		o := &batch.Observations[i]
		d, known := device(s, o.DeviceID)
		if !known || devices[o.DeviceID] || len(o.Values) == 0 {
			return s, fmt.Errorf("unknown, duplicate or empty observation device %q", o.DeviceID)
		}
		devices[o.DeviceID] = true
		if err := e.validateAttributes(d, o.Values); err != nil {
			return s, err
		}
		if len(o.Data) == 0 {
			o.Data, _ = json.Marshal(o.Values)
		}
		if manual {
			var body map[string]json.RawMessage
			if json.Unmarshal(o.Data, &body) != nil || body == nil {
				return s, errors.New("manual events require a JSON object")
			}
		} else if batch.EventType != chains.DeviceObserved {
			if err := e.validate(s.Events[batch.EventType][m.SchemaVersion], o.Data); err != nil {
				return s, err
			}
		}
		o.DeviceName, o.DeviceClasses, o.Placement = d.Name, d.Classes, d.Placement
	}
	return s, nil
}

func reservedEvent(kind string) bool {
	switch kind {
	case chains.Registration, chains.Report, chains.Changed, chains.Request, chains.Outcome,
		chains.StateQuery, chains.StateReply, chains.Heartbeat, chains.AvailabilityChanged,
		chains.SnapshotRequest, chains.SnapshotResponse, chains.ObservationSubmitted, chains.DeviceObserved,
		state.Updated, state.HeadType, "time.matched":
		return true
	}
	return strings.HasPrefix(kind, "manual.") || kind == operator.TraceType
}

func (e *Engine) observe(ctx context.Context, m chains.Message) error {
	var batch chains.ObservationBatch
	if err := json.Unmarshal(m.Data, &batch); err != nil {
		return err
	}
	service, err := e.validateObservation(m, &batch)
	if err != nil {
		return err
	}
	return e.commitObservation(ctx, m, service, batch)
}

func (e *Engine) commitObservation(ctx context.Context, m chains.Message, service chains.Service, batch chains.ObservationBatch) error {
	var frames []chains.Message
	var encoded [][]byte
	update, _, err := e.authority.Observe(m, batch.Observations, func(u state.Update, changes []state.Change) error {
		frames = frames[:0]
		encoded = encoded[:0]
		patch, err := chains.New(state.Updated, e.node.Source, u)
		if err != nil {
			return err
		}
		patch.CausedBy(m)
		patch.ServiceID = service.UUID
		if len(batch.Observations) == 1 {
			patch.DeviceID = batch.Observations[0].DeviceID
		}
		if !m.Silent && m.CorrelationID == "" {
			patch.CorrelationID = observationID(m)
		}
		frames = append(frames, patch)
		if !m.Silent {
			event := m
			event.ID = observationID(m)
			event.Type, event.Source = batch.EventType, e.node.Source
			event.ServiceID, event.ServiceName, event.ServiceType = service.UUID, service.Name, service.ServiceType
			event.DeviceID, event.DeviceName, event.DeviceClasses = "", "", nil
			event.Placement = chains.Placement{}
			if len(batch.Observations) == 1 {
				o := batch.Observations[0]
				event.DeviceID, event.DeviceName, event.DeviceClasses, event.Placement = o.DeviceID, o.DeviceName, o.DeviceClasses, o.Placement
			}
			event.Epoch, event.Revision = u.Epoch, u.Revision
			event.Observations = batch.Observations
			event.Data = json.RawMessage("null")
			if event.ExpiresAt == nil {
				expiry := time.Now().Add(time.Minute)
				event.ExpiresAt = &expiry
			}
			if event.CorrelationID == "" {
				event.CorrelationID = event.ID
			}
			frames = append(frames, event)
			for _, change := range changes {
				leaf, err := chains.New(chains.Changed, e.node.Source, change)
				if err != nil {
					return err
				}
				d, _ := device(service, change.DeviceID)
				leaf.ServiceID, leaf.ServiceName, leaf.ServiceType = service.UUID, service.Name, service.ServiceType
				leaf.DeviceID, leaf.DeviceName, leaf.DeviceClasses, leaf.Placement = d.ID, d.Name, d.Classes, d.Placement
				leaf.Epoch, leaf.Revision = u.Epoch, u.Revision
				leaf.ObservedAt, leaf.ExpiresAt, leaf.ActionID = m.ObservedAt, event.ExpiresAt, m.ActionID
				leaf.CausedBy(event)
				frames = append(frames, leaf)
			}
		}
		encoded = make([][]byte, len(frames))
		for i, frame := range frames {
			raw, err := chains.Encode(frame, e.node.Encoding)
			if err != nil {
				return err
			}
			if len(raw) > bus.MaxPayload {
				return bus.ErrTooLarge
			}
			encoded[i] = raw
		}
		return nil
	})
	if err != nil {
		return err
	}
	if update.Revision == 0 {
		return nil
	}
	for _, frame := range frames {
		if frame.Type != state.Updated && (frame.Type == chains.Changed || len(e.eventTypes) == 0 || contains(e.eventTypes, frame.Type)) {
			e.runtime.Dispatch(frame)
		}
	}
	for i, frame := range frames {
		if err := e.publishDerivedEncoded(ctx, frame, encoded[i]); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) receiveObservation(ctx context.Context, m chains.Message) error {
	if m.Source != e.authorityID || m.Epoch == "" || m.Revision == 0 {
		return errors.New("expected authority-committed event")
	}
	if e.authority != nil {
		return nil
	}
	epoch, revision := e.view.Position()
	if e.ready.Load() && epoch == m.Epoch && revision >= m.Revision {
		e.runtime.Dispatch(m)
	} else if len(e.pendingEvents) < 1024 {
		e.pendingEvents = append(e.pendingEvents, m)
		if epoch != m.Epoch {
			e.ready.Store(false)
			e.requestSnapshot(ctx)
		}
	}
	return nil
}

func (e *Engine) registerOperator() error {
	if _, exists := e.view.Service(chains.OperatorService); exists {
		return nil
	}
	service := chains.Service{UUID: chains.OperatorService, Name: "Operator", ServiceType: "operator", Devices: []chains.Device{
		{ID: "system", Name: "System", Classes: []string{}, Attributes: chains.Attributes("last_manual_event")},
	}}
	m, err := chains.New(chains.Registration, service.UUID, service)
	if err != nil {
		return err
	}
	_, err = e.authority.Register(m, service)
	return err
}

func (e *Engine) makeObservation(source, kind, version, device, attribute string, value, data any) (chains.Message, error) {
	o, err := chains.NewObservation(device, map[string]any{attribute: value}, data)
	if err != nil {
		return chains.Message{}, err
	}
	m, err := chains.SubmitObservation(source, kind, version, []chains.Observation{o})
	if err != nil {
		return m, err
	}
	if err = m.Validate(time.Now()); err != nil {
		return m, err
	}
	_, err = e.validateObservation(m, &chains.ObservationBatch{EventType: kind, Observations: []chains.Observation{o}})
	return m, err
}

func (e *Engine) ResolveDevices(selectors map[string]string, placement chains.PlacementFilter) []rules.DeviceRef {
	var result []rules.DeviceRef
	for _, service := range e.view.Services() {
		if id := selectors["service_id"]; id != "" && id != service.UUID {
			continue
		}
		if name := selectors["service_name"]; name != "" && name != service.Name {
			continue
		}
		for _, device := range service.Devices {
			if !placement.Match(device.Placement) {
				continue
			}
			if class := selectors["device_class"]; class != "" && !slices.Contains(device.Classes, class) {
				continue
			}
			if id := selectors["device_id"]; id != "" && id != device.ID {
				continue
			}
			if name := selectors["device_name"]; name != "" && name != device.Name {
				continue
			}
			result = append(result, rules.DeviceRef{ServiceID: service.UUID, DeviceID: device.ID})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ServiceID == result[j].ServiceID {
			return result[i].DeviceID < result[j].DeviceID
		}
		return result[i].ServiceID < result[j].ServiceID
	})
	return result
}

func observationID(m chains.Message) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(m.Source+"/"+m.ID)))
}
