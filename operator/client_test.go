package operator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchResumesFromIdleCursor(t *testing.T) {
	var attempts atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch attempts.Add(1) {
		case 1:
			fmt.Fprint(w, "id: epoch:0\n\n")
		case 2:
			if cursor := r.Header.Get("Last-Event-ID"); cursor != "epoch:0" {
				t.Errorf("resumed from %q", cursor)
			}
			fmt.Fprint(w, "id: epoch:1\ndata: {\"id\":\"epoch:1\",\"type\":\"manual.test\"}\n\n")
		default:
			t.Errorf("unexpected extra connection")
		}
	}))
	defer httpServer.Close()
	client, err := NewClient(httpServer.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	finished := errors.New("received event")
	var reconnected bool
	err = client.Watch(ctx, 0, func(record Record) error {
		switch record.Type {
		case "watch.error":
			reconnected = true
		case "manual.test":
			if record.ID != "epoch:1" {
				t.Fatalf("message %+v", record)
			}
			return finished
		default:
			t.Fatalf("unexpected message %+v", record)
		}
		return nil
	})
	if !reconnected || !errors.Is(err, finished) {
		t.Fatalf("reconnected %v; stream result %v", reconnected, err)
	}
}
