package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"chains-automation/engine"
	"chains-automation/z2m"
)

type config struct {
	Z2M                      z2m.Config                   `json:"z2m"`
	ID                       string                       `json:"id"`
	Authority                string                       `json:"authority"`
	Bus                      string                       `json:"bus"`
	Namespace                string                       `json:"namespace"`
	Database                 string                       `json:"database"`
	WebRulesDir              string                       `json:"web_rules_dir"`
	Rules                    string                       `json:"rules"`
	Encoding                 string                       `json:"encoding"`
	Listen                   string                       `json:"listen"`
	Token                    string                       `json:"token"`
	TokenCommand             []string                     `json:"token_command"`
	HouseholdTokenCommand    []string                     `json:"household_token_command"`
	HouseholdToken           string                       `json:"household_token"`
	Timezone                 string                       `json:"timezone"`
	Week                     string                       `json:"week"`
	Scheduler                bool                         `json:"scheduler"`
	OperatorWebLoopbackProxy bool                         `json:"operator_web_loopback_proxy"`
	HouseholdWeb             bool                         `json:"household_web"`
	OperatorWeb              bool                         `json:"operator_web"`
	OperatorAPI              bool                         `json:"operator_api"`
	OperatorTrace            bool                         `json:"operator_trace"`
	Debug                    bool                         `json:"debug"`
	Types                    []string                     `json:"types"`
	HTTP                     map[string]engine.HTTPSource `json:"http"`
	Sensor                   sensorMapping                `json:"sensor"`
	SensorBroker             string                       `json:"sensor_broker"`
	SensorTopic              string                       `json:"sensor_topic"`
}

