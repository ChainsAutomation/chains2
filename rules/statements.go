package rules

import (
	"errors"
	"fmt"
)

var (
	errBreak    = errors.New("break")
	errContinue = errors.New("continue")
	errReturn   = errors.New("return")
)

func (e *execution) block(body []Statement) error {
	for _, s := range body {
		if err := e.statement(s); err != nil {
			return err
		}
	}
	return nil
}

func (e *execution) statement(s Statement) error {
	if err := e.tick(); err != nil {
		return err
	}
	switch s.Kind {
	case "break":
		return errBreak
	case "continue":
		return errContinue
	case "return":
		return errReturn
	case "while":
		return e.loop(s)
	case "select":
		return e.execSelect(s)
	}
	var value any
	var err error
	if s.Guard != nil && s.Value != nil && s.Value.Kind == "call" && s.Value.Text == "Event" {
		binding := ""
		if s.Kind == "assign" {
			binding = s.Name
		}
		value, err = e.callEvent(s.Value, binding, s.Guard)
	} else {
		value, err = e.expr(s.Value)
	}
	if err != nil {
		return fmt.Errorf("%d:%d: %w", s.At.Line, s.At.Column, err)
	}
	switch s.Kind {
	case "assign":
		e.env[s.Name] = value
	case "expression":
	case "if":
		condition, ok := value.(bool)
		if !ok {
			return invalid(s.At, "condition must be boolean")
		}
		if condition {
			return e.block(s.Body)
		}
		return e.block(s.Else)
	case "for":
		var items []any
		switch list := value.(type) {
		case []any:
			items = list
		case []string:
			items = make([]any, len(list))
			for i, item := range list {
				items[i] = item
			}
		default:
			return invalid(s.At, "for expects a list")
		}
		prev, hadPrev := e.env[s.Name]
		defer func() {
			if hadPrev {
				e.env[s.Name] = prev
			} else {
				delete(e.env, s.Name)
			}
		}()
		for _, item := range items {
			e.env[s.Name] = item
			done, err := e.iteration(s.Body)
			if done {
				return err
			}
		}
	case "switch":
		var fallback []Statement
		for _, clause := range s.Cases {
			if len(clause.Values) == 0 {
				fallback = clause.Body
				continue
			}
			for _, match := range clause.Values {
				candidate, err := e.expr(match)
				if err != nil {
					return err
				}
				if e.equalValues(value, candidate) {
					return e.caseBody(clause.Body)
				}
			}
		}
		return e.caseBody(fallback)
	default:
		return invalid(s.At, "unsupported statement %q", s.Kind)
	}
	return nil
}

func (e *execution) loop(s Statement) error {
	for {
		value, err := e.expr(s.Value)
		if err != nil {
			return err
		}
		condition, ok := value.(bool)
		if !ok {
			return invalid(s.At, "condition must be boolean")
		}
		if !condition {
			return nil
		}
		done, err := e.iteration(s.Body)
		if done {
			return err
		}
	}
}

func (e *execution) iteration(body []Statement) (bool, error) {
	switch err := e.block(body); {
	case errors.Is(err, errBreak):
		return true, nil
	case err == nil || errors.Is(err, errContinue):
		return false, nil
	default:
		return true, err
	}
}

func (e *execution) caseBody(body []Statement) error {
	err := e.block(body)
	if errors.Is(err, errBreak) {
		return nil
	}
	return err
}
