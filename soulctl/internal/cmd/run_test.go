package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
)

// TestRunCmd_BuildPayload — `soulctl run cmd 'uptime' --target-coven dev
// --target-glob 'web-*'` → POST /v1/voyages with the right body (kind=command).
func TestRunCmd_BuildPayload(t *testing.T) {
	var captured map[string]any
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/voyages": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				t.Errorf("method: got %s want POST", r.Method)
			}
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &captured); err != nil {
				t.Fatalf("unmarshal body: %v (raw=%s)", err, raw)
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"voyage_id":  "01HX0000000000000000000000",
				"kind":       "command",
				"scope_size": 7,
				"status":     "pending",
				"location":   "/v1/voyages/01HX0000000000000000000000",
			})
		},
	})

	tf := targetFlags{
		Coven: "dev",
		Glob:  "web-*",
	}
	target, _ := tf.resolve()
	req := client.VoyageCreateRequest{
		Kind:   "command",
		Module: "core.cmd.shell",
		Input:  map[string]any{"cmd": "uptime"},
		Target: client.VoyageTarget{
			SIDs:  target.SIDs,
			Coven: target.Coven,
			Where: target.Where,
		},
		OnFailure:   "continue",
		Concurrency: 50,
	}
	reply, err := cl.Voyages.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if reply.VoyageID == "" {
		t.Error("voyage_id пуст")
	}

	if got := captured["kind"]; got != "command" {
		t.Errorf("kind: %v", got)
	}
	if got := captured["module"]; got != "core.cmd.shell" {
		t.Errorf("module: %v", got)
	}
	if got := captured["input"].(map[string]any)["cmd"]; got != "uptime" {
		t.Errorf("input.cmd: %v", got)
	}
	target_ := captured["target"].(map[string]any)
	covens := target_["coven"].([]any)
	if len(covens) != 1 || covens[0] != "dev" {
		t.Errorf("target.coven: %v", covens)
	}
	if got, want := target_["where"], `sid.glob("web-*")`; got != want {
		t.Errorf("target.where: got %v want %s", got, want)
	}
	if got := captured["on_failure"]; got != "continue" {
		t.Errorf("on_failure: %v", got)
	}
	if got := captured["concurrency"]; got != float64(50) {
		t.Errorf("concurrency: %v", got)
	}
}

// TestRunCmd_TargetSIDs — exact-match via --target-sids.
func TestRunCmd_TargetSIDs(t *testing.T) {
	var captured map[string]any
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/voyages": func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &captured)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"voyage_id":  "01HX0000000000000000000001",
				"kind":       "command",
				"scope_size": 2, "status": "pending",
				"location": "/v1/voyages/01HX0000000000000000000001",
			})
		},
	})
	tf := targetFlags{SIDs: "host1,host2"}
	target, _ := tf.resolve()
	_, err := cl.Voyages.Create(context.Background(), client.VoyageCreateRequest{
		Kind:   "command",
		Module: "core.cmd.shell",
		Input:  map[string]any{"cmd": "id"},
		Target: client.VoyageTarget{SIDs: target.SIDs},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sids := captured["target"].(map[string]any)["sids"].([]any)
	if len(sids) != 2 || sids[0] != "host1" || sids[1] != "host2" {
		t.Errorf("target.sids: %v", sids)
	}
}

// TestRunCmd_BatchAndMaxFailures — raw --batch/--max-failures are forwarded
// into the body as strings (the client doesn't parse them; Keeper is authoritative).
func TestRunCmd_BatchAndMaxFailures(t *testing.T) {
	var captured map[string]any
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/voyages": func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &captured)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"voyage_id":  "01HX0000000000000000000002",
				"kind":       "command",
				"scope_size": 10, "status": "pending",
				"location": "/v1/voyages/01HX0000000000000000000002",
			})
		},
	})
	_, err := cl.Voyages.Create(context.Background(), client.VoyageCreateRequest{
		Kind:        "command",
		Module:      "core.cmd.shell",
		Input:       map[string]any{"cmd": "uptime"},
		Target:      client.VoyageTarget{SIDs: []string{"h1"}},
		Batch:       "25%",
		MaxFailures: "3",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := captured["batch"]; got != "25%" {
		t.Errorf("batch: got %v want 25%%", got)
	}
	if got := captured["max_failures"]; got != "3" {
		t.Errorf("max_failures: got %v want 3", got)
	}
}

// TestVoyageCreate_BatchOmittedWhenEmpty — empty batch/max_failures don't end
// up in the body (omitempty): "unset" != empty string for Keeper.
func TestVoyageCreate_BatchOmittedWhenEmpty(t *testing.T) {
	var captured map[string]any
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/voyages": func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &captured)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"voyage_id":  "01HX0000000000000000000003",
				"kind":       "command",
				"scope_size": 1, "status": "pending",
				"location": "/v1/voyages/01HX0000000000000000000003",
			})
		},
	})
	_, err := cl.Voyages.Create(context.Background(), client.VoyageCreateRequest{
		Kind:   "command",
		Module: "core.cmd.shell",
		Input:  map[string]any{"cmd": "uptime"},
		Target: client.VoyageTarget{SIDs: []string{"h1"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := captured["batch"]; ok {
		t.Errorf("пустой batch не должен попадать в body, got %v", captured["batch"])
	}
	if _, ok := captured["max_failures"]; ok {
		t.Errorf("пустой max_failures не должен попадать в body, got %v", captured["max_failures"])
	}
}

// writeCreds writes a temporary credentials.yaml pointing at the fake Keeper.
// Returns the path to pass via --config. Ensures the cobra command reaches
// loadClient and hits the httptest server instead of a real host.
func writeCreds(t *testing.T, keeperURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.yaml")
	body := "keeper_url: " + keeperURL + "\narchon_jwt: test-jwt\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	return path
}

// TestRunCmdCobra_BatchFlagsForwarded — cobra-level guard: `run cmd ... --batch
// 25% --max-failures 3` actually forwards the flags into the POST /v1/voyages
// body. Mutation target — zeroing `Batch`/`MaxFailures` in RunE
// (run_cmd.go:73-74): this test must go red for that (the serialization
// tests won't).
func TestRunCmdCobra_BatchFlagsForwarded(t *testing.T) {
	var captured map[string]any
	srv, _ := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/voyages": func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &captured)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"voyage_id": "01HX0000000000000000000010",
				"kind":      "command", "scope_size": 1, "status": "pending",
				"location": "/v1/voyages/01HX0000000000000000000010",
			})
		},
	})

	root := NewRoot("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{
		"--config", writeCreds(t, srv.URL),
		"run", "cmd", "uptime",
		"--target-sids", "h1",
		"--batch", "25%",
		"--max-failures", "3",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v (out=%s)", err, out.String())
	}
	if got := captured["batch"]; got != "25%" {
		t.Errorf("batch: got %v want 25%%", got)
	}
	if got := captured["max_failures"]; got != "3" {
		t.Errorf("max_failures: got %v want 3", got)
	}
}

