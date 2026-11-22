// Package state stores authoritative reports and serves revisioned read views.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	chains "chains-automation"
)

type Attribute struct {
	Value      json.RawMessage `json:"value"`
	ChangedAt  time.Time       `json:"changed_at"`
	ObservedAt time.Time       `json:"observed_at"`
	Revision   uint64          `json:"revision"`
}

type Snapshot struct {
	Epoch    string                          `json:"epoch"`
	Revision uint64                          `json:"revision"`
	Devices  map[string]map[string]Attribute `json:"devices"`
	Services map[string]chains.Service       `json:"services"`
}

type Change struct {
	DeviceID  string          `json:"-"`
	Epoch     string          `json:"epoch"`
	Attribute string          `json:"attribute"`
	Previous  json.RawMessage `json:"previous,omitempty"`
	Value     json.RawMessage `json:"value"`
	ChangedAt time.Time       `json:"changed_at"`
	Revision  uint64          `json:"revision"`
	Initial   bool            `json:"initial"`
}

type Update struct {
	registration *chains.Service
	placement    *placementWrite
	Epoch        string                          `json:"epoch"`
	Revision     uint64                          `json:"revision"`
	Devices      map[string]map[string]Attribute `json:"devices,omitempty"`
	Service      *chains.Service                 `json:"service,omitempty"`
}

const Updated = "state.committed"
const HeadType = "state.head"

type Head struct {
	Epoch    string `json:"epoch"`
	Revision uint64 `json:"revision"`
}

type View struct {
	mu   sync.RWMutex
	data Snapshot
}

func Address(source, device string) string { return source + "/" + device }

func NewView() *View {
	return &View{data: Snapshot{Devices: map[string]map[string]Attribute{}, Services: map[string]chains.Service{}}}
}

func (v *View) Replace(s Snapshot) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.data = s
}

func (v *View) Lookup(address string) map[string]any {
	v.mu.RLock()
	defer v.mu.RUnlock()
	values := map[string]any{}
	metadata := map[string]any{}
	for k, a := range v.visibleAttributes(address) {
		var value any
		_ = json.Unmarshal(a.Value, &value)
		values[k] = value
		metadata[k] = map[string]any{"changed_at": a.ChangedAt.Format(time.RFC3339Nano), "observed_at": a.ObservedAt.Format(time.RFC3339Nano), "revision": a.Revision}
	}
	values["_attributes"] = metadata
	return values
}

func (v *View) Service(id string) (chains.Service, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s, ok := v.data.Services[id]
	return s, ok
}

// Services returns the registered descriptions, whose contents are immutable.
func (v *View) Services() []chains.Service {
	v.mu.RLock()
	defer v.mu.RUnlock()
	result := make([]chains.Service, 0, len(v.data.Services))
	for _, service := range v.data.Services {
		result = append(result, service)
	}
	return result
}

func (v *View) Snapshot() Snapshot {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s := v.data
	s.Devices = map[string]map[string]Attribute{}
	for key, device := range v.data.Devices {
		copy := map[string]Attribute{}
		for k, a := range device {
			copy[k] = a
		}
		s.Devices[key] = copy
	}
	s.Services = map[string]chains.Service{}
	for k, service := range v.data.Services {
		s.Services[k] = service
	}
	return s
}

func (v *View) Apply(u Update) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if u.Epoch != v.data.Epoch {
		return false
	}
	if u.Revision <= v.data.Revision {
		return true
	}
	if u.Revision != v.data.Revision+1 {
		return false
	}
	v.apply(u)
	return true
}

func (v *View) apply(u Update) {
	if u.Service != nil {
		v.data.Services[u.Service.UUID] = *u.Service
	}
	for address, attributes := range u.Devices {
		device := v.data.Devices[address]
		if device == nil {
			device = map[string]Attribute{}
			v.data.Devices[address] = device
		}
		for k, a := range attributes {
			device[k] = a
		}
	}
	v.data.Revision = u.Revision
}

type Authority struct {
	defaults   map[string]chains.Service
	placements map[string]PlacementOverride
	*View
	db *database
	mu sync.Mutex
}

func Open(path string) (*Authority, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	v := NewView()
	placements := map[string]PlacementOverride{}
	if err = db.load(&v.data, placements); err != nil {
		db.close()
		return nil, err
	}
	v.data.Epoch = chains.ID()
	a := &Authority{View: v, db: db, defaults: maps.Clone(v.data.Services), placements: placements}
	for id, service := range a.defaults {
		v.data.Services[id] = a.effective(service)
	}
	return a, nil
}

func (a *Authority) Close() { a.db.close() }

