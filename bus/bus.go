// Package bus adapts protocol messages to independently selectable transports.
package bus

import (
	"context"
	"errors"
	"strings"

	chains "chains-automation"
)

type Subscription struct {
	// An empty Types list selects all message types.
	Types []string
	// Raw contains MQTT source topic filters for a sensor integration.
	Raw []string
}

type Delivery struct {
	Topic    string
	Data     []byte
	Retained bool
	// Reset marks loss of continuity after a reconnect or queue overflow.
	Reset bool
}

type Bus interface {
	Publish(context.Context, string, []byte) error
	Receive() <-chan Delivery
	Close() error
}

func Route(m chains.Message) string {
	return strings.Join([]string{"msg", m.Type, m.Source, target(m.Target)}, "/")
}

func target(s string) string {
	if s == "" {
		return "_"
	}
	return s
}

func Match(topic string, m chains.Message) bool {
	return topic == Route(m)
}

const MaxPayload = 1 << 20

var ErrTooLarge = errors.New("message exceeds maximum payload")
