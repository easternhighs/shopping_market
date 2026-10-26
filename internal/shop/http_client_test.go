package shop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shopping_market/internal/pkg/trace"
)

func TestHTTPClientDoSendsBodyAndDecodesResponse(t *testing.T) {
	const traceID = "trace-shop-test"

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
			ProductID uint   `json:"product_id"`
			Spec      string `json:"spec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if req.ProductID != 7 || req.Spec != "black" {
			t.Errorf("request body = %+v, want product_id=7 spec=black", req)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sku_id":11}`))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL+"/", server.URL+"/", time.Second)
	ctx := trace.WithID(context.Background(), traceID)

	var out struct {
		SKUID uint `json:"sku_id"`
	}
	err := client.do(ctx, http.MethodPost, server.URL+"/internal/products/skus/find", map[string]any{
		"product_id": 7,
		"spec":       "black",
	}, &out)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if out.SKUID != 11 {
		t.Fatalf("SKUID = %d, want 11", out.SKUID)
	}
}

func TestHTTPClientDoReturnsStatusAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"duplicate"}`, http.StatusConflict)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, server.URL, time.Second)
	err := client.do(context.Background(), http.MethodPost, server.URL, nil, nil)
	if err == nil {
		t.Fatal("do returned nil, want error")
	}
	for _, want := range []string{"409", "duplicate"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}