func xdg(env, fallback string) string {
	if value := os.Getenv(env); filepath.IsAbs(value) {
		return value
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

func readConfig(args []string) (config, error) {
	c := config{Z2M: z2m.DefaultConfig(), ID: "engine", Authority: "engine", Bus: "mqtt://localhost:1883", Namespace: "chains", Encoding: "json", Week: "system", Scheduler: true,
		Database:    filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "chains", "state.sqlite"),
		Rules:       filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "chains", "rules.chains"),
		SensorTopic: "sensors/bathroom/humidity",
		Sensor:      sensorMapping{Device: "bathroom-sensor", Event: "humidity.reported", Attribute: "humidity", Field: "value", Format: "number", Schema: json.RawMessage(numericSchema)}}
	f := flag.NewFlagSet("chains", flag.ContinueOnError)
	path := f.String("config", "", "configuration file")
	fields := map[string]*string{"z2m-mqtt-url": &c.Z2M.MQTTURL, "z2m-base-topic": &c.Z2M.BaseTopic, "z2m-service-id": &c.Z2M.ServiceID, "z2m-service-name": &c.Z2M.ServiceName, "z2m-ca-file": &c.Z2M.CAFile, "id": &c.ID, "authority": &c.Authority, "bus": &c.Bus, "namespace": &c.Namespace, "database": &c.Database, "web-rules-dir": &c.WebRulesDir, "rules": &c.Rules, "encoding": &c.Encoding, "listen": &c.Listen, "token": &c.Token, "household-token": &c.HouseholdToken, "timezone": &c.Timezone, "week": &c.Week, "sensor-broker": &c.SensorBroker, "sensor-topic": &c.SensorTopic}
	var cli = map[string]*string{}
	for name, ptr := range fields {
		cli[name] = f.String(name, *ptr, name)
	}
	z2mEnabled := f.Bool("z2m-enabled", false, "enable the Zigbee2MQTT bridge")
	scheduler := f.Bool("scheduler", c.Scheduler, "enable calendar rules")
	operatorWebLoopbackProxy := f.Bool("operator-web-loopback-proxy", false, "allow loopback browser origins through container forwarding")
	householdWeb := f.Bool("household-web", false, "serve the bundled household app")
	operatorWeb := f.Bool("operator-web", false, "serve the bundled web console")
	operatorAPI := f.Bool("operator-api", false, "enable the authority operator API")
	operatorTrace := f.Bool("operator-trace", false, "emit rule lifecycle telemetry")
	debug := f.Bool("debug", false, "log event and rule diagnostics")
	types := f.String("types", "", "comma-separated subscribed event types")
	if err := f.Parse(args); err != nil {
		return c, err
	}
	if f.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	if value := os.Getenv("CHAINS_CONFIG"); value != "" {
		*path = value
	}
	if *path != "" {
		if err := loadConfig(*path, &c); err != nil {
			return c, err
		}
	} else {
		dirs := os.Getenv("XDG_CONFIG_DIRS")
		if dirs == "" {
			dirs = "/etc/xdg"
		}
		system := strings.Split(dirs, ":")
		for i := len(system) - 1; i >= 0; i-- {
			if filepath.IsAbs(system[i]) {
				if err := loadConfig(filepath.Join(system[i], "chains", "config.json"), &c); err != nil && !errors.Is(err, os.ErrNotExist) {
					return c, err
				}
			}
		}
		if err := loadConfig(filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "chains", "config.json"), &c); err != nil && !errors.Is(err, os.ErrNotExist) {
			return c, err
		}
	}
	// A literal token from a flag or environment replaces the whole file source.
	clearCommand := func(name string) {
		switch name {
		case "token":
			c.TokenCommand = nil
		case "household-token":
			c.HouseholdTokenCommand = nil
		}
	}
	f.Visit(func(flag *flag.Flag) {
		if p := fields[flag.Name]; p != nil {
			*p = *cli[flag.Name]
			clearCommand(flag.Name)
		}
		switch flag.Name {
		case "z2m-enabled":
			c.Z2M.Enabled = *z2mEnabled
		case "scheduler":
			c.Scheduler = *scheduler
		case "operator-web-loopback-proxy":
			c.OperatorWebLoopbackProxy = *operatorWebLoopbackProxy
		case "household-web":
			c.HouseholdWeb = *householdWeb
		case "operator-web":
			c.OperatorWeb = *operatorWeb
		case "operator-api":
			c.OperatorAPI = *operatorAPI
		case "operator-trace":
			c.OperatorTrace = *operatorTrace
		case "debug":
			c.Debug = *debug
		case "types":
			c.Types = splitTypes(*types)
		}
	})
	for name, ptr := range fields {
		env := "CHAINS_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		if value, ok := os.LookupEnv(env); ok {
			*ptr = value
			clearCommand(name)
		}
	}
	for name, ptr := range map[string]*bool{"CHAINS_Z2M_ENABLED": &c.Z2M.Enabled, "CHAINS_SCHEDULER": &c.Scheduler, "CHAINS_DEBUG": &c.Debug, "CHAINS_OPERATOR_WEB_LOOPBACK_PROXY": &c.OperatorWebLoopbackProxy, "CHAINS_OPERATOR_WEB": &c.OperatorWeb, "CHAINS_HOUSEHOLD_WEB": &c.HouseholdWeb, "CHAINS_OPERATOR_API": &c.OperatorAPI, "CHAINS_OPERATOR_TRACE": &c.OperatorTrace} {
		if value, ok := os.LookupEnv(name); ok {
			if err := json.Unmarshal([]byte(value), ptr); err != nil {
				return c, fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	if value, ok := os.LookupEnv("CHAINS_TYPES"); ok {
		c.Types = splitTypes(value)
	}
	return c, nil
}

func splitTypes(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func loadConfig(path string, c *config) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	// Decode credentials separately so a higher-priority file can replace a
	// literal with a command (or vice versa) without inheriting the other field.
	type settings config
	file := struct {
		*settings
		Token                 *string  `json:"token"`
		TokenCommand          []string `json:"token_command"`
		HouseholdToken        *string  `json:"household_token"`
		HouseholdTokenCommand []string `json:"household_token_command"`
	}{settings: (*settings)(c)}
	if err = d.Decode(&file); err != nil {
		var fieldErr *json.UnmarshalTypeError
		if errors.As(err, &fieldErr) {
			switch fieldErr.Field {
			case "token", "token_command", "household_token", "household_token_command":
				return fmt.Errorf("%s: invalid credential setting %s", path, fieldErr.Field)
			}
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%s: expected one config object", path)
	}
	if file.Token != nil && file.TokenCommand != nil {
		return fmt.Errorf("%s: configure either token or token_command", path)
	}
	if file.HouseholdToken != nil && file.HouseholdTokenCommand != nil {
		return fmt.Errorf("%s: configure either household_token or household_token_command", path)
	}
	if file.Token != nil {
		c.Token, c.TokenCommand = *file.Token, nil
	} else if file.TokenCommand != nil {
		c.Token, c.TokenCommand = "", file.TokenCommand
	}
	if file.HouseholdToken != nil {
		c.HouseholdToken, c.HouseholdTokenCommand = *file.HouseholdToken, nil
	} else if file.HouseholdTokenCommand != nil {
		c.HouseholdToken, c.HouseholdTokenCommand = "", file.HouseholdTokenCommand
	}
	return nil
}
