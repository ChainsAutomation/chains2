// Package sdk provides the transport-neutral Go client protocol.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
)

type Client struct {
	Bus         bus.Bus
	Encoding    chains.Encoding
	Source      string
	ServiceType string
}

func (c Client) Send(ctx context.Context, m chains.Message) error {
	if m.Source != c.Source {
		return errors.New("message source does not match client")
	}
	if err := m.Validate(time.Now()); err != nil {
		return err
	}
	encoding := c.Encoding
	if encoding == "" {
		encoding = chains.JSON
	}
	payload, err := chains.Encode(m, encoding)
	if err != nil {
		return err
	}
	return c.Bus.Publish(ctx, bus.Route(m), payload)
}

func (c Client) Register(ctx context.Context, s chains.Service) error {
	m, err := chains.New(chains.Registration, c.Source, s)
	if err != nil {
		return err
	}
	return c.Send(ctx, m)
}

func (c Client) Event(ctx context.Context, kind, device, version string, data any) error {
	m, err := chains.New(kind, c.Source, data)
	if err != nil {
		return err
	}
	m.DeviceID, m.ServiceType, m.SchemaVersion = device, c.ServiceType, version
	return c.Send(ctx, m)
}

func (c Client) Report(ctx context.Context, device string, values map[string]any, cause *chains.Message) error {
	m, err := chains.New(chains.Report, c.Source, values)
	if err != nil {
		return err
	}
	m.DeviceID, m.ServiceType = device, c.ServiceType
	if cause != nil {
		m.CausedBy(*cause)
		m.ActionID = cause.ActionID
	}
	return c.Send(ctx, m)
}

func (c Client) Outcome(ctx context.Context, request chains.Message, status string, actionError error) error {
	result := chains.Result{Status: status}
	if actionError != nil {
		result.Error = actionError.Error()
	}
	m, err := chains.New(chains.Outcome, c.Source, result)
	if err != nil {
		return err
	}
	m.Target, m.ActionID, m.DeviceID = request.Source, request.ActionID, request.DeviceID
	m.CausedBy(request)
	return c.Send(ctx, m)
}

type Handler func(context.Context, chains.Message, chains.Action) error

// Serve processes actions serially and deduplicates retries within the most recent 4096 requests.
func (c Client) Serve(ctx context.Context, service chains.Service, handler Handler) error {
	if err := c.Register(ctx, service); err != nil {
		return err
	}
	results := map[string]chains.Result{}
	order := make([]string, 0, 4096)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-c.Bus.Receive():
			if !ok {
				return errors.New("action subscription closed")
			}
			if d.Reset {
				if err := c.Register(ctx, service); err != nil {
					return err
				}
				continue
			}
			if d.Retained {
				continue
			}
			m, err := chains.Decode(d.Data)
			if err != nil || !bus.Match(d.Topic, m) || m.Type != chains.Request || m.Target != c.Source || m.ActionID == "" {
				continue
			}
			key := m.Source + "/" + m.ActionID
			if result, ok := results[key]; ok {
				var cause error
				if result.Error != "" {
					cause = errors.New(result.Error)
				}
				if err = c.Outcome(ctx, m, result.Status, cause); err != nil {
					return err
				}
				continue
			}
			if err = m.Validate(time.Now()); err != nil {
				continue
			}
			var action chains.Action
			if err = json.Unmarshal(m.Data, &action); err != nil {
				continue
			}
			device, exists := serviceDevice(service, m.DeviceID)
			version := service.Actions[action.Name][m.SchemaVersion]
			if !exists || len(version) == 0 || !supports(device, action.Name) {
				if err = c.Outcome(ctx, m, chains.Failed, errors.New("action is unavailable on device")); err != nil {
					return err
				}
				continue
			}
			if err = c.Outcome(ctx, m, chains.Accepted, nil); err != nil {
				return err
			}
			actionCtx := ctx
			cancel := func() {}
			if m.ExpiresAt != nil {
				actionCtx, cancel = context.WithDeadline(ctx, *m.ExpiresAt)
			}
			actionErr := handler(actionCtx, m, action)
			cancel()
			result := chains.Result{Status: chains.Completed}
			if actionErr != nil {
				result.Status = chains.Failed
				result.Error = actionErr.Error()
			}
			results[key] = result
			order = append(order, key)
			if len(order) > 4096 {
				delete(results, order[0])
				order = order[1:]
			}
			if err = c.Outcome(ctx, m, result.Status, actionErr); err != nil {
				return err
			}
		}
	}
}

func serviceDevice(s chains.Service, id string) (chains.Device, bool) {
	for _, d := range s.Devices {
		if d.ID == id {
			return d, true
		}
	}
	return chains.Device{}, false
}

func supports(d chains.Device, action string) bool {
	for _, name := range d.Actions {
		if name == action {
			return true
		}
	}
	return false
}
