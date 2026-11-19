// Package operator defines the HTTP interface shared by chainsctl and the engine.
package operator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Base = "/operator/v1"
const TraceType = "operator.trace"

type Record struct {
	ID          string          `json:"id,omitempty"`
	At          time.Time       `json:"time"`
	Type        string          `json:"type"`
	Source      string          `json:"source,omitempty"`
	Service     string          `json:"service_id,omitempty"`
	Device      string          `json:"device_id,omitempty"`
	Target      string          `json:"target,omitempty"`
	Engine      string          `json:"engine_id,omitempty"`
	Rule        string          `json:"rule,omitempty"`
	Run         string          `json:"run_id,omitempty"`
	Event       string          `json:"event_id,omitempty"`
	Action      string          `json:"action_id,omitempty"`
	Cause       string          `json:"causation_id,omitempty"`
	Correlation string          `json:"correlation_id,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
	Truncated   bool            `json:"truncated,omitempty"`
}

type State struct {
	Epoch    string     `json:"epoch"`
	Revision uint64     `json:"revision"`
	Rows     []StateRow `json:"rows"`
}

type StateRow struct {
	Kind        string          `json:"kind"`
	Service     string          `json:"service_id"`
	ServiceName string          `json:"service_name"`
	Device      string          `json:"device_id,omitempty"`
	DeviceName  string          `json:"device_name,omitempty"`
	Attribute   string          `json:"attribute,omitempty"`
	Value       json.RawMessage `json:"value,omitempty"`
	Observed    *time.Time      `json:"observed_at,omitempty"`
	Changed     *time.Time      `json:"changed_at,omitempty"`
	Revision    uint64          `json:"revision,omitempty"`
}

type Profile struct {
	Name    string          `json:"name"`
	Source  string          `json:"source"`
	Type    string          `json:"type"`
	Device  string          `json:"device_id,omitempty"`
	Version string          `json:"schema_version"`
	Schema  json.RawMessage `json:"schema,omitempty"`
	Ready   bool            `json:"ready"`
}

type Submission struct {
	Profile string          `json:"profile,omitempty"`
	Name    string          `json:"name,omitempty"`
	Data    json.RawMessage `json:"data"`
}

type Receipt struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Source string `json:"source"`
}

const HistorySize = 2048

type Feed struct {
	mu      sync.Mutex
	epoch   string
	records [HistorySize]Record
	count   uint64
	changed chan struct{}
}

func NewFeed() *Feed {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return &Feed{epoch: hex.EncodeToString(id[:]), changed: make(chan struct{})}
}

// Bounded limits text and payload previews before placing activity on a queue.
func (record Record) Bounded() Record {
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	for _, field := range []*string{&record.Type, &record.Source, &record.Service, &record.Device, &record.Target, &record.Engine, &record.Rule, &record.Run, &record.Event, &record.Action, &record.Cause, &record.Correlation} {
		if len(*field) > 256 {
			*field = strings.Clone((*field)[:256])
			record.Truncated = true
		}
	}
	if len(record.Data) > 16<<10 {
		record.Data, _ = json.Marshal(string(record.Data[:2048]))
		record.Truncated = true
	}
	if len(record.Summary) > 2048 {
		record.Summary = strings.Clone(record.Summary[:2048])
		record.Truncated = true
	}
	return record
}

func (f *Feed) Append(record Record) {
	record = record.Bounded()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	record.ID = f.cursor(f.count)
	f.records[(f.count-1)%HistorySize] = record
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *Feed) cursor(n uint64) string { return fmt.Sprintf("%s:%d", f.epoch, n) }

func (f *Feed) Read(after string, recent int) (records []Record, cursor string, gap bool, changed <-chan struct{}, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.count
	oldest := last - min(last, HistorySize)
	position := last
	if after == "" {
		position = last - min(last-oldest, uint64(max(0, min(recent, HistorySize))))
	} else {
		epoch, seq, ok := strings.Cut(after, ":")
		parsed, parseErr := strconv.ParseUint(seq, 10, 64)
		if !ok || parseErr != nil {
			return nil, "", false, nil, errors.New("invalid activity cursor")
		}
		if epoch != f.epoch || parsed < oldest || parsed > last {
			gap = true
			position = oldest
		} else {
			position = parsed
		}
	}
	for n := position; n < last; n++ {
		records = append(records, f.records[n%HistorySize])
	}
	return records, f.cursor(position), gap, f.changed, nil
}

type Filter struct {
	Type, Service, Device, Engine, Rule, Correlation, Search string
	All                                                      bool
}

func (f Filter) Match(r Record) bool {
	if r.Type == "watch.gap" || r.Type == "watch.error" {
		return true
	}
	if !f.All && f.Type == "" {
		switch r.Type {
		case "service.heartbeat", "state.committed", "state.reported", "state.head", "state.query.requested", "state.query.responded", "state.snapshot.requested", "state.snapshot.responded":
			return false
		}
	}
	if f.Type != "" {
		found := false
		for _, kind := range strings.Split(f.Type, ",") {
			if kind == r.Type || strings.HasSuffix(kind, "*") && strings.HasPrefix(r.Type, strings.TrimSuffix(kind, "*")) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.Service != "" && f.Service != r.Service && f.Service != r.Source {
		return false
	}
	if f.Device != "" && f.Device != r.Device || f.Engine != "" && f.Engine != r.Engine || f.Rule != "" && f.Rule != r.Rule || f.Correlation != "" && f.Correlation != r.Correlation {
		return false
	}
	return f.Search == "" || strings.Contains(strings.ToLower(strings.Join([]string{r.Type, r.Source, r.Service, r.Device, r.Engine, r.Rule, r.Summary, string(r.Data)}, " ")), strings.ToLower(f.Search))
}
