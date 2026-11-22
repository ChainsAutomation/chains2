package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	chains "chains-automation"
	"chains-automation/rules"
	"chains-automation/sdk"
)

func TestTypedCapabilitiesAndSetterWait(t *testing.T) {
	f := newFixture(t, (&memoryBroker{}).open)
	e, _ := f.start(t, authorityID, ":memory:", `
Event(type="device.observed", device_class=light, attribute=temperature, value=22)
target = Device(service_id=lamp, device_id=one)
receipt = Set(device=target, attribute="power", value=true, wait="sent")
if receipt.status == "sent":
    Action("dim", device=target, value=25, wait="sent")
`)
	observer := f.open(t, chains.Request)
	client := sdk.Client{Bus: f.open(t), Source: "lamp"}
	power := chains.Writable(chains.PowerSchema, "turn")
	dim := chains.Writable(chains.DimlevelSchema, "dim")
	device := chains.Device{ID: "one", Name: "Light", Classes: []string{"dimmer"}, Actions: []string{"turn", "dim"},
		Attributes: map[string]chains.Attribute{"power": power, "dimlevel": dim, "temperature": {Schema: json.RawMessage(`{"type":"number"}`)}}}
	s := chains.Service{UUID: client.Source, Name: "Lamps", ServiceType: "fixture",
		Actions: map[string]map[string]json.RawMessage{
			"turn": {"1": json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"boolean"}}}`)},
			"dim":  {"1": json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"integer","minimum":0,"maximum":100}}}`)},
		}, Devices: []chains.Device{device}}
	broken := device
	broken.Attributes = map[string]chains.Attribute{"power": power}
	if err := e.describeDevice(s, &broken); err == nil {
		t.Fatal("registered an incomplete dimmer")
	}
	if err := client.Register(f.ctx, s); err != nil {
		t.Fatal(err)
	}
	eventually(t, "expanded classes", func() bool {
		got, ok := e.view.Service(client.Source)
		return ok && len(got.Devices) > 0 && slices.Equal(got.Devices[0].Classes, []string{"toggle", "light", "dimmer"})
	})
	ref := rules.DeviceRef{ServiceID: client.Source, DeviceID: device.ID}
	if out := e.ResolveDevices(map[string]string{"device_class": "toggle"}, chains.PlacementFilter{}); !slices.Equal(out, []rules.DeviceRef{ref}) {
		t.Fatalf("inherited toggle selection: %+v", out)
	}
	for name, value := range map[string]any{"temperature": 22, "dimlevel": 101, "power": "on"} {
		if _, _, err := e.ResolveSetter(ref, name, value); err == nil {
			t.Fatalf("accepted invalid setter %s", name)
		}
	}
	if err := client.Report(f.ctx, device.ID, map[string]any{"temperature": 20, "power": false}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "initial state", func() bool { return e.State("lamp/one")["power"] == false })
	if err := client.Report(f.ctx, device.ID, map[string]any{"temperature": 21, "dimlevel": 101}, nil); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/operator/v1/actions", bytes.NewBufferString(
		`{"service_id":"lamp","device_id":"one","attribute":"power","value":true}`))
	e.operatorSet(w, req)
	if w.Code != 202 {
		t.Fatalf("setter response %d: %s", w.Code, w.Body.String())
	}
	request := readMessage(t, observer, func(m chains.Message) bool { return m.Type == chains.Request })
	var receipt map[string]any
	if json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt["status"] != chains.Sent || receipt["action_id"] != request.ActionID {
		t.Fatalf("invalid receipt %v", receipt)
	}
	if err := client.Outcome(f.ctx, request, chains.Completed, nil); err != nil {
		t.Fatal(err)
	}
	if got := e.State("lamp/one")["power"]; got != false {
		t.Fatalf("request changed State to %v", got)
	}

	parent, _ := chains.New("fixture.event", "fixture", nil)
	result := make(chan map[string]any, 1)
	go func() {
		reply, _ := e.Act(f.ctx, parent, ref, "turn", "1", map[string]any{"value": true})
		result <- reply
	}()
	blocking := readMessage(t, observer, func(m chains.Message) bool { return m.Type == chains.Request })
	select {
	case <-result:
		t.Fatal("outcome mode returned before service response")
	default:
	}
	if err := client.Report(f.ctx, device.ID, map[string]any{"power": true}, &blocking); err != nil {
		t.Fatal(err)
	}
	if err := client.Outcome(f.ctx, blocking, chains.Completed, nil); err != nil {
		t.Fatal(err)
	}
	if reply := <-result; reply["status"] != chains.Completed {
		t.Fatalf("wrong result %v", reply)
	}
	eventually(t, "observed power", func() bool { return e.State("lamp/one")["power"] == true })
	if got := e.State("lamp/one")["temperature"]; got != float64(20) {
		t.Fatalf("invalid batch partly applied: %v", got)
	}
	if err := client.Report(f.ctx, device.ID, map[string]any{"temperature": 22}, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"turn", "dim"} {
		request := readMessage(t, observer, func(m chains.Message) bool { return m.Type == chains.Request })
		var action chains.Action
		if err := json.Unmarshal(request.Data, &action); err != nil || action.Name != name {
			t.Fatalf("expected %s without waiting for an outcome, got %+v (%v)", name, action, err)
		}
	}
}
