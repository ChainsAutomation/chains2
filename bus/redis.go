package bus

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Redis struct {
	url    *url.URL
	prefix string
	keys   []string
	output chan Delivery
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	writer *redisConn
	closed bool
}

type redisConn struct {
	net.Conn
	reader *bufio.Reader
}

func OpenRedis(ctx context.Context, address, namespace string, sub Subscription) (*Redis, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" {
		return nil, errors.New("expected redis:// or rediss:// broker URL")
	}
	if len(sub.Raw) != 0 {
		return nil, errors.New("raw topics require MQTT")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	r := &Redis{url: u, prefix: namespace + ":", output: make(chan Delivery, 1024), ctx: lifetime, cancel: cancel}
	if len(sub.Types) == 0 {
		r.keys = []string{r.prefix + "all"}
	}
	for _, kind := range sub.Types {
		r.keys = append(r.keys, r.prefix+"type:"+kind)
	}
	c, err := r.connect(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	cursor, err := r.heads(c)
	if err != nil {
		c.Close()
		cancel()
		return nil, err
	}
	r.wg.Add(1)
	go r.read(c, cursor)
	return r, nil
}

func (r *Redis) connect(ctx context.Context) (*redisConn, error) {
	port := r.url.Port()
	if port == "" {
		port = "6379"
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	var dial func(context.Context, string, string) (net.Conn, error) = d.DialContext
	if r.url.Scheme == "rediss" {
		td := tls.Dialer{NetDialer: &d, Config: &tls.Config{MinVersion: tls.VersionTLS12}}
		dial = td.DialContext
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(r.url.Hostname(), port))
	if err != nil {
		return nil, err
	}
	c := &redisConn{Conn: conn, reader: bufio.NewReader(conn)}
	if r.url.User != nil {
		args := []string{"AUTH"}
		if r.url.User.Username() != "" {
			args = append(args, r.url.User.Username())
		}
		password, _ := r.url.User.Password()
		args = append(args, password)
		if _, err = c.command(args...); err != nil {
			c.Close()
			return nil, err
		}
	}
	if database := strings.TrimPrefix(r.url.Path, "/"); database != "" {
		if _, err = c.command("SELECT", database); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

func (c *redisConn) command(args ...string) (any, error) {
	if err := c.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		return nil, err
	}
	var body strings.Builder
	fmt.Fprintf(&body, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&body, "$%d\r\n%s\r\n", len(arg), arg)
	}
	if _, err := io.WriteString(c, body.String()); err != nil {
		return nil, err
	}
	return c.response(0)
}

func (c *redisConn) response(depth int) (any, error) {
	if depth > 12 {
		return nil, errors.New("Redis reply nested too deeply")
	}
	line, err := c.reader.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 || line[len(line)-2] != '\r' {
		return nil, errors.New("invalid Redis reply")
	}
	value := string(line[1 : len(line)-2])
	switch line[0] {
	case '+', ':':
		return value, nil
	case '-':
		return nil, errors.New(value)
	case '$', '*':
		size, err := strconv.Atoi(value)
		if err != nil || size < -1 || size > MaxPayload {
			return nil, errors.New("invalid Redis reply length")
		}
		if size == -1 {
			return nil, nil
		}
		if line[0] == '$' {
			b := make([]byte, size+2)
			if _, err = io.ReadFull(c.reader, b); err != nil {
				return nil, err
			}
			if string(b[size:]) != "\r\n" {
				return nil, errors.New("invalid Redis bulk terminator")
			}
			return string(b[:size]), nil
		}
		if size > 4096 {
			return nil, errors.New("Redis response has too many elements")
		}
		result := make([]any, size)
		for i := range result {
			result[i], err = c.response(depth + 1)
			if err != nil {
				return nil, err
			}
		}
		return result, nil
	default:
		return nil, errors.New("unsupported Redis response")
	}
}

func array(v any) []any { a, _ := v.([]any); return a }
func str(v any) string  { s, _ := v.(string); return s }

func (r *Redis) heads(c *redisConn) (map[string]string, error) {
	v, err := c.command("XREVRANGE", r.prefix+"all", "+", "-", "COUNT", "1")
	if err != nil {
		return nil, err
	}
	head := "0-0"
	if a := array(v); len(a) == 1 {
		if entry := array(a[0]); len(entry) == 2 {
			head = str(entry[0])
		}
	}
	result := make(map[string]string, len(r.keys))
	for _, key := range r.keys {
		result[key] = head
	}
	return result, nil
}

type streamID struct{ milliseconds, sequence uint64 }

func parseStreamID(s string) streamID {
	ms, seq, _ := strings.Cut(s, "-")
	a, _ := strconv.ParseUint(ms, 10, 64)
	b, _ := strconv.ParseUint(seq, 10, 64)
	return streamID{a, b}
}

func (a streamID) less(b streamID) bool {
	return a.milliseconds < b.milliseconds || (a.milliseconds == b.milliseconds && a.sequence < b.sequence)
}

type streamEntry struct {
	key    string
	cursor string
	id     streamID
	fields []any
}

const streamBatchSize = 32

func mergeStreams(v any) []streamEntry {
	var merged []streamEntry
	var watermark *streamID
	for _, stream := range array(v) {
		parts := array(stream)
		if len(parts) != 2 {
			continue
		}
		key := str(parts[0])
		rows := array(parts[1])
		for i, item := range rows {
			entry := array(item)
			if len(entry) != 2 {
				continue
			}
			cursor := str(entry[0])
			id := parseStreamID(cursor)
			merged = append(merged, streamEntry{key: key, cursor: cursor, id: id, fields: array(entry[1])})
			if len(rows) == streamBatchSize && i == len(rows)-1 && (watermark == nil || id.less(*watermark)) {
				watermark = &id
			}
		}
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].id.less(merged[j].id) })
	if watermark != nil {
		return merged[:sort.Search(len(merged), func(i int) bool { return watermark.less(merged[i].id) })]
	}
	return merged
}

