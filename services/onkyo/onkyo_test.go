package onkyo

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
	"chains-automation/sdk"
)

func TestPacketFraming(t *testing.T) {
	data := packet("PWR01")
	payload := []byte("!1PWR01\x1a\r\n!1MVL20\r\n")
	binary.BigEndian.PutUint32(data[8:12], uint32(len(payload)))
	combined := append(data[:headerSize], payload...)
	reader := io.MultiReader(bytes.NewReader(combined[:7]), bytes.NewReader(combined[7:]), bytes.NewReader(packet("AMT01")))
	first, err := readPacket(reader)
	if err != nil || len(first) != 2 || first[0].code != "PWR" || first[1].value != "20" {
		t.Fatalf("combined packet = %v, %v", first, err)
	}
	second, err := readPacket(reader)
	if err != nil || len(second) != 1 || second[0].code != "AMT" {
		t.Fatalf("next packet = %v, %v", second, err)
	}
	binary.BigEndian.PutUint32(combined[8:12], 1<<30)
	if _, err := readPacket(bytes.NewReader(combined)); err == nil {
		t.Fatal("accepted oversized frame")
	}
}

func TestZoneControls(t *testing.T) {
	for mode, want := range map[string]int{"disabled": 0, "external": 2, "speakers": 4} {
		settings := DefaultSettings()
		settings.Zone2Mode = mode
		settings.Zone2.Location = []string{"House", "Kitchen"}
		d := Definition(settings)
		got := 0
		for _, device := range d.Service.Devices {
			if device.ID == "zone2" {
				got = len(device.Attributes)
				if device.Location[1] != "Kitchen" || device.Attributes["input"].Setter.Action != "set_zone2_input" {
					t.Fatalf("zone2 registration: %+v", device)
				}
			}
		}
		if got != want {
			t.Errorf("%s mode has %d Zone 2 controls, want %d", mode, got, want)
		}
	}
}

type testBus struct {
	in  chan bus.Delivery
	out chan chains.Message
}

func (b *testBus) Receive() <-chan bus.Delivery { return b.in }
func (b *testBus) Close() error                 { return nil }
func (b *testBus) Publish(ctx context.Context, _ string, raw []byte) error {
	m, err := chains.Decode(raw)
	if err != nil {
		return err
	}
	select {
	case b.out <- m:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *testBus) expect(t *testing.T, predicate func(chains.Message) bool) chains.Message {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case m := <-b.out:
			if predicate(m) {
				return m
			}
		case <-timeout.C:
			t.Fatal("missing bus message")
		}
	}
}

func (b *testBus) action(t *testing.T, name string, value any, expires time.Duration) chains.Message {
	t.Helper()
	m, err := chains.New(chains.Request, "engine", chains.Action{Name: name, Args: map[string]any{"value": value}})
	if err != nil {
		t.Fatal(err)
	}
	m.Target, m.DeviceID, m.SchemaVersion, m.ActionID = "receiver", "main", "1", m.ID
	m.CorrelationID = "test-trace"
	deadline := time.Now().Add(expires)
	m.ExpiresAt = &deadline
	raw, err := chains.Encode(m, chains.JSON)
	if err != nil {
		t.Fatal(err)
	}
	b.in <- bus.Delivery{Topic: bus.Route(m), Data: raw}
	return m
}

func matchPatch(m chains.Message, device, attribute, value string) bool {
	var batch chains.ObservationBatch
	if m.Type != chains.ObservationSubmitted || json.Unmarshal(m.Data, &batch) != nil ||
		batch.EventType != chains.DeviceObserved || len(batch.Observations) != 1 {
		return false
	}
	o := batch.Observations[0]
	return o.DeviceID == device && string(o.Values[attribute]) == value
}

