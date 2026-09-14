// The behaviour of the four actions, driven through the object's own dispatch so
// the path under test is the one a Keeper takes.
package main

import (
	"slices"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func apply(t *testing.T, m *VMLocal, state string, own map[string]any) *applyStream {
	t.Helper()
	s := &applyStream{}
	if err := m.vm().Apply(&pluginv1.ApplyRequest{State: state, Params: params(t, own)}, s); err != nil {
		t.Fatalf("Apply(%s) returned a transport error: %v", state, err)
	}
	return s
}

func createOwn(edits map[string]any) map[string]any {
	own := map[string]any{
		"name":    "batch",
		"count":   float64(2),
		"profile": validProfile(),
	}
	for k, v := range edits {
		own[k] = v
	}
	return own
}

func mustSucceed(t *testing.T, s *applyStream) *pluginv1.ApplyEvent {
	t.Helper()
	last := s.last()
	if last == nil {
		t.Fatal("no events emitted")
	}
	if last.GetFailed() {
		t.Fatalf("action failed: %s", last.GetMessage())
	}
	return last
}

// ★ The keys `core.soul.registered` and the bootstrap consumers read, guarded
// against a well-meant "improvement" — and the one that is deliberately absent.
//
// Nesting the address under `network:` — the SOULPRINT's shape, and a plausible
// thing to reach for — makes primary_ip invisible to the consumer while every
// test that only checks sid stays green. That is what the flat assertion is for.
//
// ⚠ bootstrap_token is asserted ABSENT, and that is a known red downstream, not a
// passing contract: minting one needs the Keeper's token store, which a plugin
// has no access to. The assertion is here so that the day someone adds a token to
// this map, they are made to come and delete this comment rather than quietly
// shipping a fake one.
func TestCreatedOutputIsTheShapeTheConsumerReads(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)

	s := apply(t, m, "created", createOwn(map[string]any{"count": float64(1)}))
	mustSucceed(t, s)

	hosts := s.hosts(t)
	if len(hosts) != 1 {
		t.Fatalf("hosts=%v, want one entry", hosts)
	}
	h := hosts[0]
	for _, key := range []string{"sid", "vm_id", "primary_ip"} {
		if v, ok := h[key].(string); !ok || v == "" {
			t.Errorf("hosts[0].%s = %v, want a non-empty string at the TOP level", key, h[key])
		}
	}
	if _, nested := h["network"]; nested {
		t.Error("hosts[0].network must not exist: primary_ip is flat here, and network.primary_ip is the soulprint's shape")
	}
	if _, ok := h["bootstrap_token"]; ok {
		t.Error("hosts[0].bootstrap_token exists — a plugin cannot mint one (no token store, no DB). " +
			"If this is now real, the epic closed the gap and this assertion plus its comment must go; " +
			"if it is a placeholder, the bootstrap consumer will accept a token that redeems nothing.")
	}
	attrs, ok := h["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("hosts[0].attributes = %v, want a map", h["attributes"])
	}
	if attrs["run_label"] != "batch" {
		t.Errorf("attributes.run_label=%v, want the batch identity echoed back", attrs["run_label"])
	}
}

// ★ sid is the hostname the GUEST announced over DHCP, not the name this artifact
// chose. That is what makes it the machine's own account of itself, and it is
// what catches a machine whose cloud-init rewrote its hostname.
func TestSidComesFromTheLeaseHostname(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)

	// The guest announces something other than the domain name.
	s := apply(t, m, "created", createOwn(map[string]any{"count": float64(1)}))
	mustSucceed(t, s)
	first := s.hosts(t)[0]
	mac := f.domains[first["vm_id"].(string)].MAC
	f.leases[mac] = [2]string{"192.168.122.55", "renamed-by-cloud-init"}

	s = apply(t, m, "probed", nil)
	mustSucceed(t, s)
	got := s.hosts(t)[0]
	if got["sid"] != "renamed-by-cloud-init" {
		t.Errorf("sid=%v, want the hostname the guest announced", got["sid"])
	}
	if got["primary_ip"] != "192.168.122.55" {
		t.Errorf("primary_ip=%v, want the leased address", got["primary_ip"])
	}
}

