// Package z2m translates Zigbee2MQTT discovery, observations, and commands.
package z2m

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	chains "chains-automation"
	"chains-automation/bus"
)

const ActionEvent = "zigbee2mqtt.action"

type Config struct {
	Enabled     bool   `json:"enabled"`
	MQTTURL     string `json:"mqtt_url"`
	BaseTopic   string `json:"base_topic"`
	ServiceID   string `json:"service_id"`
	ServiceName string `json:"service_name"`
	CAFile      string `json:"ca_file"`
}

func DefaultConfig() Config {
	return Config{BaseTopic: "zigbee2mqtt", ServiceID: "zigbee2mqtt", ServiceName: "Zigbee2MQTT"}
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	u, err := url.Parse(c.MQTTURL)
	if err != nil || (u.Scheme != "mqtt" && u.Scheme != "mqtts") || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("z2m mqtt_url must be a valid mqtt:// or mqtts:// broker URL")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("invalid z2m MQTT port")
		}
	}
	if c.CAFile != "" {
		if u.Scheme != "mqtts" {
			return errors.New("z2m ca_file requires mqtts://")
		}
		file, err := os.Open(c.CAFile)
		if err != nil {
			return fmt.Errorf("z2m ca_file: %w", err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("z2m ca_file must be a readable file")
		}
	}
	if c.BaseTopic == "" || strings.ContainsAny(c.BaseTopic, "+#\x00") || strings.Trim(c.BaseTopic, "/") != c.BaseTopic {
		return errors.New("invalid z2m base_topic")
	}
	if !chains.Segment(c.ServiceID) || strings.TrimSpace(c.ServiceName) == "" {
		return errors.New("invalid z2m service identity")
	}
	return nil
}

type Bridge struct {
	config  Config
	log     *slog.Logger
	mu      sync.RWMutex
	mqtt    *bus.MQTT
	devices map[string]device
	online  bool
}

func New(config Config, log *slog.Logger) *Bridge {
	if log == nil {
		log = slog.Default()
	}
	return &Bridge{config: config, log: log}
}

func (b *Bridge) ID() string { return b.config.ServiceID }

type submitFunc = func(context.Context, chains.Message, bool) error

func (b *Bridge) Run(ctx context.Context, submit submitFunc) {
	for ctx.Err() == nil {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		mqtt, err := bus.OpenRawMQTT(attempt, b.config.MQTTURL, b.config.CAFile, b.config.BaseTopic+"/#")
		cancel()
		if err == nil {
			b.mu.Lock()
			b.mqtt = mqtt
			b.online = true
			b.mu.Unlock()
			b.log.Info("Zigbee2MQTT broker connected", "service", b.ID())
			b.consume(ctx, mqtt, submit)
			b.mu.Lock()
			b.mqtt = nil
			b.online = false
			b.devices = nil
			b.mu.Unlock()
			_ = mqtt.Close()
			if ctx.Err() == nil {
				b.log.Warn("Zigbee2MQTT broker disconnected", "service", b.ID())
			}
		} else if ctx.Err() == nil {
			b.log.Warn("Zigbee2MQTT broker offline", "service", b.ID(), "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *Bridge) consume(ctx context.Context, mqtt *bus.MQTT, submit submitFunc) {
	byName := map[string]device{}
	inventory := b.config.BaseTopic + "/bridge/devices"
	status := b.config.BaseTopic + "/bridge/state"
	var buffered []bus.Delivery
	discovered := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-mqtt.Disconnected():
			return
		case d, ok := <-mqtt.Receive():
			if !ok {
				return
			}
			if d.Reset {
				// A gap can lose both discovery and observations. Resubscribe for retained state.
				return
			}
			if d.Topic == inventory {
				devices, err := b.discover(ctx, d.Data, submit)
				if err != nil {
					b.log.Warn("Zigbee2MQTT discovery rejected", "error", err)
					continue
				}
				byName, discovered = devices, true
				for _, seed := range buffered {
					b.observe(ctx, byName, seed, submit)
				}
				buffered = nil
				continue
			}
			if d.Topic == status {
				var state struct{ State string }
				if json.Unmarshal(d.Data, &state) == nil {
					b.mu.Lock()
					b.online = state.State == "online"
					b.mu.Unlock()
				}
				continue
			}
			if !discovered {
				if len(buffered) < 256 {
					buffered = append(buffered, d)
				}
				continue
			}
			b.observe(ctx, byName, d, submit)
		}
	}
}

func (b *Bridge) observe(ctx context.Context, devices map[string]device, d bus.Delivery, submit submitFunc) {
	name, ok := strings.CutPrefix(d.Topic, b.config.BaseTopic+"/")
	dev, known := devices[name]
	if !ok || !known {
		return
	}
	messages, err := b.translate(dev, d.Data, d.Retained)
	if err != nil {
		b.log.Debug("invalid Zigbee2MQTT observation", "device", dev.ID, "error", err)
		return
	}
	for _, m := range messages {
		if err = submit(ctx, m, d.Retained); err != nil {
			b.log.Warn("Zigbee2MQTT observation rejected", "device", dev.ID, "error", err)
		}
	}
}

func (b *Bridge) Execute(ctx context.Context, id string, action chains.Action) error {
	b.mu.RLock()
	mqtt, dev, online := b.mqtt, b.devices[id], b.online
	b.mu.RUnlock()
	if mqtt == nil || !online {
		return errors.New("Zigbee2MQTT is offline")
	}
	if dev.ID == "" {
		return errors.New("Zigbee2MQTT device unavailable")
	}
	payload, err := dev.command(action)
	if err != nil {
		return err
	}
	return mqtt.PublishAcknowledged(ctx, b.config.BaseTopic+"/"+dev.Name+"/set", payload, false)
}
