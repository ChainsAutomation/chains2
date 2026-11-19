package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/operator"
	"chains-automation/rules"
	"chains-automation/state"
)

type operatorMonitor struct {
	feed    *operator.Feed
	open    func(context.Context, bus.Subscription) (bus.Bus, error)
	trace   chan operator.Record
	dropped atomic.Uint64
	wg      sync.WaitGroup
}

func (e *Engine) offerTrace(record operator.Record) {
	if e.operator == nil {
		return
	}
	record.Engine = e.node.Source
	select {
	case e.operator.trace <- record.Bounded():
	default:
		e.operator.dropped.Add(1)
	}
}

func (e *Engine) ruleTrace(t rules.Trace) {
	e.offerTrace(operator.Record{At: t.At, Type: t.Type, Rule: t.Rule, Run: t.Run, Event: t.Event, Correlation: t.Correlation, Source: t.Source, Service: t.Service, Device: t.Device, Action: t.Action, Cause: t.Cause, Summary: t.Summary})
}

func (e *Engine) startOperator(ctx context.Context) func() {
	m := e.operator
	if m == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	if m.feed != nil && m.open != nil {
		m.wg.Add(1)
		go func() { defer m.wg.Done(); e.observeBus(ctx) }()
	}
	m.wg.Add(1)
	go func() { defer m.wg.Done(); e.flushTraces(ctx) }()
	return func() { cancel(); m.wg.Wait() }
}

func (e *Engine) operatorConnect(ctx context.Context, sub bus.Subscription) (bus.Bus, error) {
	attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return e.operator.open(attempt, sub)
}

func operatorPause(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Second):
		return true
	}
}

