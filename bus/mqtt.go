package bus

/*
#cgo pkg-config: libmosquitto
#include <stdlib.h>
#include <stdint.h>
#include <mosquitto.h>
extern void chainsMQTTMessage(void *, char *, void *, int, int);
extern void chainsMQTTConnect(void *, int);
extern void chainsMQTTSubscribe(void *, int, int);
static void message_callback(struct mosquitto *client, void *id, const struct mosquitto_message *msg) {
 chainsMQTTMessage(id, msg->topic, msg->payload, msg->payloadlen, msg->retain);
}
static void connect_callback(struct mosquitto *client, void *id, int reason) {
 chainsMQTTConnect(id, reason);
}
static void subscribe_callback(struct mosquitto *client, void *id, int mid, int count, const int *granted) {
 chainsMQTTSubscribe(id, mid, count ? granted[0] : 128);
}
extern void chainsMQTTPublished(void *, int, int);
static void published_callback(struct mosquitto *client, void *id, int mid, int reason, const mosquitto_property *props) {
 chainsMQTTPublished(id, mid, reason);
}
extern void chainsMQTTDisconnected(void *);
static void disconnected_callback(struct mosquitto *client, void *id, int reason) {
 chainsMQTTDisconnected(id);
}
static int use_os_certs(struct mosquitto *m) {
#ifdef MOSQ_OPT_TLS_USE_OS_CERTS
 return mosquitto_int_option(m, MOSQ_OPT_TLS_USE_OS_CERTS, 1);
#else
 return mosquitto_tls_set(m, NULL, "/etc/ssl/certs", NULL, NULL, NULL);
#endif
}
static struct mosquitto *new_client(char *id, uintptr_t h) {
 struct mosquitto *m = mosquitto_new(id, true, (void *)h);
 if (m) {
  mosquitto_int_option(m, MOSQ_OPT_PROTOCOL_VERSION, MQTT_PROTOCOL_V5);
  mosquitto_int_option(m, MOSQ_OPT_TCP_NODELAY, 1);
  mosquitto_message_callback_set(m, message_callback);
  mosquitto_publish_v5_callback_set(m, published_callback);
  mosquitto_connect_callback_set(m, connect_callback);
  mosquitto_disconnect_callback_set(m, disconnected_callback);
  mosquitto_subscribe_callback_set(m, subscribe_callback);
  mosquitto_reconnect_delay_set(m, 1, 15, true);
 }
 return m;
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"runtime/cgo"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	chains "chains-automation"
)

type MQTT struct {
	client         *C.struct_mosquitto
	handle         cgo.Handle
	prefix         string
	filters        []string
	output         chan Delivery
	ready          chan error
	pending        int
	connected      bool
	overflow       atomic.Bool
	mu             sync.Mutex
	closed         bool
	ackMu          sync.Mutex
	acks           map[C.int]chan error
	earlyAcks      map[C.int]error
	inFlightAck    atomic.Int32
	done           chan struct{}
	raw            bool
	disconnected   chan struct{}
	disconnectOnce sync.Once
}

var mqttInit = sync.OnceValue(func() C.int { return C.mosquitto_lib_init() })

func mqttError(code C.int) error {
	if code == 0 {
		return nil
	}
	return errors.New(C.GoString(C.mosquitto_strerror(code)))
}

func OpenMQTT(ctx context.Context, address, namespace string, sub Subscription) (*MQTT, error) {
	return openMQTT(ctx, address, namespace, sub, false, "")
}

func OpenRawMQTT(ctx context.Context, address, caFile string, filters ...string) (*MQTT, error) {
	return openMQTT(ctx, address, "", Subscription{Raw: filters}, true, caFile)
}

func openMQTT(ctx context.Context, address, namespace string, sub Subscription, raw bool, caFile string) (*MQTT, error) {
	if err := mqttError(mqttInit()); err != nil {
		return nil, err
	}
	u, err := url.Parse(address)
	if err != nil {
		return nil, errors.New("invalid MQTT URL")
	}
	unix := u.Scheme == "mqtt+unix" && u.Host == "" && strings.HasPrefix(u.Path, "/")
	if !unix && ((u.Scheme != "mqtt" && u.Scheme != "mqtts") || u.Hostname() == "") {
		return nil, errors.New("expected mqtt:// or mqtts:// broker URL")
	}
	port := 1883
	if u.Scheme == "mqtts" {
		port = 8883
	}
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return nil, err
		}
	}
	m := &MQTT{prefix: namespace + "/", output: make(chan Delivery, 1024), ready: make(chan error, 1), acks: make(map[C.int]chan error), earlyAcks: make(map[C.int]error), done: make(chan struct{})}
	if raw {
		m.prefix = ""
		m.raw = true
		m.disconnected = make(chan struct{})
	}
	if !raw && len(sub.Types) == 0 {
		m.filters = append(m.filters, m.prefix+"msg/#")
	}
	for _, kind := range sub.Types {
		m.filters = append(m.filters, m.prefix+"msg/"+kind+"/#")
	}
	m.filters = append(m.filters, sub.Raw...)
	m.handle = cgo.NewHandle(m)
	name := C.CString("chains-" + chains.ID())
	m.client = C.new_client(name, C.uintptr_t(m.handle))
	C.free(unsafe.Pointer(name))
	if m.client == nil {
		m.handle.Delete()
		return nil, errors.New("allocate MQTT client")
	}
	started := false
	defer func() {
		if !started {
			C.mosquitto_destroy(m.client)
			m.handle.Delete()
		}
	}()
	if u.User != nil {
		user := C.CString(u.User.Username())
		password, _ := u.User.Password()
		pass := C.CString(password)
		err := mqttError(C.mosquitto_username_pw_set(m.client, user, pass))
		C.free(unsafe.Pointer(user))
		C.free(unsafe.Pointer(pass))
		if err != nil {
			return nil, err
		}
	}
	if u.Scheme == "mqtts" {
		if caFile != "" {
			trust := C.CString(caFile)
			err = mqttError(C.mosquitto_tls_set(m.client, trust, nil, nil, nil, nil))
			C.free(unsafe.Pointer(trust))
		} else {
			err = mqttError(C.use_os_certs(m.client))
		}
		if err != nil {
			return nil, err
		}
	}
	hostName := u.Hostname()
	if unix {
		hostName, port = u.Path, 0
	} else if _, err := (&net.Resolver{}).LookupHost(ctx, hostName); err != nil {
		return nil, err
	}
	host := C.CString(hostName)
	err = mqttError(C.mosquitto_connect_async(m.client, host, C.int(port), 30))
	C.free(unsafe.Pointer(host))
	if err != nil {
		return nil, err
	}
	if err := mqttError(C.mosquitto_loop_start(m.client)); err != nil {
		return nil, err
	}
	started = true
	select {
	case err := <-m.ready:
		if err != nil {
			m.Close()
		}
		return m, err
	case <-ctx.Done():
		m.Close()
		return nil, ctx.Err()
	}
}

func (m *MQTT) signal(err error) {
	select {
	case m.ready <- err:
	default:
	}
}

//export chainsMQTTDisconnected
func chainsMQTTDisconnected(id unsafe.Pointer) {
	m := cgo.Handle(uintptr(id)).Value().(*MQTT)
	if m.raw {
		m.disconnectOnce.Do(func() { close(m.disconnected) })
	}
}

func (m *MQTT) Disconnected() <-chan struct{} { return m.disconnected }

//export chainsMQTTConnect
func chainsMQTTConnect(id unsafe.Pointer, reason C.int) {
	m := cgo.Handle(uintptr(id)).Value().(*MQTT)
	if reason != 0 {
		m.signal(fmt.Errorf("MQTT connection rejected: %d", reason))
		return
	}
	m.pending = len(m.filters)
	for _, filter := range m.filters {
		topic := C.CString(filter)
		err := mqttError(C.mosquitto_subscribe(m.client, nil, topic, 1))
		C.free(unsafe.Pointer(topic))
		if err != nil {
			m.signal(err)
			return
		}
	}
	if m.pending == 0 {
		m.subscribed()
	}
}

//export chainsMQTTSubscribe
func chainsMQTTSubscribe(id unsafe.Pointer, mid, reason C.int) {
	m := cgo.Handle(uintptr(id)).Value().(*MQTT)
	if reason >= 128 {
		m.signal(fmt.Errorf("MQTT subscription rejected: %d", reason))
		return
	}
	m.pending--
	if m.pending == 0 {
		m.subscribed()
	}
}

func (m *MQTT) subscribed() {
	if m.connected {
		m.deliver(Delivery{Reset: true})
	}
	m.connected = true
	m.signal(nil)
}

//export chainsMQTTMessage
func chainsMQTTMessage(id unsafe.Pointer, topic *C.char, payload unsafe.Pointer, n, retained C.int) {
	m := cgo.Handle(uintptr(id)).Value().(*MQTT)
	if n < 0 || n > MaxPayload {
		return
	}
	name := C.GoString(topic)
	if m.prefix != "" && strings.HasPrefix(name, m.prefix+"msg/") {
		name = strings.TrimPrefix(name, m.prefix)
	}
	m.deliver(Delivery{Topic: name, Data: C.GoBytes(payload, n), Retained: retained != 0})
}

func (m *MQTT) deliver(d Delivery) {
	if m.overflow.Swap(false) {
		select {
		case m.output <- Delivery{Reset: true}:
		default:
			m.overflow.Store(true)
			return
		}
	}
	select {
	case m.output <- d:
	default:
		m.overflow.Store(true)
	}
}

//export chainsMQTTPublished
func chainsMQTTPublished(id unsafe.Pointer, mid, reason C.int) {
	m := cgo.Handle(uintptr(id)).Value().(*MQTT)
	var err error
	if reason >= 128 {
		err = fmt.Errorf("MQTT publish rejected: %d", reason)
	}
	m.ackMu.Lock()
	defer m.ackMu.Unlock()
	if result := m.acks[mid]; result != nil {
		result <- err
		delete(m.acks, mid)
	} else if m.inFlightAck.Load() > 0 {
		m.earlyAcks[mid] = err
	}
}

func (m *MQTT) Publish(ctx context.Context, topic string, data []byte) error {
	_, err := m.publish(ctx, topic, data, false, nil)
	return err
}

// PublishAcknowledged waits for the QoS 1 broker acknowledgement.
func (m *MQTT) PublishAcknowledged(ctx context.Context, topic string, data []byte, retain bool) error {
	result := make(chan error, 1)
	m.inFlightAck.Add(1)
	mid, err := m.publish(ctx, topic, data, retain, result)
	defer func() {
		m.ackMu.Lock()
		if err == nil && m.acks[mid] == result {
			delete(m.acks, mid)
		}
		if m.inFlightAck.Add(-1) == 0 {
			clear(m.earlyAcks)
		}
		m.ackMu.Unlock()
	}()
	if err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return net.ErrClosed
	case <-m.disconnected:
		return errors.New("MQTT disconnected")
	}
}

func (m *MQTT) publish(ctx context.Context, topic string, data []byte, retain bool, result chan error) (C.int, error) {
	if len(data) > MaxPayload {
		return 0, ErrTooLarge
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, net.ErrClosed
	}
	select {
	case <-m.disconnected:
		return 0, errors.New("MQTT disconnected")
	default:
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	name := C.CString(m.prefix + topic)
	defer C.free(unsafe.Pointer(name))
	var p unsafe.Pointer
	if len(data) > 0 {
		p = unsafe.Pointer(&data[0])
	}
	var mid C.int
	err := mqttError(C.mosquitto_publish(m.client, &mid, name, C.int(len(data)), p, 1, C.bool(retain)))
	if err == nil && result != nil {
		m.ackMu.Lock()
		if ackErr, early := m.earlyAcks[mid]; early {
			delete(m.earlyAcks, mid)
			result <- ackErr
		} else {
			m.acks[mid] = result
		}
		m.ackMu.Unlock()
	}
	return mid, err
}

func (m *MQTT) Receive() <-chan Delivery { return m.output }
func (m *MQTT) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	close(m.done)
	C.mosquitto_disconnect(m.client)
	C.mosquitto_loop_stop(m.client, true)
	C.mosquitto_destroy(m.client)
	m.handle.Delete()
	close(m.output)
	return nil
}
