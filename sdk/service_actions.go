package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
)

type actionContextKey struct{}

func (r *Runtime) actionLoop() {
	results := map[string]chains.Result{}
	var order [4096]string
	orderPos := 0
	for {
		select {
		case <-r.ctx.Done():
			return
		case request := <-r.incoming:
			if request.Validate(time.Now()) != nil || r.ctx.Err() != nil {
				continue
			}
			key := request.Source + "/" + request.ActionID
			result, cached := results[key]
			if !cached {
				var executed bool
				result, executed = r.invoke(request)
				if executed {
					if evicted := order[orderPos]; evicted != "" {
						delete(results, evicted)
					}
					results[key] = result
					order[orderPos] = key
					orderPos = (orderPos + 1) % len(order)
				}
			}
			if err := r.outcome(r.ctx, request, result); err != nil && r.ctx.Err() == nil {
				r.log.Warn("action outcome publication failed", "error", err)
			}
		}
	}
}

func (r *Runtime) invoke(m chains.Message) (chains.Result, bool) {
	failed := func(err error) chains.Result {
		if errors.Is(err, context.DeadlineExceeded) {
			return chains.Result{Status: chains.TimedOut}
		}
		return chains.Result{Status: chains.Failed, Error: err.Error()}
	}
	var action chains.Action
	if err := json.Unmarshal(m.Data, &action); err != nil {
		return failed(errors.New("invalid action request")), false
	}
	d, ok := serviceDevice(r.registration, m.DeviceID)
	schema := r.actions[action.Name][m.SchemaVersion]
	if !ok || !supports(d, action.Name) || schema == nil {
		return failed(errors.New("action is unavailable on device")), false
	}
	if err := schema.Validate(action.Args); err != nil {
		return failed(errors.New("invalid action arguments")), false
	}
	ctx := context.WithValue(r.ctx, actionContextKey{}, m)
	cancel := func() {}
	if m.ExpiresAt != nil {
		ctx, cancel = context.WithDeadline(ctx, *m.ExpiresAt)
	}
	defer cancel()
	if err := r.outcome(ctx, m, chains.Result{Status: chains.Accepted}); err != nil {
		return failed(err), false
	}
	if err := ctx.Err(); err != nil {
		return failed(err), false
	}
	var value any
	var err error
	if action.Name == "get_state" {
		device, _ := action.Args["device_id"].(string)
		attribute, _ := action.Args["attribute"].(string)
		value, err = r.GetState(device, attribute)
	} else {
		value, err = r.definition.Actions[action.Name][m.SchemaVersion](ctx, r, m, action)
	}
	if err != nil {
		return failed(err), true
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return failed(errors.New("action result is not JSON encodable")), true
	}
	result := chains.Result{Status: chains.Completed, Value: raw}
	if _, err = r.resultEnvelope(m, result); err != nil {
		return failed(err), true
	}
	return result, true
}

func (r *Runtime) resultEnvelope(request chains.Message, result chains.Result) (chains.Message, error) {
	m, err := chains.New(chains.Outcome, r.Config.ServiceID, result)
	if err != nil {
		return m, err
	}
	m.Target, m.DeviceID, m.ActionID = request.Source, request.DeviceID, request.ActionID
	m.CausedBy(request)
	raw, err := chains.Encode(m, r.Config.Encoding)
	if err != nil {
		return m, err
	}
	if len(raw) > bus.MaxPayload {
		return m, errors.New("action result exceeds message limit")
	}
	return m, nil
}

func (r *Runtime) outcome(ctx context.Context, request chains.Message, result chains.Result) error {
	m, err := r.resultEnvelope(request, result)
	if err != nil {
		return err
	}
	return r.send(ctx, m)
}
