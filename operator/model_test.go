package operator

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestFeedReplayAndGap(t *testing.T) {
	feed := NewFeed()
	_, initial, _, available, err := feed.Read("", 0)
	if err != nil {
		t.Fatal(err)
	}
	feed.Append(Record{Type: "one"})
	select {
	case <-available:
	default:
		t.Fatal("append did not wake readers")
	}
	availableRecords, _, gap, _, err := feed.Read(initial, 0)
	if err != nil || gap || len(availableRecords) != 1 || availableRecords[0].Type != "one" {
		t.Fatalf("%+v %v %v", availableRecords, gap, err)
	}
	stale := availableRecords[0].ID
	for i := 0; i < HistorySize+1; i++ {
		feed.Append(Record{Type: "extra", Summary: fmt.Sprint(i)})
	}
	records, _, gap, _, err := feed.Read(stale, 0)
	if err != nil || !gap || len(records) != HistorySize || records[0].Summary != "1" {
		t.Fatalf("replayed %d records, gap %v: %v", len(records), gap, err)
	}
	records, _, gap, _, err = feed.Read("", 2)
	if err != nil || gap || len(records) != 2 {
		t.Fatalf("recent replay %d, gap %v: %v", len(records), gap, err)
	}
	feed.Append(Record{Type: "large", Data: json.RawMessage(`"` + strings.Repeat("x", 20<<10) + `"`)})
	records, _, _, _, _ = feed.Read("", 1)
	if !records[0].Truncated || !json.Valid(records[0].Data) || len(records[0].Data) > 4096 {
		t.Fatalf("preview size %d", len(records[0].Data))
	}
	_, _, gap, _, err = NewFeed().Read(records[0].ID, 0)
	if err != nil || !gap {
		t.Fatalf("expected authority restart gap: %v", err)
	}
}

func TestActivityFilters(t *testing.T) {
	record := Record{Type: "rule.finished", Engine: "worker", Rule: "test", Service: "svc", Device: "sensor", Correlation: "abc", Summary: "Done"}
	filter := Filter{Type: "action.*,rule.*", Service: "svc", Device: "sensor", Engine: "worker", Rule: "test", Correlation: "abc", Search: "done"}
	if !filter.Match(record) {
		t.Fatal("matching record hidden")
	}
	record.Rule = "different"
	if filter.Match(record) {
		t.Fatal("nonmatching rule displayed")
	}
	heartbeat := Record{Type: "service.heartbeat"}
	if (Filter{}).Match(heartbeat) || !(Filter{All: true}).Match(heartbeat) || !(Filter{Type: "service.*"}).Match(heartbeat) {
		t.Fatal("incorrect noise filtering")
	}
	if !filter.Match(Record{Type: "watch.gap"}) {
		t.Fatal("gap must always be visible")
	}
}
