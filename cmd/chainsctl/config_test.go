package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chains-automation/operator"
)

func TestOperatorConnection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	for _, key := range []string{"CHAINS_OPERATOR_CONFIG", "CHAINS_OPERATOR_URL", "CHAINS_OPERATOR_TOKEN"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "chains"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "chains", "chainsctl.toml")
	if err := os.WriteFile(path, []byte(`url = "https://authority.test"
token = "file-token"`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	url, token, err := operatorConnection(ctx, "", "")
	if err != nil || url != "https://authority.test" || token != "file-token" {
		t.Fatalf("TOML lookup returned url=%q, valid token=%t, err=%v", url, token == "file-token", err)
	}
	if _, _, err := operatorConnection(ctx, "", "https://another.test"); err == nil {
		t.Fatal("endpoint override accepted a token from the default file")
	}
	t.Setenv("CHAINS_OPERATOR_URL", "https://env.test")
	t.Setenv("CHAINS_OPERATOR_TOKEN", "env-token")
	t.Setenv("CHAINS_OPERATOR_CONFIG", path)
	url, token, err = operatorConnection(ctx, "missing.toml", "https://flag.test")
	if err != nil || url != "https://env.test" || token != "env-token" {
		t.Fatalf("environment override returned url=%q, valid token=%t, err=%v", url, token == "env-token", err)
	}
}

func TestDescribeWithTokenCommand(t *testing.T) {
	t.Setenv("CHAINS_OPERATOR_CONFIG", "")
	for _, key := range []string{"CHAINS_OPERATOR_URL", "CHAINS_OPERATOR_TOKEN"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != operator.Base+"/services" || r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unexpected request", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "chainsctl.toml")
	if err := os.WriteFile(path, []byte(`url = "`+srv.URL+`"
token_command = ["printf", "%s\n", "test-token"]`), 0600); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	err := run(context.Background(), []string{"describe", "--config", path, "--output", "json"}, strings.NewReader(""), &got, &got)
	if err != nil || got.String() != "[]\n" {
		t.Fatalf("describe: %v, output %q", err, got.String())
	}
}
