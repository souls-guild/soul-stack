package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
)

// fakeServer creates an httptest.NewServer with a route mapper. Returns the
// server + a client ready to use in a command.
func fakeServer(t *testing.T, handlers map[string]http.HandlerFunc) (*httptest.Server, *client.Client) {
	t.Helper()
	mux := http.NewServeMux()
	for path, h := range handlers {
		mux.HandleFunc(path, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cl, err := client.NewWithDoer(srv.URL, "test-jwt", srv.Client())
	if err != nil {
		t.Fatalf("NewWithDoer: %v", err)
	}
	return srv, cl
}

// runWithClient injects cl into a command by monkey-patching loadClient via
// the context. Simpler than rewriting cmd.go for this; instead we call the
// client methods directly here (output-formatting logic is tested
// separately where needed).
func TestIncarnationsList(t *testing.T) {
	called := int32(0)
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations": func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&called, 1)
			if r.Method != "GET" {
				t.Errorf("ожидался GET, получено %s", r.Method)
			}
			q := r.URL.Query()
			if got, want := q.Get("service"), "redis-cluster"; got != want {
				t.Errorf("service-фильтр: got %q, want %q", got, want)
			}
			if got, want := q.Get("status"), "ready"; got != want {
				t.Errorf("status-фильтр: got %q, want %q", got, want)
			}
			if got, want := q.Get("limit"), "10"; got != want {
				t.Errorf("limit: got %q, want %q", got, want)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-jwt" {
				t.Errorf("Authorization header: got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"name": "redis-prod", "service": "redis-cluster",
						"service_version": "v1.2.3", "state_schema_version": 1,
						"covens": []string{"prod", "dc1"}, "status": "ready",
						"created_by_aid": "archon-alice", "created_at": "2026-05-26T10:00:00Z",
						"updated_at": "2026-05-26T11:00:00Z",
					},
				},
				"offset": 0, "limit": 10, "total": 1,
			})
		},
	})

	reply, err := cl.Incarnations.List(context.Background(), client.IncarnationListOptions{
		Service: "redis-cluster", Status: "ready", Limit: 10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Fatalf("обработчик не вызван")
	}
	if len(reply.Items) != 1 || reply.Items[0].Name != "redis-prod" {
		t.Fatalf("неожиданный ответ: %+v", reply)
	}
	if reply.Total != 1 {
		t.Errorf("total: got %d, want 1", reply.Total)
	}
}

func TestIncarnationsListCovenClientSide(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("coven") != "" {
				t.Errorf("coven НЕ должен уходить в query (отсутствует в openapi для incarnations); got=%q", r.URL.Query().Get("coven"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"name": "a", "service": "s", "service_version": "v", "state_schema_version": 1,
						"covens": []string{"prod"}, "status": "ready",
						"created_by_aid": "archon-x", "created_at": "t", "updated_at": "t"},
					{"name": "b", "service": "s", "service_version": "v", "state_schema_version": 1,
						"covens": []string{"dev"}, "status": "ready",
						"created_by_aid": "archon-x", "created_at": "t", "updated_at": "t"},
				},
				"offset": 0, "limit": 50, "total": 2,
			})
		},
	})

	reply, err := cl.Incarnations.List(context.Background(), client.IncarnationListOptions{Coven: "prod"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(reply.Items) != 1 || reply.Items[0].Name != "a" {
		t.Fatalf("ожидалась только incarnation 'a', got %+v", reply.Items)
	}
}

func TestIncarnationsGet(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations/redis-prod": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Errorf("ожидался GET, получено %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "redis-prod", "service": "redis-cluster",
				"service_version": "v1.2.3", "state_schema_version": 1,
				"covens": []string{"prod"}, "status": "ready",
				"created_by_aid": "archon-alice", "created_at": "t", "updated_at": "t",
			})
		},
	})
	it, err := cl.Incarnations.Get(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if it.Name != "redis-prod" {
		t.Errorf("name: got %q", it.Name)
	}
}

