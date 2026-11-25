package rules

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Position identifies a source location. Generated nodes may omit it.
type Position struct{ Line, Column int }

type Rule struct {
	Name        string
	Concurrency int
	Timeout     time.Duration
	Mode        string // "", "drop", or "restart"
	Body        []Statement
}

type Statement struct {
	At         Position
	Kind       string // expression, assign, if, while, for, switch, break, continue, return
	Name       string
	Value      *Expression
	Body, Else []Statement
	Cases      []Case
}

type Case struct {
	Values []*Expression // empty for default
	Body   []Statement
}

type Expression struct {
	At          Position
	Kind        string // literal, name, call, field, index, unary, binary, list, object
	Text        string
	Literal     any
	Left, Right *Expression
	Arguments   []Argument
}

type Argument struct {
	Name  string
	Value *Expression
}

type lexeme struct {
	kind, text string
	at         Position
}

type syntaxError struct{ error }

func fail(at Position, format string, args ...any) {
	panic(syntaxError{fmt.Errorf("%d:%d: %s", at.Line, at.Column, fmt.Sprintf(format, args...))})
}

func identified(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func digit(c byte) bool      { return c >= '0' && c <= '9' }

func lex(source string) []lexeme {
	var tokens []lexeme
	indents := []int{0}
	var grouping []byte
	lines := strings.Split(source, "\n")
	emit := func(kind, word string, row, col int) { tokens = append(tokens, lexeme{kind, word, Position{row, col}}) }
	for row, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		row++
		i := 0
		for i < len(line) && line[i] == ' ' {
			i++
		}
		if i == len(line) || line[i] == '#' {
			continue
		}
		if len(grouping) == 0 {
			if line[i] == '\t' {
				fail(Position{row, i + 1}, "indent with spaces")
			}
			if i > indents[len(indents)-1] {
				indents = append(indents, i)
				emit("indent", "", row, i+1)
			}
			for i < indents[len(indents)-1] {
				indents = indents[:len(indents)-1]
				emit("dedent", "", row, i+1)
			}
			if i != indents[len(indents)-1] {
				fail(Position{row, i + 1}, "inconsistent indentation")
			}
		}
		for i < len(line) {
			c, start := line[i], i
			i++
			switch {
			case c == ' ' || c == '\t':
				continue
			case c == '#':
				i = len(line)
			case identified(c):
				for i < len(line) && (identified(line[i]) || digit(line[i])) {
					i++
				}
				emit("name", line[start:i], row, start+1)
			case digit(c):
				for i < len(line) && digit(line[i]) {
					i++
				}
				if i+1 < len(line) && line[i] == '.' && digit(line[i+1]) {
					i++
					for i < len(line) && digit(line[i]) {
						i++
					}
				}
				if i < len(line) && (line[i] == 'e' || line[i] == 'E') {
					i++
					if i < len(line) && (line[i] == '+' || line[i] == '-') {
						i++
					}
					for i < len(line) && digit(line[i]) {
						i++
					}
				}
				emit("number", line[start:i], row, start+1)
			case c == '\'' || c == '"':
				var value strings.Builder
				for i < len(line) && line[i] != c {
					if line[i] != '\\' {
						value.WriteByte(line[i])
						i++
						continue
					}
					if i+1 < len(line) && (line[i+1] == '\'' || line[i+1] == '"') {
						value.WriteByte(line[i+1])
						i += 2
						continue
					}
					r, multi, rest, err := strconv.UnquoteChar(line[i:], 0)
					if err != nil {
						fail(Position{row, i + 1}, "invalid string escape")
					}
					if multi {
						value.WriteRune(r)
					} else {
						value.WriteByte(byte(r))
					}
					i = len(line) - len(rest)
				}
				if i == len(line) {
					fail(Position{row, start + 1}, "unterminated string")
				}
				i++
				emit("string", value.String(), row, start+1)
			case strings.ContainsRune("([{", rune(c)):
				grouping = append(grouping, c)
				emit(string(c), "", row, start+1)
			case strings.ContainsRune(")]}", rune(c)):
				if len(grouping) == 0 || strings.IndexByte("([{", grouping[len(grouping)-1]) != strings.IndexByte(")]}", c) {
					fail(Position{row, start + 1}, "unmatched %c", c)
				}
				grouping = grouping[:len(grouping)-1]
				emit(string(c), "", row, start+1)
			case strings.ContainsRune("=<>!+-*/%:.,", rune(c)):
				if i < len(line) && line[i] == '=' && strings.ContainsRune("=<>!", rune(c)) {
					i++
				}
				emit(line[start:i], "", row, start+1)
			default:
				fail(Position{row, start + 1}, "unexpected character %q", c)
			}
		}
		if len(grouping) == 0 {
			emit("newline", "", row, len(line)+1)
		}
	}
	if len(grouping) > 0 {
		fail(Position{len(lines), 1}, "unclosed delimiter")
	}
	for range indents[1:] {
		emit("dedent", "", len(lines), 1)
	}
	emit("eof", "", len(lines), 1)
	return tokens
}

