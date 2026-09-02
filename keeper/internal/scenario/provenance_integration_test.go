//go:build integration

// Engine provenance end-to-end (ADR-0076(l), NIM-162): after a real run, the
// rows it wrote say WHICH ENGINES executed it — the keeper build that rendered
// each row and the agent version each was dispatched to — and the state it
// committed says which engine contract it was produced under.
//
// The stamp is a record, never a rule: every guard here also checks that
// dispatch went ahead exactly as before, including when the provenance source is
// broken or missing. A run must never fail because an audit field could not be
// read.

package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fixedSoulVersion answers every host with the same announced version; err
// simulates a Redis outage on the read.
type fixedSoulVersion struct {
	version string
	err     error
}

func (f fixedSoulVersion) ReadSoulVersion(context.Context, string) (string, error) {
	return f.version, f.err
}

// newRunnerWithProvenance — a Runner wired the way production is: this
// instance's build version (the renderer) plus the announced-version reader.
func newRunnerWithProvenance(t *testing.T, disp ApplyDispatcher, keeperVersion string, sv SoulVersionReader) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:        artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:      topology.NewResolver(integrationPool, nil, nil),
		ServiceVars:   servicevars.NewResolver(nil),
		Render:        render.NewPipeline(nil, engine, nil, nil),
		Outbound:      disp,
		DB:            integrationPool,
		SoulCap:       stubSoulCap{},
		SoulVersion:   sv,
		KeeperVersion: keeperVersion,
		PollInterval:  20 * time.Millisecond,
		RunTimeout:    20 * time.Second,
	})
}

// compatServiceRepo — the noop service with a declared keeper window, so the run
// has an effective window to record. compatBlock is spliced into service.yml as
// written (empty = no compat: key at all).
func compatServiceRepo(t *testing.T, compatBlock string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `description: noop service for the engine-provenance integration test
`+compatBlock+`state_schema: {}
`)
	write("scenario/create/main.yml", `name: create
description: smoke core.exec.run
tasks:
  - name: Echo hello on every host
    module: core.exec.run
    params:
      cmd: echo
      args: ["hello"]
    changed_when: "false"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init noop", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

func readRunProvenance(t *testing.T, applyID, sid string) (keeperVersion, soulVersion *string) {
	t.Helper()
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT keeper_version, soul_version FROM apply_runs WHERE apply_id = $1 AND sid = $2`,
		applyID, sid).Scan(&keeperVersion, &soulVersion); err != nil {
		t.Fatalf("read apply_runs provenance (%s/%s): %v", applyID, sid, err)
	}
	return keeperVersion, soulVersion
}

