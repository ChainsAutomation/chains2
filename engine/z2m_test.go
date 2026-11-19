package engine

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/rules"
	"chains-automation/z2m"
)

func TestZigbee2MQTTBridge(t *testing.T) {
	nativeAddress := os.Getenv("CHAINS_TEST_MQTT")
	if nativeAddress == "" {
		t.Skip("set CHAINS_TEST_MQTT for Zigbee2MQTT fixture broker")
	}
	for _, pair := range []struct{ name, env string }{{"mqtt", "CHAINS_TEST_MQTT"}, {"redis", "CHAINS_TEST_REDIS"}} {
		t.Run(pair.name, func(t *testing.T) {
			connect := realConnector(pair.env)
			if connect == nil {
				t.Skip("set " + pair.env)
			}
			f := newFixture(t, connect)
			config := z2m.DefaultConfig()
			config.Enabled, config.MQTTURL, config.BaseTopic = true, nativeAddress, f.namespace+"/zigbee"
			const address = "0x0011223344556677"
			commandTopic := config.BaseTopic + "/kitchen/light/set"
			native, err := bus.OpenRawMQTT(f.ctx, nativeAddress, "", commandTopic)
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			publish := func(topic, body string, retained bool) {
				t.Helper()
				ctx, cancel := context.WithTimeout(f.ctx, time.Second)
				defer cancel()
				if err := native.PublishAcknowledged(ctx, topic, []byte(body), retained); err != nil {
					t.Fatal(err)
				}
			}
			inventory := `[{"ieee_address":"` + address + `","friendly_name":"kitchen/light","supported":true,"definition":{"exposes":[{"type":"light","features":[{"property":"state","access":3}]},{"property":"action","access":1}]}}]`
			publish(config.BaseTopic+"/bridge/devices", inventory, true)
			publish(config.BaseTopic+"/kitchen/light", `{"state":"OFF","action":"single"}`, true)
			rule, err := rules.Parse("local", `
ev = Event(type="zigbee2mqtt.action", action=single)
Action("set_power", device=ev.device, value=true)
`)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(f.ctx)
			b := f.open(t)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			e, err := Open(ctx, b, Options{ID: authorityID, AuthorityID: authorityID, Database: ":memory:", Rules: []rules.Rule{rule}, LocalService: z2m.New(config, logger), Log: logger})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); _ = e.Run(ctx) }()
			defer func() { cancel(); <-done; e.Close() }()
			worker, _ := f.start(t, "worker", "", `
ev = Event(type="zigbee2mqtt.action", action=remote)
Action("set_power", device=ev.device, value=false)
`)
			observer := f.open(t, z2m.ActionEvent, chains.Outcome)
			eventually(t, "bridge inventory", func() bool { _, ok := e.view.Service(config.ServiceID); return ok })
			eventually(t, "remote registration and retained seed", func() bool {
				_, ok := worker.view.Service(config.ServiceID)
				return ok && worker.ready.Load() && worker.State(config.ServiceID + "/" + address)["power"] == false
			})
			select {
			case d := <-native.Receive():
				t.Fatalf("retained event triggered action: %+v", d)
			case <-time.After(100 * time.Millisecond):
			}
			publish(config.BaseTopic+"/kitchen/light", `{"state":"OFF","action":"single"}`, false)
			event := readMessage(t, observer, func(m chains.Message) bool { return m.Type == z2m.ActionEvent })
			expectCommand := func(payload string) {
				t.Helper()
				select {
				case d := <-native.Receive():
					if d.Topic != commandTopic || string(d.Data) != payload {
						t.Fatalf("unexpected command: %+v", d)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("missing native command")
				}
			}
			expectCommand(`{"state":"ON"}`)
			outcome := readMessage(t, observer, func(m chains.Message) bool { return m.Type == chains.Outcome })
			var result chains.Result
			if json.Unmarshal(outcome.Data, &result) != nil || result.Status != chains.Submitted || outcome.CorrelationID != event.CorrelationID {
				t.Fatalf("unexpected local outcome: %+v / %+v", outcome, result)
			}
			if state := e.State(config.ServiceID + "/" + address)["power"]; state != false {
				t.Fatalf("command changed state: %v", state)
			}
			select {
			case d := <-native.Receive():
				t.Fatalf("echo repeated a command: %+v", d)
			case <-time.After(100 * time.Millisecond):
			}
			resultMap, err := worker.Act(f.ctx, event, rules.DeviceRef{ServiceID: config.ServiceID, DeviceID: address}, "set_power", "1", map[string]any{"value": false})
			if err != nil || resultMap["status"] != chains.Submitted {
				t.Fatalf("remote action: %v, %v", resultMap, err)
			}
			expectCommand(`{"state":"OFF"}`)
			publish(config.BaseTopic+"/kitchen/light", `{"action":"remote"}`, false)
			expectCommand(`{"state":"OFF"}`)
			select {
			case d := <-native.Receive():
				t.Fatalf("remote rule repeated a command: %+v", d)
			case <-time.After(100 * time.Millisecond):
			}
			publish(config.BaseTopic+"/kitchen/light", `{"state":"ON"}`, false)
			eventually(t, "reported state propagation", func() bool { return worker.State(config.ServiceID + "/" + address)["power"] == true })
		})
	}
}
