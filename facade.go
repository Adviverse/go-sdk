package adviverse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is the ergonomic flagship facade over the Adviverse API.
type Client struct {
	apiKey, baseURL string
	bearer          bool
	maxRetries      int
	hc              *http.Client
}

// Option configures a Client.
type Option func(*Client)

func WithAPIKey(k string) Option         { return func(c *Client) { c.apiKey = k } }
func WithBaseURL(u string) Option        { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }
func WithBearer(b bool) Option           { return func(c *Client) { c.bearer = b } }
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.hc.Timeout = d } }
func WithMaxRetries(n int) Option        { return func(c *Client) { c.maxRetries = n } }

// ErrAuth is returned by New when no API key was supplied.
var ErrAuth = fmt.Errorf("adviverse: api key required")

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("adviverse: HTTP %d: %s", e.Status, e.Message) }

func New(opts ...Option) (*Client, error) {
	c := &Client{baseURL: "https://adviverse.com", maxRetries: 3, hc: &http.Client{Timeout: 30 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	if c.apiKey == "" {
		return nil, ErrAuth
	}
	return c, nil
}

var retryStatus = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}

// canRetry gates retries to idempotent methods so POST/PATCH never double-submit.
func canRetry(method string) bool {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "OPTIONS", "PUT", "DELETE":
		return true
	}
	return false
}

// retryAfter parses a Retry-After seconds header, capped at 10s.
func retryAfter(h string) (time.Duration, bool) {
	s, err := strconv.ParseFloat(strings.TrimSpace(h), 64)
	if err != nil || s < 0 {
		return 0, false
	}
	d := time.Duration(s * float64(time.Second))
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d, true
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) Do(ctx context.Context, method, path string, query map[string]string, body any) (json.RawMessage, error) {
	if c.maxRetries < 0 {
		c.maxRetries = 0
	}
	u := c.baseURL + path
	if len(query) > 0 {
		q := url.Values{}
		for k, v := range query {
			if v != "" {
				q.Set(k, v)
			}
		}
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	retriable := canRetry(method)
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("accept", "application/json")
		req.Header.Set("user-agent", "go-sdk/0.1.0")
		if payload != nil {
			req.Header.Set("content-type", "application/json")
		}
		if c.bearer {
			req.Header.Set("authorization", "Bearer "+c.apiKey)
		} else {
			req.Header.Set("x-api-key", c.apiKey)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			if retriable && attempt < c.maxRetries {
				if serr := sleepCtx(ctx, backoff(attempt)); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if len(raw) == 0 {
				return nil, nil
			}
			return json.RawMessage(raw), nil
		}
		if retriable && retryStatus[resp.StatusCode] && attempt < c.maxRetries {
			lastErr = &APIError{resp.StatusCode, string(raw)}
			wait := backoff(attempt)
			if resp.StatusCode == 429 {
				if d, ok := retryAfter(resp.Header.Get("Retry-After")); ok {
					wait = d
				}
			}
			if serr := sleepCtx(ctx, wait); serr != nil {
				return nil, serr
			}
			continue
		}
		msg := string(raw)
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil {
			if m, ok := parsed["message"].(string); ok {
				msg = m
			} else if m, ok := parsed["error"].(string); ok {
				msg = m
			}
		}
		return nil, &APIError{resp.StatusCode, msg}
	}
	return nil, lastErr
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * 200 * time.Millisecond
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

func replacePath(p, name string, v any) string {
	return strings.ReplaceAll(p, "{"+name+"}", url.PathEscape(toStr(v)))
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}
