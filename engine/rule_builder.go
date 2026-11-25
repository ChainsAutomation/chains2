package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"chains-automation/rules"
)

type ruleBuilder struct {
	Trigger     string              `json:"trigger"`
	Interval    string              `json:"interval,omitempty"`
	Selectors   map[string]any      `json:"selectors"`
	Actions     []ruleBuilderAction `json:"actions"`
	Concurrency int                 `json:"concurrency,omitempty"`
	Timeout     float64             `json:"timeout,omitempty"`
}

type ruleBuilderAction struct {
	Kind      string         `json:"kind,omitempty"`
	Seconds   *float64       `json:"seconds,omitempty"`
	Service   string         `json:"service_id"`
	Device    string         `json:"device_id"`
	Name      string         `json:"name"`
	Version   string         `json:"version,omitempty"`
	Attribute string         `json:"attribute,omitempty"`
	Wait      string         `json:"wait,omitempty"`
	Args      map[string]any `json:"args"`
}

func (e *Engine) buildRule(name string, b ruleBuilder) (string, error) {
	for i, action := range b.Actions {
		if action.Kind == "delay" {
			continue
		}
		if action.Kind != "" || action.Seconds != nil {
			return "", fmt.Errorf("invalid step %d", i+1)
		}
		if action.Attribute != "" {
			value, ok := action.Args["value"]
			if !ok {
				return "", fmt.Errorf("action %d needs a value", i+1)
			}
			setter, args, err := e.ResolveSetter(rules.DeviceRef{ServiceID: action.Service, DeviceID: action.Device}, action.Attribute, value)
			if err != nil {
				return "", err
			}
			action.Name, action.Version, action.Args = setter.Action, setter.Version, args
		}
		s, known := e.view.Service(action.Service)
		d, found := device(s, action.Device)
		if !known || !found || !contains(d.Actions, action.Name) {
			return "", fmt.Errorf("action %d needs an action supported by its target device", i+1)
		}
		version := action.Version
		if version == "" {
			version = "1"
		}
		data, _ := json.Marshal(action.Args)
		if action.Args == nil {
			data = []byte("{}")
		}
		if err := e.validate(s.Actions[action.Name][version], data); err != nil {
			return "", fmt.Errorf("action %d (%s): %w", i+1, action.Name, err)
		}
	}
	return formatBuilder(name, b)
}

const maxBuilderSeconds = 9e9

func formatBuilder(name string, b ruleBuilder) (string, error) {
	if math.IsNaN(b.Timeout) || math.IsInf(b.Timeout, 0) || b.Timeout < 0 || b.Timeout > maxBuilderSeconds {
		return "", errors.New("rule timeout is outside the supported range")
	}
	r := rules.Rule{Name: name, Concurrency: b.Concurrency, Timeout: time.Duration(math.Round(b.Timeout * float64(time.Second)))}
	opening := &rules.Expression{Kind: "call", Text: b.Trigger}
	switch b.Trigger {
	case "Event", "Time":
		opening.Arguments = builderArgs(b.Selectors)
	case "Every":
		opening.Arguments = []rules.Argument{{Value: builderLiteral(b.Interval)}}
	default:
		return "", errors.New("choose an event or schedule trigger")
	}
	r.Body = append(r.Body, rules.Statement{Kind: "expression", Value: opening})
	if len(b.Actions) == 0 {
		return "", errors.New("add at least one step")
	}
	for _, action := range b.Actions {
		if action.Kind == "delay" {
			if action.Seconds == nil || math.IsNaN(*action.Seconds) || math.IsInf(*action.Seconds, 0) || *action.Seconds < 0 || *action.Seconds > maxBuilderSeconds {
				return "", errors.New("enter nonnegative delay seconds within the supported duration")
			}
			r.Body = append(r.Body, rules.Statement{Kind: "expression", Value: &rules.Expression{
				Kind: "call", Text: "Sleep", Arguments: []rules.Argument{{Value: builderLiteral(*action.Seconds)}},
			}})
			continue
		}
		if action.Service == "" || action.Device == "" || (action.Name == "" && action.Attribute == "") {
			return "", errors.New("select an action and its target")
		}
		dev := &rules.Expression{Kind: "call", Text: "Device", Arguments: []rules.Argument{
			{Name: "service_id", Value: builderLiteral(action.Service)}, {Name: "device_id", Value: builderLiteral(action.Device)},
		}}
		call := &rules.Expression{Kind: "call", Text: "Action", Arguments: []rules.Argument{
			{Value: builderLiteral(action.Name)}, {Name: "device", Value: dev},
		}}
		if action.Attribute != "" {
			call.Text = "Set"
			call.Arguments = []rules.Argument{{Name: "device", Value: dev}, {Name: "attribute", Value: builderLiteral(action.Attribute)}}
		}
		if action.Wait != "" && action.Wait != "outcome" {
			if action.Wait != "sent" {
				return "", errors.New("wait must be outcome or sent")
			}
			call.Arguments = append(call.Arguments, rules.Argument{Name: "wait", Value: builderLiteral(action.Wait)})
		}
		if action.Attribute == "" && action.Version != "" && action.Version != "1" {
			call.Arguments = append(call.Arguments, rules.Argument{Name: "schema_version", Value: builderLiteral(action.Version)})
		}
		call.Arguments = append(call.Arguments, builderArgs(action.Args)...)
		r.Body = append(r.Body, rules.Statement{Kind: "expression", Value: call})
	}
	return rules.Format(r)
}

