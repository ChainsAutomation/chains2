package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	chains "chains-automation"
	"chains-automation/rules"
	"chains-automation/state"
)

type dashboardCatalog struct {
	Version    int         `json:"version"`
	Revision   uint64      `json:"revision"`
	Home       string      `json:"home_id"`
	Dashboards []dashboard `json:"dashboards"`
}

type dashboard struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Pinned bool             `json:"pinned"`
	Blocks []dashboardBlock `json:"blocks"`
}

type dashboardBlock struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Title    string   `json:"title,omitempty"`
	Service  string   `json:"service_id,omitempty"`
	Device   string   `json:"device_id,omitempty"`
	Location []string `json:"location,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Class    string   `json:"device_class,omitempty"`
}

type dashboardLayout struct {
	ID     string             `json:"id"`
	Name   string             `json:"name"`
	Pinned bool               `json:"pinned"`
	Blocks []dashboardSection `json:"blocks"`
}

type dashboardSection struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Title   string   `json:"title,omitempty"`
	Devices []string `json:"devices"`
	Missing bool     `json:"missing,omitempty"`
}

var errDashboardConflict = errors.New("dashboards changed in another editor")

func readDashboard(raw json.RawMessage) (dashboardCatalog, error) {
	c := dashboardCatalog{Version: 1, Dashboards: []dashboard{}}
	if len(raw) == 0 {
		return c, nil
	}
	err := json.Unmarshal(raw, &c)
	return c, err
}

func validDashboard(c dashboardCatalog) error {
	if c.Version != 1 || len(c.Dashboards) > 32 {
		return errors.New("use dashboard version 1 and at most 32 dashboards")
	}
	boards := map[string]bool{}
	for _, board := range c.Dashboards {
		if !chains.Segment(board.ID) || board.ID == "." || board.ID == ".." || boards[board.ID] ||
			strings.TrimSpace(board.Name) == "" || len(board.Name) > 100 || len(board.Blocks) > 128 {
			return errors.New("invalid dashboard ID, name, or block count")
		}
		boards[board.ID] = true
		blocks := map[string]bool{}
		for _, b := range board.Blocks {
			if !chains.Segment(b.ID) || blocks[b.ID] || len(b.Title) > 100 {
				return errors.New("invalid dashboard block ID or title")
			}
			blocks[b.ID] = true
			switch b.Kind {
			case "device":
				if !chains.Segment(b.Service) || !chains.Segment(b.Device) {
					return errors.New("choose a device")
				}
			case "group":
				if len(b.Location)+len(b.Tags) == 0 && b.Class == "" {
					return errors.New("choose at least one group filter")
				}
				if _, err := b.filter(); err != nil {
					return err
				}
				if b.Class != "" && !chains.Segment(b.Class) {
					return errors.New("invalid device class")
				}
			case "separator":
			default:
				return errors.New("unknown dashboard block")
			}
		}
	}
	if c.Home != "" && !boards[c.Home] {
		return errors.New("choose a saved dashboard for home")
	}
	return nil
}

func (b dashboardBlock) filter() (chains.PlacementFilter, error) {
	input := map[string]any{}
	if len(b.Location) > 0 {
		input["location"] = b.Location
	}
	if len(b.Tags) > 0 {
		input["tags"] = b.Tags
	}
	return chains.ParsePlacementFilter(input)
}

func dashboardDevices(snapshot state.Snapshot) []deviceEntry {
	entries := []deviceEntry{}
	for _, service := range snapshot.Services {
		if service.UUID == chains.OperatorService {
			continue
		}
		for _, d := range service.Devices {
			values := map[string]state.Attribute{}
			for name, value := range snapshot.Devices[state.Address(service.UUID, d.ID)] {
				if hasAttribute(d, name) {
					values[name] = value
				}
			}
			entries = append(entries, deviceEntry{service.UUID, service.Name, service.ServiceType, d, values})
		}
	}
	slices.SortFunc(entries, compareDeviceEntries)
	return entries
}

func resolveDashboard(board dashboard, devices []deviceEntry) dashboardLayout {
	layout := dashboardLayout{board.ID, board.Name, board.Pinned, []dashboardSection{}}
	fixed, used, available := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, d := range devices {
		available[state.Address(d.ServiceID, d.Device.ID)] = true
	}
	for _, b := range board.Blocks {
		if b.Kind == "device" {
			fixed[state.Address(b.Service, b.Device)] = true
		}
	}
	for _, b := range board.Blocks {
		block := dashboardSection{ID: b.ID, Kind: b.Kind, Title: b.Title, Devices: []string{}}
		switch b.Kind {
		case "device":
			key := state.Address(b.Service, b.Device)
			if available[key] && !used[key] {
				block.Devices = append(block.Devices, key)
				used[key] = true
			} else if !available[key] {
				block.Missing = true
			}
		case "group":
			filter, err := b.filter()
			if err != nil {
				continue
			}
			for _, d := range devices {
				key := state.Address(d.ServiceID, d.Device.ID)
				if !fixed[key] && !used[key] && filter.Match(d.Device.Placement) &&
					(b.Class == "" || slices.Contains(d.Device.Classes, b.Class)) {
					block.Devices = append(block.Devices, key)
					used[key] = true
				}
			}
		}
		layout.Blocks = append(layout.Blocks, block)
	}
	return layout
}

func dashboardBody(w http.ResponseWriter, r *http.Request, target any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid or oversized request", 400)
		return false
	}
	return true
}

func (e *Engine) dashboardRoutes(mux *http.ServeMux, sessions *browserSessions, loopbackProxy bool) {
	const base = householdBase + "/dashboard"
	handle := func(pattern string, operatorOnly bool, next http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			session, ok := sessions.lookup(r)
			if !ok {
				http.Error(w, "unauthorized", 401)
				return
			}
			if operatorOnly && session.role != "operator" {
				http.Error(w, "operator access required", 403)
				return
			}
			if r.Method != http.MethodGet {
				if _, ok := webOrigin(r, loopbackProxy); !ok {
					http.Error(w, "forbidden origin", 403)
					return
				}
			}
			if e.authority == nil {
				http.NotFound(w, r)
				return
			}
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			stop := context.AfterFunc(session.Context, cancel)
			defer stop()
			next(w, r.WithContext(ctx))
		})
	}
	catalog := func(w http.ResponseWriter) (dashboardCatalog, bool) {
		raw, err := e.authority.DashboardCatalog()
		if err == nil {
			c, err := readDashboard(raw)
			if err == nil {
				return c, true
			}
		}
		http.Error(w, "dashboard catalog unavailable", 500)
		return dashboardCatalog{}, false
	}
	handle("GET "+base, false, func(w http.ResponseWriter, _ *http.Request) {
		c, ok := catalog(w)
		if !ok {
			return
		}
		snapshot := e.view.Snapshot()
		devices := dashboardDevices(snapshot)
		layouts := make([]dashboardLayout, 0, len(c.Dashboards))
		for _, board := range c.Dashboards {
			layouts = append(layouts, resolveDashboard(board, devices))
		}
		writeJSON(w, 200, struct {
			Home       string            `json:"home_id"`
			Devices    []deviceEntry     `json:"devices"`
			Dashboards []dashboardLayout `json:"dashboards"`
		}{c.Home, devices, layouts})
	})
	handle("GET "+base+"/config", true, func(w http.ResponseWriter, _ *http.Request) {
		if c, ok := catalog(w); ok {
			writeJSON(w, 200, c)
		}
	})
	handle("PUT "+base+"/config", true, func(w http.ResponseWriter, r *http.Request) {
		var c dashboardCatalog
		if !dashboardBody(w, r, &c) {
			return
		}
		if err := validDashboard(c); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if c.Dashboards == nil {
			c.Dashboards = []dashboard{}
		}
		for i := range c.Dashboards {
			if c.Dashboards[i].Blocks == nil {
				c.Dashboards[i].Blocks = []dashboardBlock{}
			}
		}
		err := e.authority.EditDashboardCatalog(func(current json.RawMessage) (json.RawMessage, error) {
			old, err := readDashboard(current)
			if err != nil {
				return nil, err
			}
			if old.Revision != c.Revision {
				return nil, errDashboardConflict
			}
			c.Revision++
			return json.Marshal(c)
		})
		if errors.Is(err, errDashboardConflict) {
			http.Error(w, err.Error(), 409)
			return
		}
		if err != nil {
			http.Error(w, "dashboard save failed", 500)
			return
		}
		writeJSON(w, 200, c)
	})
	handle("POST "+base+"/preview", true, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Board dashboard `json:"dashboard"`
		}
		if !dashboardBody(w, r, &req) {
			return
		}
		if err := validDashboard(dashboardCatalog{Version: 1, Dashboards: []dashboard{req.Board}}); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, 200, resolveDashboard(req.Board, dashboardDevices(e.view.Snapshot())))
	})
	handle("POST "+householdBase+"/actions", false, e.dashboardAction)
}

func (e *Engine) dashboardAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Service   string          `json:"service_id"`
		Device    string          `json:"device_id"`
		Attribute string          `json:"attribute"`
		Value     json.RawMessage `json:"value"`
	}
	if !dashboardBody(w, r, &req) {
		return
	}
	s, _ := e.view.Service(req.Service)
	d, known := device(s, req.Device)
	if !known || req.Service == chains.OperatorService {
		http.Error(w, "device unavailable", 404)
		return
	}
	var value any
	if json.Unmarshal(req.Value, &value) != nil {
		http.Error(w, "provide a value", 400)
		return
	}
	ref := rules.DeviceRef{ServiceID: req.Service, DeviceID: req.Device}
	setter, args, err := e.resolveDeviceSetter(d, req.Attribute, value)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	raw, _ := json.Marshal(args)
	if err = e.validate(s.Actions[setter.Action][setter.Version], raw); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	parent, _ := chains.New("operator.setter", e.node.Source, nil)
	result, err := e.Act(ctx, parent, ref, setter.Action, setter.Version, args)
	if err != nil {
		http.Error(w, "unable to send device request", 503)
		return
	}
	writeJSON(w, 200, map[string]any{"status": result["status"], "action_id": result["action_id"]})
}
