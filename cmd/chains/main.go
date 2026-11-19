package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/engine"
	"chains-automation/rules"
	"chains-automation/sdk"
	"chains-automation/state"
	webui "chains-automation/web"
	"chains-automation/z2m"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, flag.ErrHelp) {
		slog.Error("chains stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	mode := "engine"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		mode, args = args[0], args[1:]
	}
	c, err := readConfig(args)
	if err != nil {
		return err
	}
	if !chains.Segment(c.Namespace) || !chains.Segment(c.ID) || !chains.Segment(c.Authority) {
		return errors.New("invalid namespace or engine identity")
	}
	if c.Encoding != "json" && c.Encoding != "cbor" {
		return errors.New("encoding must be json or cbor")
	}
	if c.Debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	for _, kind := range c.Types {
		if !chains.Segment(kind) {
			return fmt.Errorf("invalid event type %q", kind)
		}
	}
	switch mode {
	case "engine":
		return runEngine(ctx, c)
	case "sensor":
		return runSensor(ctx, c)
	case "fan":
		return runFan(ctx, c)
	default:
		return fmt.Errorf("unknown command %q (use engine, sensor or fan)", mode)
	}
}

func openBus(ctx context.Context, address, namespace string, sub bus.Subscription) (bus.Bus, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if strings.HasPrefix(address, "redis://") || strings.HasPrefix(address, "rediss://") {
		return bus.OpenRedis(ctx, address, namespace, sub)
	}
	return bus.OpenMQTT(ctx, address, namespace, sub)
}

func runEngine(ctx context.Context, c config) error {
	assets := webui.Assets()
	if c.OperatorWeb && (!c.OperatorAPI || assets == nil) {
		return errors.New("operator web requires -operator-api and a build with -tags webui")
	}
	if c.OperatorAPI && (c.ID != c.Authority || c.Listen == "" || c.Token == "") {
		return errors.New("operator API requires the authority, HTTP listen address, and token")
	}
	if err := c.Z2M.Validate(); err != nil {
		return err
	}
	var bridge engine.LocalService
	if c.Z2M.Enabled {
		if c.ID != c.Authority || c.Z2M.ServiceID == c.ID {
			return errors.New("Zigbee2MQTT requires the authority and a distinct service ID")
		}
		bridge = z2m.New(c.Z2M, nil)
	}
	compiled, err := loadRules(c.Rules)
	if err != nil {
		return err
	}
	cal := rules.Calendar{Week: c.Week, Disabled: !c.Scheduler}
	switch c.Week {
	case "iso", "sunday":
	case "system":
		cal.Week = "iso"
		probe, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		locale, err := exec.CommandContext(probe, "locale", "week-1stday", "week-1stweek", "first_weekday").Output()
		if err == nil {
			cal.LocaleWeek = parseWeekConvention(string(locale))
		}
	default:
		return errors.New("week must be iso, sunday or system")
	}
	if c.Timezone != "" {
		cal.Zone, err = time.LoadLocation(c.Timezone)
		if err != nil {
			return err
		}
	}
	if c.ID == c.Authority && c.Database != ":memory:" {
		if err = os.MkdirAll(filepath.Dir(c.Database), 0700); err != nil {
			return err
		}
	}
	types := c.Types
	if len(types) > 0 {
		types = mergeTypes(types, []string{chains.Changed, chains.AvailabilityChanged, chains.Outcome, chains.SnapshotResponse, state.Updated, state.HeadType})
		if c.ID == c.Authority {
			types = mergeTypes(types, []string{chains.Registration, chains.Report, chains.SnapshotRequest, chains.StateQuery, chains.Heartbeat})
			if bridge != nil {
				types = mergeTypes(types, []string{chains.Request})
			}
		}
	}
	b, err := openBus(ctx, c.Bus, c.Namespace, bus.Subscription{Types: types})
	if err != nil {
		return err
	}
	defer b.Close()
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()
	e, err := engine.Open(lifetime, b, engine.Options{
		ID: c.ID, AuthorityID: c.Authority, Database: c.Database, Rules: compiled, Calendar: cal,
		LocalService: bridge, Encoding: chains.Encoding(c.Encoding),
		OperatorAPI: c.OperatorAPI, OperatorTrace: c.OperatorTrace,
		OpenOperatorBus: func(ctx context.Context, sub bus.Subscription) (bus.Bus, error) {
			return openBus(ctx, c.Bus, c.Namespace, sub)
		},
	})
	if err != nil {
		return err
	}
	defer e.Close()
	if c.Listen == "" {
		return e.Run(lifetime)
	}
	if c.Token == "" {
		return errors.New("HTTP ingress requires an authentication token")
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	handler := e.HTTP(c.Token, c.HTTP)
	if c.OperatorWeb {
		handler = e.WebHTTP(c.Token, c.HTTP, assets, c.OperatorWebLoopbackProxy)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	defer server.Close()
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener); cancel() }()
	runErr := e.Run(lifetime)
	_ = server.Close()
	if err := <-result; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return runErr
}