// A rerun of the same batch adopts what it already made and creates nothing.
func TestCreatedIsIdempotentOnTheBatchIdentity(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)

	mustSucceed(t, apply(t, m, "created", createOwn(nil)))
	if len(f.domains) != 2 {
		t.Fatalf("first run made %d domains, want 2", len(f.domains))
	}
	firstCalls := len(f.calls)

	s := apply(t, m, "created", createOwn(nil))
	last := mustSucceed(t, s)
	if len(f.domains) != 2 {
		t.Errorf("second run left %d domains, want 2 — the batch was not adopted", len(f.domains))
	}
	if len(f.calls) != firstCalls {
		t.Errorf("second run issued %v, want no hypervisor writes", f.calls[firstCalls:])
	}
	if last.GetChanged() {
		t.Error("a rerun that created nothing reported changed=true")
	}
	if len(s.hosts(t)) != 2 {
		t.Errorf("a rerun must still report the whole batch, got %d", len(s.hosts(t)))
	}
}

// Growing the batch tops up only what is missing, at the free indexes.
func TestCreatedTopsUpOnlyWhatIsMissing(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)

	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(2)})))
	before := len(f.calls)

	s := apply(t, m, "created", createOwn(map[string]any{"count": float64(3)}))
	mustSucceed(t, s)
	if len(f.domains) != 3 {
		t.Fatalf("domains=%d, want 3", len(f.domains))
	}
	added := f.calls[before:]
	if len(added) != 1 || !strings.HasPrefix(added[0], "create batch-2") {
		t.Errorf("top-up issued %v, want exactly one create at the first free index", added)
	}
}

// Readiness is a wait, not a guess: a machine with no lease yet is polled until it
// has one.
func TestCreatedWaitsForTheLease(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)

	// Withhold the lease for the first few looks at the machine.
	f.leaseAfter["batch-0"] = 3

	s := apply(t, m, "created", createOwn(map[string]any{"count": float64(1)}))
	mustSucceed(t, s)
	if f.leaseSeen["batch-0"] <= 3 {
		t.Errorf("lease was polled %d times, want more than the 3 that were withheld", f.leaseSeen["batch-0"])
	}
	if ip := s.hosts(t)[0]["primary_ip"]; ip == "" {
		t.Error("the batch was reported ready with no address")
	}
}

// ★ Anti-orphan: a machine that never came up is still in output.hosts, so the
// caller can tear down what this run made. Reporting only the healthy ones is how
// a failed run leaves billed machines nobody knows about.
func TestCreatedReportsEveryVMEvenWhenOneNeverCameUp(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)

	// One of the two never takes a lease.
	f.leaseAfter["batch-1"] = 1 << 30

	s := apply(t, m, "created", createOwn(nil))
	last := s.last()
	if last == nil {
		t.Fatal("no events")
	}
	if !strings.Contains(last.GetMessage(), "did not become usable") {
		t.Errorf("message=%q, want it to name the machines that did not come up", last.GetMessage())
	}
	hosts := s.hosts(t)
	if len(hosts) != 2 {
		t.Fatalf("hosts=%d, want both machines including the one that failed", len(hosts))
	}
	for _, h := range hosts {
		if v, _ := h["vm_id"].(string); v == "" {
			t.Error("a host entry carries no vm_id — it cannot be cleaned up")
		}
	}
}

// ★ probed creates, deletes and modifies nothing, and reports changed=false. A
// probe that reported changed would make every scenario using it look like a
// mutation.
func TestProbedIsReadOnly(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(nil)))
	before := len(f.calls)

	s := apply(t, m, "probed", nil)
	last := mustSucceed(t, s)
	if last.GetChanged() {
		t.Error("probed reported changed=true")
	}
	if len(f.calls) != before {
		t.Errorf("probed issued hypervisor writes: %v", f.calls[before:])
	}
	if len(s.hosts(t)) != 2 {
		t.Errorf("probed reported %d VM, want 2", len(s.hosts(t)))
	}
}