type parser struct {
	tokens []lexeme
	index  int
	depth  int
}

func (p *parser) peek() lexeme { return p.tokens[p.index] }
func (p *parser) take() lexeme {
	t := p.peek()
	if t.kind != "eof" {
		p.index++
	}
	return t
}
func (p *parser) is(s string) bool {
	t := p.peek()
	if t.kind == s {
		return true
	}
	switch s {
	case "rule", "in", "case", "default", "elseif", "else":
		return t.kind == "name" && t.text == s
	}
	return false
}
func (p *parser) accept(s string) bool {
	if p.is(s) {
		p.take()
		return true
	}
	return false
}
func (p *parser) expect(s string) lexeme {
	if !p.is(s) {
		fail(p.peek().at, "expected %s", s)
	}
	return p.take()
}

// Parse reads one rule. The caller supplies its name, typically the filename stem.
func Parse(name, source string) (Rule, error) {
	return parse(name, source, nil)
}

// Bind formats source with additional assignments after rule settings and before the body.
func Bind(name, source string, assignments []Statement) (string, error) {
	rule, err := parse(name, source, assignments)
	if err != nil {
		return "", err
	}
	return Format(rule)
}

func parse(name, source string, assignments []Statement) (rule Rule, err error) {
	defer func() {
		if v := recover(); v != nil {
			if known, ok := v.(syntaxError); ok {
				err = known.error
			} else {
				panic(v)
			}
		}
	}()
	p := parser{tokens: lex(source)}
	rule = Rule{Name: name}
	settings := map[string]bool{}
	for p.is("rule") {
		p.take()
		p.expect(".")
		setting := p.expect("name")
		p.expect("=")
		if settings[setting.text] {
			fail(setting.at, "invalid or duplicate rule setting %q", setting.text)
		}
		settings[setting.text] = true
		switch setting.text {
		case "mode":
			value := p.expect("string")
			if value.text != "drop" && value.text != "restart" {
				fail(value.at, "rule.mode must be \"drop\" or \"restart\"")
			}
			rule.Mode = value.text
		case "concurrency":
			value := p.expect("number")
			n, e := strconv.ParseFloat(value.text, 64)
			if e != nil || n <= 0 || n > 1024 || math.Trunc(n) != n {
				fail(value.at, "concurrency must be an integer from 1 to 1024")
			}
			rule.Concurrency = int(n)
		case "timeout":
			value := p.expect("number")
			n, e := strconv.ParseFloat(value.text, 64)
			d, ok := secondsDuration(n)
			if e != nil || n <= 0 || !ok || d < 1 {
				fail(value.at, "timeout is outside the supported range")
			}
			rule.Timeout = d
		default:
			fail(setting.at, "unknown rule setting %q", setting.text)
		}
		p.expect("newline")
	}
	rule.Body = append(append([]Statement(nil), assignments...), p.block("eof")...)
	err = Validate(rule)
	return
}

func (p *parser) enter() func() {
	p.depth++
	if p.depth > 128 {
		fail(p.peek().at, "nesting limit exceeded")
	}
	return func() { p.depth-- }
}

func (p *parser) block(end string) []Statement {
	defer p.enter()()
	var body []Statement
	for !p.is(end) && !p.is("eof") {
		body = append(body, p.statement())
	}
	if end != "eof" {
		p.expect(end)
	}
	return body
}

func (p *parser) suite() []Statement {
	p.expect(":")
	p.expect("newline")
	p.expect("indent")
	return p.block("dedent")
}

