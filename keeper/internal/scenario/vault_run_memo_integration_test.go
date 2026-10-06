//go:build integration

// Run-level guards for the run-wide vault() memo (NIM-934). They go through
// run()+PG with the production Vault client against a fake KV v2 server, so both
// halves are the real code: the binding in Runner.run and the forget in
// vault.Client.WriteKV, reached by a real `core.vault.kv-present` step.

package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	coremodvault "github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// memoKV is a KV v2 server on mount `secret` holding what the run reads and writes.
type memoKV struct {
	mu      sync.Mutex
	secrets map[string]map[string]any
	reads   map[string]int
}

func (f *memoKV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/sys/health" {
		_, _ = w.Write([]byte(`{"initialized":true,"sealed":false,"standby":false}`))
		return
	}
	rel, ok := strings.CutPrefix(r.URL.Path, "/v1/secret/data/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		var body struct {
			Data map[string]any `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.secrets[rel] = body.Data
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": 2}})
		return
	}
	f.reads[rel]++
	data, found := f.secrets[rel]
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": data, "metadata": map[string]any{"version": 1}}})
}

func (f *memoKV) readCount(rel string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[rel]
}

func (f *memoKV) value(rel, field string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.secrets[rel][field]
}

func (f *memoKV) remove(rel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.secrets, rel)
}

// lockedBuffer is the run log: the runner logs from its own goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hookKeeperModule records each state's params and runs onApply inside the step —
// what happens to Vault while a keeper-side step runs.
type hookKeeperModule struct {
	module.BaseModule
	mu      sync.Mutex
	got     map[string]map[string]any
	onApply func(state string)
}

func (m *hookKeeperModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	m.mu.Lock()
	if m.got == nil {
		m.got = map[string]map[string]any{}
	}
	m.got[req.GetState()] = req.GetParams().AsMap()
	m.mu.Unlock()
	if m.onApply != nil {
		m.onApply(req.GetState())
	}
	return stream.Send(&pluginv1.ApplyEvent{Changed: true, Output: mustStructAny(map[string]any{"done": true})})
}

func (m *hookKeeperModule) params(state string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.got[state]
}

// runVaultMemoRun starts scenarioMain on noop-prod (all keeper-side, so no hosts)
// with the production Vault client over kv, and waits for the terminal.
func runVaultMemoRun(t *testing.T, kv *memoKV, probe *hookKeeperModule, scenarioMain string, want incarnation.Status) string {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	srv := httptest.NewServer(kv)
	t.Cleanup(srv.Close)
	client, err := keepervault.NewClient(context.Background(), config.KeeperVault{
		Addr: srv.URL, Token: "root", KVMount: "secret", KVVersion: "2",
	})
	if err != nil {
		t.Fatalf("vault.NewClient: %v", err)
	}
	engine, err := cel.New(cel.WithVault(client))
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	logs := &lockedBuffer{}
	r := NewRunner(Deps{
		Loader:      artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:    topology.NewResolver(integrationPool, nil, nil),
		ServiceVars: servicevars.NewResolver(nil),
		Render:      render.NewPipeline(client, engine, nil, nil),
		Outbound:    &mockDispatcher{t: t, result: applyrun.StatusSuccess},
		KeeperModules: fakeKeeperRegistry{
			"fakekeeper.probe": probe,
			"core.vault":       coremodvault.New(client, nil),
		},
		DB:           integrationPool,
		SoulCap:      stubSoulCap{},
		PollInterval: 20 * time.Millisecond,
		RunTimeout:   20 * time.Second,
		Logger:       slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: writeServiceRepo(t, scenarioMain), Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, want)
	return logs.String()
}

// ★ A secret only a finished task read, removed from Vault while the run is still
// going, does not fail the run. The Passage-1 render re-renders that task; with a
// memo per pass the re-read is `render_failed` and the incarnation error_locked.
// One Vault read for the whole run.
func TestIntegration_RunVaultMemo_SecretGoneMidRunDoesNotFailTheRun(t *testing.T) {
	kv := &memoKV{
		secrets: map[string]map[string]any{"shared/gone": {"password": "pw-gone-mid-run"}},
		reads:   map[string]int{},
	}
	probe := &hookKeeperModule{onApply: func(state string) {
		if state == "created" {
			kv.remove("shared/gone")
		}
	}}
	logs := runVaultMemoRun(t, kv, probe, `name: create
description: a secret only a finished task read disappears mid-run
tasks:
  - name: read the secret
    module: fakekeeper.probe.created
    on: keeper
    register: first
    params:
      pw: "${ vault('secret/shared/gone#password') }"
  - name: after it
    module: fakekeeper.probe.delivered
    on: keeper
    params:
      done: "${ register.first.done }"
`, incarnation.StatusReady)

	if got := probe.params("delivered"); got == nil || got["done"] != true {
		t.Fatalf("Passage-1 step params = %v, want done=true (the run did not reach it)", got)
	}
	if got := kv.readCount("shared/gone"); got != 1 {
		t.Errorf("Vault reads of shared/gone across the run = %d, want 1 (one memo for the run)", got)
	}
	if strings.Contains(logs, "pw-gone-mid-run") {
		t.Errorf("the run log carries a secret read through the memo:\n%s", logs)
	}
}

// ★ A keeper-side step that writes Vault mid-run is seen by the next pass. The
// first step reads secret/shared/db, which puts it in the run's memo without a
// `token` field; core.vault.kv-present then generates `token` on that path; the
// Passage-1 step reads `#token`. Without the Vault client forgetting the written
// secret, the memo serves the pre-write map and the render fails on a missing field.
func TestIntegration_RunVaultMemo_KeeperStepWriteIsSeenByTheNextPass(t *testing.T) {
	kv := &memoKV{
		secrets: map[string]map[string]any{"shared/db": {"password": "pw-before-the-write"}},
		reads:   map[string]int{},
	}
	probe := &hookKeeperModule{}
	logs := runVaultMemoRun(t, kv, probe, `name: create
description: a keeper-side step's Vault write is seen by the next pass
tasks:
  - name: read before the write
    module: fakekeeper.probe.created
    on: keeper
    register: first
    params:
      pw: "${ vault('secret/shared/db#password') }"
  - name: generate a token next to it
    module: core.vault.kv-present
    register: k
    params:
      targets:
        - path: secret/shared/db
          field: token
  - name: read after the write
    module: fakekeeper.probe.delivered
    on: keeper
    params:
      pw: "${ vault('secret/shared/db#password') }"
      token: "${ vault('secret/shared/db#token') }"
      generated: "${ register.k.generated }"
`, incarnation.StatusReady)

	token, _ := kv.value("shared/db", "token").(string)
	if token == "" {
		t.Fatal("core.vault.kv-present wrote no token — the step did not run")
	}
	got := probe.params("delivered")
	if got == nil {
		t.Fatal("the Passage-1 step never ran")
	}
	if got["token"] != token {
		t.Errorf("Passage-1 token = %v, want the value the step wrote", got["token"])
	}
	if got["pw"] != "pw-before-the-write" {
		t.Errorf("Passage-1 pw = %v, want the neighbour field kept by the write", got["pw"])
	}
	for _, secret := range []string{"pw-before-the-write", token} {
		if strings.Contains(logs, secret) {
			t.Errorf("the run log carries a secret that went through the memo:\n%s", logs)
		}
	}
}
