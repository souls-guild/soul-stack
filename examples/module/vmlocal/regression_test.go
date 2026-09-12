// One test per defect an independent review of this artifact found. Each names
// the failure it prevents, because the code reads plausible in every one of these
// cases and the tests are the only thing that says otherwise.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func mustFail(t *testing.T, s *applyStream) string {
	t.Helper()
	last := s.last()
	if last == nil {
		t.Fatal("no events emitted")
	}
	if !last.GetFailed() {
		t.Fatalf("action reported SUCCESS: %s", last.GetMessage())
	}
	return last.GetMessage()
}

// ★ A batch where a machine never took a lease is a FAILURE, and the machine gets
// a bare vm_id.
//
// Reporting it complete hands the scenario a roster it cannot use, and the entry
// is the reason: `sidFor` would fall back to `<name>.<namespace>` and manufacture
// a plausible sid for a machine that never announced one — which is precisely the
// field the downstream bootstrap guard keys on, so filling it in disables the
// check that would have caught this.
func TestUnreadyMachineFailsTheStepAndGetsNoFabricatedSid(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	f.leaseAfter["batch-1"] = 1 << 30

	s := apply(t, m, "created", createOwn(nil))
	msg := mustFail(t, s)
	if !strings.Contains(msg, "did not become usable") {
		t.Errorf("message=%q, want it to name what happened", msg)
	}

	hosts := s.hosts(t)
	if len(hosts) != 2 {
		t.Fatalf("hosts=%d, want both machines including the one that failed", len(hosts))
	}
	var stubs int
	for _, h := range hosts {
		if id, _ := h["vm_id"].(string); id == "" {
			t.Error("a host entry carries no vm_id — it cannot be cleaned up")
		}
		if _, hasSid := h["sid"]; !hasSid {
			stubs++
			if len(h) != 1 {
				t.Errorf("the stub for an unready machine carries more than vm_id: %v", h)
			}
		}
	}
	if stubs != 1 {
		t.Errorf("%d entries lack a sid, want exactly 1 — a fabricated sid disables the bootstrap guard", stubs)
	}
}

// ★ A create that fails part-way still hands back the machines it already made.
// A failure event with no output loses them: the caller's cleanup step has
// nothing to work from, and they keep running.
func TestCreateFailurePartWayStillReportsWhatWasMade(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)

	// Two succeed, the third does not.
	var made int
	m.dial = func(string) (hypervisor, error) { return &countingHV{fakeHV: f, failAfter: 2, made: &made}, nil }

	s := apply(t, m, "created", createOwn(map[string]any{"count": float64(3)}))
	msg := mustFail(t, s)
	if !strings.Contains(msg, "create batch-2") {
		t.Errorf("message=%q, want it to name the machine that failed", msg)
	}

	hosts := s.hosts(t)
	if len(hosts) != 2 {
		t.Fatalf("hosts=%d, want the 2 machines that WERE created — without them nobody can tear them down", len(hosts))
	}
	for _, h := range hosts {
		if id, _ := h["vm_id"].(string); id == "" {
			t.Error("a reported machine has no id")
		}
	}
	if !s.last().GetChanged() {
		t.Error("changed=false although two machines were created")
	}
}

// countingHV fails CreateDomain after N successes.
type countingHV struct {
	*fakeHV
	failAfter int
	made      *int
}

func (c *countingHV) CreateDomain(ctx context.Context, spec domainSpec) (domainInfo, error) {
	if *c.made >= c.failAfter {
		return domainInfo{}, errors.New("hypervisor refused")
	}
	*c.made++
	return c.fakeHV.CreateDomain(ctx, spec)
}

// ★ Adoption by name is ANCHORED to `<name>-<digits>`.
//
// An unanchored prefix test adopts a machine from another batch: with
// `name: web`, an unrelated `web-db-0` is counted towards the batch, reported in
// output.hosts, handed to the scenario to bootstrap, and torn down by a later
// `destroyed`.
func TestAnUnrelatedMachineWithASharedPrefixIsNotAdopted(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)

	f.domains["legacy"] = &domainInfo{
		UUID: "legacy", Name: "web-db-0", Namespace: "proofns",
		Running: true, Labels: map[string]string{},
	}

	s := apply(t, m, "created", createOwn(map[string]any{"name": "web", "count": float64(1)}))
	mustSucceed(t, s)

	hosts := s.hosts(t)
	if len(hosts) != 1 {
		t.Fatalf("hosts=%d, want 1", len(hosts))
	}
	if hosts[0]["vm_id"] == "legacy" {
		t.Fatal("a machine from another batch was adopted, reported, and would be bootstrapped and later destroyed")
	}
	if _, still := f.domains["legacy"]; !still {
		t.Error("the unrelated machine disappeared")
	}
	// `web-0` must have been created rather than the batch declared complete.
	if f.idOf(t, "web-0") == "" {
		t.Error("no machine was created: the batch was declared complete by adopting a stranger")
	}
}

