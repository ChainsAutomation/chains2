package rules

import (
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"
)

func isTrigger(name string) bool { return name == "Event" || name == "Every" || name == "Time" }

func openingIndex(rule Rule) int {
	for i, s := range rule.Body {
		if (s.Kind == "assign" || s.Kind == "expression") && s.Value != nil && s.Value.Kind == "call" && isTrigger(s.Value.Text) {
			return i
		}
	}
	return -1
}

func invalid(at Position, format string, args ...any) error {
	return fmt.Errorf("%d:%d: %s", at.Line, at.Column, fmt.Sprintf(format, args...))
}

func validName(name string) bool {
	if name == "" || !identified(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !identified(name[i]) && !digit(name[i]) {
			return false
		}
	}
	return true
}

func variable(name string) bool {
	if !validName(name) {
		return false
	}
	switch name {
	case "rule", "true", "false", "null", "if", "elseif", "else", "while", "for", "in", "switch", "case", "default", "break", "continue", "return", "and", "or", "not":
		return false
	}
	return true
}

// Validate checks a parsed or UI-generated rule before execution.
func Validate(rule Rule) error {
	if rule.Name == "" {
		return fmt.Errorf("rule name is required")
	}
	if rule.Concurrency < 0 || rule.Concurrency > 1024 || rule.Timeout < 0 {
		return fmt.Errorf("invalid settings for rule %q", rule.Name)
	}
	start := openingIndex(rule)
	if start < 0 {
		return fmt.Errorf("rule %q needs an opening Event(), Every(), or Time()", rule.Name)
	}
	names := map[string]bool{}
	for _, s := range rule.Body[:start] {
		if s.Kind != "assign" {
			return invalid(s.At, "only pure assignments may precede the opening wait")
		}
		if err := validateExpr(s.Value, names, false, true); err != nil {
			return err
		}
		if !variable(s.Name) {
			return invalid(s.At, "invalid variable %q", s.Name)
		}
		names[s.Name] = true
	}
	opening := rule.Body[start]
	if err := validateCall(opening.Value, names, false, true); err != nil {
		return err
	}
	if opening.Kind == "assign" {
		if !variable(opening.Name) {
			return invalid(opening.At, "invalid variable %q", opening.Name)
		}
		names[opening.Name] = true
	}
	return validateBlock(rule.Body[start+1:], names, 0, 0)
}

func validateBlock(body []Statement, names map[string]bool, loops, switches int) error {
	for _, s := range body {
		switch s.Kind {
		case "break":
			if loops+switches == 0 {
				return invalid(s.At, "break needs a loop or switch")
			}
			continue
		case "continue":
			if loops == 0 {
				return invalid(s.At, "continue needs a loop")
			}
			continue
		case "return":
			continue
		}
		if err := validateExpr(s.Value, names, false, false); err != nil {
			return err
		}
		switch s.Kind {
		case "expression":
		case "assign":
			if !variable(s.Name) {
				return invalid(s.At, "invalid variable %q", s.Name)
			}
			names[s.Name] = true
		case "if":
			onTrue, onFalse := maps.Clone(names), maps.Clone(names)
			if len(s.Body) == 0 {
				return invalid(s.At, "expected a nonempty block")
			}
			if err := validateBlock(s.Body, onTrue, loops, switches); err != nil {
				return err
			}
			if err := validateBlock(s.Else, onFalse, loops, switches); err != nil {
				return err
			}
			for name := range onTrue {
				if onFalse[name] {
					names[name] = true
				}
			}
		case "for", "while":
			inside := maps.Clone(names)
			if s.Kind == "for" {
				if !variable(s.Name) {
					return invalid(s.At, "invalid loop variable %q", s.Name)
				}
				inside[s.Name] = true
			}
			if len(s.Body) == 0 {
				return invalid(s.At, "expected a nonempty block")
			}
			if err := validateBlock(s.Body, inside, loops+1, switches); err != nil {
				return err
			}
		case "switch":
			if len(s.Cases) == 0 {
				return invalid(s.At, "switch needs a case")
			}
			hasDefault := false
			for _, clause := range s.Cases {
				if len(clause.Values) == 0 {
					if hasDefault {
						return invalid(s.At, "duplicate default case")
					}
					hasDefault = true
				}
				for _, v := range clause.Values {
					if err := validateExpr(v, names, false, false); err != nil {
						return err
					}
				}
				if len(clause.Body) == 0 {
					return invalid(s.At, "expected a nonempty case")
				}
				if err := validateBlock(clause.Body, maps.Clone(names), loops, switches+1); err != nil {
					return err
				}
			}
		default:
			return invalid(s.At, "unknown statement %q", s.Kind)
		}
	}
	return nil
}

