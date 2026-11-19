package sdk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type ConfigFixture struct {
	Metrics struct {
		URL      string         `toml:"url" required:"true"`
		Interval float64        `toml:"interval" help:"Polling interval"`
		Hosts    []string       `toml:"hosts"`
		Quotas   map[string]int `toml:"quotas"`
		Enabled  *bool          `toml:"enabled"`
	} `toml:"metrics"`
}

func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, "chains", "services", name+".toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigPrecedence(t *testing.T) {
	home, system := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_CONFIG_DIRS", system)
	t.Setenv("XDG_STATE_HOME", home)
	writeConfig(t, system, "keyboard", `[service]
uri = "mqtt://system"
[settings.metrics]
url = "system"
quotas = {west = 1, east = 2}
`)
	writeConfig(t, home, "keyboard", `[service]
uri = "mqtt://local"
[settings.metrics]
interval = 8
quotas = {north = 3}
`)
	t.Setenv("CHAINS_BUS", "mqtt://environment")
	t.Setenv("CHAINS_SETTINGS__METRICS__INTERVAL", "3.5")
	t.Setenv("CHAINS_SETTINGS__METRICS__ENABLED", "false")
	var settings ConfigFixture
	c, err := LoadConfig("keyboard", []string{"--bus", "mqtt://flag", "--metrics-interval", "overridden", "--metrics-hosts", `["west","east"]`}, &settings)
	if err != nil {
		t.Fatal(err)
	}
	m := settings.Metrics
	if c.URI != "mqtt://environment" || c.Name != "keyboard" || m.URL != "system" || m.Interval != 3.5 ||
		len(m.Hosts) != 2 || len(m.Quotas) != 1 || m.Quotas["north"] != 3 || m.Enabled == nil || *m.Enabled {
		t.Fatalf("config %+v, settings %+v", c, m)
	}
	paths := []string{
		writeConfig(t, home, "west", "[settings.metrics]\nurl = \"west\"\n"),
		writeConfig(t, home, "east", "[settings.metrics]\nurl = \"east\"\n"),
	}
	var ids []string
	for _, path := range paths {
		config, err := LoadConfig("keyboard", []string{"-f", path}, &settings)
		if err != nil {
			t.Fatal(err)
		}
		if config.Name != settings.Metrics.URL {
			t.Fatalf("instance %+v", config)
		}
		id, err := persistedID("keyboard", config.Instance)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	renamed, err := LoadConfig("keyboard", []string{"-f", paths[0], "--service-name", "West keyboard"}, &settings)
	if err != nil {
		t.Fatal(err)
	}
	id, err := persistedID("keyboard", renamed.Instance)
	if err != nil || ids[0] == ids[1] || id != ids[0] {
		t.Fatalf("unstable identities: %v, %s, %v", ids, id, err)
	}
}

func TestInvalidSettingDoesNotExposeValue(t *testing.T) {
	dir := t.TempDir()
	file := writeConfig(t, dir, "keyboard", "[settings.metrics]\nurl = \"valid\"\ninterval = \"secret\"\n")
	var settings ConfigFixture
	_, err := LoadConfig("keyboard", []string{"-f", file}, &settings)
	if err == nil || !strings.Contains(err.Error(), "settings.metrics.interval") || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}
