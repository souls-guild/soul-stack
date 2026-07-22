package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
)

func TestSoulsListFilters(t *testing.T) {
	var capturedQuery url.Values
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/souls": func(w http.ResponseWriter, r *http.Request) {
			capturedQuery = r.URL.Query()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"sid": "host-1.example", "transport": "agent",
						"status": "connected", "covens": []string{"prod", "dc1"},
						"registered_at": "2026-05-20T00:00:00Z",
						"last_seen_at":  "2026-05-26T12:00:00Z",
					},
				},
				"offset": 0, "limit": 50, "total": 1,
			})
		},
	})
	reply, err := cl.Souls.List(context.Background(), client.SoulListOptions{
		Covens: []string{"prod", "dc1"}, Status: "connected", Transport: "agent", Limit: 100,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	covens := capturedQuery["coven"]
	if !reflect.DeepEqual(covens, []string{"prod", "dc1"}) {
		t.Errorf("coven query: got %v, want [prod dc1]", covens)
	}
	if capturedQuery.Get("status") != "connected" {
		t.Errorf("status query: %q", capturedQuery.Get("status"))
	}
	if capturedQuery.Get("transport") != "agent" {
		t.Errorf("transport query: %q", capturedQuery.Get("transport"))
	}
	if capturedQuery.Get("limit") != "100" {
		t.Errorf("limit: %q", capturedQuery.Get("limit"))
	}
	if len(reply.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(reply.Items))
	}
}

func TestSoulsGetFallback(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/souls": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"sid": "a.example", "transport": "agent", "status": "connected",
						"registered_at": "t"},
					{"sid": "b.example", "transport": "ssh", "status": "pending",
						"registered_at": "t"},
				},
				"offset": 0, "limit": 1000, "total": 2,
			})
		},
	})
	item, err := cl.Souls.Get(context.Background(), "b.example")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.Transport != "ssh" {
		t.Errorf("transport: %q", item.Transport)
	}
}

func TestSoulsGetNotFound(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/souls": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":  []any{},
				"offset": 0, "limit": 1000, "total": 0,
			})
		},
	})
	_, err := cl.Souls.Get(context.Background(), "ghost.example")
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := client.AsAPIError(err)
	if !ok || apiErr.Status != 404 {
		t.Errorf("expected a 404 APIError, got %v", err)
	}
}