func validateExpr(x *Expression, names map[string]bool, shorthand, pure bool) error {
	if x == nil {
		return fmt.Errorf("missing expression")
	}
	switch x.Kind {
	case "literal":
		switch value := x.Literal.(type) {
		case nil, bool, string, int:
		case float64:
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return invalid(x.At, "non-finite number")
			}
		default:
			return invalid(x.At, "unsupported literal %T", value)
		}
	case "name":
		if !variable(x.Text) || (!shorthand && !names[x.Text]) {
			return invalid(x.At, "unknown variable %q", x.Text)
		}
	case "call":
		return validateCall(x, names, pure, false)
	case "binary", "index":
		if x.Kind == "binary" && precedence[x.Text] == 0 {
			return invalid(x.At, "unknown operator %q", x.Text)
		}
		if err := validateExpr(x.Right, names, false, pure); err != nil {
			return err
		}
		return validateExpr(x.Left, names, false, pure)
	case "field", "unary":
		if x.Kind == "field" && !validName(x.Text) {
			return invalid(x.At, "invalid field %q", x.Text)
		}
		if x.Kind == "unary" && x.Text != "+" && x.Text != "-" && x.Text != "not" {
			return invalid(x.At, "invalid operator %q", x.Text)
		}
		return validateExpr(x.Left, names, false, pure)
	case "list", "object":
		keys := map[string]bool{}
		for _, arg := range x.Arguments {
			if x.Kind == "object" && keys[arg.Name] {
				return invalid(x.At, "duplicate object key %q", arg.Name)
			}
			keys[arg.Name] = true
			if err := validateExpr(arg.Value, names, false, pure); err != nil {
				return err
			}
		}
	default:
		return invalid(x.At, "unknown expression %q", x.Kind)
	}
	return nil
}

func bareArgument(function, key string) bool {
	return (function == "Event" && key != "device") || ((function == "Action" || function == "Set" || function == "Device" || function == "Devices") && (key == "service_name" || key == "device_name" || key == "service_id" || key == "device_id"))
}

func validateCall(x *Expression, names map[string]bool, pure, opening bool) error {
	keys := map[string]bool{}
	positional := 0
	for _, arg := range x.Arguments {
		if arg.Name == "" {
			if len(keys) > 0 {
				return invalid(x.At, "positional arguments must precede named arguments")
			}
			positional++
		} else {
			if !validName(arg.Name) || keys[arg.Name] {
				return invalid(x.At, "invalid or duplicate argument %q", arg.Name)
			}
			keys[arg.Name] = true
		}
		if err := validateExpr(arg.Value, names, bareArgument(x.Text, arg.Name), pure || opening); err != nil {
			return err
		}
	}
	bad := func(want string) error { return invalid(x.At, "%s expects %s", x.Text, want) }
	if pure && !opening && x.Text != "Has" && x.Text != "Exists" && x.Text != "Device" {
		return invalid(x.At, "%s cannot run before the opening match", x.Text)
	}
	switch x.Text {
	case "Event":
		if keys["device"] && (keys["service_id"] || keys["device_id"] || keys["service_name"] || keys["device_name"]) {
			return bad("device= or separate service and device selectors")
		}
		fallthrough
	case "Time":
		if positional != 0 {
			return bad("named arguments")
		}
	case "Set":
		if positional != 0 || !keys["attribute"] || !keys["value"] {
			return bad("a target, attribute=, and value=")
		}
		for key := range keys {
			switch key {
			case "device", "service_name", "device_name", "attribute", "value", "wait":
			default:
				return bad("a target, attribute=, value=, and optional wait=")
			}
		}
		if keys["device"] == (keys["service_name"] || keys["device_name"]) ||
			(!keys["device"] && (!keys["service_name"] || !keys["device_name"])) {
			return bad("one target")
		}
	case "Action":
		if positional != 1 {
			return bad("an action name followed by named arguments")
		}
		if keys["device"] {
			if keys["service_name"] || keys["device_name"] {
				return bad("one target")
			}
		} else if !keys["service_name"] || !keys["device_name"] {
			return bad("device= or both service_name= and device_name=")
		}
	case "Devices":
		if positional != 0 || len(keys) == 0 {
			return bad("named device selectors")
		}
		for key := range keys {
			switch key {
			case "service_id", "service_name", "device_id", "device_name", "device_class", "location", "tag", "tags":
			default:
				return bad("device, service, class, location or tag selectors")
			}
		}

	case "Device":
		if positional != 0 || len(keys) != 2 || !(keys["service_id"] && keys["device_id"] || keys["service_name"] && keys["device_name"]) {
			return bad("a service/device ID pair or a service/device name pair")
		}
	case "Exists", "Every", "State", "Sleep":
		if positional != 1 || len(keys) > 0 {
			return bad("one positional argument")
		}
	case "Has":
		if positional != 2 || len(keys) > 0 {
			return bad("two positional arguments")
		}
	default:
		return invalid(x.At, "unknown function %q", x.Text)
	}
	return nil
}

