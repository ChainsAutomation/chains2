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

const Version = 1

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
	Version       int             `json:"version"`
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Source        string          `json:"source"`
	ServiceID     string          `json:"service_id,omitempty"`
	ServiceType   string          `json:"service_type,omitempty"`
	DeviceID      string          `json:"device_id,omitempty"`
	DeviceClass   string          `json:"device_class,omitempty"`
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
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Class      string   `json:"class"`
	Attributes []string `json:"attributes,omitempty"`
	Actions    []string `json:"actions,omitempty"`
}

type Service struct {
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
	var fields map[string]any
	data, _ := json.Marshal(m)
	_ = json.Unmarshal(data, &fields)
	return fields
}
