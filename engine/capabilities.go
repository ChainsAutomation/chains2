package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/rules"
)

func hasAttribute(d chains.Device, name string) bool {
	_, ok := d.Attributes[name]
	return ok
}

func (e *Engine) describeDevice(s chains.Service, d *chains.Device) error {
	for name, attribute := range d.Attributes {
		if !chains.Segment(name) || name == "_attributes" {
			return fmt.Errorf("invalid attribute %q", name)
		}
		if _, err := e.schema(attribute.Schema); err != nil {
			return fmt.Errorf("attribute %q: %w", name, err)
		}
		if setter := attribute.Setter; setter != nil {
			if _, err := setterProperty(s, *d, setter); err != nil {
				return fmt.Errorf("attribute %q: %w", name, err)
			}
		}
	}
	classes := []string{}
	var expand func(string) error
	expand = func(class string) error {
		if slices.Contains(classes, class) {
			return nil
		}
		switch class {
		case "dimmer":
			if err := e.requireAttribute(s, *d, "dimlevel", chains.DimlevelSchema, 101); err != nil {
				return err
			}
			if err := expand("light"); err != nil {
				return err
			}
		case "light":
			if err := expand("toggle"); err != nil {
				return err
			}
		case "toggle":
			if err := e.requireAttribute(s, *d, "power", chains.PowerSchema, 2); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown device class %q", class)
		}
		classes = append(classes, class)
		return nil
	}
	for _, class := range d.Classes {
		if err := expand(class); err != nil {
			return fmt.Errorf("class %q: %w", class, err)
		}
	}
	d.Classes = classes
	return nil
}

func setterProperty(s chains.Service, d chains.Device, setter *chains.Setter) (json.RawMessage, error) {
	if !chains.Segment(setter.Argument) || !contains(d.Actions, setter.Action) {
		return nil, fmt.Errorf("invalid setter mapping to %q", setter.Action)
	}
	var schema struct {
		Properties map[string]json.RawMessage
		Required   []string
	}
	raw := s.Actions[setter.Action][setter.Version]
	if json.Unmarshal(raw, &schema) != nil || len(schema.Properties[setter.Argument]) == 0 {
		return nil, fmt.Errorf("setter %q needs a declared value argument", setter.Action)
	}
	for _, required := range schema.Required {
		if required != setter.Argument {
			return nil, fmt.Errorf("setter %q requires extra argument %q", setter.Action, required)
		}
	}
	return schema.Properties[setter.Argument], nil
}

func schemaShape(raw json.RawMessage) map[string]any {
	var shape map[string]any
	_ = json.Unmarshal(raw, &shape)
	for _, key := range []string{"title", "description", "default", "examples", "$comment", "$schema"} {
		delete(shape, key)
	}
	return shape
}

func (e *Engine) requireAttribute(s chains.Service, d chains.Device, name, required string, n int) error {
	attr := d.Attributes[name]
	want := schemaShape(json.RawMessage(required))
	if attr.Setter == nil || !reflect.DeepEqual(schemaShape(attr.Schema), want) {
		return fmt.Errorf("requires writable %s with schema %s", name, required)
	}
	property, err := setterProperty(s, d, attr.Setter)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(schemaShape(property), want) {
		return fmt.Errorf("setter schema conflicts with %s contract", name)
	}
	action, err := e.schema(s.Actions[attr.Setter.Action][attr.Setter.Version])
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		var value any = float64(i)
		if name == "power" {
			value = i != 0
		}
		if err := action.Validate(map[string]any{attr.Setter.Argument: value}); err != nil {
			return fmt.Errorf("setter does not accept all %s values: %w", name, err)
		}
	}
	return nil
}

func (e *Engine) validateAttributes(d chains.Device, values map[string]json.RawMessage) error {
	for name, raw := range values {
		attr, ok := d.Attributes[name]
		if !ok {
			return fmt.Errorf("unknown attribute %q", name)
		}
		if err := e.validate(attr.Schema, raw); err != nil {
			return fmt.Errorf("attribute %q: %w", name, err)
		}
	}
	return nil
}

func (e *Engine) ResolveSetter(ref rules.DeviceRef, name string, value any) (chains.Setter, map[string]any, error) {
	s, known := e.view.Service(ref.ServiceID)
	d, ok := device(s, ref.DeviceID)
	if !known || !ok {
		return chains.Setter{}, nil, errors.New("unknown device")
	}
	attr, ok := d.Attributes[name]
	if !ok || attr.Setter == nil {
		return chains.Setter{}, nil, fmt.Errorf("attribute %q is not writable", name)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return chains.Setter{}, nil, err
	}
	if err = e.validate(attr.Schema, raw); err != nil {
		return chains.Setter{}, nil, fmt.Errorf("attribute %q: %w", name, err)
	}
	return *attr.Setter, map[string]any{attr.Setter.Argument: value}, nil
}

func (e *Engine) operatorSet(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Service   string          `json:"service_id"`
		Device    string          `json:"device_id"`
		Attribute string          `json:"attribute"`
		Value     json.RawMessage `json:"value"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, bus.MaxPayload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid or oversized setter request", http.StatusBadRequest)
		return
	}
	var value any
	if json.Unmarshal(request.Value, &value) != nil {
		http.Error(w, "provide a value", 400)
		return
	}
	ref := rules.DeviceRef{ServiceID: request.Service, DeviceID: request.Device}
	setter, args, err := e.ResolveSetter(ref, request.Attribute, value)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	raw, _ := json.Marshal(args)
	service, _ := e.view.Service(ref.ServiceID)
	if err = e.validate(service.Actions[setter.Action][setter.Version], raw); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	parent, _ := chains.New("operator.setter", e.node.Source, nil)
	receipt, err := e.SendAction(r.Context(), parent, ref, setter.Action, setter.Version, args)
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	receipt["correlation_id"] = parent.ID
	writeJSON(w, 202, receipt)
}