// ★ The namespace is the scope. A machine this artifact did not make — the
// operator's own VM on the same workstation — is invisible to probed and
// untouchable by destroyed.
func TestForeignMachinesAreInvisibleAndUntouchable(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(nil)))

	// Somebody else's machine, and one of ours in a different namespace.
	f.domains["stranger"] = &domainInfo{UUID: "stranger", Name: "my-laptop-vm", Labels: map[string]string{}}
	f.domains["other-ns"] = &domainInfo{UUID: "other-ns", Name: "otherns-0", Namespace: "otherns", Labels: map[string]string{}}

	s := apply(t, m, "probed", nil)
	mustSucceed(t, s)
	for _, h := range s.hosts(t) {
		if id := h["vm_id"]; id == "stranger" || id == "other-ns" {
			t.Errorf("probed reported %v, which is outside the namespace scope", id)
		}
	}

	s = apply(t, m, "destroyed", map[string]any{"vm_ids": []any{"stranger"}})
	if last := s.last(); last == nil || !last.GetFailed() {
		t.Fatal("destroyed accepted a machine outside its namespace")
	}
	if _, still := f.domains["stranger"]; !still {
		t.Error("a machine outside the namespace was destroyed")
	}
}

// A machine that is already gone is an idempotent success, not a failure.
func TestDestroyedIsIdempotent(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1)})))
	id := f.idOf(t, "batch-0")

	s := apply(t, m, "destroyed", map[string]any{"vm_ids": []any{id}})
	last := mustSucceed(t, s)
	if !last.GetChanged() {
		t.Error("a real teardown reported changed=false")
	}

	s = apply(t, m, "destroyed", map[string]any{"vm_ids": []any{id}})
	last = mustSucceed(t, s)
	if last.GetChanged() {
		t.Error("tearing down an already-gone machine reported changed=true")
	}
}

// deletion_protection is HONOURED, not merely recorded. A protection that does not
// protect is worse than none.
func TestDestroyedRefusesAProtectedMachine(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	prof := validProfile()
	prof["deletion_protection"] = true
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1), "profile": prof})))
	id := f.idOf(t, "batch-0")

	s := apply(t, m, "destroyed", map[string]any{"vm_ids": []any{id}})
	last := s.last()
	if last == nil || !last.GetFailed() {
		t.Fatal("a protected machine was torn down")
	}
	if !strings.Contains(last.GetMessage(), "deletion_protection") {
		t.Errorf("message=%q, want it to name the protection", last.GetMessage())
	}
	if _, still := f.domains[id]; !still {
		t.Error("the protected machine is gone")
	}
}

// ★ The quota precheck fails CLOSED, before the first machine is touched. Without
// it a batch ends up half-resized because the box ran out on VM four of six.
func TestResizedFailsClosedOnQuota(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(nil)))
	before := len(f.calls)

	f.freeMemory = 1 << 20 // a megabyte free, nowhere near the ask

	s := apply(t, m, "resized", map[string]any{
		"vm_ids":         []any{f.idOf(t, "batch-0"), f.idOf(t, "batch-1")},
		"ram_mb":         float64(8192),
		"allow_downtime": true,
	})
	last := s.last()
	if last == nil || !last.GetFailed() {
		t.Fatal("a resize the host cannot fit was accepted")
	}
	if !strings.Contains(last.GetMessage(), "before any VM was touched") {
		t.Errorf("message=%q, want it to say nothing was touched", last.GetMessage())
	}
	if len(f.calls) != before {
		t.Errorf("the failed precheck still issued %v", f.calls[before:])
	}
}

// ★ cpu/ram go through stop → update → start, so `allow_downtime` is consent to
// a stop that actually happens.
// libvirt can hot-plug some of this; doing so would let a scenario pass here
// without allow_downtime and fail there.
func TestResizedStopsBeforeChangingCPU(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1)})))
	id := f.idOf(t, "batch-0")
	f.calls = nil

	s := apply(t, m, "resized", map[string]any{
		"vm_ids":         []any{id},
		"cpu_cores":      float64(4),
		"allow_downtime": true,
	})
	mustSucceed(t, s)

	stop := slices.Index(f.calls, "stop "+id)
	set := slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "setvcpus ") })
	start := slices.Index(f.calls, "start "+id)
	if stop < 0 || set < 0 || start < 0 {
		t.Fatalf("calls=%v, want stop, setvcpus and start", f.calls)
	}
	if !(stop < set && set < start) {
		t.Errorf("calls=%v, want stop before the change and start after it", f.calls)
	}
	if f.domains[id].VCPUs != 4 {
		t.Errorf("vcpus=%d, want 4", f.domains[id].VCPUs)
	}
}