// TestRunScenarioCobra_BatchFlagsForwarded — cobra-level guard for the second,
// independent forwarding point (run_scenario.go:88-89). Zeroing `Batch`/
// `MaxFailures` in this RunE must turn the test red.
func TestRunScenarioCobra_BatchFlagsForwarded(t *testing.T) {
	var captured map[string]any
	srv, _ := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/voyages": func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &captured)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"voyage_id": "01HX0000000000000000000011",
				"kind":      "scenario", "scope_size": 1, "status": "pending",
				"location": "/v1/voyages/01HX0000000000000000000011",
			})
		},
	})

	root := NewRoot("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{
		"--config", writeCreds(t, srv.URL),
		"run", "scenario", "redis/converge",
		"--incarnation", "redis-prod",
		"--batch", "5",
		"--max-failures", "10%",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v (out=%s)", err, out.String())
	}
	if got := captured["batch"]; got != "5" {
		t.Errorf("batch: got %v want 5", got)
	}
	if got := captured["max_failures"]; got != "10%" {
		t.Errorf("max_failures: got %v want 10%%", got)
	}
}

// TestRunScenario_BuildPayload — `run scenario redis/converge` → POST /v1/voyages
// (kind=scenario) with target.incarnations.
func TestRunScenario_BuildPayload(t *testing.T) {
	var captured map[string]any
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/voyages": func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &captured)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"voyage_id":  "01HXC00000000000000000000C",
				"kind":       "scenario",
				"scope_size": 1, "status": "pending",
				"location": "/v1/voyages/01HXC00000000000000000000C",
			})
		},
	})

	reply, err := cl.Voyages.Create(context.Background(), client.VoyageCreateRequest{
		Kind:         "scenario",
		ScenarioName: "converge",
		Input:        map[string]any{"shards": 3},
		Target:       client.VoyageTarget{Incarnations: []string{"redis-prod"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if reply.VoyageID == "" {
		t.Error("voyage_id пуст")
	}
	if got := captured["kind"]; got != "scenario" {
		t.Errorf("kind: %v", got)
	}
	if got := captured["scenario_name"]; got != "converge" {
		t.Errorf("scenario_name: %v", got)
	}
	incs := captured["target"].(map[string]any)["incarnations"].([]any)
	if len(incs) != 1 || incs[0] != "redis-prod" {
		t.Errorf("target.incarnations: %v", incs)
	}
}

// TestRunPush_BuildPayload — --target-sids → inventory, ssh_provider is forwarded.
func TestRunPush_BuildPayload(t *testing.T) {
	var captured map[string]any
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/push/apply": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				t.Errorf("method: %s", r.Method)
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &captured)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apply_id": "01HXP00000000000000000000P",
			})
		},
	})
	reply, err := cl.Push.Apply(context.Background(), client.PushApplyRequest{
		Inventory:   []string{"bastion-a", "bastion-b"},
		Destiny:     "redis-cluster@v2.0.0",
		SSHProvider: "vault-prod",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if reply.ApplyID == "" {
		t.Error("apply_id пуст")
	}
	inv := captured["inventory"].([]any)
	if len(inv) != 2 || inv[0] != "bastion-a" {
		t.Errorf("inventory: %v", inv)
	}
	if got := captured["destiny"]; got != "redis-cluster@v2.0.0" {
		t.Errorf("destiny: %v", got)
	}
	if got := captured["ssh_provider"]; got != "vault-prod" {
		t.Errorf("ssh_provider: %v", got)
	}
}

// TestRunPush_RejectsDynamicTarget — push only supports exact SIDs.
func TestRunPush_RejectsDynamicTarget(t *testing.T) {
	cases := []targetFlags{
		{Glob: "web-*"},
		{Regex: "host-.*"},
		{Where: "soulprint.self.os.family == \"debian\""},
		{Coven: "prod-eu"},
		{},
	}
	for _, tf := range cases {
		target, _ := tf.resolve()
		if err := validatePushTarget(target); err == nil {
			t.Errorf("validatePushTarget(%+v) должен ошибиться", tf)
		}
	}
	// happy path
	tf := targetFlags{SIDs: "h1,h2"}
	target, _ := tf.resolve()
	if err := validatePushTarget(target); err != nil {
		t.Errorf("validatePushTarget(SIDs): %v", err)
	}
}

// TestParseServiceScenario — the `<service>/<scenario>` format.
func TestParseServiceScenario(t *testing.T) {
	svc, sc, err := parseServiceScenario("redis/create")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if svc != "redis" || sc != "create" {
		t.Errorf("got (%q,%q)", svc, sc)
	}

	bad := []string{"redis", "", "/", "redis/", "/create", "redis/sub/path"}
	for _, b := range bad {
		if _, _, err := parseServiceScenario(b); err == nil {
			t.Errorf("parseServiceScenario(%q) должен ошибиться", b)
		}
	}
}

// TestAutoDetectIncarnation — exactly one → ok; 0 or N → error.
func TestAutoDetectIncarnation(t *testing.T) {
	cases := []struct {
		items   []map[string]any
		wantErr bool
		wantOK  string
	}{
		{items: []map[string]any{{
			"name": "redis-prod", "service": "redis", "service_version": "v",
			"state_schema_version": 1, "covens": []string{}, "status": "ready",
			"created_by_aid": "x", "created_at": "t", "updated_at": "t",
		}}, wantOK: "redis-prod"},
		{items: []map[string]any{}, wantErr: true},
		{items: []map[string]any{
			{"name": "a", "service": "redis", "service_version": "v", "state_schema_version": 1, "covens": []string{}, "status": "ready", "created_by_aid": "x", "created_at": "t", "updated_at": "t"},
			{"name": "b", "service": "redis", "service_version": "v", "state_schema_version": 1, "covens": []string{}, "status": "ready", "created_by_aid": "x", "created_at": "t", "updated_at": "t"},
		}, wantErr: true},
	}
	for _, tc := range cases {
		items := tc.items
		_, cl := fakeServer(t, map[string]http.HandlerFunc{
			"/v1/incarnations": func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"items": items, "offset": 0, "limit": 50, "total": len(items),
				})
			},
		})
		got, err := autoDetectIncarnation(context.Background(), cl, "redis")
		if tc.wantErr {
			if err == nil {
				t.Errorf("autoDetect(%d items): ожидалась ошибка", len(items))
			}
			continue
		}
		if err != nil {
			t.Errorf("autoDetect: %v", err)
		}
		if got != tc.wantOK {
			t.Errorf("autoDetect: got %q want %q", got, tc.wantOK)
		}
	}
}