// ★ A member of our own batch that is powered down is STARTED, not abandoned and
// not mistaken for a stranger.
//
// This is the case a workstation hits constantly: the host reboots and nothing
// autostarts. Building a sibling instead would leave the dead machine behind for
// every later rerun to ignore as well, and treating it as foreign fails the run
// on a name collision with itself.
func TestAStoppedMemberOfTheBatchIsStarted(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1)})))

	id := f.idOf(t, "batch-0")
	f.domains[id].Running = false
	f.calls = nil

	s := apply(t, m, "created", createOwn(map[string]any{"count": float64(1)}))
	mustSucceed(t, s)

	if len(f.domains) != 1 {
		t.Errorf("domains=%d, want the same one started rather than a sibling built beside it", len(f.domains))
	}
	if !f.domains[id].Running {
		t.Error("the stopped member was left stopped")
	}
	if !containsCall(f.calls, "start "+id) {
		t.Errorf("calls=%v, want a start for the stopped member", f.calls)
	}
	if !s.last().GetChanged() {
		t.Error("changed=false although a machine was started")
	}
}

// A name already held by a machine outside the batch is reported as such, rather
// than colliding inside libvirt with a message about a storage volume.
func TestANameHeldByAForeignMachineIsNamed(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)

	f.domains["squatter"] = &domainInfo{
		UUID: "squatter", Name: "batch-0", Namespace: "proofns",
		Running: true, Labels: map[string]string{runLabelKey: "someone-else"},
	}

	s := apply(t, m, "created", createOwn(map[string]any{"count": float64(1)}))
	msg := mustFail(t, s)
	if !strings.Contains(msg, "is not part of batch") {
		t.Errorf("message=%q, want it to explain the name is taken by another batch", msg)
	}
}

// ★ `resized` publishes `output.results` with one entry per machine — the same
// key and shape the cloud publishes. Emitting no output was a contract divergence
// the param-surface guard cannot see, because that guard only compares INPUTS.
func TestResizedPublishesPerMachineResults(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(nil)))

	s := apply(t, m, "resized", map[string]any{
		"vm_ids":  []any{f.idOf(t, "batch-0"), f.idOf(t, "batch-1")},
		"disk_gb": float64(50),
	})
	mustSucceed(t, s)

	raw, ok := s.last().GetOutput().AsMap()["results"].([]any)
	if !ok {
		t.Fatalf("no results in output: %v — a scenario reading register.<task>.results gets nothing", s.last().GetOutput().AsMap())
	}
	if len(raw) != 2 {
		t.Fatalf("results=%d, want one per machine", len(raw))
	}
	for _, e := range raw {
		r, _ := e.(map[string]any)
		for _, key := range []string{"vm_id", "caused_downtime", "changed"} {
			if _, ok := r[key]; !ok {
				t.Errorf("result entry lacks %q: %v", key, r)
			}
		}
	}
}

// ★ Past the quota precheck, one machine's failure does not abandon the batch —
// and a machine stopped for an update that then failed is STARTED AGAIN.
//
// Returning early leaves it stopped with nobody told which one it was, and the
// machines already resized go unreported.
func TestResizedContinuesPastAFailureAndRestartsTheStoppedMachine(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(3)})))

	bad := f.idOf(t, "batch-1")
	f.failOn["setvcpus:"+bad] = errors.New("hypervisor refused")
	f.calls = nil

	ids := []any{f.idOf(t, "batch-0"), bad, f.idOf(t, "batch-2")}
	s := apply(t, m, "resized", map[string]any{
		"vm_ids": ids, "cpu_cores": float64(4), "allow_downtime": true,
	})
	msg := mustFail(t, s)
	if !strings.Contains(msg, bad) {
		t.Errorf("message=%q, want it to name the machine that failed", msg)
	}

	raw, _ := s.last().GetOutput().AsMap()["results"].([]any)
	if len(raw) != 3 {
		t.Fatalf("results=%d, want one per machine even when one failed", len(raw))
	}
	var withError int
	for _, e := range raw {
		if r, _ := e.(map[string]any); r["error"] != nil {
			withError++
		}
	}
	if withError != 1 {
		t.Errorf("%d entries carry an error, want exactly 1", withError)
	}

	// The two healthy machines were actually resized.
	for _, name := range []string{"batch-0", "batch-2"} {
		if got := f.domains[f.idOf(t, name)].VCPUs; got != 4 {
			t.Errorf("%s has %d vcpus, want 4 — the batch was abandoned at the first error", name, got)
		}
	}
	// And the failed one is not left powered off.
	if !f.domains[bad].Running {
		t.Error("the machine whose update failed was left STOPPED")
	}
	if !containsCall(f.calls, "start "+bad) {
		t.Errorf("calls=%v, want a start-back for the failed machine", f.calls)
	}
}