func builderArgs(values map[string]any) []rules.Argument {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	args := make([]rules.Argument, 0, len(keys))
	for _, key := range keys {
		args = append(args, rules.Argument{Name: key, Value: builderLiteral(values[key])})
	}
	return args
}

func builderLiteral(value any) *rules.Expression {
	switch v := value.(type) {
	case map[string]any:
		return &rules.Expression{Kind: "object", Arguments: builderArgs(v)}
	case []any:
		expr := &rules.Expression{Kind: "list"}
		for _, item := range v {
			expr.Arguments = append(expr.Arguments, rules.Argument{Value: builderLiteral(item)})
		}
		return expr
	case float64:
		if v < 0 {
			return &rules.Expression{Kind: "unary", Text: "-", Left: builderLiteral(-v)}
		}
	}
	return &rules.Expression{Kind: "literal", Literal: value}
}

func literalValue(expr *rules.Expression) (any, bool) {
	switch expr.Kind {
	case "literal":
		return expr.Literal, true
	case "unary":
		if expr.Text == "-" && expr.Left.Kind == "literal" {
			n, ok := expr.Left.Literal.(float64)
			return -n, ok
		}
	case "object":
		return namedLiteralValues(expr.Arguments)
	case "list":
		values := make([]any, 0, len(expr.Arguments))
		for _, arg := range expr.Arguments {
			value, ok := literalValue(arg.Value)
			if !ok {
				return nil, false
			}
			values = append(values, value)
		}
		return values, true
	}
	return nil, false
}

func namedLiteralValues(args []rules.Argument) (map[string]any, bool) {
	values := make(map[string]any, len(args))
	for _, arg := range args {
		value, ok := literalValue(arg.Value)
		if !ok {
			return nil, false
		}
		values[arg.Name] = value
	}
	return values, true
}

func recognizeBuilder(item ruleSource) *ruleBuilder {
	r, err := rules.Parse(item.Name, item.Source)
	if err != nil || len(r.Body) < 2 {
		return nil
	}
	for _, st := range r.Body {
		if st.Kind != "expression" || st.Value.Kind != "call" {
			return nil
		}
	}
	start := r.Body[0].Value
	b := ruleBuilder{Trigger: start.Text, Concurrency: r.Concurrency, Timeout: r.Timeout.Seconds(), Selectors: map[string]any{}}
	if b.Trigger == "Every" {
		if len(start.Arguments) != 1 {
			return nil
		}
		b.Interval, _ = start.Arguments[0].Value.Literal.(string)
	} else {
		var ok bool
		b.Selectors, ok = namedLiteralValues(start.Arguments)
		if !ok {
			return nil
		}
	}
	for _, st := range r.Body[1:] {
		call := st.Value
		a := ruleBuilderAction{}
		deviceIndex := 0
		switch call.Text {
		case "Sleep":
			value, _ := literalValue(call.Arguments[0].Value)
			n, ok := value.(float64)
			if !ok {
				return nil
			}
			b.Actions = append(b.Actions, ruleBuilderAction{Kind: "delay", Seconds: &n})
			continue
		case "Action":
			if len(call.Arguments) < 2 {
				return nil
			}
			a.Name, _ = call.Arguments[0].Value.Literal.(string)
			deviceIndex = 1
		case "Set":
			if len(call.Arguments) < 3 || call.Arguments[1].Name != "attribute" {
				return nil
			}
			a.Attribute, _ = call.Arguments[1].Value.Literal.(string)
		default:
			return nil
		}
		if call.Arguments[deviceIndex].Name != "device" {
			return nil
		}
		dev := call.Arguments[deviceIndex].Value
		if dev.Kind != "call" || dev.Text != "Device" {
			return nil
		}
		target, ok := namedLiteralValues(dev.Arguments)
		if !ok {
			return nil
		}
		a.Service, _ = target["service_id"].(string)
		a.Device, _ = target["device_id"].(string)
		a.Args, ok = namedLiteralValues(call.Arguments[2:])
		if !ok {
			return nil
		}
		a.Version, _ = a.Args["schema_version"].(string)
		a.Wait, _ = a.Args["wait"].(string)
		delete(a.Args, "wait")
		delete(a.Args, "schema_version")
		b.Actions = append(b.Actions, a)
	}
	source, err := formatBuilder(item.Name, b)
	// Exact round trips preserve comments, source layout, and advanced logic.
	if err != nil || source != item.Source {
		return nil
	}
	return &b
}
