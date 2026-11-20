package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chains "chains-automation"
	"chains-automation/sdk"
)

func TestHTTPMappedInput(t *testing.T) {
	broker := &memoryBroker{}
	f := newFixture(t, broker.open)
	engine, _ := f.start(t, authorityID, ":memory:", "")
	observer := f.open(t, readingKind)
	sensor := sdk.Client{Bus: f.open(t), Source: sourceID}
	if err := sensor.Register(f.ctx, sensorService()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "sensor registration", func() bool { _, ok := engine.view.Service(sourceID); return ok })
	httpHandler := engine.HTTP("test-token", map[string]HTTPSource{
		"humidity": {Source: sourceID, Device: "bathroom", Type: readingKind, SchemaVersion: "1", Attribute: "value", StateAttribute: "humidity"},
	})
	do := func(token, value string, want int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/events/humidity", strings.NewReader(value))
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		httpHandler.ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("HTTP %d: %s, expected %d", res.Code, res.Body, want)
		}
	}
	do("wrong", "72.5", http.StatusUnauthorized)
	do("test-token", "ON", http.StatusBadRequest)
	do("test-token", "72.5", http.StatusAccepted)
	found := readMessage(t, observer, func(m chains.Message) bool { return m.Type == readingKind })
	if found.ServiceID != sourceID || len(found.Observations) != 1 || string(found.Observations[0].Data) != `{"value":72.5}` {
		t.Fatalf("invalid mapped event: %+v", found)
	}
}