func (r *Redis) read(c *redisConn, cursor map[string]string) {
	defer r.wg.Done()
	defer close(r.output)
	for r.ctx.Err() == nil {
		if c == nil {
			var err error
			c, err = r.connect(r.ctx)
			if err == nil {
				cursor, err = r.heads(c)
			}
			if err != nil {
				if c != nil {
					c.Close()
					c = nil
				}
				select {
				case <-time.After(time.Second):
					continue
				case <-r.ctx.Done():
					return
				}
			}
			if !r.emit(Delivery{Reset: true}) {
				c.Close()
				return
			}
		}
		args := append([]string{"XREAD", "COUNT", strconv.Itoa(streamBatchSize), "BLOCK", "500", "STREAMS"}, r.keys...)
		for _, key := range r.keys {
			args = append(args, cursor[key])
		}
		v, err := c.command(args...)
		if err != nil {
			c.Close()
			c = nil
			continue
		}
		for _, entry := range mergeStreams(v) {
			cursor[entry.key] = entry.cursor
			var d Delivery
			for i := 0; i+1 < len(entry.fields); i += 2 {
				switch str(entry.fields[i]) {
				case "topic":
					d.Topic = str(entry.fields[i+1])
				case "data":
					d.Data = []byte(str(entry.fields[i+1]))
				}
			}
			if d.Topic != "" && !r.emit(d) {
				c.Close()
				return
			}
		}
	}
	if c != nil {
		c.Close()
	}
}

func (r *Redis) emit(d Delivery) bool {
	select {
	case r.output <- d:
		return true
	case <-r.ctx.Done():
		return false
	}
}

const appendStreams = `
local id = redis.call("XADD", KEYS[1], "MAXLEN", "~", "10000", "*", "topic", ARGV[1], "data", ARGV[2])
return redis.call("XADD", KEYS[2], "MAXLEN", "~", "10000", id, "topic", ARGV[1], "data", ARGV[2])
`

func (r *Redis) Publish(ctx context.Context, topic string, data []byte) error {
	if len(data) > MaxPayload {
		return ErrTooLarge
	}
	parts := strings.Split(topic, "/")
	if len(parts) != 4 || parts[0] != "msg" {
		return errors.New("invalid Redis route")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return net.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.writer == nil {
		var err error
		r.writer, err = r.connect(ctx)
		if err != nil {
			return err
		}
	}
	_, err := r.writer.command("EVAL", appendStreams, "2", r.prefix+"all", r.prefix+"type:"+parts[1], topic, string(data))
	if err != nil {
		r.writer.Close()
		r.writer = nil
	}
	return err
}

func (r *Redis) Receive() <-chan Delivery { return r.output }
func (r *Redis) Close() error {
	r.cancel()
	r.mu.Lock()
	if r.writer != nil {
		r.writer.Close()
		r.writer = nil
	}
	r.closed = true
	r.mu.Unlock()
	r.wg.Wait()
	return nil
}
