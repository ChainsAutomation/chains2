// Package onkyo connects Chains to an Onkyo TX-NR696 over eISCP.
package onkyo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	chains "chains-automation"
	"chains-automation/sdk"
)

type ZoneSettings struct {
	Name     string   `toml:"name"`
	Location []string `toml:"location"`
	Tags     []string `toml:"tags"`
}

type Settings struct {
	Host      string       `toml:"host" required:"true" help:"Receiver LAN hostname or IP address"`
	Port      int          `toml:"port" help:"Receiver eISCP TCP port"`
	Zone2Mode string       `toml:"zone2_mode" help:"disabled, speakers, or external"`
	Main      ZoneSettings `toml:"main"`
	Zone2     ZoneSettings `toml:"zone2"`
}

func DefaultSettings() Settings {
	return Settings{Port: 60128, Zone2Mode: "disabled", Main: ZoneSettings{Name: "Main"}, Zone2: ZoneSettings{Name: "Zone 2"}}
}

type control struct {
	device, attribute, code string
	inputs                  map[string]string // canonical name to eISCP selector
}

var mainInputs = map[string]string{
	"bd_dvd": "10", "game": "02", "cbl_sat": "01", "strm_box": "11", "pc": "05", "aux": "03",
	"cd": "23", "tv": "12", "phono": "22", "fm": "24", "am": "25", "net": "2B", "bluetooth": "2E",
}
var zone2Inputs = map[string]string{
	"bd_dvd": "10", "game": "02", "cbl_sat": "01", "cd": "23", "tv": "12",
	"fm": "24", "am": "25", "net": "2B", "bluetooth": "2E", "same_as_main": "80",
}

func (c control) schema() string {
	switch c.attribute {
	case "volume":
		return `{"type":"integer","minimum":0,"maximum":200}`
	case "input":
		var names []string
		for name := range c.inputs {
			names = append(names, name)
		}
		slices.Sort(names)
		raw, _ := json.Marshal(names)
		return `{"type":"string","enum":` + string(raw) + `}`
	default:
		return chains.PowerSchema
	}
}

func (c control) action() string {
	if c.device == "zone2" && c.attribute == "input" {
		return "set_zone2_input"
	}
	return "set_" + c.attribute
}

func (c control) decode(raw string) (any, bool) {
	switch c.attribute {
	case "input":
		for name, code := range c.inputs {
			if raw == code {
				return name, true
			}
		}
	case "volume":
		if len(raw) == 2 {
			n, err := strconv.ParseUint(raw, 16, 8)
			if err == nil && n <= 200 {
				return int(n), true
			}
		}
	default:
		switch raw {
		case "00":
			return false, true
		case "01":
			return true, true
		}
	}
	return nil, false
}

func (c control) encode(v any) (string, bool) {
	switch c.attribute {
	case "input":
		name, ok := v.(string)
		code, found := c.inputs[name]
		return code, ok && found
	case "volume":
		n, ok := v.(float64)
		if ok && n >= 0 && n <= 200 && n == float64(int(n)) {
			return fmt.Sprintf("%02X", int(n)), true
		}
	default:
		if b, ok := v.(bool); ok {
			if b {
				return "01", true
			}
			return "00", true
		}
	}
	return "", false
}

func Definition(settings Settings) sdk.Definition {
	controls := []control{
		{"main", "power", "PWR", nil}, {"main", "volume", "MVL", nil},
		{"main", "muted", "AMT", nil}, {"main", "input", "SLI", mainInputs},
	}
	zones := map[string]ZoneSettings{"main": settings.Main}
	if settings.Zone2Mode != "disabled" {
		zones["zone2"] = settings.Zone2
		controls = append(controls, control{"zone2", "power", "ZPW", nil}, control{"zone2", "input", "SLZ", zone2Inputs})
		if settings.Zone2Mode == "speakers" {
			controls = append(controls, control{"zone2", "volume", "ZVL", nil}, control{"zone2", "muted", "ZMT", nil})
		}
	}
	receiver := &receiver{controls: controls}
	d := sdk.Definition{
		Service: chains.Service{ServiceType: "onkyo", Description: "Onkyo TX-NR696 network receiver", Actions: map[string]map[string]json.RawMessage{}},
		Actions: map[string]map[string]sdk.ActionHandler{},
	}
	for _, id := range []string{"main", "zone2"} {
		zone, ok := zones[id]
		if !ok {
			continue
		}
		device := chains.Device{ID: id, Name: zone.Name, Placement: chains.Placement{Location: zone.Location, Tags: zone.Tags},
			Classes: []string{"toggle"}, Attributes: map[string]chains.Attribute{}}
		for _, c := range controls {
			if c.device != id {
				continue
			}
			action, schema := c.action(), c.schema()
			attr := chains.Writable(schema, action)
			if c.attribute == "volume" {
				attr.Label, attr.Unit = "Volume", "steps"
				attr.Description = "Native receiver volume steps"
			}
			if c.attribute == "muted" {
				attr.Label = "Muted"
			}
			device.Attributes[c.attribute] = attr
			device.Actions = append(device.Actions, action)
			if _, exists := d.Actions[action]; !exists {
				d.Service.Actions[action] = map[string]json.RawMessage{"1": json.RawMessage(
					`{"type":"object","required":["value"],"properties":{"value":` + schema + `},"additionalProperties":false}`)}
				d.Service.IdempotentActions = append(d.Service.IdempotentActions, action)
				d.Actions[action] = map[string]sdk.ActionHandler{"1": receiver.act}
			}
		}
		d.Service.Devices = append(d.Service.Devices, device)
	}
	d.Service.Devices = append(d.Service.Devices, chains.Device{ID: "system", Name: "Receiver connection", Attributes: map[string]chains.Attribute{
		"connected": {Schema: json.RawMessage(chains.PowerSchema), Description: "Receiver is answering eISCP requests"},
	}})
	d.Hooks.OnInit = func(_ context.Context, _ *sdk.Runtime) error {
		if settings.Host == "" || strings.ContainsAny(settings.Host, "/[]") ||
			strings.IndexFunc(settings.Host, unicode.IsSpace) >= 0 || settings.Port < 1 || settings.Port > 65535 {
			return errors.New("set an Onkyo LAN hostname or IP address and a valid TCP port")
		}
		switch settings.Zone2Mode {
		case "disabled", "speakers", "external":
		default:
			return errors.New("zone2_mode must be disabled, speakers, or external")
		}
		receiver.address = net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port))
		return nil
	}
	d.Hooks.OnStart = func(_ context.Context, runtime *sdk.Runtime) error {
		runtime.Go(func(ctx context.Context) error { return receiver.run(ctx, runtime) })
		return nil
	}
	return d
}

const requestTimeout = 3 * time.Second

func (r *receiver) act(ctx context.Context, _ *sdk.Runtime, envelope chains.Message, action chains.Action) (any, error) {
	for _, c := range r.controls {
		if c.device != envelope.DeviceID || c.action() != action.Name {
			continue
		}
		value, ok := c.encode(action.Args["value"])
		if !ok {
			return nil, errors.New("invalid receiver value")
		}
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		return r.submit(ctx, c, value)
	}
	return nil, errors.New("receiver control unavailable on this zone")
}
