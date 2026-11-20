package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	chains "chains-automation"
)

// LocalService can be hosted by the state authority alongside the rules engine.
type LocalService interface {
	ID() string
	Run(context.Context, func(context.Context, chains.Message, bool) error)
	Execute(context.Context, string, chains.Action) error
}

type localInput struct {
	message chains.Message
	seed    bool
	mirror  bool
	done    chan error
}

type hostedService struct {
	service      LocalService
	input        chan localInput
	publications chan chains.Message
	slots        chan struct{}
	wg           sync.WaitGroup
	results      map[string]*chains.Message
	order        []string
}

func newHosted(service LocalService) *hostedService {
	return &hostedService{service: service, input: make(chan localInput, 128), publications: make(chan chains.Message, 1024), slots: make(chan struct{}, 16), results: map[string]*chains.Message{}}
}

func (e *Engine) startLocal(ctx context.Context) func() {
	h := e.local
	if h == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	h.wg.Add(2)
	go func() {
		defer h.wg.Done()
		h.service.Run(ctx, func(ctx context.Context, m chains.Message, seed bool) error {
			if m.Source != h.service.ID() {
				return errors.New("invalid local service source")
			}
			return h.submit(ctx, m, seed, true)
		})
	}()
	go func() {
		defer h.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-h.publications:
				client := e.node
				client.Source = m.Source
				if err := client.Send(ctx, m); err != nil {
					e.log.Warn("local service bus publication failed", "type", m.Type, "error", err)
				}
			}
		}
	}()
	return func() { cancel(); h.wg.Wait() }
}

func (h *hostedService) submit(ctx context.Context, m chains.Message, seed, mirror bool) error {
	input := localInput{m, seed, mirror, make(chan error, 1)}
	select {
	case h.input <- input:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-input.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) mirror(m chains.Message) {
	select {
	case e.local.publications <- m:
	default:
		e.log.Warn("local service publication queue full", "type", m.Type, "id", m.ID)
	}
}

func (e *Engine) ingestLocal(ctx context.Context, input localInput) error {
	m := input.message
	if input.seed {
		m.Silent = true
	}
	if err := m.Validate(time.Now()); err != nil {
		return err
	}
	if e.duplicate(m) {
		return nil
	}
	if err := e.handle(ctx, m); err != nil {
		return err
	}
	if input.mirror && m.Type != chains.ObservationSubmitted && m.Type != chains.Report {
		e.mirror(m)
	}
	return nil
}

func (e *Engine) executeLocal(ctx context.Context, m chains.Message) chains.Result {
	var a chains.Action
	if err := json.Unmarshal(m.Data, &a); err != nil {
		return chains.Result{Status: chains.Failed, Error: "invalid action"}
	}
	s, known := e.view.Service(m.Target)
	d, found := device(s, m.DeviceID)
	if !known || !found || !contains(d.Actions, a.Name) {
		return chains.Result{Status: chains.Failed, Error: "unknown device or action"}
	}
	args, err := json.Marshal(a.Args)
	if err == nil {
		err = e.validate(s.Actions[a.Name][m.SchemaVersion], args)
	}
	if err == nil {
		err = e.local.service.Execute(ctx, m.DeviceID, a)
	}
	if err == nil {
		return chains.Result{Status: chains.Submitted}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return chains.Result{Status: chains.TimedOut}
	}
	return chains.Result{Status: chains.Failed, Error: err.Error()}
}

func (e *Engine) localOutcome(m chains.Message, result chains.Result) chains.Message {
	out, _ := chains.New(chains.Outcome, e.local.service.ID(), result)
	out.Target, out.DeviceID, out.ActionID = m.Source, m.DeviceID, m.ActionID
	out.CausedBy(m)
	return out
}

func (e *Engine) remoteLocalAction(ctx context.Context, m chains.Message) error {
	if m.ActionID == "" {
		return errors.New("action ID required")
	}
	h := e.local
	key := m.Source + "/" + m.ActionID
	if previous, exists := h.results[key]; exists {
		if previous != nil {
			var outcome chains.Result
			if json.Unmarshal(previous.Data, &outcome) == nil {
				e.mirror(e.localOutcome(m, outcome))
			}
		}
		return nil
	}
	select {
	case h.slots <- struct{}{}:
	default:
		e.mirror(e.localOutcome(m, chains.Result{Status: chains.Failed, Error: "local action queue full"}))
		return nil
	}
	h.results[key] = nil
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() { <-h.slots }()
		deadline := time.Now().Add(e.actionTimeout)
		if m.ExpiresAt != nil && m.ExpiresAt.Before(deadline) {
			deadline = *m.ExpiresAt
		}
		actionCtx, cancel := context.WithDeadline(ctx, deadline)
		result := e.executeLocal(actionCtx, m)
		cancel()
		out := e.localOutcome(m, result)
		// Completion returns to the engine loop, which owns request deduplication.
		out.CorrelationID = m.CorrelationID
		if out.CorrelationID == "" {
			out.CorrelationID = m.ID
		}
		_ = h.submit(ctx, out, false, true)
	}()
	return nil
}

func (e *Engine) rememberLocalOutcome(m chains.Message) {
	key := m.Target + "/" + m.ActionID
	h := e.local
	if current, ok := h.results[key]; ok && current == nil {
		h.results[key] = &m
		h.order = append(h.order, key)
		if len(h.order) > 4096 {
			delete(h.results, h.order[0])
			h.order = h.order[1:]
		}
	}
}

func (e *Engine) actLocal(ctx context.Context, m chains.Message) map[string]any {
	result := e.executeLocal(ctx, m)
	out := e.localOutcome(m, result)
	// An action outcome remains observable even if the caller's deadline elapsed.
	select {
	case e.local.input <- localInput{message: out, mirror: true}:
	default:
		e.mirror(out)
	}
	e.log.Debug("local action outcome", "action", m.ActionID, "status", result.Status)
	return map[string]any{"status": result.Status, "error": result.Error, "action_id": m.ActionID}
}

func (e *Engine) publishDerived(ctx context.Context, m chains.Message) error {
	if e.local == nil {
		return e.node.Send(ctx, m)
	}
	// The engine loop is the sole owner of ingress deduplication.
	e.duplicate(m)
	if m.Type == chains.AvailabilityChanged {
		e.runtime.Dispatch(m)
	}
	e.mirror(m)
	return nil
}
