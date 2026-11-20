package z2m

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	chains "chains-automation"
)

type expose struct {
	Type     string
	Property string
	Access   int
	On       any      `json:"value_on"`
	Off      any      `json:"value_off"`
	Min      *float64 `json:"value_min"`
	Max      *float64 `json:"value_max"`
	Features []expose
}

type inventoryDevice struct {
	ID         string `json:"ieee_address"`
	Name       string `json:"friendly_name"`
	Supported  bool
	Disabled   bool
	Definition *struct{ Exposes []expose }
}

type device struct {
	chains.Device
	fields map[string]expose
}

var readings = map[string]string{
	"temperature": "temperature", "humidity": "humidity", "battery": "battery",
	"contact": "contact", "occupancy": "occupancy", "state": "power", "brightness": "brightness",
}

func mapDevice(d inventoryDevice) (device, bool) {
	id := strings.ToLower(d.ID)
	address, err := hex.DecodeString(strings.TrimPrefix(id, "0x"))
	if err != nil || len(address) != 8 || !strings.HasPrefix(id, "0x") || !d.Supported || d.Disabled || d.Definition == nil ||
		d.Name == "" || strings.ContainsAny(d.Name, "+#\x00") || strings.Trim(d.Name, "/") != d.Name || d.Name == "bridge" || strings.HasPrefix(d.Name, "bridge/") {
		return device{}, false
	}
	result := device{Device: chains.Device{ID: id, Name: d.Name, Class: "sensor"}, fields: map[string]expose{}}
	var walk func([]expose)
	walk = func(items []expose) {
		for _, item := range items {
			if item.Type == "light" {
				result.Class = "light"
			}
			if item.Property == "action" && result.Class == "sensor" {
				result.Class = "button"
			}
			if _, ok := readings[item.Property]; ok {
				result.fields[item.Property] = item
			}
			if item.Property == "action" && item.Access&1 != 0 {
				result.fields[item.Property] = item
			}
			walk(item.Features)
		}
	}
	walk(d.Definition.Exposes)
	for native, field := range result.fields {
		if native == "action" {
			result.Attributes = append(result.Attributes, "last_action")
		}
		if name, ok := readings[native]; ok && field.Access&1 != 0 {
			result.Attributes = append(result.Attributes, name)
		}
		if field.Access&2 == 0 {
			continue
		}
		switch native {
		case "state":
			result.Actions = append(result.Actions, "set_power")
		case "brightness":
			result.Actions = append(result.Actions, "set_brightness")
		}
	}
	sort.Strings(result.Attributes)
	sort.Strings(result.Actions)
	return result, len(result.fields) > 0
}

func (b *Bridge) discover(ctx context.Context, raw []byte, submit submitFunc) (map[string]device, error) {
	var candidates []inventoryDevice
	if err := json.Unmarshal(raw, &candidates); err != nil {
		return nil, err
	}
	if candidates == nil {
		return nil, errors.New("expected device inventory array")
	}
	s := chains.Service{UUID: b.ID(), Name: b.config.ServiceName, ServiceType: "zigbee2mqtt",
		Events: map[string]map[string]json.RawMessage{
			ActionEvent: {"1": json.RawMessage(`{"type":"object","required":["action"],"properties":{"action":{"type":"string","minLength":1}}}`)},
		},
		Actions: map[string]map[string]json.RawMessage{
			"set_power":      {"1": json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"boolean"}},"additionalProperties":false}`)},
			"set_brightness": {"1": json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"integer"}},"additionalProperties":false}`)},
		}}
	byName, byID := map[string]device{}, map[string]device{}
	for _, candidate := range candidates {
		d, ok := mapDevice(candidate)
		if !ok {
			continue
		}
		if byName[d.Name].ID != "" || byID[d.ID].ID != "" {
			return nil, errors.New("duplicate Zigbee2MQTT device identity")
		}
		s.Devices = append(s.Devices, d.Device)
		byName[d.Name], byID[d.ID] = d, d
	}
	for name := range byName {
		if _, conflict := byName[name+"/set"]; conflict {
			return nil, errors.New("overlapping Zigbee2MQTT report and command topics")
		}
	}
	m, err := chains.New(chains.Registration, b.ID(), s)
	if err == nil {
		err = submit(ctx, m, false)
	}
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.devices = byID
	b.mu.Unlock()
	return byName, nil
}

func (b *Bridge) translate(d device, raw []byte, retained bool) ([]chains.Message, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	state := map[string]any{}
	kind := chains.DeviceObserved
	var data any
	for name, value := range body {
		field, ok := d.fields[name]
		if !ok || field.Access&1 == 0 {
			continue
		}
		switch name {
		case "action":
			if action, ok := value.(string); !retained && ok && action != "" {
				state["last_action"] = action
				kind = ActionEvent
				data = map[string]string{"action": action}
			}
		case "state":
			if power, ok := field.power(value); ok {
				state["power"] = power
			}
		case "contact", "occupancy":
			if v, ok := value.(bool); ok {
				state[readings[name]] = v
			}
		default:
			if v, ok := value.(float64); ok {
				state[readings[name]] = v
			}
		}
	}
	if len(state) == 0 {
		return nil, nil
	}
	if retained {
		m, err := b.message(d, chains.Report, state)
		m.Silent = true
		return []chains.Message{m}, err
	}
	observation, err := chains.NewObservation(d.ID, state, data)
	if err != nil {
		return nil, err
	}
	m, err := chains.SubmitObservation(b.ID(), kind, "1", []chains.Observation{observation})
	return []chains.Message{m}, err
}

func (b *Bridge) message(d device, kind string, data any) (chains.Message, error) {
	m, err := chains.New(kind, b.ID(), data)
	m.DeviceID, m.ServiceID, m.ServiceType, m.DeviceClass = d.ID, b.ID(), "zigbee2mqtt", d.Class
	return m, err
}

func (f expose) power(value any) (bool, bool) {
	if f.On == nil {
		f.On = "ON"
	}
	if f.Off == nil {
		f.Off = "OFF"
	}
	switch value.(type) {
	case string, float64, bool:
		if value == f.On {
			return true, true
		}
		if value == f.Off {
			return false, true
		}
	}
	return false, false
}

func (d device) command(action chains.Action) ([]byte, error) {
	value := action.Args["value"]
	native := ""
	switch action.Name {
	case "set_power":
		native = "state"
		enabled, ok := value.(bool)
		if !ok {
			return nil, errors.New("power must be boolean")
		}
		field := d.fields[native]
		value = field.Off
		if enabled {
			value = field.On
		}
		if value == nil {
			value = "OFF"
			if enabled {
				value = "ON"
			}
		}
	case "set_brightness":
		native = "brightness"
		field := d.fields[native]
		min, max := 0.0, 254.0
		if field.Min != nil {
			min = *field.Min
		}
		if field.Max != nil {
			max = *field.Max
		}
		n, ok := value.(float64)
		if !ok || math.Trunc(n) != n || n < min || n > max {
			return nil, fmt.Errorf("brightness must be an integer from %g to %g", min, max)
		}
	default:
		return nil, errors.New("unsupported Zigbee2MQTT action")
	}
	if d.fields[native].Access&2 == 0 {
		return nil, errors.New("Zigbee2MQTT capability is not writable")
	}
	return json.Marshal(map[string]any{native: value})
}