func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// `probed` reports `state`, which the cloud always sets, and `cluster`, which its
// attributes always carry. A scenario filtering on either resolves to null
// otherwise.
func TestProbedReportsStateAndCluster(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1)})))

	s := apply(t, m, "probed", nil)
	mustSucceed(t, s)
	h := s.hosts(t)[0]
	if h["state"] != "RUNNING" {
		t.Errorf("state=%v, want RUNNING", h["state"])
	}
	attrs, _ := h["attributes"].(map[string]any)
	if _, ok := attrs["cluster"]; !ok {
		t.Error("attributes has no cluster key; the cloud always sets one")
	}

	f.domains[f.idOf(t, "batch-0")].Running = false
	s = apply(t, m, "probed", nil)
	mustSucceed(t, s)
	if got := s.hosts(t)[0]["state"]; got != "STOPPED" {
		t.Errorf("state=%v for a stopped machine, want STOPPED", got)
	}
}

// ★ A lease that cannot be read costs that machine its address, not the whole
// probe its answer. A probe exists precisely to look at machines that are in a
// bad way, and a domain whose metadata lost its network id would otherwise make
// the entire namespace permanently unlistable.
func TestProbedSurvivesALeaseReadError(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(nil)))
	f.failOn["lease:batch-1"] = errors.New("network id is not a UUID")

	s := apply(t, m, "probed", nil)
	mustSucceed(t, s)
	hosts := s.hosts(t)
	if len(hosts) != 2 {
		t.Fatalf("hosts=%d, want both machines — one unreadable lease must not hide the namespace", len(hosts))
	}
}

// A teardown that leaves the disk behind is not confirmed. The next create under
// the same name collides on the volume, with nothing pointing back here.
func TestDestroyReportsAVolumeThatSurvived(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1)})))
	id := f.idOf(t, "batch-0")
	f.failOn["destroy:"+id] = fmt.Errorf("%w: volume /pool/disks/batch-0.qcow2 survives", errVolumeSurvived)

	s := apply(t, m, "destroyed", map[string]any{"vm_ids": []any{id}})
	msg := mustFail(t, s)
	if !strings.Contains(msg, "could not be confirmed") {
		t.Errorf("message=%q, want the teardown reported unconfirmed", msg)
	}
}

// `key_id: ""` is a missing credential written down. The cloud would refuse it,
// so refusing it here keeps the surface from being laxer than the cloud's.
func TestEmptyCredentialsAreRefused(t *testing.T) {
	p := createParams(nil, nil)
	p[connKeyID] = ""
	if errs := validateCreated(p); !hasError(errs, "key_id is required") {
		t.Errorf("errors=%v, want an empty key_id refused", errs)
	}
	p = createParams(nil, nil)
	p[connSecret] = ""
	if errs := validateCreated(p); !hasError(errs, "secret is required") {
		t.Errorf("errors=%v, want an empty secret refused", errs)
	}
}

// ★ `image_version` is REFUSED rather than accepted and ignored. The local
// catalogue is a storage pool with one volume per name, so there is no set of
// versions for a version to pick from — and resolving the one volume anyway would
// leave the operator believing they had pinned something.
func TestImageVersionIsRefusedRatherThanIgnored(t *testing.T) {
	errs := validateCreated(createParams(map[string]any{"image_version": float64(3)}, nil))
	if !hasError(errs, "no versions to pin") {
		t.Errorf("errors=%v, want image_version refused with a reason", errs)
	}
	// Carrying the key at its default asks for nothing, so it still runs.
	if errs := validateCreated(createParams(map[string]any{"image_version": float64(0)}, nil)); len(errs) > 0 {
		t.Errorf("image_version: 0 asks for nothing and must be accepted: %v", errs)
	}
}

// ── second review round ──────────────────────────────────────────────────────

