package chains

import (
	"encoding/json"
	"time"
)

const (
	ObservationSubmitted = "observation.submitted"
	DeviceObserved       = "device.observed"
	OperatorService      = "chains.operator"
)

type Observation struct {
	DeviceID    string                     `json:"device_id"`
	Values      map[string]json.RawMessage `json:"values"`
	Data        json.RawMessage            `json:"data,omitempty"`
	DeviceName  string                     `json:"device_name,omitempty"`
	DeviceClass string                     `json:"device_class,omitempty"`
}

type ObservationBatch struct {
	EventType    string        `json:"event_type"`
	Observations []Observation `json:"observations"`
}

func NewObservation(device string, values map[string]any, data any) (Observation, error) {
	o := Observation{DeviceID: device, Values: make(map[string]json.RawMessage, len(values))}
	for key, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return o, err
		}
		o.Values[key] = raw
	}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return o, err
		}
		o.Data = raw
	}
	return o, nil
}

func SubmitObservation(source, kind, version string, observations []Observation) (Message, error) {
	m, err := New(ObservationSubmitted, source, ObservationBatch{kind, observations})
	if version == "" {
		version = "1"
	}
	m.SchemaVersion = version
	expiry := m.ObservedAt.Add(time.Minute)
	m.ExpiresAt = &expiry
	return m, err
}
