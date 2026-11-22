package tokencommand

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestReadRejectsInvalidOutput(t *testing.T) {
	for _, format := range []string{"", "one\\ntwo", "one\\n\\n", "%5000s"} {
		if _, err := Read(context.Background(), []string{"printf", format}); err == nil {
			t.Fatalf("accepted printf format %q", format)
		}
	}
	_, err := Read(context.Background(), []string{"sh", "-c", "printf secret; printf secret >&2; exit 1"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("expected a redacted error, got %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Read(ctx, []string{"sleep", "10"}); err != context.DeadlineExceeded {
		t.Fatalf("expected helper deadline, got %v", err)
	}
}