// ★ The id list is complete BEFORE anything is written.
//
// Appending as the loop goes means a failure half-way hands back only the
// machines it happened to have reached: the rest — already existing, already
// running — become invisible to the cleanup step.
func TestAStartFailureStillReportsEveryAdoptedMachine(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(3)})))

	var ids []string
	for _, n := range []string{"batch-0", "batch-1", "batch-2"} {
		id := f.idOf(t, n)
		ids = append(ids, id)
		f.domains[id].Running = false
	}
	// The middle one refuses to start; the first has already been started by then.
	f.failOn["start:"+ids[1]] = errors.New("hypervisor refused")

	s := apply(t, m, "created", createOwn(map[string]any{"count": float64(3)}))
	mustFail(t, s)

	hosts := s.hosts(t)
	if len(hosts) != 3 {
		t.Fatalf("hosts=%d, want all three adopted machines — the two the loop never reached are still ours", len(hosts))
	}
	seen := map[string]bool{}
	for _, h := range hosts {
		id, _ := h["vm_id"].(string)
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("%s is missing from output.hosts and cannot be cleaned up", id)
		}
	}
	if !s.last().GetChanged() {
		t.Error("changed=false although the first machine WAS started")
	}
}

// ★ A transient error on the verification lookup must not outlive the round that
// clears it: the teardown completed, and reporting it unconfirmed sends an
// operator after a machine that is already gone.
func TestATransientLookupErrorDoesNotFailAConfirmedTeardown(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1)})))
	id := f.idOf(t, "batch-0")

	// The namespace check reads it fine (call 1); the destroy lands; the lookup
	// that would CONFIRM it (call 2) blips once, and the round after that sees it
	// gone.
	f.failLookupAt[id] = 2

	s := apply(t, m, "destroyed", map[string]any{"vm_ids": []any{id}})
	last := s.last()
	if last == nil {
		t.Fatal("no events")
	}
	if last.GetFailed() {
		t.Errorf("a completed teardown was reported unconfirmed: %s", last.GetMessage())
	}
	if _, still := f.domains[id]; still {
		t.Error("the machine is still there")
	}
}

// ★ The machine name is the cloud's, character for character. On both sides the
// name is what `sid` falls back to when the guest announced none, so a different
// name here is a different sid there.
func TestBatchMemberNameMatchesTheCloud(t *testing.T) {
	if got := batchMemberName("web", "run-7", 0); got != "web-0" {
		t.Errorf("named batch: got %q, want web-0", got)
	}
	if got := batchMemberName("web", "run-7", 3); got != "web-3" {
		t.Errorf("named batch: got %q, want web-3", got)
	}
	// Label-only: the cloud prefixes `soul-`; dropping it is a silent sid change.
	if got := batchMemberName("", "run-7", 0); got != "soul-run-7-0" {
		t.Errorf("label-only batch: got %q, want soul-run-7-0 — the cloud's vmName prefixes `soul-`", got)
	}
}

// A label-only batch names its machines the cloud's way end to end, and adopts
// them again on a rerun.
func TestALabelOnlyBatchUsesTheCloudNamingAndIsIdempotent(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)

	prof := validProfile()
	prof["labels"] = map[string]any{runLabelKey: "run-7"}
	own := map[string]any{"count": float64(1), "profile": prof}

	s := apply(t, m, "created", own)
	mustSucceed(t, s)
	if _, err := f.LookupDomain(f.idOf(t, "soul-run-7-0")); err != nil {
		t.Fatalf("the machine is not named soul-run-7-0: %v", err)
	}

	s = apply(t, m, "created", own)
	mustSucceed(t, s)
	if s.last().GetChanged() {
		t.Error("a rerun of a label-only batch created something instead of adopting")
	}
	if len(f.domains) != 1 {
		t.Errorf("domains=%d, want 1", len(f.domains))
	}
}

// ★ A surviving volume is TERMINAL and survives a retry round.
//
// This is the combination that re-opened the hole: the destroy removes the domain
// and reports the disk survived, then the confirming lookup blips once. On the
// next round `DestroyDomain` answers "already gone" — and if that answer is
// allowed to clear the error, the step reports a confirmed teardown over a disk
// that is still on the host, which wedges the next create under that name on
// "storage volume already exists" with nothing pointing back.
//
// Nothing can re-derive it: once the domain is undefined, nothing names its paths.
func TestASurvivingVolumeIsNotClearedByARetryRound(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1)})))
	id := f.idOf(t, "batch-0")

	f.failOn["destroy:"+id] = fmt.Errorf("%w: volume /pool/disks/batch-0.qcow2 survives", errVolumeSurvived)
	f.failLookupAt[id] = 2 // the confirming lookup blips, forcing a second round

	s := apply(t, m, "destroyed", map[string]any{"vm_ids": []any{id}})
	msg := mustFail(t, s)
	if !strings.Contains(msg, "volumes may survive") && !strings.Contains(msg, "survives") {
		t.Errorf("message=%q, want it to name the surviving volume", msg)
	}
	if _, still := f.domains[id]; still {
		t.Error("the domain is still there, so this exercised the wrong path")
	}
}
