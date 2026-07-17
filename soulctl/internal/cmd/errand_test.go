package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
)

// TestErrandExec_Sync — 200 from the Keeper → returns the full result, async=false.
func TestErrandExec_Sync(t *testing.T) {
	exit := int32(0)
	dur := int64(123)
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/souls/web-01.example.com/exec": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				t.Errorf("method = %s, want POST", r.Method)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["module"] != "core.cmd.shell" {
				t.Errorf("module: got %v", body["module"])
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errand_id":        "01HFTEST",
				"sid":              "web-01.example.com",
				"module":           "core.cmd.shell",
				"status":           "success",
				"exit_code":        exit,
				"stdout":           "uptime ok\n",
				"stderr":           "",
				"stdout_truncated": false,
				"stderr_truncated": false,
				"duration_ms":      dur,
				"started_by_aid":   "archon-alice",
				"started_at":       "2026-05-26T12:00:00Z",
				"finished_at":      "2026-05-26T12:00:01Z",
			})
		},
	})
	res, async, err := cl.Errand.Exec(context.Background(), client.ErrandExecRequest{
		SID:    "web-01.example.com",
		Module: "core.cmd.shell",
		Input:  map[string]any{"command": "uptime"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if async {
		t.Error("async = true, expected sync")
	}
	if res.Status != "success" {
		t.Errorf("status = %q, want success", res.Status)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit_code = %v, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "uptime ok") {
		t.Errorf("stdout: %q", res.Stdout)
	}
}

// TestErrandExec_Async — the 202 form (only errand_id + status:running,
// no finished_at) → the client must mark async=true.
func TestErrandExec_Async(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/souls/long.example.com/exec": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/v1/errands/01HFASYNC")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errand_id": "01HFASYNC",
				"status":    "running",
			})
		},
	})
	res, async, err := cl.Errand.Exec(context.Background(), client.ErrandExecRequest{
		SID:            "long.example.com",
		Module:         "core.cmd.shell",
		TimeoutSeconds: 120,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !async {
		t.Fatalf("async = false, expected true (202)")
	}
	if res.ErrandID != "01HFASYNC" {
		t.Errorf("errand_id = %q", res.ErrandID)
	}
	if res.Status != "running" {
		t.Errorf("status = %q, want running", res.Status)
	}
}

// TestErrandExec_Forbidden — 403 from the Keeper → APIError with Status=403.
func TestErrandExec_Forbidden(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/souls/web-01.example.com/exec": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":   "/problems/forbidden",
				"title":  "forbidden",
				"detail": "operator lacks required permission errand.run",
				"status": 403,
			})
		},
	})
	_, _, err := cl.Errand.Exec(context.Background(), client.ErrandExecRequest{
		SID:    "web-01.example.com",
		Module: "core.cmd.shell",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := client.AsAPIError(err)
	if !ok {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.Status != 403 {
		t.Errorf("status = %d, want 403", apiErr.Status)
	}
}

// TestErrandGet_Terminal — 200 with finished_at → async=false, full result.
func TestErrandGet_Terminal(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/errands/01HFTEST": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errand_id":      "01HFTEST",
				"sid":            "web-01.example.com",
				"module":         "core.cmd.shell",
				"status":         "success",
				"started_by_aid": "archon-alice",
				"started_at":     "2026-05-26T12:00:00Z",
				"finished_at":    "2026-05-26T12:00:01Z",
			})
		},
	})
	res, async, err := cl.Errand.Get(context.Background(), "01HFTEST")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if async {
		t.Error("async = true, expected terminal")
	}
	if res.Status != "success" {
		t.Errorf("status = %q", res.Status)
	}
}

// TestErrandList_Filters — the sid / status / limit query parameters are passed through.
func TestErrandList_Filters(t *testing.T) {
	called := int32(0)
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/errands": func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&called, 1)
			q := r.URL.Query()
			if q.Get("sid") != "web-01.example.com" {
				t.Errorf("sid query: %q", q.Get("sid"))
			}
			if q.Get("status") != "success" {
				t.Errorf("status query: %q", q.Get("status"))
			}
			if q.Get("limit") != "10" {
				t.Errorf("limit query: %q", q.Get("limit"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":  []any{},
				"offset": 0, "limit": 10, "total": 0,
			})
		},
	})
	reply, err := cl.Errand.List(context.Background(), client.ErrandListOptions{
		SID: "web-01.example.com", Status: "success", Limit: 10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if reply.Total != 0 {
		t.Errorf("total = %d, want 0", reply.Total)
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Error("server was not called")
	}
}

// TestErrandCancel_Happy — DELETE /v1/errands/{id} → 204 No Content (ADR-033 E5).
func TestErrandCancel_Happy(t *testing.T) {
	var hits atomic.Int32
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/errands/01HFCANCEL": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "DELETE" {
				t.Errorf("method = %s, want DELETE", r.Method)
			}
			hits.Add(1)
			w.WriteHeader(http.StatusNoContent)
		},
	})
	if err := cl.Errand.Cancel(context.Background(), "01HFCANCEL"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
}

// TestErrandCancel_Conflict — 409 (terminal Errand) is propagated as an APIError.
func TestErrandCancel_Conflict(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/errands/01HFDONE": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":   "https://soul-stack.io/errors/errand-not-cancellable",
				"title":  "Errand is not cancellable",
				"detail": "errand 01HFDONE is already in a terminal state",
				"status": 409,
			})
		},
	})
	err := cl.Errand.Cancel(context.Background(), "01HFDONE")
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := client.AsAPIError(err)
	if !ok || apiErr.Status != 409 {
		t.Errorf("err = %v, want 409 APIError", err)
	}
}

// TestErrandCancel_EmptyID — the client itself rejects an empty errand_id.
func TestErrandCancel_EmptyID(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{})
	if err := cl.Errand.Cancel(context.Background(), ""); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "errand_id is empty") {
		t.Errorf("err = %v, want errand_id-empty", err)
	}
}

// TestErrandGet_NotFound — 404 for an errand_id that doesn't exist.
func TestErrandGet_NotFound(t *testing.T) {
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/errands/01HFNOSUCH": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":   "/problems/not-found",
				"title":  "not-found",
				"detail": "errand 01HFNOSUCH not found",
				"status": 404,
			})
		},
	})
	_, _, err := cl.Errand.Get(context.Background(), "01HFNOSUCH")
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := client.AsAPIError(err)
	if !ok || apiErr.Status != 404 {
		t.Errorf("err = %v, want 404 APIError", err)
	}
}
