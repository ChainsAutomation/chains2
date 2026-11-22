package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	chains "chains-automation"
	"chains-automation/operator"
	"chains-automation/state"
)

type placementEdit struct {
	ctx             context.Context
	service, device string
	location, tags  json.RawMessage
	done            chan error
}

func (e *Engine) editPlacement(ctx context.Context, edit placementEdit) error {
	update, err := e.authority.EditPlacement(edit.service, edit.device, func(p state.PlacementOverride) (state.PlacementOverride, error) {
		for _, field := range []struct {
			raw json.RawMessage
			dst **[]string
		}{{edit.location, &p.Location}, {edit.tags, &p.Tags}} {
			if len(field.raw) == 0 {
				continue
			}
			if bytes.Equal(field.raw, []byte("null")) {
				*field.dst = nil
				continue
			}
			var list []string
			if err := json.Unmarshal(field.raw, &list); err != nil {
				return p, errors.New("location and tags must be string arrays or null")
			}
			*field.dst = &list
		}
		validation := chains.Placement{}
		if p.Location != nil {
			validation.Location = *p.Location
		}
		if p.Tags != nil {
			validation.Tags = *p.Tags
		}
		if err := validation.Normalize(); err != nil {
			return p, err
		}
		if p.Tags != nil {
			p.Tags = &validation.Tags
		}
		return p, nil
	})
	if err != nil {
		return err
	}
	if update.Revision == 0 {
		return errors.New("placement commit failed")
	}
	cause, _ := chains.New("operator.placement", e.node.Source, nil)
	if err := e.publishUpdate(ctx, cause, update); err != nil {
		e.log.Warn("publish placement revision", "error", err)
	}
	e.offerTrace(operator.Record{Type: "device.placement.changed", Service: edit.service, Device: edit.device, Summary: "Device placement updated"})
	return nil
}

func (e *Engine) deviceRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	base := operator.Base + "/devices"
	mux.HandleFunc("GET "+base, auth(e.listDevices))
	mux.HandleFunc("PATCH "+base+"/{service}/{device}/placement", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Location json.RawMessage `json:"location"`
			Tags     json.RawMessage `json:"tags"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || (len(body.Location) == 0 && len(body.Tags) == 0) {
			http.Error(w, "provide location or tags", 400)
			return
		}
		edit := placementEdit{r.Context(), r.PathValue("service"), r.PathValue("device"), body.Location, body.Tags, make(chan error, 1)}
		select {
		case e.placementEdits <- edit:
		case <-r.Context().Done():
			return
		}
		select {
		case <-r.Context().Done():
			return
		case err := <-edit.done:
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, state.ErrUnknownDevice) {
					status = http.StatusNotFound
				}
				http.Error(w, err.Error(), status)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
}

type deviceEntry struct {
	ServiceID   string                     `json:"service_id"`
	ServiceName string                     `json:"service_name"`
	ServiceType string                     `json:"service_type"`
	Device      chains.Device              `json:"device"`
	State       map[string]state.Attribute `json:"state"`
}

func (e *Engine) listDevices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	input := map[string]any{}
	if q.Has("location") {
		input["location"] = q.Get("location")
	}
	if segments := q["location_segment"]; len(segments) > 0 {
		if q.Has("location") {
			http.Error(w, "choose one location syntax", 400)
			return
		}
		input["location"] = segments
	}
	if tags := q["tag"]; len(tags) > 0 {
		input["tags"] = tags
	}
	filter, err := chains.ParsePlacementFilter(input)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s := e.view.Snapshot()
	entries := []deviceEntry{}
	class := q.Get("device_class")
	for _, service := range s.Services {
		for _, d := range service.Devices {
			if q.Get("unassigned") == "true" && len(d.Location) > 0 || class != "" && !slices.Contains(d.Classes, class) || !filter.Match(d.Placement) {
				continue
			}
			values := map[string]state.Attribute{}
			for name, v := range s.Devices[state.Address(service.UUID, d.ID)] {
				if hasAttribute(d, name) {
					values[name] = v
				}
			}
			entries = append(entries, deviceEntry{service.UUID, service.Name, service.ServiceType, d, values})
		}
	}
	slices.SortFunc(entries, compareDeviceEntries)
	writeJSON(w, 200, struct {
		Revision uint64        `json:"revision"`
		Devices  []deviceEntry `json:"devices"`
	}{s.Revision, entries})
}

func compareDeviceEntries(a, b deviceEntry) int {
	if n := slices.Compare(a.Device.Location, b.Device.Location); n != 0 {
		return n
	}
	if n := strings.Compare(a.Device.Name, b.Device.Name); n != 0 {
		return n
	}
	if n := strings.Compare(a.ServiceID, b.ServiceID); n != 0 {
		return n
	}
	return strings.Compare(a.Device.ID, b.Device.ID)
}
