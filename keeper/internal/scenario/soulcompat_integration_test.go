//go:build integration

// Soul-side engine-compat guards (ADR-0076(i), NIM-161). The mode being closed
// here is the dangerous one of ADR-0076: an old Soul that does not implement a
// module the plan uses reads the params it knows by key and reports OK/CHANGED
// with NO EFFECT. Every guard therefore asserts two things — the run is rejected,
// AND no ApplyRequest was sent at all (fail-closed BEFORE dispatch, not a
// silently-successful apply).

package scenario

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// recordingSoulCap wraps [stubSoulCap] and records which (capability, sids) pairs
// the gate asked about — the proof that keeper derives the requirement from the
// RENDERED plan rather than asking for a fixed set.
type recordingSoulCap struct {
	inner stubSoulCap
	mu    sync.Mutex
	asked map[string][]string
}

func (r *recordingSoulCap) SoulsLackingCapability(ctx context.Context, sids []string, capability string) ([]string, error) {
	r.mu.Lock()
	if r.asked == nil {
		r.asked = map[string][]string{}
	}
	r.asked[capability] = append(r.asked[capability], sids...)
	r.mu.Unlock()
	return r.inner.SoulsLackingCapability(ctx, sids, capability)
}

func (r *recordingSoulCap) askedFor(capability string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asked[capability]
}

// TestIntegration_ModuleNotAnnounced_RejectedBeforeDispatch — ★ THE SILENT-IGNORE
// GUARD (ADR-0076(i)). The scenario runs core.exec.run on both hosts; host-b
// announced everything EXCEPT that module (a binary that predates it). Dispatching
// there would have the task report OK with no effect. ASSERT: ERROR_LOCKED with
// reason soul_capability_unsupported, the message names host-b and tells the
// operator to update the binary, and NOT ONE ApplyRequest went out — including to
// host-a, which was fine (the run is one unit of work; half-applying it is the
// drift the gate exists to prevent).
func TestIntegration_ModuleNotAnnounced_RejectedBeforeDispatch(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	cap := &recordingSoulCap{inner: stubSoulCap{
		lackingByCap: map[string][]string{
			config.ModuleCapability("core.exec"): {"host-b.example.com"},
		},
	}}
	r := newRunnerWithSoulCap(t, disp, cap)

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

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonSoulCapabilityUnsupported {
		t.Fatalf("reason = %v, want %s", inc.StatusDetails["reason"], reasonSoulCapabilityUnsupported)
	}
	errText, _ := inc.StatusDetails["error"].(string)
	for _, want := range []string{"host-b.example.com", "update the soul binary", "module:core.exec"} {
		if !strings.Contains(errText, want) {
			t.Errorf("status_details.error = %q, does not mention %q", errText, want)
		}
	}

	// ★ Rejected BEFORE dispatch: no ApplyRequest at all, not a silent OK.
	if disp.calls != 0 {
		t.Fatalf("* SendApply calls = %d, want 0 (rejection must precede any dispatch)", disp.calls)
	}

	// The requirement came from the rendered plan: keeper asked exactly about the
	// module the scenario uses, for both target hosts.
	asked := cap.askedFor(config.ModuleCapability("core.exec"))
	if len(asked) != 2 {
		t.Errorf("asked about module:core.exec for %v, want both target hosts", asked)
	}
}

// TestIntegration_ModuleAnnounced_Dispatched — the other half: a fleet that
// announces what the plan uses runs normally. Without this, a gate that rejected
// everything would look "correct" to the guard above.
func TestIntegration_ModuleAnnounced_Dispatched(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithSoulCap(t, disp, stubSoulCap{})

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
		t.Fatalf("SendApply calls = %d, want 1 (an announcing host must not be blocked)", disp.calls)
	}
}

// TestIntegration_OldSoulEmptyAnnouncement_NotSilentlyDispatched — ★ the pre-ADR-056
// binary: it sends Hello with NO capabilities at all, so keeper's heartbeat hash
// holds an empty set and every check comes back "lacking". Such a host must not
// slip through on the theory that "no announcement means an old-but-fine binary" —
// that assumption is exactly what makes the ignore silent.
func TestIntegration_OldSoulEmptyAnnouncement_NotSilentlyDispatched(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	// Announced nothing → lacking every capability asked of it.
	r := newRunnerWithSoulCap(t, disp, stubSoulCap{lacking: []string{"host-a.example.com"}})

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

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonSoulCapabilityUnsupported {
		t.Fatalf("reason = %v, want %s", inc.StatusDetails["reason"], reasonSoulCapabilityUnsupported)
	}
	if disp.calls != 0 {
		t.Fatalf("* SendApply calls = %d, want 0 (an unannouncing binary must not be dispatched to)", disp.calls)
	}
}

// TestIntegration_NilSoulCap_FailClosed — no Redis, so no presence source to
// confirm support against. A non-staged run is rejected too (the ADR-056 §S5 gate
// only covered staged): guessing "probably supported" is what the ADR-0076 axis
// exists to stop.
func TestIntegration_NilSoulCap_FailClosed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerWithSoulCap(t, disp, nil) // no Redis checker.

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

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != reasonSoulCapabilityUnsupported {
		t.Fatalf("reason = %v, want %s (nil soulCap → fail-closed)", inc.StatusDetails["reason"], reasonSoulCapabilityUnsupported)
	}
	if disp.calls != 0 {
		t.Fatalf("* SendApply calls = %d, want 0 (no presence source -> reject BEFORE dispatch)", disp.calls)
	}
}

// newDriftRunnerWithSoulCap — [newDriftRunner] with an explicit checker, for the
// dry_run guard below.
func newDriftRunnerWithSoulCap(t *testing.T, cap SoulCapabilityChecker) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		SoulCap:        cap,
		Loader:         artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:       topology.NewResolver(integrationPool, nil, nil),
		Essence:        essence.NewResolver(nil),
		Render:         render.NewPipeline(nil, engine, nil, nil),
		Outbound:       fakeDispatcher{},
		DB:             integrationPool,
		AcolyteEnabled: true,
		KID:            "keeper-drift-soulcap-test",
		PollInterval:   20 * time.Millisecond,
		RunTimeout:     30 * time.Second,
	})
}

// TestIntegration_CheckDrift_DryRunNotAnnounced_Rejected — ★ check-drift promises
// a PURE READ (ADR-031): keeper never calls module.Apply, it calls Plan. A binary
// that ignores ApplyRequest.dry_run would apply for real — a read operation
// mutating the fleet. So `dry_run` is required of EVERY roster host before a
// single planned row is written. ASSERT: CheckDrift returns an error naming the
// host, and no apply_runs row was inserted.
func TestIntegration_CheckDrift_DryRunNotAnnounced_Rejected(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := noopServiceRepoWithConverge(t, nil)

	r := newDriftRunnerWithSoulCap(t, stubSoulCap{
		lackingByCap: map[string][]string{config.CapabilityDryRun: {"host-a.example.com"}},
	})
	applyID := audit.NewULID()
	_, err := r.CheckDrift(context.Background(), CheckDriftSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		StartedByAID:    "archon-alice",
	})
	if err == nil {
		t.Fatal("a host that did not announce dry_run must not be asked to check drift - it would apply for real")
	}
	for _, want := range []string{"host-a.example.com", config.CapabilityDryRun} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, does not mention %q", err, want)
		}
	}
	st, serr := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if serr != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", serr)
	}
	if len(st) != 0 {
		t.Fatalf("* apply_runs rows = %+v, want none (rejection must precede dispatch)", st)
	}
}