// TestRunScenario_AutoDetect_Many — smoke test: the error contains the list.
func TestRunScenario_AutoDetect_Many(t *testing.T) {
	called := int32(0)
	_, cl := fakeServer(t, map[string]http.HandlerFunc{
		"/v1/incarnations": func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&called, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"name": "redis-a", "service": "redis", "service_version": "v", "state_schema_version": 1, "covens": []string{}, "status": "ready", "created_by_aid": "x", "created_at": "t", "updated_at": "t"},
					{"name": "redis-b", "service": "redis", "service_version": "v", "state_schema_version": 1, "covens": []string{}, "status": "ready", "created_by_aid": "x", "created_at": "t", "updated_at": "t"},
				},
				"offset": 0, "limit": 50, "total": 2,
			})
		},
	})
	_, err := autoDetectIncarnation(context.Background(), cl, "redis")
	if err == nil {
		t.Fatal("ожидалась ошибка о нескольких incarnation")
	}
	if !strings.Contains(err.Error(), "redis-a") || !strings.Contains(err.Error(), "redis-b") {
		t.Errorf("ошибка должна содержать список: %q", err.Error())
	}
}

// TestIsVoyageTerminal — Voyage status predicates.
func TestIsVoyageTerminal(t *testing.T) {
	terminal := []string{"succeeded", "failed", "partial_failed", "cancelled"}
	for _, s := range terminal {
		if !isVoyageTerminal(s) {
			t.Errorf("%q должен быть terminal", s)
		}
	}
	for _, s := range []string{"pending", "running", "scheduled", "", "unknown"} {
		if isVoyageTerminal(s) {
			t.Errorf("%q НЕ должен быть terminal", s)
		}
	}
}
