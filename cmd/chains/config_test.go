package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigurationPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	if err := os.WriteFile(path, []byte(`{"id":"file-id","namespace":"file-ns","debug":true,"scheduler":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHAINS_CONFIG", path)
	t.Setenv("CHAINS_ID", "env-id")
	t.Setenv("XDG_STATE_HOME", dir)
	config, err := readConfig([]string{"-config", filepath.Join(dir, "unused"), "-id", "cli-id", "-namespace", "cli-ns", "-debug=false"})
	if err != nil {
		t.Fatal(err)
	}
	if config.ID != "env-id" || config.Namespace != "cli-ns" || config.Debug || config.Scheduler || config.Database != filepath.Join(dir, "chains", "state.sqlite") {
		t.Fatalf("unexpected configuration: %+v", config)
	}
}

func TestZigbee2MQTTEnvironmentPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"z2m":{"enabled":false,"mqtt_url":"mqtt://config","service_name":"My bridge"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHAINS_CONFIG", path)
	t.Setenv("CHAINS_Z2M_ENABLED", "true")
	t.Setenv("CHAINS_Z2M_MQTT_URL", "mqtt://environment")
	c, err := readConfig([]string{"-z2m-enabled=false", "-z2m-mqtt-url=mqtt://cli", "-z2m-base-topic=home/z2m"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Z2M.Enabled || c.Z2M.MQTTURL != "mqtt://environment" || c.Z2M.BaseTopic != "home/z2m" || c.Z2M.ServiceName != "My bridge" {
		t.Fatalf("unexpected config: %+v", c.Z2M)
	}
}

func TestCredentialSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"token_command":["operator-helper"],"household_token_command":["household-helper"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := config{Token: "system-token"}
	if err := loadConfig(path, &c); err != nil {
		t.Fatal(err)
	}
	if c.Token != "" || len(c.TokenCommand) != 1 || len(c.HouseholdTokenCommand) != 1 {
		t.Fatal("config commands did not replace the existing source")
	}
	t.Setenv("CHAINS_CONFIG", path)
	t.Setenv("CHAINS_HOUSEHOLD_TOKEN", "env-token")
	c, err := readConfig([]string{"-token", "flag-token", "-household-token", "another-flag-token"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "flag-token" || c.TokenCommand != nil || c.HouseholdToken != "env-token" || c.HouseholdTokenCommand != nil {
		t.Fatal("literal flag and environment options did not replace configured commands")
	}
}
