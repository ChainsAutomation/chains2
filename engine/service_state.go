package engine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
)

type presence struct {
	instance string
	deadline time.Time
	observed time.Time
}

type availability struct {
	Online     bool   `json:"online"`
	InstanceID string `json:"instance_id,omitempty"`
}

func (e *Engine) heartbeat(ctx context.Context, m chains.Message, now time.Time) error {
	if _, ok := e.view.Service(m.Source); !ok {
		return errors.New("heartbeat source is unregistered")
	}
	var ping struct {
		InstanceID string `json:"instance_id"`
		Interval   int    `json:"interval_seconds"`
	}
	if json.Unmarshal(m.Data, &ping) != nil || !chains.Segment(ping.InstanceID) || ping.Interval < 1 || ping.Interval > 3600 {
		return errors.New("invalid service heartbeat")
	}
	previous, known := e.availability[m.Source]
	if known && !m.ObservedAt.After(previous.observed) {
		return nil
	}
	e.availability[m.Source] = presence{instance: ping.InstanceID, deadline: now.Add(3 * time.Duration(ping.Interval) * time.Second), observed: m.ObservedAt}
	if !known || previous.deadline.IsZero() {
		return e.publishAvailability(ctx, m.Source, availability{true, ping.InstanceID}, &m)
	}
	return nil
}

func (e *Engine) expirePresence(ctx context.Context, now time.Time) {
	for source, p := range e.availability {
		if !p.deadline.IsZero() && !now.Before(p.deadline) {
			e.availability[source] = presence{instance: p.instance, observed: p.observed}
			if err := e.publishAvailability(ctx, source, availability{false, p.instance}, nil); err != nil {
				e.log.Warn("publish service availability", "error", err)
			}
		}
	}
}

func (e *Engine) publishAvailability(ctx context.Context, service string, current availability, cause *chains.Message) error {
	m, err := chains.New(chains.AvailabilityChanged, e.node.Source, current)
	if err != nil {
		return err
	}
	m.ServiceID = service
	if s, ok := e.view.Service(service); ok {
		m.ServiceType = s.ServiceType
	}
	if cause != nil {
		m.CausedBy(*cause)
	}
	return e.publishDerived(ctx, m)
}

func (e *Engine) queryState(ctx context.Context, m chains.Message) error {
	if _, ok := e.view.Service(m.Source); !ok {
		return errors.New("state query source is unregistered")
	}
	var query struct {
		ServiceID string `json:"service_id"`
		DeviceID  string `json:"device_id"`
		Attribute string `json:"attribute"`
	}
	var reply struct {
		Found        bool          `json:"found"`
		State        any           `json:"state,omitempty"`
		Availability *availability `json:"availability,omitempty"`
		Error        string        `json:"error,omitempty"`
	}
	if json.Unmarshal(m.Data, &query) != nil || !chains.Segment(query.ServiceID) ||
		(query.DeviceID != "" && !chains.Segment(query.DeviceID)) ||
		(query.Attribute != "" && (query.DeviceID == "" || !chains.Segment(query.Attribute))) {
		reply.Error = "invalid state query"
	} else {
		reply.State, reply.Found = e.view.Query(query.ServiceID, query.DeviceID, query.Attribute)
		if query.DeviceID == "" && reply.Found {
			p := e.availability[query.ServiceID]
			reply.Availability = &availability{!p.deadline.IsZero() && time.Now().Before(p.deadline), p.instance}
		}
	}
	out, err := chains.New(chains.StateReply, e.node.Source, reply)
	if err != nil {
		return err
	}
	out.Target = m.Source
	out.CausedBy(m)
	out.ExpiresAt = m.ExpiresAt
	if raw, err := chains.Encode(out, chains.JSON); err != nil {
		return err
	} else if len(raw) > bus.MaxPayload-512 {
		out.Data = json.RawMessage(`{"found":false,"error":"state subtree exceeds message limit"}`)
	}
	return e.publishDerived(ctx, out)
}