func mergeTypes(a, b []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, item := range append(append([]string{}, a...), b...) {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

const numericSchema = `{"type":"object","required":["value"],"properties":{"value":{"type":"number","minimum":0,"maximum":100}},"additionalProperties":false}`
const powerSchema = `{"type":"object","required":["value"],"properties":{"value":{"type":"boolean"}},"additionalProperties":false}`

func runSensor(ctx context.Context, c config) error {
	if c.SensorTopic == "" || strings.ContainsAny(c.SensorTopic, "+#") {
		return errors.New("sensor topic must be a concrete MQTT topic")
	}
	mapping := c.Sensor
	if err := mapping.validate(); err != nil {
		return err
	}
	service := chains.Service{UUID: c.ID, Name: "MQTT sensor", ServiceType: "mqtt_sensor", Devices: []chains.Device{{ID: mapping.Device, Name: mapping.Device, Class: mapping.Class, Attributes: []string{mapping.Attribute}}}, Events: map[string]map[string]json.RawMessage{mapping.Event: {"1": mapping.Schema}}}
	inputAddress := c.SensorBroker
	if inputAddress == "" {
		inputAddress = c.Bus
	}
	input, err := openBus(ctx, inputAddress, c.Namespace, bus.Subscription{Types: []string{"unused.sensor"}, Raw: []string{c.SensorTopic}})
	if err != nil {
		return err
	}
	defer input.Close()
	out, err := openBus(ctx, c.Bus, c.Namespace, bus.Subscription{Types: []string{"unused.sensor"}})
	if err != nil {
		return err
	}
	defer out.Close()
	client := sdk.Client{Bus: out, Encoding: chains.Encoding(c.Encoding), Source: c.ID, ServiceType: service.ServiceType}
	if err = client.Register(ctx, service); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-input.Receive():
			if !ok {
				return errors.New("sensor subscription closed")
			}
			if d.Reset {
				if err = client.Register(ctx, service); err != nil {
					return err
				}
				continue
			}
			if d.Topic != c.SensorTopic {
				continue
			}
			value, err := mapping.parse(d.Data)
			if err != nil {
				slog.Warn("invalid sensor reading", "topic", d.Topic, "error", err)
				continue
			}
			if err = client.Report(ctx, mapping.Device, map[string]any{mapping.Attribute: value}, nil); err != nil {
				return err
			}
			if d.Retained {
				continue
			}
			if err = client.Event(ctx, mapping.Event, mapping.Device, "1", map[string]any{mapping.Field: value}); err != nil {
				return err
			}
		case d, ok := <-out.Receive():
			if !ok {
				return errors.New("system bus closed")
			}
			if d.Reset {
				if err = client.Register(ctx, service); err != nil {
					return err
				}
			}
		}
	}
}

func runFan(ctx context.Context, c config) error {
	service := chains.Service{UUID: c.ID, Name: "Simulated fan", ServiceType: "sim_fan", Devices: []chains.Device{{ID: "fan", Name: "Bathroom fan", Class: "fan", Attributes: []string{"power"}, Actions: []string{"set_power"}}}, Actions: map[string]map[string]json.RawMessage{"set_power": {"1": json.RawMessage(powerSchema)}}}
	b, err := openBus(ctx, c.Bus, c.Namespace, bus.Subscription{Types: []string{chains.Request}})
	if err != nil {
		return err
	}
	defer b.Close()
	client := sdk.Client{Bus: b, Encoding: chains.Encoding(c.Encoding), Source: c.ID, ServiceType: service.ServiceType}
	if err = client.Register(ctx, service); err != nil {
		return err
	}
	if err = client.Report(ctx, "fan", map[string]any{"power": false}, nil); err != nil {
		return err
	}
	return client.Serve(ctx, service, func(ctx context.Context, request chains.Message, action chains.Action) error {
		value, ok := action.Args["value"].(bool)
		if !ok {
			return errors.New("power must be boolean")
		}
		slog.Info("fan power changed", "power", value, "action", request.ActionID)
		return client.Report(ctx, "fan", map[string]any{"power": value}, &request)
	})
}

func parseWeekConvention(raw string) *rules.WeekConvention {
	var base string
	var minDays, start int
	if _, err := fmt.Sscan(raw, &base, &minDays, &start); err != nil || minDays < 1 || minDays > 7 || start < 1 || start > 7 {
		return nil
	}
	date, err := time.Parse("20060102", base)
	if err != nil {
		return nil
	}
	first := time.Weekday((int(date.Weekday()) + start - 1) % 7)
	return &rules.WeekConvention{FirstDay: first, MinDays: minDays}
}

func loadRules(path string) ([]rules.Rule, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	paths := []string{path}
	if info.IsDir() {
		paths = nil
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == ".chains" {
				paths = append(paths, filepath.Join(path, entry.Name()))
			}
		}
	}
	compiled := make([]rules.Rule, 0, len(paths))
	for _, file := range paths {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		rule, err := rules.Parse(name, string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		compiled = append(compiled, rule)
	}
	return compiled, nil
}
