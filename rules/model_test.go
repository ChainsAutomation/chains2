package rules

import (
	"strings"
	"testing"
)

func TestStructuredRuleRoundTrip(t *testing.T) {
	literal := func(value any) *Expression { return &Expression{Kind: "literal", Literal: value} }
	call := func(name string, args ...Argument) *Expression {
		return &Expression{Kind: "call", Text: name, Arguments: args}
	}
	rule := Rule{Name: "panel", Body: []Statement{
		{Kind: "expression", Value: call("Event", Argument{Name: "device_class", Value: literal("light")})},
		{Kind: "expression", Value: call("Action",
			Argument{Value: literal("writestring")},
			Argument{Name: "service_name", Value: literal("LEDPanel")},
			Argument{Name: "device_name", Value: literal("panel0")},
			Argument{Name: "value", Value: literal("Light is modified")},
		)},
	}}
	generated, err := Format(rule)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(rule.Name, generated)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Format(parsed)
	if err != nil || second != generated {
		t.Fatalf("round trip %q, %v", second, err)
	}
}

func TestParseDiagnostics(t *testing.T) {
	cases := []struct{ source, want string }{
		{"Event()\n  return\n", "2:3:"},
		{"Event()\nif true:\n    return\n  return\n", "4:3:"},
		{"Event()\nAction('write', device=missing)\n", `unknown variable "missing"`},
		{"Event()\nAction('write', service_name=LEDPanel)\n", "both service_name="},
		{"rule \"old\" { Event() }\n", "1:"},
		{"Event()\neof = missing\n", `unknown variable "missing"`},
		{"Event()\ncontinue\n", "continue needs a loop"},
	}
	for _, tc := range cases {
		_, err := Parse("invalid", tc.source)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: want %q, got %v", tc.source, tc.want, err)
		}
	}
}

func TestMultilineCallsAndControlFlow(t *testing.T) {
	source := `
# Pure setup
selected = "sensor"
event = Event(
    type="temperature.reported",
    device_class=selected, # trailing comment
)
if not event.data.active == true:
    config = {"value": [1, 2]}
elseif event.data.count > 1:
    config = {"value": [3]}
else:
    config = {"value": []}
for item in config.value:
    if item == 1:
        continue
    Action('write', device=event.device, value="line\n" + 'it\'s ready')
`
	rule, err := Parse("control", source)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := Format(rule)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse("control", canonical)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Format(parsed)
	if err != nil || second != canonical {
		t.Fatalf("format is unstable: %q, %v", second, err)
	}
	if !strings.Contains(canonical, "elseif event.data.count > 1:") {
		t.Fatalf("Format did not preserve elseif cleanly:\n%s", canonical)
	}
}

func TestScopingAndNewSyntaxValidation(t *testing.T) {
	valid := `
rule.mode = "restart"
rule.timeout = 60

ev = Event(type="sensor.sample")
if not Has(ev.data, "value"):
    return
else:
    reading = ev.data.value
switch reading:
    case 1:
        label = "one"
    default:
        label = "other"
if "o" in label and "z" not in label and Len(label) > 0:
    devs = Devices(device_class=light, tag=guest)
    if Len(devs) == 0:
        next = Event(type="followup", timeout=5)
        if next == null:
            Action("noop", device=ev.device, value=Min(Max(Round(Abs(-1.6)), 1), 10))
`
	rule, err := Parse("scoping", valid)
	if err != nil {
		t.Fatal(err)
	}
	formatted, err := Format(rule)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(formatted, `rule.mode = "restart"`) {
		t.Fatalf("missing rule.mode in formatted output:\n%s", formatted)
	}
	reparsed, err := Parse("scoping", formatted)
	if err != nil {
		t.Fatalf("reparse failed: %v\n%s", err, formatted)
	}
	again, err := Format(reparsed)
	if err != nil || again != formatted {
		t.Fatalf("format unstable:\n%s\n---\n%s", formatted, again)
	}

	for _, bad := range []string{
		"rule.mode = \"invalid\"\nEvent()\n",
		"rule.mode = \"restart\"\nrule.concurrency = 2\nEvent()\n",
		"Event(type=\"a\", timeout=5)\n",
		"x = Now()\nEvent()\n",
	} {
		if _, err := Parse("bad", bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