// ★ An unclean stop is REPORTED. Stock cloud images do not act on the ACPI power
// button, so the forced path is the normal one here — and silently pulling the
// power on a machine an operator asked to resize is the kind of thing that should
// never be learned afterwards from a corrupted filesystem.
func TestForcedStopIsReported(t *testing.T) {
	f := newFakeHV()
	f.forceStop = true
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1)})))
	id := f.idOf(t, "batch-0")

	s := apply(t, m, "resized", map[string]any{
		"vm_ids": []any{id}, "cpu_cores": float64(4), "allow_downtime": true,
	})
	mustSucceed(t, s)

	var said bool
	for _, e := range s.events {
		if strings.Contains(e.GetMessage(), "stopped the hard way") {
			said = true
		}
	}
	if !said {
		t.Error("a forced stop was not reported in any event")
	}

	// A clean stop says nothing.
	f2 := newFakeHV()
	m2 := withFake(t, f2)
	mustSucceed(t, apply(t, m2, "created", createOwn(map[string]any{"count": float64(1)})))
	s2 := apply(t, m2, "resized", map[string]any{
		"vm_ids": []any{f2.idOf(t, "batch-0")}, "cpu_cores": float64(4), "allow_downtime": true,
	})
	for _, e := range s2.events {
		if strings.Contains(e.GetMessage(), "stopped the hard way") {
			t.Error("a clean stop was reported as forced")
		}
	}
}

// Shrinking a disk is an idempotent skip — never a silent
// destructive truncation.
func TestResizedSkipsAShrink(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1)})))
	id := f.idOf(t, "batch-0")
	was := f.domains[id].DiskBytes
	f.calls = nil

	s := apply(t, m, "resized", map[string]any{"vm_ids": []any{id}, "disk_gb": float64(1)})
	mustSucceed(t, s)

	if f.domains[id].DiskBytes != was {
		t.Errorf("disk is now %d, was %d — a shrink was applied", f.domains[id].DiskBytes, was)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "resizedisk") {
			t.Errorf("a shrink issued %q", c)
		}
	}
}

// A disk-only grow is online: the machine is never stopped.
func TestDiskOnlyResizeDoesNotStopTheMachine(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1)})))
	id := f.idOf(t, "batch-0")
	f.calls = nil

	mustSucceed(t, apply(t, m, "resized", map[string]any{"vm_ids": []any{id}, "disk_gb": float64(50)}))

	for _, c := range f.calls {
		if strings.HasPrefix(c, "stop ") {
			t.Errorf("calls=%v: a disk-only resize stopped the machine", f.calls)
		}
	}
	if !f.domains[id].Running {
		t.Error("the machine is not running after an online resize")
	}
}

// run_label narrows probed to one batch.
func TestProbedNarrowsByRunLabel(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"count": float64(1)})))
	mustSucceed(t, apply(t, m, "created", createOwn(map[string]any{"name": "other", "count": float64(1)})))

	s := apply(t, m, "probed", map[string]any{"run_label": "other"})
	mustSucceed(t, s)
	hosts := s.hosts(t)
	if len(hosts) != 1 {
		t.Fatalf("hosts=%d, want only the `other` batch", len(hosts))
	}
	attrs := hosts[0]["attributes"].(map[string]any)
	if attrs["run_label"] != "other" {
		t.Errorf("run_label=%v, want other", attrs["run_label"])
	}
}

// The output survives the structpb round-trip a Keeper actually performs.
func TestOutputEncodesAsAStruct(t *testing.T) {
	f := newFakeHV()
	m := withFake(t, f)
	s := apply(t, m, "created", createOwn(nil))
	last := mustSucceed(t, s)

	if _, err := structpb.NewStruct(last.GetOutput().AsMap()); err != nil {
		t.Fatalf("the output does not round-trip through structpb: %v", err)
	}
}
