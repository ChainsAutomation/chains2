package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chains-automation/bus"
	"chains-automation/rules"
)

func TestOperatorRulesLifecycle(t *testing.T) {
	f := newFixture(t, (&memoryBroker{}).open)
	dir := filepath.Join(t.TempDir(), "rules")
	fixed := "# Deployment comment\nEvent(type=\"unused\")\nreturn\n"
	deployment, err := rules.Parse("fixed", fixed)
	if err != nil {
		t.Fatal(err)
	}
	open := func() *Engine {
		e, err := Open(f.ctx, f.open(t), Options{ID: authorityID, AuthorityID: authorityID, Database: ":memory:",
			WebRulesDir: dir, Rules: []rules.Rule{deployment}, RuleSources: map[string]string{"fixed": fixed},
			OperatorAPI: true, OpenOperatorBus: func(_ context.Context, sub bus.Subscription) (bus.Bus, error) {
				return f.connect(f.ctx, f.namespace, sub)
			}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(e.Close)
		return e
	}
	e := open()
	call := func(method, path string, body []byte, contentType string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/operator/v1/rules"+path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer test")
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		w := httptest.NewRecorder()
		e.HTTP("test", nil).ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w
	}
	publish := func(revision string, want int, changes ...ruleChange) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"revision": revision, "changes": changes})
		call(http.MethodPost, "/publish", body, "application/json", want)
	}
	first := e.ruleCatalog.revision
	source := "# User comment\nEvent(type=\"manual.test\")\nreturn\n"
	publish(first, 422, ruleChange{Name: "bad", Enabled: true, Source: "Event(\n"})
	if e.ruleCatalog.revision != first {
		t.Fatal("validation changed the revision")
	}
	publish(first, 200, ruleChange{Name: "test", Enabled: false, Source: source})
	publish(first, 409, ruleChange{Name: "stale", Source: source})
	entry := e.ruleCatalog.entries[1]
	publish(e.ruleCatalog.revision, 400, ruleChange{ID: "file-fixed", Delete: true})
	archive := call(http.MethodGet, "/export", nil, "", 200)
	if !strings.Contains(archive.Header().Get("Content-Disposition"), "chains-rules.zip") {
		t.Fatal("missing ZIP download filename")
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, _ := writer.CreateFormFile("file", "chains-rules.zip")
	_, _ = part.Write(archive.Body.Bytes())
	_ = writer.Close()
	preview := call(http.MethodPost, "/import", form.Bytes(), writer.FormDataContentType(), 200)
	var imported struct {
		Rules []struct {
			ruleSource
			Conflict *ruleSource
		}
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &imported); err != nil || len(imported.Rules) != 2 ||
		imported.Rules[0].Source != fixed || imported.Rules[1].Source != source || imported.Rules[1].Enabled ||
		imported.Rules[0].Conflict == nil || imported.Rules[0].Conflict.Origin != "file" {
		t.Fatalf("invalid archive round trip: %s", preview.Body.String())
	}
	e = open()
	if len(e.ruleCatalog.entries) != 2 || e.ruleCatalog.entries[1] != entry {
		t.Fatal("restart lost the stored rule")
	}
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	e.ruleCatalog.dir = blocked
	revision := e.ruleCatalog.revision
	publish(revision, 500, ruleChange{ID: entry.ID, Name: "renamed", Source: source, Enabled: true})
	if e.ruleCatalog.revision != revision || e.ruleCatalog.entries[1] != entry {
		t.Fatal("failed disk write changed the catalog")
	}
	if _, err := decodeRuleImport("too-large.chains", make([]byte, maxRuleSource+1)); err == nil {
		t.Fatal("oversized import accepted")
	}
}