func readIncarnationStamp(t *testing.T, name string) *incarnation.EngineCompat {
	t.Helper()
	var raw []byte
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT engine_compat FROM incarnation WHERE name = $1`, name).Scan(&raw); err != nil {
		t.Fatalf("read incarnation.engine_compat: %v", err)
	}
	if raw == nil {
		return nil
	}
	var out incarnation.EngineCompat
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode engine_compat %q: %v", raw, err)
	}
	return &out
}

// TestIntegration_ApplyRunsCarryEngineVersions — ★ THE ACCEPTANCE GUARD: after a
// run, every host row names both engines. Per-host by construction: an estate is
// heterogeneous, and one aggregate version for the run would be a lie the moment
// two agents differ.
func TestIntegration_ApplyRunsCarryEngineVersions(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := compatServiceRepo(t, "")

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithProvenance(t, disp, "v0.2.0", fixedSoulVersion{version: "v0.1.9"})

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		kv, sv := readRunProvenance(t, applyID, sid)
		if kv == nil || *kv != "v0.2.0" {
			t.Errorf("* %s: keeper_version = %v, want v0.2.0 (the instance that rendered)", sid, kv)
		}
		if sv == nil || *sv != "v0.1.9" {
			t.Errorf("* %s: soul_version = %v, want v0.1.9 (the agent that applied)", sid, sv)
		}
	}
}

// TestIntegration_EngineCompatStampedOnIncarnation — the effective window and the
// required capability set land on the state the run produced, so a later reader
// sees the engine contract this state was made under without re-resolving the
// definition at refs that may since have moved.
func TestIntegration_EngineCompatStampedOnIncarnation(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := compatServiceRepo(t, "compat:\n  keeper:\n    min: \"0.1.0\"\n    max: \"0.3.0\"\n")

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithProvenance(t, disp, "v0.2.0", fixedSoulVersion{version: "v0.1.9"})

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	stamp := readIncarnationStamp(t, "noop-prod")
	if stamp == nil {
		t.Fatal("* incarnation.engine_compat is NULL after a successful run")
	}
	if stamp.KeeperVersion != "v0.2.0" {
		t.Errorf("keeper_version = %q, want v0.2.0", stamp.KeeperVersion)
	}
	if stamp.KeeperWindow == nil || stamp.KeeperWindow.Min != "0.1.0" || stamp.KeeperWindow.Max != "0.3.0" {
		t.Errorf("keeper_window = %v, want the declared [0.1.0, 0.3.0)", stamp.KeeperWindow)
	}
	if !stamp.WindowEnforced {
		t.Error("window_enforced = false, want true (declared window, comparable build)")
	}
	// The scenario runs core.exec.run, so that is what it needed from the estate.
	found := false
	for _, c := range stamp.SoulCapabilities {
		if c == config.ModuleCapability("core.exec") {
			found = true
		}
	}
	if !found {
		t.Errorf("soul_capabilities = %v, want the module the plan uses", stamp.SoulCapabilities)
	}

	// The same contract is on the transition row, which is what survives a later
	// state change.
	var raw []byte
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT engine_compat FROM state_history WHERE apply_id = $1`, applyID).Scan(&raw); err != nil {
		t.Fatalf("read state_history.engine_compat: %v", err)
	}
	if raw == nil {
		t.Error("state_history row for the run carries no engine contract")
	}
}

// TestIntegration_ProvenanceUnavailable_RunStillDispatches — ★ the degradation
// guard. Redis is down, so the announced version cannot be read. The stamp loses
// a field; the run does NOT lose the apply. The capability gate reading the same
// Hash fails closed on exactly this error — the asymmetry is deliberate, and this
// test is what keeps someone from "fixing" it into symmetry.
func TestIntegration_ProvenanceUnavailable_RunStillDispatches(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := compatServiceRepo(t, "")

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithProvenance(t, disp, "v0.2.0",
		fixedSoulVersion{err: errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")})

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	if disp.calls != 1 {
		t.Fatalf("* SendApply calls = %d, want 1 - an unreadable audit field must not block an apply", disp.calls)
	}
	kv, sv := readRunProvenance(t, applyID, "host-a.example.com")
	if kv == nil || *kv != "v0.2.0" {
		t.Errorf("keeper_version = %v, want v0.2.0 (known locally, unaffected by Redis)", kv)
	}
	if sv != nil {
		t.Errorf("soul_version = %v, want NULL (not recorded, not invented)", sv)
	}
}

// TestIntegration_NoProvenanceWiring_BehavesAsBefore — a Runner with neither a
// version nor a reader (every unit path, and any deployment that has not wired
// them) writes NULLs and runs exactly as it did before migration 103. Nothing in
// the stamp is load-bearing.
func TestIntegration_NoProvenanceWiring_BehavesAsBefore(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := compatServiceRepo(t, "")

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithProvenance(t, disp, "", nil)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	if disp.calls != 1 {
		t.Fatalf("SendApply calls = %d, want 1", disp.calls)
	}
	kv, sv := readRunProvenance(t, applyID, "host-a.example.com")
	if kv != nil || sv != nil {
		t.Errorf("provenance = (%v, %v), want both NULL with nothing wired", kv, sv)
	}
	// A run still requires capabilities, so the stamp is not empty — but it
	// records no version and no window.
	if stamp := readIncarnationStamp(t, "noop-prod"); stamp != nil {
		if stamp.KeeperVersion != "" || stamp.WindowEnforced {
			t.Errorf("stamp = %+v, want no version and nothing enforced", stamp)
		}
	}
}
