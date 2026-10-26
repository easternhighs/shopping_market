package demo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shopping_market/internal/pkg/trace"
)

func TestHTTPClientDoSendsBodyAndDecodesResponse(t *testing.T) {
	const traceID = "trace-console-test"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := r.Header.Get(trace.HeaderKey); got != traceID {
			t.Errorf("%s = %q, want %q", trace.HeaderKey, got, traceID)
		}

		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if req.Username != "demo1" || req.Password != "demo1234" {
			t.Errorf("request body = %+v, want demo credentials", req)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":1,"username":"demo1"}}`))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, server.URL, server.URL, time.Second)
	ctx := trace.WithID(context.Background(), traceID)

	var out struct {
		User struct {
			ID       uint   `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	err := client.do(ctx, http.MethodPost, server.URL+"/register", map[string]string{
		"username": "demo1",
		"password": "demo1234",
	}, &out)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if out.User.ID != 1 || out.User.Username != "demo1" {
		t.Fatalf("decoded user = %+v, want id=1 username=demo1", out.User)
	}
}

func TestHTTPClientDoPreservesStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"username already exists"}`, http.StatusConflict)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, server.URL, server.URL, time.Second)
	err := client.do(context.Background(), http.MethodPost, server.URL+"/register", nil, nil)

	var statusErr *statusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error = %T %v, want *statusError", err, err)
	}
	if statusErr.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", statusErr.StatusCode, http.StatusConflict)
	}
	if !strings.Contains(statusErr.Body, "username already exists") {
		t.Fatalf("body = %q, want original downstream body", statusErr.Body)
	}
}

func TestHTTPClientDoClosesResponseBody(t *testing.T) {
	body := &closeTrackingBody{Reader: strings.NewReader(`{"pending":3}`)}
	client := &HTTPClient{
		client: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       body,
				}, nil
			}),
		},
	}

	var out struct {
		Pending int64 `json:"pending"`
	}
	if err := client.do(context.Background(), http.MethodGet, "http://example.test", nil, &out); err != nil {
		t.Fatalf("do: %v", err)
	}
	if out.Pending != 3 {
		t.Fatalf("pending = %d, want 3", out.Pending)
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}
