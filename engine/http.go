package engine

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
)

type HTTPSource struct {
	Source        string `json:"source"`
	Device        string `json:"device"`
	Type          string `json:"type"`
	SchemaVersion string `json:"schema_version"`
	Attribute     string `json:"attribute"`
}

func (e *Engine) HTTP(token string, mappings map[string]HTTPSource) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /events/{mapping}", func(w http.ResponseWriter, r *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !e.ready.Load() {
			http.Error(w, "state view is synchronizing", http.StatusServiceUnavailable)
			return
		}
		mapping, ok := mappings[r.PathValue("mapping")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bus.MaxPayload))
		if err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		var value any
		if err = json.Unmarshal(raw, &value); err != nil {
			value = string(raw)
		}
		data := value
		if mapping.Attribute != "" {
			data = map[string]any{mapping.Attribute: value}
		}
		m, err := chains.New(mapping.Type, mapping.Source, data)
		if err == nil {
			m.DeviceID, m.SchemaVersion = mapping.Device, mapping.SchemaVersion
			err = m.Validate(time.Now())
		}
		if err == nil {
			m, err = e.validateEvent(m)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		client := e.node
		client.Source = mapping.Source
		if err = client.Send(r.Context(), m); err != nil {
			http.Error(w, "bus unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}
