package onkyo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"chains-automation/sdk"
)

var errUnavailable = errors.New("Onkyo receiver unavailable")
var errRejected = errors.New("Onkyo receiver rejected the control")

type receiver struct {
	address  string
	controls []control
	active   atomic.Pointer[session]
}

type request struct {
	ctx     context.Context
	control control
	value   string
	result  chan response
}

type response struct {
	value any
	err   error
}

type session struct {
	conn     net.Conn
	ctx      context.Context
	requests chan request
	incoming chan message
	readErr  error
	controls map[string]control
	latest   map[string]any
	refresh  map[string]bool
	runtime  *sdk.Runtime
}

func (r *receiver) submit(ctx context.Context, c control, value string) (any, error) {
	s := r.active.Load()
	if s == nil {
		return nil, errUnavailable
	}
	result := make(chan response, 1)
	select {
	case s.requests <- request{ctx, c, value, result}:
	case <-s.ctx.Done():
		return nil, errUnavailable
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case out := <-result:
		return out.value, out.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *receiver) run(ctx context.Context, runtime *sdk.Runtime) error {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	connectedBefore := false
	if err := runtime.SeedState("system", time.Time{}, map[string]any{"connected": false}); err != nil {
		return err
	}
	for ctx.Err() == nil {
		err := r.connect(ctx, runtime, &connectedBefore)
		if r.active.Swap(nil) != nil {
			backoff = time.Second
			if ctx.Err() == nil {
				if err := runtime.SetState(ctx, "system", map[string]any{"connected": false}); err != nil {
					return err
				}
			}
		}
		if ctx.Err() != nil {
			break
		}
		slog.Warn("Onkyo receiver unavailable; reconnecting", "address", r.address, "error", err, "delay", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		backoff = min(maxBackoff, 2*backoff)
	}
	return nil
}

func (r *receiver) connect(parent context.Context, runtime *sdk.Runtime, connectedBefore *bool) error {
	dialer := net.Dialer{Timeout: requestTimeout}
	conn, err := dialer.DialContext(parent, "tcp", r.address)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s := &session{conn: conn, ctx: ctx, requests: make(chan request), incoming: make(chan message, 32), controls: map[string]control{},
		latest: map[string]any{}, refresh: map[string]bool{}, runtime: runtime}
	for _, c := range r.controls {
		s.controls[c.code] = c
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.read(ctx)
	}()
	defer func() {
		cancel()
		conn.Close()
		<-done
	}()

	if err := s.query(ctx, "PWR", true); err != nil {
		return err
	}
	for _, c := range r.controls[1:] {
		if c.attribute == "power" || s.powered(c.device) {
			if err := s.optionalQuery(ctx, c.code, true); err != nil {
				return err
			}
		}
	}
	connected := map[string]any{"connected": true}
	if *connectedBefore {
		err = runtime.SetState(ctx, "system", connected)
	} else {
		err = runtime.SeedState("system", time.Time{}, connected)
	}
	if err != nil {
		return err
	}
	*connectedBefore = true
	r.active.Store(s)
	slog.Info("Onkyo receiver connected", "address", r.address)

	health := time.NewTimer(30 * time.Second)
	defer health.Stop()
	for {
		for zone := range s.refresh {
			delete(s.refresh, zone)
			for _, c := range r.controls {
				if c.device == zone && c.attribute != "power" && s.powered(zone) {
					if err := s.optionalQuery(ctx, c.code, false); err != nil {
						return err
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-s.incoming:
			if !ok {
				return s.readErr
			}
			if err := s.report(ctx, m, false, false); err != nil {
				return err
			}
		case <-health.C:
			if err := s.query(ctx, "PWR", false); err != nil {
				return err
			}
		case req := <-s.requests:
			if req.ctx.Err() != nil {
				req.result <- response{err: req.ctx.Err()}
				continue
			}
			value, err := s.set(ctx, req)
			req.result <- response{value, err}
			if err != nil && !errors.Is(err, errRejected) {
				return err
			}
		}
		health.Reset(30 * time.Second)
	}
}

func (s *session) powered(zone string) bool {
	return s.latest["PWR"] == true && zone == "main" || s.latest["ZPW"] == true && zone == "zone2"
}

func (s *session) read(ctx context.Context) {
	defer close(s.incoming)
	for {
		messages, err := readPacket(s.conn)
		if err != nil {
			s.readErr = err
			return
		}
		for _, m := range messages {
			select {
			case s.incoming <- m:
			case <-ctx.Done():
				s.readErr = ctx.Err()
				return
			}
		}
	}
}

func (s *session) report(ctx context.Context, m message, silent, confirmed bool) error {
	c, ok := s.controls[m.code]
	if !ok {
		return nil
	}
	value, ok := c.decode(m.value)
	if !ok || (!confirmed && s.latest[m.code] == value) {
		return nil
	}
	changed := s.latest[m.code] != value
	values := map[string]any{c.attribute: value}
	var err error
	if silent {
		err = s.runtime.SeedState(c.device, time.Time{}, values)
	} else {
		err = s.runtime.SetState(ctx, c.device, values)
	}
	if err == nil {
		s.latest[m.code] = value
		if !silent && changed && c.attribute == "power" && value == true {
			s.refresh[c.device] = true
		}
	}
	return err
}

func (s *session) write(ctx context.Context, command string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	data := packet(command)
	n, err := s.conn.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func (s *session) query(parent context.Context, code string, silent bool) error {
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()
	sent := time.Now()
	if err := s.write(ctx, code+"QSTN"); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-s.incoming:
			if !ok {
				return s.readErr
			}
			if err := s.report(parent, m, silent, false); err != nil {
				return err
			}
			if m.code == code && !m.received.Before(sent) {
				if _, ok := s.controls[code].decode(m.value); ok {
					return nil
				}
				if m.value == "N/A" {
					return errRejected
				}
			}
		}
	}
}

func (s *session) optionalQuery(ctx context.Context, code string, silent bool) error {
	err := s.query(ctx, code, silent)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errRejected) {
		return nil
	}
	return err
}

func (s *session) set(sessionCtx context.Context, req request) (any, error) {
	c := req.control
	sent := time.Now()
	if err := s.write(req.ctx, c.code+req.value); err != nil {
		return nil, err
	}
	want, _ := c.decode(req.value)
	deadline, _ := req.ctx.Deadline()
	readback := time.NewTimer(time.Until(deadline) / 2)
	defer readback.Stop()
	for {
		select {
		case <-req.ctx.Done():
			return nil, req.ctx.Err()
		case <-readback.C:
			if err := s.write(req.ctx, c.code+"QSTN"); err != nil {
				return nil, err
			}
		case m, ok := <-s.incoming:
			if !ok {
				return nil, s.readErr
			}
			if m.code == c.code && !m.received.Before(sent) && req.ctx.Err() == nil {
				if m.value == "N/A" {
					return nil, errRejected
				}
				if got, ok := c.decode(m.value); ok && got == want {
					if err := s.report(req.ctx, m, false, true); err != nil {
						return nil, err
					}
					return map[string]any{"attribute": c.attribute, "value": got}, nil
				}
			}
			if err := s.report(sessionCtx, m, false, false); err != nil {
				return nil, err
			}
		}
	}
}