func (a *Authority) Register(m chains.Message, s chains.Service) (Update, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.UUID != m.Source || s.Name == "" || !chains.Segment(s.ServiceType) {
		return Update{}, errors.New("invalid registration")
	}
	a.View.mu.Lock()
	defer a.View.mu.Unlock()
	effective := a.effective(s)
	u := Update{Epoch: a.data.Epoch, Revision: a.data.Revision + 1, Service: &effective, registration: &s}
	applied, err := a.db.commit(m.Source+"/"+m.ID, u)
	if err != nil || !applied {
		return Update{}, err
	}
	a.defaults[s.UUID] = s
	a.apply(u)
	return u, nil
}

// Observe previews the complete update before storing any leaves.
func (a *Authority) Observe(m chains.Message, observations []chains.Observation, preview func(Update, []Change) error) (Update, []Change, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.View.mu.Lock()
	defer a.View.mu.Unlock()
	u := Update{Epoch: a.data.Epoch, Revision: a.data.Revision + 1, Devices: map[string]map[string]Attribute{}}
	var changes []Change
	for _, observation := range observations {
		address := Address(m.Source, observation.DeviceID)
		attributes := map[string]Attribute{}
		names := make([]string, 0, len(observation.Values))
		for name := range observation.Values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			value := observation.Values[name]
			if !chains.Segment(name) || name == "_attributes" || !json.Valid(value) {
				return Update{}, nil, fmt.Errorf("invalid attribute %q", name)
			}
			previous, exists := a.data.Devices[address][name]
			if exists && m.ObservedAt.Before(previous.ObservedAt) {
				if m.Silent {
					continue
				}
				return Update{}, nil, nil
			}
			current := Attribute{Value: bytes.Clone(value), ChangedAt: previous.ChangedAt, ObservedAt: m.ObservedAt, Revision: u.Revision}
			if !exists || !equalJSON(previous.Value, value) {
				current.ChangedAt = m.ObservedAt
				changes = append(changes, Change{DeviceID: observation.DeviceID, Epoch: u.Epoch, Attribute: name, Previous: previous.Value, Value: current.Value, ChangedAt: current.ChangedAt, Revision: u.Revision, Initial: !exists})
			}
			attributes[name] = current
		}
		if len(attributes) != 0 {
			u.Devices[address] = attributes
		}
	}
	if len(u.Devices) == 0 {
		return Update{}, nil, nil
	}
	if preview != nil {
		if err := preview(u, changes); err != nil {
			return Update{}, nil, err
		}
	}
	applied, err := a.db.commit(m.Source+"/"+m.ID, u)
	if err != nil || !applied {
		return Update{}, nil, err
	}
	a.apply(u)
	return u, changes, nil
}

func equalJSON(left, right []byte) bool {
	if bytes.Equal(left, right) {
		return true
	}
	var a, b any
	dl, dr := json.NewDecoder(bytes.NewReader(left)), json.NewDecoder(bytes.NewReader(right))
	dl.UseNumber()
	dr.UseNumber()
	if dl.Decode(&a) != nil || dr.Decode(&b) != nil {
		return false
	}
	l, _ := json.Marshal(a)
	r, _ := json.Marshal(b)
	return bytes.Equal(l, r)
}

func (v *View) Position() (string, uint64) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.data.Epoch, v.data.Revision
}

// ResolveDevice finds a unique registered device by its service and device names.
func (v *View) ResolveDevice(serviceName, deviceName string) (serviceID, deviceID string, err error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	for id, service := range v.data.Services {
		if service.Name != serviceName {
			continue
		}
		for _, device := range service.Devices {
			if device.Name != deviceName {
				continue
			}
			if serviceID != "" {
				return "", "", fmt.Errorf("ambiguous device %q in service %q", deviceName, serviceName)
			}
			serviceID, deviceID = id, device.ID
		}
	}
	if serviceID == "" {
		return "", "", fmt.Errorf("device %q in service %q was not found", deviceName, serviceName)
	}
	return serviceID, deviceID, nil
}

// Query returns an attribute record, device subtree, or service subtree.
func (v *View) Query(service, device, attribute string) (any, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s, known := v.data.Services[service]
	if !known {
		return nil, false
	}
	if device != "" {
		for _, d := range s.Devices {
			if d.ID != device {
				continue
			}
			values := v.visibleAttributes(Address(service, device))
			if attribute != "" {
				a, ok := values[attribute]
				return a, ok
			}
			return values, true
		}
		return nil, false
	}
	devices := make(map[string]map[string]Attribute, len(s.Devices))
	for _, d := range s.Devices {
		devices[d.ID] = v.visibleAttributes(Address(service, d.ID))
	}
	return devices, true
}

func (v *View) visibleAttributes(address string) map[string]Attribute {
	visible := map[string]Attribute{}
	service, device, _ := strings.Cut(address, "/")
	for _, d := range v.data.Services[service].Devices {
		if d.ID != device {
			continue
		}
		for key := range d.Attributes {
			if value, ok := v.data.Devices[address][key]; ok {
				visible[key] = value
			}
		}
		break
	}
	return visible
}