func (p *parser) statement() Statement {
	t := p.peek()
	if t.kind == "name" {
		switch t.text {
		case "if":
			p.take()
			return p.branch(t)
		case "while":
			p.take()
			value := p.expression(1)
			return Statement{At: t.at, Kind: "while", Value: value, Body: p.suite()}
		case "for":
			p.take()
			name := p.expect("name")
			p.expect("in")
			value := p.expression(1)
			return Statement{At: t.at, Kind: "for", Name: name.text, Value: value, Body: p.suite()}
		case "switch":
			p.take()
			value := p.expression(1)
			p.expect(":")
			p.expect("newline")
			p.expect("indent")
			var cases []Case
			for !p.is("dedent") {
				var values []*Expression
				if !p.accept("default") {
					p.expect("case")
					for {
						values = append(values, p.expression(1))
						if !p.accept(",") {
							break
						}
					}
				}
				cases = append(cases, Case{Values: values, Body: p.suite()})
			}
			p.expect("dedent")
			return Statement{At: t.at, Kind: "switch", Value: value, Cases: cases}
		case "return", "break", "continue":
			p.take()
			p.expect("newline")
			return Statement{At: t.at, Kind: t.text}
		}
		if p.tokens[p.index+1].kind == "=" {
			p.take()
			p.take()
			value := p.expression(1)
			p.expect("newline")
			return Statement{At: t.at, Kind: "assign", Name: t.text, Value: value}
		}
	}
	value := p.expression(1)
	p.expect("newline")
	return Statement{At: t.at, Kind: "expression", Value: value}
}

func (p *parser) branch(t lexeme) Statement {
	defer p.enter()()
	s := Statement{At: t.at, Kind: "if", Value: p.expression(1), Body: p.suite()}
	if p.is("elseif") {
		s.Else = []Statement{p.branch(p.take())}
	} else if p.accept("else") {
		s.Else = p.suite()
	}
	return s
}

var precedence = map[string]int{"or": 1, "and": 2, "==": 3, "!=": 3, "<": 4, "<=": 4, ">": 4, ">=": 4, "in": 4, "not in": 4, "+": 5, "-": 5, "*": 6, "/": 6, "%": 6}

// maxDurationSeconds bounds float64-to-time.Duration conversions safely below 2^63 ns.
const maxDurationSeconds = 9e9

func secondsDuration(seconds float64) (time.Duration, bool) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > maxDurationSeconds {
		return 0, false
	}
	return time.Duration(math.Round(seconds * float64(time.Second))), true
}

func (p *parser) expression(minimum int) *Expression {
	defer p.enter()()
	t := p.take()
	x := &Expression{At: t.at}
	switch t.kind {
	case "number":
		value, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			fail(t.at, "invalid number")
		}
		x.Kind, x.Literal = "literal", value
	case "string":
		x.Kind, x.Literal = "literal", t.text
	case "name":
		switch t.text {
		case "true", "false":
			x.Kind, x.Literal = "literal", t.text == "true"
		case "null":
			x.Kind = "literal"
		case "not":
			x.Kind, x.Text, x.Left = "unary", "not", p.expression(3)
		default:
			x.Kind, x.Text = "name", t.text
		}
	case "+", "-":
		x.Kind, x.Text, x.Left = "unary", t.kind, p.expression(7)
	case "(":
		x = p.expression(1)
		p.expect(")")
	case "[":
		x.Kind, x.Arguments = "list", p.arguments("]", "list")
	case "{":
		x.Kind, x.Arguments = "object", p.arguments("}", "object")
	default:
		fail(t.at, "expected expression")
	}
	for {
		next := p.peek()
		switch next.kind {
		case "(":
			if x.Kind != "name" {
				fail(next.at, "expected function name")
			}
			p.take()
			x = &Expression{At: x.At, Kind: "call", Text: x.Text, Arguments: p.arguments(")", "call")}
			continue
		case "[":
			p.take()
			key := p.expression(1)
			p.expect("]")
			x = &Expression{At: x.At, Kind: "index", Left: x, Right: key}
			continue
		case ".":
			p.take()
			field := p.expect("name")
			x = &Expression{At: x.At, Kind: "field", Left: x, Text: field.text}
			continue
		}
		op := next.kind
		twoTokens := false
		if op == "name" {
			op = next.text
			if op == "not" && p.index+1 < len(p.tokens) && p.tokens[p.index+1].kind == "name" && p.tokens[p.index+1].text == "in" {
				op = "not in"
				twoTokens = true
			}
		}
		rank := precedence[op]
		if rank < minimum {
			return x
		}
		p.take()
		if twoTokens {
			p.take()
		}
		x = &Expression{At: x.At, Kind: "binary", Text: op, Left: x, Right: p.expression(rank + 1)}
	}
}

func (p *parser) arguments(end, kind string) []Argument {
	var args []Argument
	for !p.accept(end) {
		var name string
		if kind == "object" {
			key := p.take()
			if key.kind != "name" && key.kind != "string" {
				fail(key.at, "expected object key")
			}
			name = key.text
			p.expect(":")
		} else if kind == "call" && p.peek().kind == "name" && p.tokens[p.index+1].kind == "=" {
			name = p.take().text
			p.take()
		}
		args = append(args, Argument{Name: name, Value: p.expression(1)})
		if !p.accept(",") {
			p.expect(end)
			break
		}
	}
	return args
}
