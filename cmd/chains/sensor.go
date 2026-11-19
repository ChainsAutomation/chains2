package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	chains "chains-automation"
	"github.com/google/jsonschema-go/jsonschema"
)

type sensorMapping struct {
	Device    string          `json:"device"`
	Class     string          `json:"class"`
	Event     string          `json:"event"`
	Attribute string          `json:"attribute"`
	Field     string          `json:"field"`
	Format    string          `json:"format"`
	Schema    json.RawMessage `json:"schema"`
	resolved  *jsonschema.Resolved
}

func (m *sensorMapping) validate() error {
	for _, value := range []string{m.Device, m.Class, m.Event, m.Attribute, m.Field} {
		if !chains.Segment(value) {
			return fmt.Errorf("invalid sensor mapping field %q", value)
		}
	}
	switch m.Format {
	case "number", "boolean", "string", "json":
	default:
		return errors.New("sensor format must be number, boolean, string, or json")
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(m.Schema, &s); err != nil {
		return fmt.Errorf("sensor schema: %w", err)
	}
	resolved, err := s.Resolve(nil)
	m.resolved = resolved
	return err
}

func (m sensorMapping) parse(raw []byte) (any, error) {
	value, err := parseSensorValue(m.Format, raw)
	if err != nil {
		return nil, err
	}
	if err = m.resolved.Validate(map[string]any{m.Field: value}); err != nil {
		return nil, err
	}
	return value, nil
}

func parseSensorValue(format string, raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("sensor message must be UTF-8")
	}
	text := strings.TrimSpace(string(raw))
	switch format {
	case "string":
		return text, nil
	case "number":
		n, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, errors.New("expected a finite number")
		}
		return n, nil
	case "boolean":
		switch strings.ToLower(text) {
		case "on", "true", "1":
			return true, nil
		case "off", "false", "0":
			return false, nil
		}
		return nil, errors.New("expected on/off, true/false, or 1/0")
	case "json":
		var value any
		err := json.Unmarshal(raw, &value)
		return value, err
	}
	return nil, errors.New("unsupported sensor format")
}
