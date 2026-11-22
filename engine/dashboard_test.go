package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	chains "chains-automation"
	"chains-automation/state"
)

func TestDashboardResolution(t *testing.T) {
	devices := []deviceEntry{
		{ServiceID: "s", Device: chains.Device{ID: "one", Placement: chains.Placement{Location: []string{"House", "Shelf.1"}, Tags: []string{"guest"}}, Classes: []string{"toggle"}}},
		{ServiceID: "s", Device: chains.Device{ID: "two", Placement: chains.Placement{Location: []string{"House", "Shelf.1"}, Tags: []string{"guest"}}, Classes: []string{"toggle"}}},
	}
	board := dashboard{Blocks: []dashboardBlock{
		{Kind: "group", Location: []string{"**", "Shelf.1"}, Tags: []string{"guest"}, Class: "toggle"},
		{Kind: "group", Class: "toggle"},
		{Kind: "device", Service: "s", Device: "one"},
		{Kind: "device", Service: "s", Device: "private"},
	}}
	resolved := resolveDashboard(board, devices, false)
	if len(resolved.Blocks[0].Devices) != 1 || resolved.Blocks[0].Devices[0] != "s/two" ||
		len(resolved.Blocks[1].Devices) != 0 || resolved.Blocks[2].Devices[0] != "s/one" || !resolved.Blocks[3].Missing {
		t.Fatalf("unexpected layout: %+v", resolved)
	}
	if household := resolveDashboard(board, devices, true); len(household.Blocks) != 3 {
		t.Fatalf("household received hidden device: %+v", household)
	}
}

func TestDashboardRolesAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	authority, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{authority: authority, view: authority.View}
	defer func() { authority.Close() }()
	service := chains.Service{UUID: "fixture", Name: "Fixture", ServiceType: "test", Devices: []chains.Device{{
		ID: "private", Name: "Private device", Placement: chains.Placement{Tags: []string{state.HouseholdTag}},
	}}}
	register, _ := chains.New(chains.Registration, service.UUID, service)
	if _, err := authority.Register(register, service); err != nil {
		t.Fatal(err)
	}
	handler := e.WebHTTP("operator", nil, fstest.MapFS{"index.html": {Data: []byte("web")}, "ui/index.html": {Data: []byte("operator")}}, WebOptions{Operator: true, Household: true, HouseholdToken: "family"})
	const base = householdBase
	const origin = "http://127.0.0.1"
	call := func(method, path, host, body string, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, origin+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:6543"
		r.Header.Set("Origin", host)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, w.Code, want, w.Body)
		}
		return w
	}
	login := func(token string) *http.Cookie {
		return call("POST", base+"/session", origin, `{"token":"`+token+`"}`, nil, 200).Result().Cookies()[0]
	}
	op, family := login("operator"), login("family")
	call("GET", base+"/dashboard", "", "", nil, 401)
	call("GET", base+"/dashboard/config", "", "", family, 403)
	call("GET", "/operator/v1/activity", "", "", family, 404)
	call("GET", "/operator/v1/state", "", "", family, 404)
	call("POST", "/operator/v1/actions", origin, "{}", family, 404)
	call("PATCH", base+"/devices/fixture/private/sharing", origin, `{"shared":true}`, family, 403)
	target := `{"service_id":"fixture","device_id":"private","attribute":"power","value":true}`
	call("POST", base+"/actions", "https://elsewhere.example", target, family, 403)
	call("POST", base+"/actions", origin, target, family, 404)
	if body := call("GET", base+"/dashboard", "", "", family, 200).Body.String(); strings.Contains(body, "Private device") {
		t.Fatalf("forged tag shared device: %s", body)
	}
	setSharing := func(value bool) {
		t.Helper()
		if _, err := authority.EditPlacement("fixture", "private", func(p state.PlacementOverride) (state.PlacementOverride, error) {
			p.HouseholdShared = value
			return p, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setSharing(true)
	if body := call("GET", base+"/dashboard", "", "", family, 200).Body.String(); !strings.Contains(body, "Private device") {
		t.Fatalf("missing shared device: %s", body)
	}
	call("POST", base+"/actions", origin, target, family, 400) // It has no setter.
	setSharing(false)
	call("POST", base+"/actions", origin, target, family, 404)

	config := `{"version":1,"revision":0,"home_id":"home","dashboards":[{"id":"home","name":"Home","pinned":true,"blocks":[]}]}`
	call("PUT", base+"/dashboard/config", origin, config, family, 403)
	call("PUT", base+"/dashboard/config", "https://elsewhere.example", config, op, 403)
	call("PUT", base+"/dashboard/config", origin, config, op, 200)
	call("PUT", base+"/dashboard/config", origin, config, op, 409)
	authority.Close()
	authority, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := authority.DashboardCatalog()
	var saved dashboardCatalog
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Home != "home" || saved.Revision != 1 {
		t.Fatalf("catalog did not survive restart: %s, %v", raw, err)
	}
}
