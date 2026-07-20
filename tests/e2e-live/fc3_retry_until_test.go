//go:build e2e_live

// FC-3 L3b: retry:+until: health-gate against REAL timing on real-apply.
//
// The gap this test closes. L3a-stub (tests/e2e/) returns the final RunResult
// immediately - the Soul daemon retry loop (runTaskWithRetry, applyrunner.go) is NOT
// exercised, and it's not proven that until: is evaluated against the real service
// startup (rather than matching on the 1st attempt). Intermediate attempts are NOT
// emitted externally (contract: "one TaskEvent per task_idx"), so retries are invisible via
// apply_runs / audit_log / register. Here we use a GENUINE soul binary in a Debian-12
// container, the probe hits a deterministic gate that's physically not ready on the
// 1st attempt.
//
// Service tests/e2e-live/fc3-retry-until (NOT examples/** - WIP zone). scenario
// create:
//   - Task 0 (prepare): writes the start epoch to the host (/tmp/fc3-start) and a ready
//     epoch in the FUTURE (now + gateDelaySec -> /tmp/fc3-ready-at). Gate without a
//     background process/race: "ready" = wall-clock time caught up to the ready epoch.
//   - Task 1 (probe): core.cmd.shell with retry:{count:20, delay:2s,
//     until: register.self.stdout == 'READY'}. While now < ready-at -> NOTYET
//     (until is false -> delay -> retry); now >= ready-at -> READY + writes its OWN
//     done epoch to /tmp/fc3-done. The probe's 1st attempt runs right after prepare,
//     so it MUST see NOTYET -> a real retry.
//
// ASSERT (* proof on real-apply):
//  1. probe eventually OK (until matched), apply_runs success - until converged on
//     real timing (didn't fail early, didn't exhaust the loop).
//  2. * until actually took >=2 attempts - TWO independent ways to check:
//     (a) EXACT: soul_apply_task_retries_total (retry counter starting at the 2nd attempt,
//     applyrunner.go runTaskWithRetry) >= 1, scraped container-side via
//     Exec(curl 127.0.0.1:9091/metrics). >=1 => attempt #2+ actually ran.
//     (b) TIMING (corroboration): done epoch - start epoch >= gateDelaySec.
//     On the 1st attempt (immediately after prepare) the difference would be ~0; >= gate
//     => a real retry budget elapsed between prepare and the success probe.
//  3. * Defects: probe-register stdout != READY on success / done epoch
//     missing / counter == 0 on success -> real retry:until defect
//     (until doesn't retry / register.self isn't updated between attempts /
//     the metric isn't incrementing). Each gets its own t.Fatal with a diagnosis.
package e2e_live_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// gateDelaySec - health-gate readiness delay (scenario prepare: ready-at =
// now + 6). probe-delay 2s x count 20 = 40s budget with margin. 6s guarantees that
// the 1st (immediate) and 2nd (~+2s) probe attempts see NOTYET - until MUST
// retry. Must match `+ 6` in scenario/create/main.yml::prepare.
const gateDelaySec = 6

func TestFC3RetryUntil_HealthGateOnRealTiming(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "tests/e2e-live/fc3-retry-until",
		ServiceName: "fc3-retry-until",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("expected 1 soul container, got %d", got)
	}
	const wantSID = "soul-live-a.example.com"
	if got := stack.SoulContainers[0].SID; got != wantSID {
		t.Fatalf("SoulContainers[0].SID = %q, expected %q", got, wantSID)
	}

	const incName = "fc3-retry-until"

	// Membership BEFORE Create: the roster resolves members via incarnation_membership
	// (ADR-008 amendment/NIM-124). Without it the scenario sees no_hosts -> zero apply_runs rows.
	stack.AddMember(t, 0, incName)

	// POST /v1/incarnations auto-runs the create scenario and returns apply_id.
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "fc3-retry-until@main", nil)

	// ── (1) probe eventually OK, apply_runs success ─────────────────────────────
	// Budget: gate 6s + retry loop up to 40s + container cold-start. 90s with margin.
	// success => until matched (exhaustion would give FAILED flowcontrol.until_exhausted
	// -> WaitApplySuccess would fail on terminal failed).
	stack.WaitApplySuccess(t, applyID, 90)
	stack.WaitIncarnationReady(t, inc, 30)

	// ── (2a) * EXACT: soul_apply_task_retries_total >= 1 ─────────────────────
	// The counter is incremented by runTaskWithRetry on EVERY attempt starting at the 2nd
	// (applyrunner.go: `if attempt > 1 { r.metrics.ObserveRetry() }`). >=1 =>
	// attempt #2 actually ran -> until didn't match on the 1st. Scrape container-
	// side: metrics port on loopback (not exposed externally).
	retries := scrapeSoulMetricSum(t, stack, 0, "soul_apply_task_retries_total")
	if retries < 1 {
		t.Fatalf("* DEFECT retry:until: soul_apply_task_retries_total = %v, expected >=1 - "+
			"probe matched until on the 1st attempt (gate didn't trigger / until doesn't retry)", retries)
	}
	t.Logf("FC-3: soul_apply_task_retries_total = %v (>=1 -> a retry actually happened)", retries)

	// ── (2b) * TIMING (corroboration): done - start >= gateDelaySec ──────────
	// probe wrote the done epoch on the READY branch; prepare wrote the start epoch. The
	// difference is wall-clock time from prepare to the probe's success attempt. >= gate
	// => a real retry budget elapsed between them (on the 1st attempt it would be ~0).
	startEpoch := readHostEpochFile(t, stack, 0, "/tmp/fc3-start")
	doneEpoch := readHostEpochFile(t, stack, 0, "/tmp/fc3-done") // DEFECT if the file is missing: probe never reached READY
	elapsed := doneEpoch - startEpoch
	if elapsed < gateDelaySec {
		t.Fatalf("* DEFECT retry:until: done-start = %d s, expected >= %d s - "+
			"probe matched READY before the gate opened (until was computed against a STALE register / gate logic is broken)",
			elapsed, gateDelaySec)
	}
	t.Logf("FC-3: done-start = %d s (>= gate %d s -> until converged on real timing)", elapsed, gateDelaySec)

	// ── (3) * Defect backstop: register.self.stdout probe == READY on success ─
	// register is persisted by keeper from the TaskEvent.register_data of the LAST attempt
	// (contract of runTaskWithRetry: only the final attempt is emitted externally). On success
	// the final attempt MUST be READY - otherwise until matched on a non-READY value
	// (register.self wasn't updated between attempts - a real defect).
	// plan_index of probe = 1 (task 0 prepare, task 1 probe; single Passage).
	stack.AssertTaskRegisterField(t, applyID, wantSID, 1, "stdout", "READY")
}