func (e *Engine) flushTraces(ctx context.Context) {
	m := e.operator
	var publisher bus.Bus
	defer func() {
		if publisher != nil {
			_ = publisher.Close()
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var r operator.Record
		select {
		case <-ctx.Done():
			return
		case r = <-m.trace:
		case <-ticker.C:
			if n := m.dropped.Swap(0); n > 0 {
				r = operator.Record{Type: "watch.gap", Engine: e.node.Source, Summary: fmt.Sprintf("%d engine trace records dropped", n)}
			}
		}
		if r.Type == "" {
			continue
		}
		if m.feed != nil {
			m.feed.Append(r)
			continue
		}
		if m.open == nil {
			continue
		}
		if publisher == nil {
			opened, err := e.operatorConnect(ctx, bus.Subscription{Types: []string{"operator.publisher.idle"}})
			if err != nil {
				m.dropped.Add(1)
				if !operatorPause(ctx) {
					return
				}
				continue
			}
			publisher = opened
		}
		message, err := chains.New(operator.TraceType, e.node.Source, r)
		if err != nil {
			continue
		}
		client := e.node
		client.Bus = publisher
		publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = client.Send(publishCtx, message)
		cancel()
		if err != nil {
			m.dropped.Add(1)
			_ = publisher.Close()
			publisher = nil
		}
	}
}

func (e *Engine) observeBus(ctx context.Context) {
	feed := e.operator.feed
	seen := map[string]bool{}
	ids := make([]string, 8192)
	index := 0
	for ctx.Err() == nil {
		b, err := e.operatorConnect(ctx, bus.Subscription{})
		if err != nil {
			feed.Append(operator.Record{Type: "watch.gap", Summary: "Broker observer unavailable"})
			if !operatorPause(ctx) {
				return
			}
			continue
		}
		observing := true
		for observing {
			select {
			case <-ctx.Done():
				_ = b.Close()
				return
			case delivery, ok := <-b.Receive():
				if !ok {
					observing = false
					continue
				}
				if delivery.Reset {
					feed.Append(operator.Record{Type: "watch.gap", Summary: "Broker observer lost continuity"})
					continue
				}
				if delivery.Retained {
					continue
				}
				msg, err := chains.Decode(delivery.Data)
				if err != nil || !bus.Match(delivery.Topic, msg) {
					continue
				}
				if err := msg.Validate(time.Now()); err != nil && !errors.Is(err, chains.ErrExpired) {
					continue
				}
				key := msg.Source + "/" + msg.ID
				if seen[key] {
					continue
				}
				delete(seen, ids[index])
				ids[index] = key
				seen[key] = true
				index = (index + 1) % len(ids)
				if msg.Type == operator.TraceType {
					var r operator.Record
					if json.Unmarshal(msg.Data, &r) == nil && r.Type != "" {
						r.Engine = msg.Source
						feed.Append(r)
					}
					continue
				}
				record := messageActivity(msg)
				feed.Append(record)
			}
		}
		_ = b.Close()
		feed.Append(operator.Record{Type: "watch.gap", Summary: "Broker observer disconnected"})
		if !operatorPause(ctx) {
			return
		}
	}
}

func messageActivity(msg chains.Message) operator.Record {
	record := operator.Record{At: msg.ObservedAt, Type: msg.Type, Source: msg.Source, Service: msg.ServiceID, Device: msg.DeviceID, Target: msg.Target, Event: msg.ID, Action: msg.ActionID, Correlation: msg.CorrelationID, Cause: msg.CausationID, Data: msg.Data}
	if record.Service == "" {
		record.Service = msg.Source
	}
	switch msg.Type {
	case chains.SnapshotResponse:
		record.Data = nil
		record.Summary = "State snapshot chunk"
	case chains.Request:
		record.Service, record.Engine = msg.Target, msg.Source
		var a chains.Action
		if json.Unmarshal(msg.Data, &a) == nil {
			record.Summary = a.Name
		}
	case chains.Outcome:
		record.Engine = msg.Target
		var result chains.Result
		if json.Unmarshal(msg.Data, &result) == nil {
			record.Summary = result.Status
		}
	case chains.Report:
		if msg.Silent {
			record.Summary = "Silent state synchronization"
		}
	case state.Updated:
		var update struct {
			Address string `json:"address"`
			Service *struct {
				UUID string `json:"uuid"`
			} `json:"service"`
		}
		if json.Unmarshal(msg.Data, &update) == nil {
			if service, device, ok := strings.Cut(update.Address, "/"); ok {
				record.Service, record.Device = service, device
			} else if update.Service != nil {
				record.Service = update.Service.UUID
			}
		}
	}
	if record.Correlation == "" {
		record.Correlation = msg.ID
	}
	return record
}

// traceAction reports action activity that has no equivalent broker message.
func (e *Engine) traceAction(m chains.Message, kind, summary string) {
	if e.operator == nil {
		return
	}
	record := messageActivity(m)
	record.Type = kind
	if kind != m.Type {
		record.At = time.Now().UTC()
		record.Data = nil
	}
	if summary != "" {
		record.Summary = summary
	}
	e.offerTrace(record)
}

func (e *Engine) operatorRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, mappings map[string]HTTPSource) {
	if e.operator == nil || e.operator.feed == nil {
		return
	}
	mux.HandleFunc("GET "+operator.Base+"/state", auth(e.operatorState))
	mux.HandleFunc("GET "+operator.Base+"/services", auth(func(w http.ResponseWriter, _ *http.Request) {
		services := e.view.Services()
		sort.Slice(services, func(i, j int) bool { return services[i].UUID < services[j].UUID })
		writeJSON(w, http.StatusOK, services)
	}))
	mux.HandleFunc("GET "+operator.Base+"/traces", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("correlation")
		if !chains.Segment(id) {
			http.Error(w, "invalid correlation ID", http.StatusBadRequest)
			return
		}
		records, _, _, _, _ := e.operator.feed.Read("", operator.HistorySize)
		result := operator.TraceHistory{Records: []operator.Record{}}
		if len(records) > 0 {
			result.OldestID = records[0].ID
			result.OldestAt = &records[0].At
		}
		for _, record := range records {
			if record.Correlation == id {
				result.Records = append(result.Records, record)
			}
		}
		writeJSON(w, http.StatusOK, result)
	}))
	mux.HandleFunc("GET "+operator.Base+"/profiles", auth(func(w http.ResponseWriter, _ *http.Request) {
		list := []operator.Profile{}
		for name, mapping := range mappings {
			profile := operator.Profile{Name: name, Source: mapping.Source, Type: mapping.Type, Device: mapping.Device, Version: mapping.SchemaVersion}
			if s, ok := e.view.Service(mapping.Source); ok {
				raw := s.Events[mapping.Type][mapping.SchemaVersion]
				_, known := device(s, mapping.Device)
				profile.Ready = len(raw) > 0 && (mapping.Device == "" || known) && e.ready.Load()
				profile.Schema = raw
				if mapping.Attribute != "" {
					var object struct{ Properties map[string]json.RawMessage }
					_ = json.Unmarshal(raw, &object)
					profile.Schema = object.Properties[mapping.Attribute]
				}
			}
			list = append(list, profile)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		writeJSON(w, http.StatusOK, list)
	}))
	mux.HandleFunc("POST "+operator.Base+"/events", auth(func(w http.ResponseWriter, r *http.Request) { e.operatorSend(w, r, mappings) }))
	mux.HandleFunc("GET "+operator.Base+"/activity", auth(e.operatorWatch))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (e *Engine) operatorState(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	serviceID, deviceID, attribute := query.Get("service"), query.Get("device"), query.Get("attribute")
	if deviceID != "" && serviceID == "" || attribute != "" && deviceID == "" {
		http.Error(w, "state filter requires its parent ID", http.StatusBadRequest)
		return
	}
	s := e.view.Snapshot()
	result := operator.State{Epoch: s.Epoch, Revision: s.Revision, Rows: []operator.StateRow{}}
	for id, service := range s.Services {
		if serviceID != "" && id != serviceID {
			continue
		}
		if deviceID == "" {
			result.Rows = append(result.Rows, operator.StateRow{Kind: "service", Service: id, ServiceName: service.Name})
		}
		for _, d := range service.Devices {
			if deviceID != "" && d.ID != deviceID {
				continue
			}
			if attribute == "" {
				result.Rows = append(result.Rows, operator.StateRow{Kind: "device", Service: id, ServiceName: service.Name, Device: d.ID, DeviceName: d.Name})
			}
			for name, value := range s.Devices[id+"/"+d.ID] {
				if attribute != "" && attribute != name {
					continue
				}
				result.Rows = append(result.Rows, operator.StateRow{Kind: "attribute", Service: id, ServiceName: service.Name, Device: d.ID, DeviceName: d.Name, Attribute: name, Value: value.Value, Observed: &value.ObservedAt, Changed: &value.ChangedAt, Revision: value.Revision})
			}
		}
	}
	sort.Slice(result.Rows, func(i, j int) bool {
		a, b := result.Rows[i], result.Rows[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Device != b.Device {
			return a.Device < b.Device
		}
		return a.Attribute < b.Attribute
	})
	writeJSON(w, http.StatusOK, result)
}

var manualName = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,119}$`)

func (e *Engine) operatorSend(w http.ResponseWriter, r *http.Request, mappings map[string]HTTPSource) {
	if !e.ready.Load() {
		http.Error(w, "state view is synchronizing", http.StatusServiceUnavailable)
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, bus.MaxPayload-1024))
	decoder.DisallowUnknownFields()
	var request operator.Submission
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid event submission", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF || (request.Name == "") == (request.Profile == "") || len(request.Data) == 0 {
		http.Error(w, "supply one event profile or custom name and JSON data", http.StatusBadRequest)
		return
	}
	var m chains.Message
	var err error
	if request.Profile != "" {
		mapping, ok := mappings[request.Profile]
		if !ok {
			http.NotFound(w, r)
			return
		}
		m, err = e.mappedEvent(mapping, request.Data)
	} else {
		if !manualName.MatchString(request.Name) {
			http.Error(w, "invalid manual event name", http.StatusBadRequest)
			return
		}
		m, err = chains.New("manual."+request.Name, e.authorityID, request.Data)
		if err == nil {
			m, err = e.validateEvent(m)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	client := e.node
	client.Source = m.Source
	if err = client.Send(r.Context(), m); err != nil {
		http.Error(w, "bus unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusAccepted, operator.Receipt{ID: m.ID, Type: m.Type, Source: m.Source})
}

func (e *Engine) operatorWatch(w http.ResponseWriter, r *http.Request) {
	recent := 0
	if raw := r.URL.Query().Get("recent"); raw != "" {
		var err error
		recent, err = strconv.Atoi(raw)
		if err != nil || recent < 0 || recent > operator.HistorySize {
			http.Error(w, "invalid recent count", http.StatusBadRequest)
			return
		}
	}
	feed := e.operator.feed
	cursor := r.Header.Get("Last-Event-ID")
	records, cursor, gap, changed, err := feed.Read(cursor, recent)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	flush := func() error {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return controller.Flush()
	}
	if !gap {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err = fmt.Fprintf(w, "id: %s\n\n", cursor); err != nil {
			return
		}
	}
	if err = flush(); err != nil {
		return
	}
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	send := func(record operator.Record) error {
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err = fmt.Fprintf(w, "id: %s\ndata: %s\n\n", record.ID, raw); err != nil {
			return err
		}
		return flush()
	}
	for {
		if gap {
			if send(operator.Record{ID: cursor, At: time.Now().UTC(), Type: "watch.gap", Summary: "Activity cursor expired or authority restarted"}) != nil {
				return
			}
		}
		for _, record := range records {
			if send(record) != nil {
				return
			}
			cursor = record.ID
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-timer.C:
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err = io.WriteString(w, ": keepalive\n\n"); err != nil || flush() != nil {
				return
			}
		}
		records, cursor, gap, changed, err = feed.Read(cursor, 0)
		if err != nil {
			return
		}
	}
}

func (e *Engine) normalizeManual(m chains.Message) (chains.Message, error) {
	name := strings.TrimPrefix(m.Type, "manual.")
	var body map[string]json.RawMessage
	if !manualName.MatchString(name) || m.Source != e.authorityID || m.DeviceID != "" || m.Target != "" ||
		json.Unmarshal(m.Data, &body) != nil || body == nil {
		return m, errors.New("manual events require authority origin and a JSON object")
	}
	m.ServiceID, m.ServiceType, m.DeviceClass, m.SchemaVersion = e.authorityID, "operator", "", "1"
	return m, nil
}