// Format generates canonical executable source from the structured rule model.
func Format(rule Rule) (string, error) {
	if err := Validate(rule); err != nil {
		return "", err
	}
	var out strings.Builder
	if rule.Concurrency != 0 {
		fmt.Fprintf(&out, "rule.concurrency = %d\n", rule.Concurrency)
	}
	if rule.Timeout != 0 {
		seconds := rule.Timeout / time.Second
		nanos := rule.Timeout % time.Second
		fmt.Fprintf(&out, "rule.timeout = %d", seconds)
		if nanos != 0 {
			fmt.Fprintf(&out, ".%s", strings.TrimRight(fmt.Sprintf("%09d", nanos), "0"))
		}
		out.WriteByte('\n')
	}
	if out.Len() > 0 {
		out.WriteByte('\n')
	}
	formatBlock(&out, rule.Body, 0)
	return out.String(), nil
}

func formatBlock(out *strings.Builder, body []Statement, depth int) {
	indent := strings.Repeat("    ", depth)
	for _, s := range body {
		line := func(text string) { out.WriteString(indent + text + "\n") }
		switch s.Kind {
		case "expression":
			line(formatExpr(s.Value))
		case "assign":
			line(s.Name + " = " + formatExpr(s.Value))
		case "return", "continue", "break":
			line(s.Kind)
		case "for":
			line("for " + s.Name + " in " + formatExpr(s.Value) + ":")
			formatBlock(out, s.Body, depth+1)
		case "while":
			line("while " + formatExpr(s.Value) + ":")
			formatBlock(out, s.Body, depth+1)
		case "if":
			line("if " + formatExpr(s.Value) + ":")
			formatBlock(out, s.Body, depth+1)
			if len(s.Else) > 0 {
				line("else:")
				formatBlock(out, s.Else, depth+1)
			}
		case "switch":
			line("switch " + formatExpr(s.Value) + ":")
			for _, clause := range s.Cases {
				header := "default"
				if len(clause.Values) > 0 {
					values := make([]string, 0, len(clause.Values))
					for _, v := range clause.Values {
						values = append(values, formatExpr(v))
					}
					header = "case " + strings.Join(values, ", ")
				}
				line("    " + header + ":")
				formatBlock(out, clause.Body, depth+2)
			}
		}
	}
}

func formatExpr(x *Expression) string {
	switch x.Kind {
	case "literal":
		switch v := x.Literal.(type) {
		case nil:
			return "null"
		case string:
			return strconv.Quote(v)
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		default:
			return fmt.Sprint(v)
		}
	case "name":
		return x.Text
	case "field":
		return formatExpr(x.Left) + "." + x.Text
	case "index":
		return formatExpr(x.Left) + "[" + formatExpr(x.Right) + "]"
	case "unary":
		return "(" + x.Text + " " + formatExpr(x.Left) + ")"
	case "binary":
		return "(" + formatExpr(x.Left) + " " + x.Text + " " + formatExpr(x.Right) + ")"
	}
	args := make([]string, 0, len(x.Arguments))
	for _, a := range x.Arguments {
		item := formatExpr(a.Value)
		if x.Kind == "object" {
			item = strconv.Quote(a.Name) + ": " + item
		} else if a.Name != "" {
			item = a.Name + "=" + item
		}
		args = append(args, item)
	}
	joined := strings.Join(args, ", ")
	switch x.Kind {
	case "call":
		return x.Text + "(" + joined + ")"
	case "list":
		return "[" + joined + "]"
	default:
		return "{" + joined + "}"
	}
}