func TestServiceWithReceiver(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &testBus{make(chan bus.Delivery, 32), make(chan chains.Message, 128)}
	settings := DefaultSettings()
	settings.Host, settings.Port = "127.0.0.1", listener.Addr().(*net.TCPAddr).Port
	definition := Definition(settings)
	definition.OpenBus = func(context.Context, sdk.Config, bus.Subscription) (bus.Bus, error) { return b, nil }
	done := make(chan error, 1)
	go func() {
		done <- sdk.RunService(ctx, sdk.Config{URI: "mqtt://test", Namespace: "chains", Instance: "onkyo",
			Authority: "engine", Encoding: chains.JSON, ServiceID: "receiver", HeartbeatInterval: 1}, definition)
	}()

	write := func(conn net.Conn, commands ...string) {
		t.Helper()
		for _, command := range commands {
			if _, err := conn.Write(packet(command)); err != nil {
				t.Fatal(err)
			}
		}
	}
	expect := func(conn net.Conn, code, value string) {
		t.Helper()
		messages, err := readPacket(conn)
		if err != nil || len(messages) != 1 || messages[0].code != code || messages[0].value != value {
			t.Fatalf("expected %s%s, got %v, %v", code, value, messages, err)
		}
	}
	connect := func(volume string) net.Conn {
		t.Helper()
		listener.SetDeadline(time.Now().Add(5 * time.Second))
		conn, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		for _, query := range [][2]string{{"PWR", "01"}, {"MVL", volume}, {"AMT", "00"}, {"SLI", "12"}} {
			expect(conn, query[0], "QSTN")
			write(conn, query[0]+query[1])
		}
		return conn
	}
	outcome := func(action chains.Message, status string) chains.Result {
		t.Helper()
		var result chains.Result
		b.expect(t, func(m chains.Message) bool {
			return m.Type == chains.Outcome && m.ActionID == action.ActionID &&
				json.Unmarshal(m.Data, &result) == nil && result.Status == status
		})
		return result
	}

	conn := connect("20")
	b.expect(t, func(m chains.Message) bool {
		if m.Type == chains.ObservationSubmitted {
			t.Fatal("startup produced a live event")
		}
		return m.Type == chains.Report && m.DeviceID == "system" && m.Silent && string(m.Data) == `{"connected":true}`
	})
	write(conn, "MVL21")
	b.expect(t, func(m chains.Message) bool { return matchPatch(m, "main", "volume", "33") })

	action := b.action(t, "set_muted", true, 3*time.Second)
	expect(conn, "AMT", "01")
	write(conn, "SLI02")
	b.expect(t, func(m chains.Message) bool { return matchPatch(m, "main", "input", `"game"`) && m.ActionID == "" })
	expect(conn, "AMT", "QSTN")
	write(conn, "AMT01")
	var completed, observed bool
	b.expect(t, func(m chains.Message) bool {
		var result chains.Result
		if m.Type == chains.Outcome && m.ActionID == action.ActionID &&
			json.Unmarshal(m.Data, &result) == nil && result.Status == chains.Completed {
			if string(result.Value) != `{"attribute":"muted","value":true}` {
				t.Fatalf("setter result = %s", result.Value)
			}
			completed = true
		}
		if matchPatch(m, "main", "muted", "true") {
			if m.ActionID != action.ActionID || m.CausationID != action.ID || m.CorrelationID != action.CorrelationID {
				t.Fatalf("missing setter trace: %+v", m)
			}
			observed = true
		}
		return completed && observed
	})

	action = b.action(t, "set_volume", 30, 450*time.Millisecond)
	expect(conn, "MVL", "1E")
	expect(conn, "MVL", "QSTN")
	outcome(action, chains.TimedOut)
	b.expect(t, func(m chains.Message) bool { return matchPatch(m, "system", "connected", "false") })
	conn.Close()
	conn = connect("1E") // The new connection starts with queries and never resends the setter.
	b.expect(t, func(m chains.Message) bool {
		if matchPatch(m, "main", "volume", "30") {
			t.Fatal("reconnect produced a live volume event")
		}
		return matchPatch(m, "system", "connected", "true")
	})
	action = b.action(t, "set_power", false, 3*time.Second)
	expect(conn, "PWR", "00")
	conn.Close()
	outcome(action, chains.Failed)
	b.expect(t, func(m chains.Message) bool {
		if matchPatch(m, "main", "power", "false") {
			t.Fatal("unconfirmed power command changed State")
		}
		return matchPatch(m, "system", "connected", "false")
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("service failed to stop")
	}
}
