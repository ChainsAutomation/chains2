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

func isOpeningStatement(s Statement) bool {
	if s.Kind == "select" {
		return true
	}
	return (s.Kind == "assign" || s.Kind == "expression") && s.Value != nil && s.Value.Kind == "call" && isTrigger(s.Value.Text)
}

func openingIndex(rule Rule) int {
	for i, s := range rule.Body {
		if isOpeningStatement(s) {
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
	case "rule", "true", "false", "null", "if", "elseif", "else", "while", "for", "in", "switch", "select", "case", "default", "when", "break", "continue", "return", "and", "or", "not":
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
	if rule.Mode != "" && rule.Mode != "drop" && rule.Mode != "restart" {
		return fmt.Errorf("invalid mode %q for rule %q", rule.Mode, rule.Name)
	}
	if rule.Scope != "" && rule.Scope != "rule" && rule.Scope != "device" && rule.Scope != "service" {
		return fmt.Errorf("invalid scope %q for rule %q", rule.Scope, rule.Name)
	}
	if rule.Mode == "restart" && rule.Concurrency > 1 {
		return fmt.Errorf("rule %q cannot combine mode \"restart\" with concurrency > 1", rule.Name)
	}
	start := openingIndex(rule)
	if start < 0 {
		return fmt.Errorf("rule %q needs an opening Event(), Every(), Time(), or select", rule.Name)
	}
	names := map[string]bool{}
	for _, s := range rule.Body[:start] {
		if s.Kind != "assign" || s.Guard != nil {
			return invalid(s.At, "only pure assignments may precede the opening wait")
		}
		if err := validateExpr(s.Value, names, false, exprPure); err != nil {
			return err
		}
		if !variable(s.Name) {
			return invalid(s.At, "invalid variable %q", s.Name)
		}
		names[s.Name] = true
	}
	opening := rule.Body[start]
	if opening.Kind == "select" {
		if _, err := validateSelect(opening, names, 0, 0, true); err != nil {
			return err
		}
		_, err := validateBlock(rule.Body[start+1:], names, 0, 0)
		return err
	}
	if err := validateCall(opening.Value, names, exprNormal, true, false); err != nil {
		return err
	}
	if opening.Kind == "assign" {
		if !variable(opening.Name) {
			return invalid(opening.At, "invalid variable %q", opening.Name)
		}
		names[opening.Name] = true
	}
	if opening.Guard != nil {
		if opening.Value.Text != "Event" {
			return invalid(opening.At, "when guard is only valid on Event()")
		}
		if err := validateExpr(opening.Guard, names, false, exprGuard); err != nil {
			return err
		}
	}
	_, err := validateBlock(rule.Body[start+1:], names, 0, 0)
	return err
}

type exprMode int

const (
	exprNormal exprMode = iota
	exprPure
	exprGuard
)

func validateSelect(s Statement, names map[string]bool, loops, switches int, opening bool) (bool, error) {
	if len(s.Arms) == 0 {
		return false, invalid(s.At, "select needs at least one case")
	}
	allRet := true
	var common map[string]bool
	for _, arm := range s.Arms {
		if arm.Wait == nil || arm.Wait.Kind != "call" {
			return false, invalid(arm.At, "select case expects Event(), Every(), Time(), or Sleep()")
		}
		waitName := arm.Wait.Text
		if opening {
			if !isTrigger(waitName) {
				return false, invalid(arm.At, "opening select case expects Event(), Every(), or Time()")
			}
		} else {
			if !isTrigger(waitName) && waitName != "Sleep" {
				return false, invalid(arm.At, "select case expects Event(), Every(), Time(), or Sleep()")
			}
		}
		if err := validateCall(arm.Wait, names, exprNormal, opening, true); err != nil {
			return false, err
		}
		branch := maps.Clone(names)
		if arm.Binding != "" {
			if waitName == "Sleep" {
				return false, invalid(arm.At, "Sleep cannot bind a variable")
			}
			if !variable(arm.Binding) {
				return false, invalid(arm.At, "invalid variable %q", arm.Binding)
			}
			branch[arm.Binding] = true
		}
		if arm.Guard != nil {
			if waitName != "Event" {
				return false, invalid(arm.At, "when guard is only valid on Event()")
			}
			if err := validateExpr(arm.Guard, branch, false, exprGuard); err != nil {
				return false, err
			}
		}
		if len(arm.Body) == 0 {
			return false, invalid(arm.At, "expected a nonempty case")
		}
		armRet, err := validateBlock(arm.Body, branch, loops, switches+1)
		if err != nil {
			return false, err
		}
		if !armRet {
			allRet = false
			if common == nil {
				common = branch
			} else {
				for name := range common {
					if !branch[name] {
						delete(common, name)
					}
				}
			}
		}
	}
	if allRet {
		return true, nil
	}
	if common != nil {
		maps.Copy(names, common)
	}
	return false, nil
}

func validateBlock(body []Statement, names map[string]bool, loops, switches int) (bool, error) {
	returned := false
	for _, s := range body {
		switch s.Kind {
		case "break":
			if loops+switches == 0 {
				return false, invalid(s.At, "break needs a loop or switch")
			}
			continue
		case "continue":
			if loops == 0 {
				return false, invalid(s.At, "continue needs a loop")
			}
			continue
		case "return":
			returned = true
			continue
		case "select":
			selRet, err := validateSelect(s, names, loops, switches, false)
			if err != nil {
				return false, err
			}
			if selRet {
				returned = true
			}
			continue
		}
		if err := validateExpr(s.Value, names, false, exprNormal); err != nil {
			return false, err
		}
		if s.Guard != nil {
			if (s.Kind != "expression" && s.Kind != "assign") || s.Value == nil || s.Value.Kind != "call" || s.Value.Text != "Event" {
				return false, invalid(s.At, "when guard is only valid on Event()")
			}
			guardNames := maps.Clone(names)
			if s.Kind == "assign" {
				if !variable(s.Name) {
					return false, invalid(s.At, "invalid variable %q", s.Name)
				}
				guardNames[s.Name] = true
			}
			if err := validateExpr(s.Guard, guardNames, false, exprGuard); err != nil {
				return false, err
			}
		}
		switch s.Kind {
		case "expression":
		case "assign":
			if !variable(s.Name) {
				return false, invalid(s.At, "invalid variable %q", s.Name)
			}
			names[s.Name] = true
		case "if":
			onTrue, onFalse := maps.Clone(names), maps.Clone(names)
			if len(s.Body) == 0 {
				return false, invalid(s.At, "expected a nonempty block")
			}
			trueRet, err := validateBlock(s.Body, onTrue, loops, switches)
			if err != nil {
				return false, err
			}
			falseRet, err := validateBlock(s.Else, onFalse, loops, switches)
			if err != nil {
				return false, err
			}
			switch {
			case trueRet && falseRet:
				returned = true
			case trueRet:
				maps.Copy(names, onFalse)
			case falseRet:
				maps.Copy(names, onTrue)
			default:
				for name := range onTrue {
					if onFalse[name] {
						names[name] = true
					}
				}
			}
		case "for", "while":
			inside := maps.Clone(names)
			if s.Kind == "for" {
				if !variable(s.Name) {
					return false, invalid(s.At, "invalid loop variable %q", s.Name)
				}
				inside[s.Name] = true
			}
			if len(s.Body) == 0 {
				return false, invalid(s.At, "expected a nonempty block")
			}
			if _, err := validateBlock(s.Body, inside, loops+1, switches); err != nil {
				return false, err
			}
		case "switch":
			if len(s.Cases) == 0 {
				return false, invalid(s.At, "switch needs a case")
			}
			hasDefault := false
			allRet := true
			var common map[string]bool
			for _, clause := range s.Cases {
				if len(clause.Values) == 0 {
					if hasDefault {
						return false, invalid(s.At, "duplicate default case")
					}
					hasDefault = true
				}
				for _, v := range clause.Values {
					if err := validateExpr(v, names, false, exprNormal); err != nil {
						return false, err
					}
				}
				if len(clause.Body) == 0 {
					return false, invalid(s.At, "expected a nonempty case")
				}
				branch := maps.Clone(names)
				caseRet, err := validateBlock(clause.Body, branch, loops, switches+1)
				if err != nil {
					return false, err
				}
				if !caseRet {
					allRet = false
					if common == nil {
						common = branch
					} else {
						for name := range common {
							if !branch[name] {
								delete(common, name)
							}
						}
					}
				}
			}
			if hasDefault {
				if allRet {
					returned = true
				} else if common != nil {
					maps.Copy(names, common)
				}
			}
		default:
			return false, invalid(s.At, "unknown statement %q", s.Kind)
		}
	}
	return returned, nil
}

func validateExpr(x *Expression, names map[string]bool, shorthand bool, mode exprMode) error {
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
		return validateCall(x, names, mode, false, false)
	case "binary", "index":
		if x.Kind == "binary" && precedence[x.Text] == 0 {
			return invalid(x.At, "unknown operator %q", x.Text)
		}
		if err := validateExpr(x.Right, names, false, mode); err != nil {
			return err
		}
		return validateExpr(x.Left, names, false, mode)
	case "field", "unary":
		if x.Kind == "field" && !validName(x.Text) {
			return invalid(x.At, "invalid field %q", x.Text)
		}
		if x.Kind == "unary" && x.Text != "+" && x.Text != "-" && x.Text != "not" {
			return invalid(x.At, "invalid operator %q", x.Text)
		}
		return validateExpr(x.Left, names, false, mode)
	case "list", "object":
		keys := map[string]bool{}
		for _, arg := range x.Arguments {
			if x.Kind == "object" && keys[arg.Name] {
				return invalid(x.At, "duplicate object key %q", arg.Name)
			}
			keys[arg.Name] = true
			if err := validateExpr(arg.Value, names, false, mode); err != nil {
				return err
			}
		}
	default:
		return invalid(x.At, "unknown expression %q", x.Kind)
	}
	return nil
}

func bareArgument(function, key string) bool {
	if function == "Event" && key != "device" && key != "timeout" {
		return true
	}
	if function == "Devices" && (key == "device_class" || key == "location" || key == "tag") {
		return true
	}
	return (function == "Action" || function == "Set" || function == "Device" || function == "Devices") &&
		(key == "service_name" || key == "device_name" || key == "service_id" || key == "device_id")
}

func pureFunction(name string) bool {
	switch name {
	case "Has", "Exists", "Device", "Len", "ParseTime", "Duration", "Min", "Max", "Abs", "Round":
		return true
	}
	return false
}

func guardFunction(name string) bool {
	return pureFunction(name) || name == "State" || name == "Devices" || name == "LocalTime" || name == "Now"
}

func validateCall(x *Expression, names map[string]bool, mode exprMode, opening, inSelect bool) error {
	keys := map[string]bool{}
	positional := 0
	argMode := mode
	if opening {
		argMode = exprPure
	}
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
		if err := validateExpr(arg.Value, names, bareArgument(x.Text, arg.Name), argMode); err != nil {
			return err
		}
	}
	bad := func(want string) error { return invalid(x.At, "%s expects %s", x.Text, want) }
	if mode == exprPure && !opening && !pureFunction(x.Text) {
		return invalid(x.At, "%s cannot run before the opening match", x.Text)
	}
	if mode == exprGuard && !guardFunction(x.Text) {
		return invalid(x.At, "%s cannot run inside a when guard", x.Text)
	}
	switch x.Text {
	case "Event":
		if opening && keys["timeout"] {
			return invalid(x.At, "opening Event cannot set timeout")
		}
		if inSelect && keys["timeout"] {
			return invalid(x.At, "use case Sleep(...) instead of timeout= inside select")
		}
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
	case "Now", "LocalTime":
		if positional != 0 || len(keys) > 0 {
			return bad("no arguments")
		}
	case "Exists", "Every", "State", "Sleep", "Len", "ParseTime", "Duration", "Abs", "Round":
		if positional != 1 || len(keys) > 0 {
			return bad("one positional argument")
		}
	case "Has", "Min", "Max":
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
	if rule.Mode != "" {
		fmt.Fprintf(&out, "rule.mode = %q\n", rule.Mode)
	}
	if rule.Scope != "" {
		fmt.Fprintf(&out, "rule.scope = %q\n", rule.Scope)
	}
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
		guardSuffix := ""
		if s.Guard != nil {
			guardSuffix = " when " + formatExpr(s.Guard)
		}
		switch s.Kind {
		case "expression":
			line(formatExpr(s.Value) + guardSuffix)
		case "assign":
			line(s.Name + " = " + formatExpr(s.Value) + guardSuffix)
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
			branch := s
			for len(branch.Else) == 1 && branch.Else[0].Kind == "if" {
				branch = branch.Else[0]
				line("elseif " + formatExpr(branch.Value) + ":")
				formatBlock(out, branch.Body, depth+1)
			}
			if len(branch.Else) > 0 {
				line("else:")
				formatBlock(out, branch.Else, depth+1)
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
		case "select":
			line("select:")
			for _, arm := range s.Arms {
				header := "case "
				if arm.Binding != "" {
					header += arm.Binding + " = "
				}
				header += formatExpr(arm.Wait)
				if arm.Guard != nil {
					header += " when " + formatExpr(arm.Guard)
				}
				line("    " + header + ":")
				formatBlock(out, arm.Body, depth+2)
			}
		}
	}
}

func formatExpr(x *Expression) string { return formatExprPrec(x, 0) }

func formatExprPrec(x *Expression, minPrec int) string {
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
		return formatExprPrec(x.Left, 8) + "." + x.Text
	case "index":
		return formatExprPrec(x.Left, 8) + "[" + formatExprPrec(x.Right, 0) + "]"
	case "unary":
		var out string
		prec := 7
		if x.Text == "not" {
			prec = 3
			out = "not " + formatExprPrec(x.Left, 3)
		} else {
			child := formatExprPrec(x.Left, 7)
			if x.Left.Kind == "unary" && x.Left.Text != "not" {
				child = "(" + child + ")"
			}
			out = x.Text + child
		}
		if prec < minPrec {
			return "(" + out + ")"
		}
		return out
	case "binary":
		rank := precedence[x.Text]
		out := formatExprPrec(x.Left, rank) + " " + x.Text + " " + formatExprPrec(x.Right, rank+1)
		if rank < minPrec {
			return "(" + out + ")"
		}
		return out
	}
	args := make([]string, 0, len(x.Arguments))
	for _, a := range x.Arguments {
		item := formatExprPrec(a.Value, 0)
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
