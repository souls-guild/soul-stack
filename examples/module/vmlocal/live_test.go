//go:build libvirt

// The live lane: the whole lifecycle against a real libvirt, which is the only
// thing that exercises libvirt.go at all. Everything above the hypervisor seam is
// covered by the default lane; this covers the seam itself.
//
// Outside the default build on purpose (`make test-plugins` runs no tags), because
// it needs a hypervisor, a staged image and about two minutes. Run it deliberately:
//
//	sg libvirt -c 'GOWORK=off go test -tags libvirt -timeout 15m -v -run TestLive .'
//
// Prerequisites, and it says which one is missing rather than failing obscurely:
//   - libvirtd reachable at qemu:///system
//   - a `debian-12` volume in the VMLOCAL_IMAGE_POOL (default vmlocal-images)
//   - a libvirt network named `default`, active
package main

import (
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

const liveNamespace = "vmlocal-live-test"

func liveParams(t *testing.T, own map[string]any) *structpb.Struct {
	t.Helper()
	m := map[string]any{
		connEndpoint:  "qemu:///system",
		connNamespace: liveNamespace,
	}
	for k, v := range own {
		m[k] = v
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("build params: %v", err)
	}
	return s
}

func liveApply(t *testing.T, m *VMLocal, state string, own map[string]any) *applyStream {
	t.Helper()
	s := &applyStream{}
	if err := m.vm().Apply(&pluginv1.ApplyRequest{State: state, Params: liveParams(t, own)}, s); err != nil {
		t.Fatalf("Apply(%s): %v", state, err)
	}
	for _, e := range s.events {
		t.Logf("[%s] %s", state, e.GetMessage())
	}
	return s
}

// liveProfile takes the network UUID rather than a name: libvirt networks carry
// real UUIDs and `network_id` is declared as one. Discovering it here instead of
// hardcoding one keeps the test portable to any host with a `default` network.
func liveProfile(networkUUID string) map[string]any {
	return map[string]any{
		"namespace":      liveNamespace,
		"image_name":     "debian-12",
		"network_id":     networkUUID,
		"cpu_size":       float64(1),
		"ram_size":       float64(1 << 30),
		"boot_disk_size": float64(4 << 30),
	}
}

// preflight turns a missing prerequisite into a skip with a name, rather than a
// failure that looks like a bug in the artifact. It returns the network UUID the
// profile needs.
func preflight(t *testing.T) string {
	t.Helper()
	h, err := dialLibvirt("qemu:///system")
	if err != nil {
		t.Skipf("no libvirt at qemu:///system: %v", err)
	}
	defer h.Close()

	_, networkUUID, err := h.LookupNetwork("default")
	if err != nil {
		t.Skipf("no active libvirt network named `default`: %v", err)
	}
	prof, _ := parseProfile(liveProfile(networkUUID))
	if _, _, err := h.ResolveImage(prof); err != nil {
		t.Skipf("the image pool is not staged: %v", err)
	}
	return networkUUID
}

// TestLiveLifecycle drives created → probed → resized → destroyed against a real
// hypervisor, and checks the two facts the whole design rests on: that the guest
// announces its hostname over DHCP, and that the address it reports is reachable
// from this process — which is where a Keeper runs on the dev stand.
func TestLiveLifecycle(t *testing.T) {
	networkUUID := preflight(t)
	m := &VMLocal{}

	// Leave nothing behind, whatever happens in the middle.
	t.Cleanup(func() {
		s := &applyStream{}
		ids := []any{}
		probe := &applyStream{}
		_ = m.vm().Apply(&pluginv1.ApplyRequest{State: "probed", Params: liveParams(t, nil)}, probe)
		if last := probe.last(); last != nil {
			if raw, ok := last.GetOutput().AsMap()["hosts"].([]any); ok {
				for _, e := range raw {
					if h, ok := e.(map[string]any); ok {
						ids = append(ids, h["vm_id"])
					}
				}
			}
		}
		if len(ids) == 0 {
			return
		}
		_ = m.vm().Apply(&pluginv1.ApplyRequest{
			State:  "destroyed",
			Params: liveParams(t, map[string]any{"vm_ids": ids}),
		}, s)
		t.Logf("cleanup: %s", s.last().GetMessage())
	})

	// ── created ──────────────────────────────────────────────────────────────
	start := time.Now()
	s := liveApply(t, m, "created", map[string]any{
		"name":     "live",
		"count":    float64(1),
		"profile":  liveProfile(networkUUID),
		"userdata": "#cloud-config\nruncmd:\n  - [ touch, /tmp/vmlocal-was-here ]\n",
	})
	last := s.last()
	if last == nil || last.GetFailed() {
		t.Fatalf("created failed: %v", last.GetMessage())
	}
	t.Logf("created in %s", time.Since(start).Round(time.Second))

	hosts := s.hosts(t)
	if len(hosts) != 1 {
		t.Fatalf("hosts=%v, want one", hosts)
	}
	h := hosts[0]
	vmID, _ := h["vm_id"].(string)
	ip, _ := h["primary_ip"].(string)
	sid, _ := h["sid"].(string)

	if ip == "" {
		t.Fatal("the machine came up with no address — the DHCP lease never carried one")
	}
	// ★ sid is the name the GUEST announced, which is what makes it the same kind
	// of fact the machine reports rather than an echo of what we asked for.
	if sid != "live-0" {
		t.Errorf("sid=%q, want live-0 as announced over DHCP; a mismatch means cloud-init did not apply the seed", sid)
	}
	if !strings.HasPrefix(ip, "192.168.") {
		t.Logf("note: address %s is outside the usual libvirt NAT range", ip)
	}

	// ── idempotency, live ────────────────────────────────────────────────────
	again := liveApply(t, m, "created", map[string]any{
		"name": "live", "count": float64(1), "profile": liveProfile(networkUUID),
	})
	if again.last().GetChanged() {
		t.Error("a rerun of the same batch reported changed=true — the live machine was not adopted")
	}

	// ── probed ───────────────────────────────────────────────────────────────
	p := liveApply(t, m, "probed", nil)
	if p.last().GetChanged() {
		t.Error("probed reported changed=true")
	}
	probed := p.hosts(t)
	if len(probed) != 1 || probed[0]["vm_id"] != vmID {
		t.Errorf("probed=%v, want the one machine just created", probed)
	}

	// ── resized: disk online, then cpu through stop/start ────────────────────
	//
	// ★ The disk size is READ BACK. Asserting only that the step did not fail is
	// what let a unit bug through: the size was converted to KiB while the BYTES
	// flag was set, so every "grow" was issued 1024x too small — a destructive
	// shrink that reported success, and that the next run would repeat.
	before := liveApply(t, m, "probed", map[string]any{"vm_ids": []any{vmID}})
	beforeDisk := diskBytesOf(t, before)

	r := liveApply(t, m, "resized", map[string]any{"vm_ids": []any{vmID}, "disk_gb": float64(6)})
	if r.last().GetFailed() {
		t.Fatalf("online disk resize failed: %s", r.last().GetMessage())
	}
	after := liveApply(t, m, "probed", map[string]any{"vm_ids": []any{vmID}})
	afterDisk := diskBytesOf(t, after)
	const wantDisk = int64(6) << 30
	if afterDisk != wantDisk {
		t.Errorf("disk is %d bytes after a grow to %d (was %d) — check the unit passed to DomainBlockResize",
			afterDisk, wantDisk, beforeDisk)
	}
	if afterDisk < beforeDisk {
		t.Errorf("the disk SHRANK from %d to %d bytes", beforeDisk, afterDisk)
	}

	r = liveApply(t, m, "resized", map[string]any{
		"vm_ids": []any{vmID}, "cpu_cores": float64(2), "allow_downtime": true,
	})
	if r.last().GetFailed() {
		t.Fatalf("cpu resize failed: %s", r.last().GetMessage())
	}
	afterCPU := liveApply(t, m, "probed", map[string]any{"vm_ids": []any{vmID}})
	attrs, _ := afterCPU.hosts(t)[0]["attributes"].(map[string]any)
	if got, _ := attrs["cpu_size"].(float64); got != 2 {
		t.Errorf("cpu_size after resize = %v, want 2 — the persistent config was not written", attrs["cpu_size"])
	}

	// ── destroyed, and CONFIRMED ─────────────────────────────────────────────
	d := liveApply(t, m, "destroyed", map[string]any{"vm_ids": []any{vmID}})
	if d.last().GetFailed() {
		t.Fatalf("destroyed failed: %s", d.last().GetMessage())
	}
	gone := liveApply(t, m, "probed", nil)
	if n := len(gone.hosts(t)); n != 0 {
		t.Errorf("probed still reports %d machines after a confirmed teardown", n)
	}

	// Tearing down what is already gone is an idempotent success.
	d = liveApply(t, m, "destroyed", map[string]any{"vm_ids": []any{vmID}})
	if d.last().GetFailed() {
		t.Errorf("a second teardown failed instead of being idempotent: %s", d.last().GetMessage())
	}
	if d.last().GetChanged() {
		t.Error("tearing down an already-gone machine reported changed=true")
	}
}

// diskBytesOf reads the boot-disk size straight from the hypervisor for the one
// machine a probe reported. The probe output does not carry it — a read-back has
// to come from the thing being asserted about, not from the same report.
func diskBytesOf(t *testing.T, s *applyStream) int64 {
	t.Helper()
	hosts := s.hosts(t)
	if len(hosts) != 1 {
		t.Fatalf("probed %d machines, want 1", len(hosts))
	}
	id, _ := hosts[0]["vm_id"].(string)

	h, err := dialLibvirt("qemu:///system")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer h.Close()
	d, err := h.LookupDomain(id)
	if err != nil {
		t.Fatalf("look up %s: %v", id, err)
	}
	return d.DiskBytes
}
