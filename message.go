package chains

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const Version = 3

const (
	Registration        = "service.registered"
	Report              = "state.reported"
	Changed             = "state.changed"
	Request             = "action.requested"
	Outcome             = "action.outcome"
	StateQuery          = "state.query.requested"
	StateReply          = "state.query.responded"
	Heartbeat           = "service.heartbeat"
	AvailabilityChanged = "service.availability.changed"
	SnapshotRequest     = "state.snapshot.requested"
	SnapshotResponse    = "state.snapshot.responded"
)

type Message struct {
	Placement
	Epoch         string          `json:"epoch,omitempty"`
	Revision      uint64          `json:"revision,omitempty"`
	ServiceName   string          `json:"service_name,omitempty"`
	DeviceName    string          `json:"device_name,omitempty"`
	Observations  []Observation   `json:"observations,omitempty"`
	Version       int             `json:"version"`
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Source        string          `json:"source"`
	ServiceID     string          `json:"service_id,omitempty"`
	ServiceType   string          `json:"service_type,omitempty"`
	DeviceID      string          `json:"device_id,omitempty"`
	DeviceClasses []string        `json:"device_classes,omitempty"`
	Target        string          `json:"target,omitempty"`
	SchemaVersion string          `json:"schema_version,omitempty"`
	ObservedAt    time.Time       `json:"observed_at"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	CausationID   string          `json:"causation_id,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	ActionID      string          `json:"action_id,omitempty"`
	Silent        bool            `json:"silent,omitempty"`
	Data          json.RawMessage `json:"data"`
}

type Device struct {
	Placement
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	Classes     []string             `json:"classes"`
	Description string               `json:"description,omitempty"`
	Attributes  map[string]Attribute `json:"attributes,omitempty"`
	Actions     []string             `json:"actions,omitempty"`
}

type Service struct {
	Description       string                                `json:"description,omitempty"`
	RetainDevices     bool                                  `json:"retain_devices,omitempty"`
	UUID              string                                `json:"uuid"`
	Name              string                                `json:"name"`
	ServiceType       string                                `json:"service_type"`
	Devices           []Device                              `json:"devices"`
	Events            map[string]map[string]json.RawMessage `json:"events"`
	Actions           map[string]map[string]json.RawMessage `json:"actions"`
	IdempotentActions []string                              `json:"idempotent_actions,omitempty"`
}

type Action struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type Result struct {
	Status string          `json:"status"`
	Value  json.RawMessage `json:"value,omitempty"`
	Error  string          `json:"error,omitempty"`
}

const (
	Sent      = "sent"
	Accepted  = "accepted"
	Submitted = "submitted"
	Completed = "completed"
	Failed    = "failed"
	TimedOut  = "timed_out"
)

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func New(kind, source string, data any) (Message, error) {
	raw, err := json.Marshal(data)
	return Message{Version: Version, ID: ID(), Type: kind, Source: source, ObservedAt: time.Now().UTC(), Data: raw}, err
}

func (m *Message) CausedBy(parent Message) {
	m.CausationID = parent.ID
	m.CorrelationID = parent.CorrelationID
	if m.CorrelationID == "" {
		m.CorrelationID = parent.ID
	}
}

var segment = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)

func Segment(s string) bool { return segment.MatchString(s) }

func (m Message) Validate(now time.Time) error {
	if m.Silent && m.Type != Report {
		return errors.New("silent is only valid on state reports")
	}
	if m.Version != Version {
		return fmt.Errorf("unsupported protocol version %d", m.Version)
	}
	if !Segment(m.Type) || !Segment(m.Source) || !Segment(m.ID) ||
		(m.Target != "" && !Segment(m.Target)) || (m.DeviceID != "" && !Segment(m.DeviceID)) {
		return errors.New("invalid message identity or route")
	}
	if m.ObservedAt.IsZero() || !json.Valid(m.Data) {
		return errors.New("missing observation time or invalid data")
	}
	if m.ExpiresAt != nil && !now.Before(*m.ExpiresAt) {
		return ErrExpired
	}
	return nil
}

var ErrExpired = errors.New("message expired")

func (m Message) Fields() map[string]any {
	fields := map[string]any{
		"version":     float64(m.Version),
		"id":          m.ID,
		"type":        m.Type,
		"source":      m.Source,
		"observed_at": m.ObservedAt.Format(time.RFC3339Nano),
	}
	if len(m.Data) > 0 {
		var data any
		_ = json.Unmarshal(m.Data, &data)
		fields["data"] = data
	} else {
		fields["data"] = nil
	}
	if m.Epoch != "" {
		fields["epoch"] = m.Epoch
	}
	if m.Revision != 0 {
		fields["revision"] = float64(m.Revision)
	}
	if m.ServiceName != "" {
		fields["service_name"] = m.ServiceName
	}
	if m.DeviceName != "" {
		fields["device_name"] = m.DeviceName
	}
	if m.ServiceID != "" {
		fields["service_id"] = m.ServiceID
	}
	if m.ServiceType != "" {
		fields["service_type"] = m.ServiceType
	}
	if m.DeviceID != "" {
		fields["device_id"] = m.DeviceID
	}
	if len(m.DeviceClasses) > 0 {
		fields["device_classes"] = stringsToAny(m.DeviceClasses)
	}
	if len(m.Location) > 0 {
		fields["location"] = stringsToAny(m.Location)
	}
	if len(m.Tags) > 0 {
		fields["tags"] = stringsToAny(m.Tags)
	}
	if m.Target != "" {
		fields["target"] = m.Target
	}
	if m.SchemaVersion != "" {
		fields["schema_version"] = m.SchemaVersion
	}
	if m.ExpiresAt != nil {
		fields["expires_at"] = m.ExpiresAt.Format(time.RFC3339Nano)
	}
	if m.CausationID != "" {
		fields["causation_id"] = m.CausationID
	}
	if m.CorrelationID != "" {
		fields["correlation_id"] = m.CorrelationID
	}
	if m.ActionID != "" {
		fields["action_id"] = m.ActionID
	}
	if m.Silent {
		fields["silent"] = true
	}
	if len(m.Observations) > 0 {
		raw, err := json.Marshal(m.Observations)
		if err == nil {
			var obs any
			if json.Unmarshal(raw, &obs) == nil {
				fields["observations"] = obs
			}
		}
	}
	return fields
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