func TestIncarnationsGet404(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations/missing": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":   "https://soul-stack.io/errors/not-found",
				"title":  "not-found",
				"status": 404,
				"detail": "incarnation missing не существует",
			})
		},
	})
	_, err := cl.Incarnations.Get(context.Background(), "missing")
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	apiErr, ok := client.AsAPIError(err)
	if !ok {
		t.Fatalf("ожидался APIError, got %T", err)
	}
	if apiErr.Status != 404 {
		t.Errorf("status: got %d, want 404", apiErr.Status)
	}
	rendered := renderAPIError(err)
	if !strings.Contains(rendered.Error(), "not found") {
		t.Errorf("renderAPIError должен содержать 'not found', got %q", rendered.Error())
	}
}

func TestIncarnationsRun(t *testing.T) {
	var capturedBody bytes.Buffer
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations/redis-prod/scenarios/converge": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				t.Errorf("ожидался POST, получено %s", r.Method)
			}
			_, _ = capturedBody.ReadFrom(r.Body)
			if r.URL.Query().Get("dry_run") != "true" {
				t.Errorf("dry_run query: got %q", r.URL.Query().Get("dry_run"))
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apply_id":    "01HX0000000000000000000000",
				"incarnation": "redis-prod",
				"scenario":    "converge",
			})
		},
	})
	reply, err := cl.Incarnations.Run(context.Background(), "redis-prod", "converge",
		map[string]any{"shards": 3}, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply.ApplyID == "" {
		t.Error("apply_id пуст")
	}
	if !strings.Contains(capturedBody.String(), `"shards":3`) {
		t.Errorf("input не пробросился в body: %s", capturedBody.String())
	}
}

func TestIncarnationsHistory(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations/redis-prod/history": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("limit") != "5" {
				t.Errorf("limit: %q", r.URL.Query().Get("limit"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"history_id":     "uuid-1",
						"scenario":       "add_user",
						"changed_by_aid": "archon-bob",
						"apply_id":       "01HX0000000000000000000000",
						"created_at":     "2026-05-26T12:00:00Z",
					},
				},
				"offset": 0, "limit": 5, "total": 1,
			})
		},
	})
	reply, err := cl.Incarnations.History(context.Background(), "redis-prod", 5, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(reply.Items) != 1 {
		t.Fatalf("ожидалась 1 запись, got %d", len(reply.Items))
	}
}

func TestWaitForApplySuccess(t *testing.T) {
	historyHits := int32(0)
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations/redis-prod/history": func(w http.ResponseWriter, _ *http.Request) {
			n := atomic.AddInt32(&historyHits, 1)
			// The first call has no matching record, the second one does.
			items := []map[string]any{}
			if n >= 2 {
				items = append(items, map[string]any{
					"history_id":     "uuid-1",
					"scenario":       "converge",
					"changed_by_aid": "archon-alice",
					"apply_id":       "01HX_TEST",
					"created_at":     "2026-05-26T12:00:00Z",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": items, "offset": 0, "limit": 50, "total": len(items),
			})
		},
		"/v1/incarnations/redis-prod": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "redis-prod", "service": "s", "service_version": "v",
				"state_schema_version": 1, "covens": []string{}, "status": "ready",
				"created_by_aid": "archon-alice", "created_at": "t", "updated_at": "t",
			})
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// waitForApply uses 2s ticks; keep the test fast via a dedicated
	// context with a generous margin.
	result, err := waitForApply(ctx, cl, "redis-prod", "01HX_TEST", 0)
	if err != nil {
		t.Fatalf("waitForApply: %v", err)
	}
	if result.FinalStatus != "ready" {
		t.Errorf("final_status: got %q, want ready", result.FinalStatus)
	}
	if result.HistoryEntry == nil || result.HistoryEntry.ApplyID != "01HX_TEST" {
		t.Errorf("history entry не возвращён: %+v", result.HistoryEntry)
	}
}

func TestWaitForApplyBlocking(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations/redis-prod/history": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{}, "offset": 0, "limit": 50, "total": 0,
			})
		},
		"/v1/incarnations/redis-prod": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "redis-prod", "service": "s", "service_version": "v",
				"state_schema_version": 1, "covens": []string{},
				"status":         "error_locked",
				"created_by_aid": "archon-alice", "created_at": "t", "updated_at": "t",
			})
		},
	})
	result, err := waitForApply(context.Background(), cl, "redis-prod", "01HX_TEST", 0)
	if err == nil {
		t.Fatal("ожидалась ошибка (error_locked)")
	}
	if result == nil || result.FinalStatus != "error_locked" {
		t.Errorf("waitResult: %+v", result)
	}
}
