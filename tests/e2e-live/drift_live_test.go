//go:build e2e_live

// L3b-6 E2E: Scry / check-drift on a REAL soul (ADR-031 Slice B).
//
// Closes the one remaining gap in the drift goal of Phase 4 clean-room: L3a
// (tests/e2e/drift_test.go) runs drift through a stub Plan (stub.SetDryRunPlan),
// the real SoulModule.Plan of the core module is NOT called. Here it's the opposite: Keeper
// renders scenario/converge, dispatches ApplyRequest{dry_run:true} to a real
// soul container, Soul calls core.file.Plan (PlanReadSafe, pure-read) and
// compares desired content vs the actual file on the host.
//
// Service is hello-world (core.file.present greeting file /tmp/soul-stack-hello),
// lightweight: ~60s vs ~5min for nginx (no apt-install). This also gives clean-room
// parity with the getting-started path (same service as in the quickstart doc).
//
// Flow:
//  1. NewStack + 1 real soul (Debian-12 systemd-PID-1).
//  2. apply create (input.greeting) -> WaitApplySuccess -> file on the host.
//  3. AssertHostFileExists/Content - real apply result on the host.
//  4. MUTATE via container.Exec: overwrite the file with different content.
//  5. CheckDrift(greeting=<same>) -> drifted=true (real Plan saw the discrepancy).
//  6. re-apply create -> CheckDrift -> drifted=false (file restored, clean).
//
// Why it catches regressions L3a doesn't catch:
//   - real core.file.Plan regressed (e.g. Plan(present) stopped
//     comparing content) -> drifted=false after mutate -> test fails;
//   - DryRun doesn't reach the soul container on the wire -> Soul does Apply instead of
//     Plan -> mutates the file, the drift branch never triggers;
//   - converge-render/dispatch/driftBarrier broke on a real roster.
package e2e_live_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bDriftLive_HelloWorld(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/hello-world",
		ServiceName: "hello-world",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("expected 1 soul container, got %d", got)
	}

	const (
		greetingFile = "/tmp/soul-stack-hello"
		greeting     = "hello from soul-stack"
	)

	const incName = "test-hello-drift"

	// Membership BEFORE Create: the roster resolves members via incarnation_membership
	// (ADR-008 amendment/NIM-124, topology/resolver.go::rosterSQL); without it the scenario sees no_hosts.
	stack.AddMember(t, 0, incName)

	// POST /v1/incarnations auto-runs create and returns its apply_id.
	// A separate RunScenario(create) would be rejected by the lock gate ("incarnation
	// already in status applying") - we wait for the apply_id of the auto-create run specifically.
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "hello-world@main", map[string]any{
		"greeting": greeting,
	})
	// hello-world - no apt: render -> core.file.present -> RunResult is fast.
	stack.WaitApplySuccess(t, applyID, 60)
	// apply_runs success != state committed - wait for ready before reading state.
	stack.WaitIncarnationReady(t, inc, 30)

	// Real apply result on the host: file created with the given content.
	stack.AssertHostFileExists(t, 0, greetingFile)
	stack.AssertHostFileContent(t, 0, greetingFile, greeting)

	// state-commit recorded the path (like L3a, but via a real RunResult).
	stack.AssertIncarnationState(t, "test-hello-drift", map[string]any{
		"greeting_file": greetingFile,
	})

	// Sanity: clean baseline before mutation - converge sees a match, no drift.
	// This separates "Plan honestly works" from "Plan always sees drift".
	baseline := stack.CheckDrift(t, "test-hello-drift", map[string]any{"greeting": greeting})
	assertSingleHostStatus(t, baseline, stack.SoulContainers[0].SID, "clean")
	if baseline.Summary.HostsClean != 1 || baseline.Summary.HostsDrifted != 0 {
		t.Fatalf("baseline CheckDrift: expected clean=1 drifted=0; summary=%+v", baseline.Summary)
	}

	// MUTATE: overwrite the greeting file with different content directly in the container.
	// core.file.Plan(present) on the next check-drift will compare desired content
	// (greeting) vs actual ("tampered") and return changed=true.
	mutateHostFile(t, stack, 0, greetingFile, "tampered out-of-band\n")

	// CheckDrift -> drifted: the REAL core.file.Plan saw the content discrepancy.
	drifted := stack.CheckDrift(t, "test-hello-drift", map[string]any{"greeting": greeting})
	if drifted.CheckedAt.IsZero() {
		t.Fatalf("drifted DriftReport.checked_at is empty: %+v", drifted)
	}
	if drifted.ScenarioRef != "converge" {
		t.Fatalf("drifted DriftReport.scenario_ref=%q, expected converge", drifted.ScenarioRef)
	}
	h := assertSingleHostStatus(t, drifted, stack.SoulContainers[0].SID, "drifted")
	gotChanged := false
	for _, tk := range h.Tasks {
		if tk.Changed {
			gotChanged = true
			if tk.Module != "core.file.present" {
				t.Fatalf("drifted task module=%q, expected core.file.present: %+v", tk.Module, tk)
			}
		}
	}
	if !gotChanged {
		t.Fatalf("drifted DriftReport: no changed task at all (Plan didn't see drift?); tasks=%+v", h.Tasks)
	}
	if drifted.Summary.HostsDrifted != 1 || drifted.Summary.HostsClean != 0 {
		t.Fatalf("drifted CheckDrift: expected drifted=1 clean=0; summary=%+v", drifted.Summary)
	}

	// re-apply create - restores the file to the desired content.
	reApplyID := stack.RunScenario(t, inc, "create", map[string]any{
		"greeting": greeting,
	})
	stack.WaitApplySuccess(t, reApplyID, 60)
	stack.AssertHostFileContent(t, 0, greetingFile, greeting)

	// CheckDrift -> clean: file restored, the real Plan sees no discrepancy.
	clean := stack.CheckDrift(t, "test-hello-drift", map[string]any{"greeting": greeting})
	hc := assertSingleHostStatus(t, clean, stack.SoulContainers[0].SID, "clean")
	for _, tk := range hc.Tasks {
		if tk.Changed {
			t.Fatalf("clean DriftReport: changed task after re-apply: %+v", tk)
		}
	}
	if clean.Summary.HostsClean != 1 || clean.Summary.HostsDrifted != 0 {
		t.Fatalf("clean CheckDrift: expected clean=1 drifted=0; summary=%+v", clean.Summary)
	}
}