// scrapeSoulMetricSum reads the sum of a prometheus metric from the soul_* listener INSIDE
// the soul container via Exec(curl loopback). The soul metrics port (127.0.0.1:9091,
// metrics.enabled in harness soul.yml) is not published externally - scraping is
// container-side only. Parser - a simple summing grep by metric name (a counter with no
// labels -> single line `<name> <value>`); HELP/TYPE lines (`# `) are skipped.
//
// Its own helper (not shared harness.AssertMetricGE): that one scrapes keeper metrics over
// HTTP from the host (Stack.MetricsURL), here it's soul metrics from the container.
func scrapeSoulMetricSum(t *testing.T, stack *harness.Stack, soulIdx int, metric string) float64 {
	t.Helper()
	sc := stack.SoulContainers[soulIdx]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, code, err := sc.Exec(ctx, []string{
		"curl", "-fsS", "http://127.0.0.1:9091/metrics",
	})
	if err != nil || code != 0 {
		t.Fatalf("scrapeSoulMetricSum(%s): curl /metrics: code=%d err=%v out=%s", metric, code, err, out)
	}

	var sum float64
	found := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Counter with no labels: `soul_apply_task_retries_total 3`. With labels
		// it would be `name{...} v` - startsWith on the name + next char ' '/'{'.
		if !strings.HasPrefix(line, metric) {
			continue
		}
		rest := line[len(metric):]
		if rest == "" || (rest[0] != ' ' && rest[0] != '{') {
			continue // a different metric with the same prefix (..._total vs ..._total_sum)
		}
		fields := strings.Fields(line)
		v, perr := strconv.ParseFloat(fields[len(fields)-1], 64)
		if perr != nil {
			t.Fatalf("scrapeSoulMetricSum(%s): parse value %q: %v", metric, fields[len(fields)-1], perr)
		}
		sum += v
		found = true
	}
	if !found {
		t.Fatalf("scrapeSoulMetricSum(%s): metric not found in soul /metrics:\n%s", metric, out)
	}
	return sum
}

// readHostEpochFile reads a unix epoch (integer) from a file on the soul container's HOST
// via Exec(cat). File missing / non-numeric -> t.Fatal with a diagnosis (for
// /tmp/fc3-done, missing means the probe never reached READY - a real retry:until defect).
func readHostEpochFile(t *testing.T, stack *harness.Stack, soulIdx int, path string) int64 {
	t.Helper()
	sc := stack.SoulContainers[soulIdx]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, code, err := sc.Exec(ctx, []string{"cat", path})
	if err != nil {
		t.Fatalf("readHostEpochFile(%s): exec: %v out=%s", path, err, out)
	}
	if code != 0 {
		t.Fatalf("* readHostEpochFile(%s): file is missing (exit=%d) - probe didn't write the marker "+
			"(never reached the READY branch / gate task didn't run)", path, code)
	}
	epoch, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		t.Fatalf("readHostEpochFile(%s): non-numeric content %q: %v", path, strings.TrimSpace(out), perr)
	}
	return epoch
}
