package engine

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	chains "chains-automation"
	"chains-automation/bus"
)

type HTTPSource struct {
	Source         string `json:"source"`
	Device         string `json:"device"`
	Type           string `json:"type"`
	SchemaVersion  string `json:"schema_version"`
	Attribute      string `json:"attribute"`
	StateAttribute string `json:"state_attribute,omitempty"`
}

func (e *Engine) HTTP(token string, mappings map[string]HTTPSource) http.Handler {
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /events/{mapping}", auth(func(w http.ResponseWriter, r *http.Request) {
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
		if !json.Valid(raw) {
			raw, _ = json.Marshal(string(raw))
		}
		m, err := e.mappedEvent(mapping, raw)
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
	}))
	e.operatorRoutes(mux, auth, mappings)
	return mux
}

func (e *Engine) mappedEvent(mapping HTTPSource, value json.RawMessage) (chains.Message, error) {
	if mapping.Attribute == "" {
		return chains.Message{}, fmt.Errorf("HTTP mapping requires an attribute")
	}
	data := map[string]json.RawMessage{mapping.Attribute: value}
	return e.makeObservation(mapping.Source, mapping.Type, mapping.SchemaVersion, mapping.Device, mapping.stateAttribute(), value, data)
}

func (m HTTPSource) stateAttribute() string {
	if m.StateAttribute != "" {
		return m.StateAttribute
	}
	return m.Attribute
}
