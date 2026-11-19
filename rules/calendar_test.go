package rules

import (
	"testing"
	"time"
)

func TestCalendarSkipsAndDoesNotRepeatWallTime(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Oslo")
	if err != nil {
		t.Fatal(err)
	}
	c := Calendar{Zone: zone}
	tests := []struct{ after, want string }{
		{"2026-03-28T23:00:00Z", "2026-03-30T00:30:00Z"},
		{"2026-10-24T23:00:00Z", "2026-10-25T00:30:00Z"},
		{"2026-10-25T00:30:00Z", "2026-10-26T01:30:00Z"},
	}
	for _, tc := range tests {
		after, _ := time.Parse(time.RFC3339, tc.after)
		next, err := c.next("Time", []any{"hour", 2, "minute", 30}, after)
		if err != nil || next.UTC().Format(time.RFC3339) != tc.want {
			t.Fatalf("after %s: got %v, %v, want %s", tc.after, next, err, tc.want)
		}
	}
}

func TestCalendarAnnualAndWeek(t *testing.T) {
	c := Calendar{Zone: time.UTC, Week: "iso"}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		args []any
		want string
	}{
		{[]any{"month", 12, "day", 25}, "2026-12-25T00:00:00Z"},
		{[]any{"week", 2, "weekday", 1}, "2026-01-05T00:00:00Z"},
		{[]any{"minute", 1}, "2026-01-01T00:01:00Z"},
	} {
		next, err := c.next("Time", tc.args, start)
		if err != nil || next.Format(time.RFC3339) != tc.want {
			t.Fatalf("calendar %v: %v, %v", tc.args, next, err)
		}
	}
}
