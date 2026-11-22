package operator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	chains "chains-automation"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

func NewClient(address, token string) (*Client, error) {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("operator URL must be an HTTP or HTTPS base URL")
	}
	if token == "" {
		return nil, errors.New("set CHAINS_OPERATOR_TOKEN")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 10 * time.Second
	return &Client{base: strings.TrimRight(u.String(), "/") + Base, token: token, http: &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

type statusError struct {
	Code int
	Body string
}

func (e *statusError) Error() string { return fmt.Sprintf("operator API HTTP %d: %s", e.Code, e.Body) }

func (c *Client) request(ctx context.Context, method, path string, data []byte, cursor string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return res, nil
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	return nil, &statusError{res.StatusCode, strings.TrimSpace(string(body))}
}

func (c *Client) json(ctx context.Context, method, path string, input, output any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var data []byte
	if input != nil {
		var err error
		data, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	res, err := c.request(ctx, method, path, data, "")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(res.Body).Decode(output)
}

func (c *Client) State(ctx context.Context, path []string) (State, error) {
	query := url.Values{}
	keys := []string{"service", "device", "attribute"}
	if len(path) > len(keys) {
		return State{}, errors.New("state path must be service[/device[/attribute]]")
	}
	for i, part := range path {
		if part == "" {
			return State{}, errors.New("state path segments cannot be empty")
		}
		query.Set(keys[i], part)
	}
	var state State
	err := c.json(ctx, http.MethodGet, "/state?"+query.Encode(), nil, &state)
	return state, err
}

func (c *Client) Services(ctx context.Context) ([]chains.Service, error) {
	services := []chains.Service{}
	err := c.json(ctx, http.MethodGet, "/services", nil, &services)
	return services, err
}

func (c *Client) Profiles(ctx context.Context) ([]Profile, error) {
	var profiles []Profile
	err := c.json(ctx, http.MethodGet, "/profiles", nil, &profiles)
	return profiles, err
}

func (c *Client) Send(ctx context.Context, submission Submission) (Receipt, error) {
	var receipt Receipt
	err := c.json(ctx, http.MethodPost, "/events", submission, &receipt)
	return receipt, err
}

// Watch reconnects after interruptions and resumes from the last delivered cursor.
func (c *Client) Watch(ctx context.Context, recent int, emit func(Record) error) error {
	if recent < 0 || recent > HistorySize {
		return fmt.Errorf("recent must be between 0 and %d", HistorySize)
	}
	cursor := ""
	for ctx.Err() == nil {
		res, err := c.request(ctx, http.MethodGet, "/activity?recent="+strconv.Itoa(recent), nil, cursor)
		if err == nil {
			if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
				res.Body.Close()
				return errors.New("operator server did not return an event stream")
			}
			scanner := bufio.NewScanner(res.Body)
			scanner.Buffer(make([]byte, 4096), 256<<10)
			var record Record
			var nextCursor string
			for scanner.Scan() {
				line := scanner.Bytes()
				if bytes.HasPrefix(line, []byte("id: ")) {
					nextCursor = string(line[4:])
				}
				if bytes.HasPrefix(line, []byte("data: ")) {
					if err = json.Unmarshal(line[6:], &record); err != nil {
						break
					}
				}
				if len(line) == 0 {
					if record.Type != "" {
						if err = emit(record); err != nil {
							res.Body.Close()
							return err
						}
						record = Record{}
					}
					if nextCursor != "" {
						cursor = nextCursor
						nextCursor = ""
					}
				}
			}
			res.Body.Close()
			if err == nil {
				err = scanner.Err()
			}
			if err == nil {
				err = io.EOF
			}
		}
		if ctx.Err() != nil {
			break
		}
		var status *statusError
		if errors.As(err, &status) && status.Code < 500 && status.Code != http.StatusTooManyRequests {
			return err
		}
		if err = emit(Record{At: time.Now().UTC(), Type: "watch.error", Summary: "Reconnecting: " + err.Error()}); err != nil {
			return err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}
	return ctx.Err()
}