// assertSingleHostStatus verifies the report has exactly one host with the expected
// SID and status, and returns it for further inspection of tasks.
func assertSingleHostStatus(t *testing.T, rep harness.DriftReport, wantSID, wantStatus string) harness.DriftHostReport {
	t.Helper()
	if len(rep.Hosts) != 1 {
		t.Fatalf("DriftReport.hosts: len=%d, expected 1; hosts=%+v", len(rep.Hosts), rep.Hosts)
	}
	h := rep.Hosts[0]
	if h.SID != wantSID {
		t.Fatalf("DriftReport.hosts[0].sid=%q, expected %q", h.SID, wantSID)
	}
	if h.Status != wantStatus {
		t.Fatalf("DriftReport.hosts[0].status=%q, expected %q; host=%+v", h.Status, wantStatus, h)
	}
	return h
}

// mutateHostFile overwrites a file inside the soul container out-of-band (bypassing
// scenario-apply) - simulates drift that the Scry check must catch.
func mutateHostFile(t *testing.T, s *harness.Stack, soulIdx int, path, content string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sc := s.SoulContainers[soulIdx]
	out, code, err := sc.Exec(ctx, []string{
		"/bin/sh", "-c", "printf %s " + shellSingleQuote(content) + " > " + shellSingleQuote(path),
	})
	if err != nil || code != 0 {
		t.Fatalf("mutateHostFile(%s): exec code=%d err=%v output=%s", path, code, err, out)
	}
}

// shellSingleQuote - POSIX single-quote escaping for safely substituting
// test literals into `/bin/sh -c` (path/content come from the test, not user input).
func shellSingleQuote(s string) string {
	out := "'"
	for _, r := range s {
		if r == '\'' {
			out += `'\''`
			continue
		}
		out += string(r)
	}
	return out + "'"
}
