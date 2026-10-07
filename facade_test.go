package adviverse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRequiresAPIKey(t *testing.T) {
	if _, err := New(); err == nil {
		t.Fatal("expected error when api key missing")
	}
}

func TestAuthHeaderRetryAndError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k_test" {
			t.Errorf("missing x-api-key")
			w.WriteHeader(401)
			return
		}
		n := atomic.AddInt32(&calls, 1)
		switch r.URL.Path {
		case "/v1/_retry":
			if n < 3 {
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":"x"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/_err":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	c, err := New(WithAPIKey("k_test"), WithBaseURL(srv.URL), WithMaxRetries(3))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(context.Background(), "GET", "/v1/_retry", nil, nil); err != nil {
		t.Fatalf("retry path: %v", err)
	}
	_, err = c.Do(context.Background(), "GET", "/v1/_err", nil, nil)
	ae, ok := err.(*APIError)
	if !ok || ae.Status != 404 {
		t.Fatalf("want APIError 404, got %v", err)
	}
}

func TestPostNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}))
	defer srv.Close()
	c, _ := New(WithAPIKey("k"), WithBaseURL(srv.URL), WithMaxRetries(3))
	_, err := c.Do(context.Background(), "POST", "/v1/x", nil, map[string]any{"a": 1})
	ae, ok := err.(*APIError)
	if !ok || ae.Status != 503 {
		t.Fatalf("want APIError 503, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("POST must not be retried; server saw %d calls", n)
	}
}

func TestCallBinding(t *testing.T) {
	var gotMethod, gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotURL = r.Method, r.URL.String()
		_, _ = w.Write([]byte(`{"url":"` + r.URL.String() + `"}`))
	}))
	defer srv.Close()
	c, _ := New(WithAPIKey("k"), WithBaseURL(srv.URL), WithMaxRetries(0))
	raw, err := c.Call(context.Background(), "webhooks", "deliveries", map[string]any{"id": "wh 1", "limit": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "GET" || gotURL != "/v1/webhooks/wh%201/deliveries?limit=5" {
		t.Fatalf("bad request: %s %s", gotMethod, gotURL)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out["url"] != gotURL {
		t.Fatalf("bad body: %s %v", raw, err)
	}
	if _, err := c.Call(context.Background(), "nope", "x", nil); err == nil {
		t.Fatal("expected unknown method error")
	}
}
