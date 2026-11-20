// Package keyboard publishes Linux input transitions as Chains events.
package keyboard

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"time"

	chains "chains-automation"
	"chains-automation/sdk"
)

const Event = "keyboard.key"

type Settings struct {
	Device            string  `toml:"device" required:"true" help:"Explicit Linux keyboard event device path"`
	Grab              bool    `toml:"grab" help:"Reserve the keyboard exclusively while connected"`
	ReconnectInterval float64 `toml:"reconnect_interval" help:"Seconds between attempts to reopen the device"`
}

func DefaultSettings() Settings { return Settings{ReconnectInterval: 2} }

type Key struct {
	Code uint16
	Name string
}
type Input struct {
	Key    Key
	Value  int32
	Resync bool
}

type Device interface {
	Held() []Key
	Next(context.Context) (Input, error)
	Close() error
}

type openDevice func(string, bool) (Device, error)

type Transition struct {
	Action string   `json:"action"`
	Code   uint16   `json:"code"`
	Key    string   `json:"key"`
	Keys   []string `json:"keys"`
	Mods   []string `json:"mods"`
}

var modifiers = map[string]bool{
	"KEY_LEFTCTRL": true, "KEY_RIGHTCTRL": true, "KEY_LEFTSHIFT": true, "KEY_RIGHTSHIFT": true,
	"KEY_LEFTALT": true, "KEY_RIGHTALT": true, "KEY_LEFTMETA": true, "KEY_RIGHTMETA": true,
}

type heldKeys map[uint16]string

func snapshot(keys []Key) heldKeys {
	state := heldKeys{}
	for _, k := range keys {
		if k.Name != "" {
			state[k.Code] = k.Name
		}
	}
	return state
}

func (held heldKeys) apply(input Input) (Transition, bool) {
	k := input.Key
	if k.Name == "" || input.Value < 0 || input.Value > 1 {
		return Transition{}, false
	}
	_, down := held[k.Code]
	pressed := input.Value == 1
	if pressed == down {
		return Transition{}, false
	}
	t := Transition{Action: "release", Code: k.Code, Key: k.Name, Keys: []string{}, Mods: []string{}}
	if pressed {
		held[k.Code] = k.Name
		t.Action = "press"
	} else {
		delete(held, k.Code)
	}
	var codes []int
	for code := range held {
		codes = append(codes, int(code))
	}
	sort.Ints(codes)
	for _, code := range codes {
		name := held[uint16(code)]
		if modifiers[name] {
			t.Mods = append(t.Mods, name)
		} else {
			t.Keys = append(t.Keys, name)
		}
	}
	return t, true
}

func Definition(settings Settings) sdk.Definition { return definition(settings, openEvdev) }

func definition(settings Settings, open openDevice) sdk.Definition {
	return sdk.Definition{
		Service: chains.Service{ServiceType: "keyboard", Devices: []chains.Device{{ID: "default", Class: "keyboard", Name: "USB keyboard", Attributes: []string{"last_key"}}},
			Events: map[string]map[string]json.RawMessage{
				Event: {"1": json.RawMessage(`{"type":"object","required":["action","code","key","keys","mods"],"properties":{"action":{"enum":["press","release"]},"code":{"type":"integer"},"key":{"type":"string"},"keys":{"type":"array","items":{"type":"string"}},"mods":{"type":"array","items":{"type":"string"}}},"additionalProperties":false}`)},
			},
		},
		Hooks: sdk.Hooks{
			OnInit: func(_ context.Context, _ *sdk.Runtime) error {
				if !filepath.IsAbs(settings.Device) {
					return errors.New("keyboard requires an absolute device path")
				}
				d := settings.ReconnectInterval
				if math.IsNaN(d) || d <= 0 || d >= float64(math.MaxInt64)/float64(time.Second) {
					return errors.New("invalid keyboard reconnect interval")
				}
				return nil
			},
			OnStart: func(_ context.Context, r *sdk.Runtime) error {
				r.Go(func(ctx context.Context) error {
					run(ctx, settings, open, func(t Transition) error { return r.Event(ctx, Event, "", "1", t, map[string]any{"last_key": t}) })
					return nil
				})
				return nil
			},
		},
	}
}

func run(ctx context.Context, settings Settings, open openDevice, publish func(Transition) error) {
	for ctx.Err() == nil {
		device, err := open(settings.Device, settings.Grab)
		if err == nil {
			slog.Info("keyboard connected", "device", settings.Device)
			err = read(ctx, device, publish)
			err = errors.Join(err, device.Close())
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("keyboard unavailable; retrying", "device", settings.Device, "error", err)
		}
		timer := time.NewTimer(max(time.Nanosecond, time.Duration(settings.ReconnectInterval*float64(time.Second))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func read(ctx context.Context, device Device, publish func(Transition) error) error {
	held := snapshot(device.Held())
	for ctx.Err() == nil {
		change, err := device.Next(ctx)
		if err != nil {
			return err
		}
		if change.Resync {
			held = snapshot(device.Held())
			continue
		}
		if t, ok := held.apply(change); ok && ctx.Err() == nil {
			if err := publish(t); err != nil && ctx.Err() == nil {
				slog.Warn("keyboard event publication failed", "error", err)
			}
		}
	}
	return ctx.Err()
}
