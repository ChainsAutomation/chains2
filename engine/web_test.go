package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/operator"
)

func TestWebConsole(t *testing.T) {
	fixture := newFixture(t, (&memoryBroker{}).open)
	e, err := Open(fixture.ctx, fixture.open(t), Options{
		ID: authorityID, AuthorityID: authorityID, Database: ":memory:", OperatorAPI: true,
		OpenOperatorBus: func(ctx context.Context, sub bus.Subscription) (bus.Bus, error) {
			return fixture.connect(ctx, fixture.namespace, sub)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	assets := fstest.MapFS{"index.html": {Data: []byte("Chains UI")}, "assets/app.js": {Data: []byte("export {}")}}
	handler := e.WebHTTP("secret", nil, assets, false)
	call := func(h http.Handler, method, path, origin, body, bearer string, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:3456"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		if recorder.Code != want {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, recorder.Code, want, recorder.Body)
		}
		return recorder
	}
	path := operator.Base
	call(e.HTTP("secret", nil), "GET", "/ui/", "", "", "", nil, 404)
	call(handler, "GET", "/ui/", "", "", "", nil, 200)
	call(handler, "GET", "/ui/trace?correlation=abc", "", "", "", nil, 200)
	call(handler, "GET", "/ui/assets/app.js", "", "", "", nil, 200)
	call(handler, "GET", path+"/unknown", "", "", "", nil, 404)
	call(handler, "GET", path+"/services", "", "", "", nil, 401)
	call(handler, "POST", path+"/session", "https://unrelated.example", `{"token":"secret"}`, "", nil, 403)
	call(handler, "POST", path+"/session", "http://127.0.0.1", `{"token":"wrong"}`, "", nil, 401)
	loggedIn := call(handler, "POST", path+"/session", "http://127.0.0.1", `{"token":"secret"}`, "", nil, 204)
	cookie := loggedIn.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != path || cookie.MaxAge != 28800 || cookie.Value == "secret" {
		t.Fatalf("cookie %+v", cookie)
	}
	call(handler, "GET", path+"/session", "", "", "", cookie, 204)
	call(handler, "POST", path+"/events", "https://unrelated.example", `{"name":"test","data":{}}`, "", cookie, 403)
	call(handler, "POST", path+"/events", "", `{"name":"test","data":{}}`, "secret", nil, 202)
	call(handler, "POST", path+"/events", "http://127.0.0.1", `{"name":"test","data":{}}`, "", cookie, 202)
	reg, _ := chains.New(chains.Registration, sourceID, sensorService())
	if err = e.handle(fixture.ctx, reg); err != nil {
		t.Fatal(err)
	}
	response := call(handler, "GET", path+"/services", "", "", "", cookie, 200)
	if !strings.Contains(response.Body.String(), `"attributes":["humidity","last_button"]`) || !strings.Contains(response.Body.String(), readingKind) {
		t.Fatal(response.Body.String())
	}
	e.operator.feed.Append(operator.Record{Type: "misc"})
	e.operator.feed.Append(operator.Record{Type: "action.outcome", Correlation: "abc"})
	response = call(handler, "GET", path+"/traces?correlation=abc", "", "", "", cookie, 200)
	var trace operator.TraceHistory
	if err = json.Unmarshal(response.Body.Bytes(), &trace); err != nil || len(trace.Records) != 1 || trace.OldestAt == nil || trace.OldestID == trace.Records[0].ID {
		t.Fatalf("trace %+v %v", trace, err)
	}
	call(handler, "DELETE", path+"/session", "http://127.0.0.1", "", "", cookie, 204)
	call(handler, "GET", path+"/session", "", "", "", cookie, 401)
	call(handler, "GET", path+"/services", "", "", "secret", nil, 200)
}

func TestBrowserOrigin(t *testing.T) {
	cases := []struct {
		host, origin, remote string
		secure, allowed      bool
	}{
		{"localhost:8080", "http://localhost:8080", "127.0.0.1:9000", false, true},
		{"192.0.2.5:8080", "http://192.0.2.5:8080", "192.0.2.42:9000", false, false},
		{"chains.example", "https://chains.example", "127.0.0.1:9000", true, true},
		{"chains.example", "https://other.example", "127.0.0.1:9000", false, false},
		{"chains.example", "https://chains.example/", "127.0.0.1:9000", false, false},
	}
	for _, test := range cases {
		req := httptest.NewRequest("POST", "http://"+test.host+"/operator/v1/session", nil)
		req.Header.Set("Origin", test.origin)
		req.RemoteAddr = test.remote
		if secure, allowed := webOrigin(req, false); secure != test.secure || allowed != test.allowed {
			t.Errorf("%s from %s: secure=%v, allowed=%v", test.origin, test.remote, secure, allowed)
		}
	}
}

func TestExpiredBrowserSession(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	s := browserSessions{sessions: map[string]browserSession{"expired": {ctx, cancel}}}
	req := httptest.NewRequest("GET", "/operator/v1/session", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "expired"})
	if _, ok := s.lookup(req); ok {
		t.Fatal("expired session was accepted")
	}
	if _, ok := s.create(); !ok || len(s.sessions) != 1 {
		t.Fatal("expired session was not replaced")
	}
}

func TestLoopbackOriginThroughProxy(t *testing.T) {
	for _, test := range []struct {
		host, origin string
		allowed      bool
	}{
		{"localhost:8080", "http://localhost:8080", true},
		{"127.0.0.1:8080", "http://127.0.0.1:8080", true},
		{"[::1]:8080", "http://[::1]:8080", true},
		{"localhost:8080", "http://localhost:9090", false},
		{"chains.example", "http://chains.example", false},
		{"localhost:8080", "", false},
	} {
		req := httptest.NewRequest("POST", "http://"+test.host+"/operator/v1/session", nil)
		req.RemoteAddr = "192.0.2.42:9000"
		req.Header.Set("Origin", test.origin)
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		if secure, allowed := webOrigin(req, true); secure || allowed != test.allowed {
			t.Errorf("%q with host %q: secure=%v allowed=%v", test.origin, test.host, secure, allowed)
		}
		if _, allowed := webOrigin(req, false); allowed {
			t.Errorf("accepted forwarded HTTP request without opt-in: %q", test.origin)
		}
	}
}
