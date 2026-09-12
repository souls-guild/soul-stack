// A hypervisor that answers from memory, and the stream the actions write to.
//
// The contract lives above the hypervisor seam, so every property worth holding
// — idempotent adoption, the output shape, the namespace scope, the quota
// precheck — is testable without a libvirt on the machine running the tests.
package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// applyStream records what an action emitted.
type applyStream struct {
	grpc.ServerStream
	events []*pluginv1.ApplyEvent
	ctx    context.Context
}

func (s *applyStream) Send(e *pluginv1.ApplyEvent) error {
	s.events = append(s.events, e)
	return nil
}

func (s *applyStream) Context() context.Context {
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// last is the final event — the one carrying changed/failed and the output.
func (s *applyStream) last() *pluginv1.ApplyEvent {
	if len(s.events) == 0 {
		return nil
	}
	return s.events[len(s.events)-1]
}

func (s *applyStream) hosts(t *testing.T) []map[string]any {
	t.Helper()
	last := s.last()
	if last == nil {
		t.Fatal("no events were emitted")
	}
	raw, ok := last.GetOutput().AsMap()["hosts"].([]any)
	if !ok {
		t.Fatalf("final event carries no hosts: %v", last.GetOutput().AsMap())
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		m, _ := e.(map[string]any)
		out = append(out, m)
	}
	return out
}

// fakeHV is an in-memory hypervisor.
type fakeHV struct {
	domains map[string]*domainInfo // by UUID
	leases  map[string][2]string   // mac -> {ip, hostname}

	networks map[string]string // name -> uuid
	images   map[string]string // name -> path

	freeMemory int64
	freePool   int64

	// leaseAfter withholds a lease for the first N looks at a machine, so the
	// readiness wait is exercised rather than satisfied on the first look. Keyed
	// by domain NAME: the MAC is not predictable before the machine exists,
	// because each creation mints a fresh identity.
	leaseAfter map[string]int
	leaseSeen  map[string]int
	macToName  map[string]string

	// Recorded calls, for the assertions that care about ORDER — a resize that
	// changed cpu without stopping the machine is the failure mode worth naming.
	calls []string

	// failOn injects a failure into one hypervisor call, keyed "<op>:<uuid>" or
	// just "<op>", so a test can break the middle of a batch.
	failOn map[string]error

	// failLookupAt fails the Nth LookupDomain for a uuid (1-based) and only that
	// one. `destroyed` looks a machine up twice — once to check the namespace,
	// once to confirm it is gone — and those two are different situations.
	failLookupAt map[string]int
	lookupSeen   map[string]int

	// forceStop makes StopDomain report an unclean stop, the normal case for a
	// stock cloud image that ignores the ACPI power button.
	forceStop bool
}

func newFakeHV() *fakeHV {
	return &fakeHV{
		domains:      map[string]*domainInfo{},
		leases:       map[string][2]string{},
		networks:     map[string]string{"default": "70b148c1-76bb-46c7-8f58-3f81783ac7a0"},
		images:       map[string]string{"debian-12": "/pool/images/debian-12.qcow2"},
		freeMemory:   64 << 30,
		freePool:     512 << 30,
		leaseAfter:   map[string]int{},
		leaseSeen:    map[string]int{},
		macToName:    map[string]string{},
		failOn:       map[string]error{},
		failLookupAt: map[string]int{},
		lookupSeen:   map[string]int{},
	}
}

func (f *fakeHV) injected(op, uuid string) error {
	if err, ok := f.failOn[op+":"+uuid]; ok {
		return err
	}
	return f.failOn[op]
}

func (f *fakeHV) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeHV) Close() error { return nil }

func (f *fakeHV) LookupNetwork(id string) (string, string, error) {
	for name, uuid := range f.networks {
		if id == name || id == uuid {
			return name, uuid, nil
		}
	}
	return "", "", fmt.Errorf("no libvirt network %q", id)
}

func (f *fakeHV) Lease(_ context.Context, _, mac string) (string, string, bool, error) {
	name := f.macToName[mac]
	if err := f.injected("lease", name); err != nil {
		return "", "", false, err
	}
	f.leaseSeen[name]++
	if f.leaseSeen[name] <= f.leaseAfter[name] {
		return "", "", false, nil
	}
	l, ok := f.leases[mac]
	if !ok {
		return "", "", false, nil
	}
	return l[0], l[1], true, nil
}

func (f *fakeHV) ListDomains(namespace string) ([]domainInfo, error) {
	out := []domainInfo{}
	for _, d := range f.domains {
		if d.Namespace != namespace {
			continue
		}
		out = append(out, *d)
	}
	return out, nil
}

func (f *fakeHV) LookupDomain(uuid string) (domainInfo, error) {
	f.lookupSeen[uuid]++
	if n, ok := f.failLookupAt[uuid]; ok && n == f.lookupSeen[uuid] {
		return domainInfo{}, errors.New("EOF")
	}
	d, ok := f.domains[uuid]
	if !ok {
		return domainInfo{}, errNoDomain
	}
	return *d, nil
}

func (f *fakeHV) ResolveImage(prof vmProfile) (string, string, error) {
	for name, path := range f.images {
		if prof.imageName == name || (prof.imageID != "" && prof.imageID == imageUUID(name)) {
			return imageUUID(name), path, nil
		}
	}
	return "", "", fmt.Errorf("image not in pool")
}

func (f *fakeHV) CreateDomain(_ context.Context, spec domainSpec) (domainInfo, error) {
	uuid, err := randomUUID()
	if err != nil {
		return domainInfo{}, err
	}
	mac, _ := macFromUUID(uuid)
	d := &domainInfo{
		UUID:               uuid,
		Name:               spec.Name,
		Namespace:          spec.Namespace,
		Labels:             spec.Labels,
		Running:            true,
		VCPUs:              spec.Profile.cpuSize,
		MemoryBytes:        spec.Profile.ramSize,
		DiskBytes:          spec.Profile.bootDiskSize,
		MAC:                mac,
		NetworkID:          spec.NetworkID,
		ImageID:            spec.ImageID,
		CreatedAt:          time.Unix(0, 0).UTC().Format(time.RFC3339),
		DeletionProtection: spec.Profile.deletionProtection,
	}
	f.domains[uuid] = d
	f.macToName[mac] = spec.Name
	f.leases[mac] = [2]string{"192.168.122.10", spec.Name}
	f.record("create %s", spec.Name)
	return *d, nil
}

func (f *fakeHV) DestroyDomain(uuid string) error {
	if _, ok := f.domains[uuid]; !ok {
		return errNoDomain
	}
	// The real one removes the DOMAIN and then deletes its volumes, so a volume
	// failure comes back with the domain already gone. Modelling it the other way
	// round would make a test of that path exercise the confirmation timeout
	// instead.
	delete(f.domains, uuid)
	f.record("destroy %s", uuid)
	return f.injected("destroy", uuid)
}

func (f *fakeHV) StopDomain(_ context.Context, uuid string) (bool, error) {
	d, ok := f.domains[uuid]
	if !ok {
		return false, errNoDomain
	}
	d.Running = false
	f.record("stop %s", uuid)
	return f.forceStop, nil
}

func (f *fakeHV) StartDomain(uuid string) error {
	if err := f.injected("start", uuid); err != nil {
		return err
	}
	d, ok := f.domains[uuid]
	if !ok {
		return errNoDomain
	}
	d.Running = true
	f.record("start %s", uuid)
	return nil
}

func (f *fakeHV) SetVCPUs(uuid string, n int64) error {
	if err := f.injected("setvcpus", uuid); err != nil {
		f.record("setvcpus-failed %s", uuid)
		return err
	}
	d, ok := f.domains[uuid]
	if !ok {
		return errNoDomain
	}
	d.VCPUs = n
	f.record("setvcpus %s=%d", uuid, n)
	return nil
}

func (f *fakeHV) SetMemoryBytes(uuid string, b int64) error {
	d, ok := f.domains[uuid]
	if !ok {
		return errNoDomain
	}
	d.MemoryBytes = b
	f.record("setmem %s=%d", uuid, b)
	return nil
}

func (f *fakeHV) ResizeDiskBytes(uuid string, b int64) error {
	if err := f.injected("resizedisk", uuid); err != nil {
		return err
	}
	d, ok := f.domains[uuid]
	if !ok {
		return errNoDomain
	}
	d.DiskBytes = b
	f.record("resizedisk %s=%d", uuid, b)
	return nil
}

// idOf resolves the vm_id of a machine by the name it was created under. Tests
// cannot predict it: each creation mints a fresh identity, deliberately.
func (f *fakeHV) idOf(t *testing.T, name string) string {
	t.Helper()
	for uuid, d := range f.domains {
		if d.Name == name {
			return uuid
		}
	}
	t.Fatalf("no domain named %q", name)
	return ""
}

func (f *fakeHV) FreeMemoryBytes() (int64, error) { return f.freeMemory, nil }
func (f *fakeHV) PoolFreeBytes() (int64, error)   { return f.freePool, nil }

// withFake points a VMLocal at this hypervisor and shortens the waits, so a test
// that exercises the readiness loop does not take five minutes to do it.
func withFake(t *testing.T, f *fakeHV) *VMLocal {
	t.Helper()
	oldTimeout, oldPoll := readyTimeout, pollInterval
	readyTimeout, pollInterval = 2*time.Second, time.Millisecond
	t.Cleanup(func() { readyTimeout, pollInterval = oldTimeout, oldPoll })

	return &VMLocal{dial: func(string) (hypervisor, error) { return f, nil }}
}

// validConn is the connection block every action carries.
func validConn() map[string]any {
	return map[string]any{
		connKeyID:     "unused-here",
		connSecret:    "unused-here",
		connEndpoint:  "qemu:///system",
		connNamespace: "proofns",
	}
}

// validProfile is a profile the cloud artifact would also accept.
func validProfile() map[string]any {
	return map[string]any{
		"namespace":      "proofns",
		"image_name":     "debian-12",
		"network_id":     "70b148c1-76bb-46c7-8f58-3f81783ac7a0",
		"cpu_size":       float64(2),
		"ram_size":       float64(2 << 30),
		"boot_disk_size": float64(5 << 30),
		"rm_external_id": "cmdb-123",
	}
}

// params merges a connection block with an action's own params and renders the
// structpb an Apply really receives — which is what turns every number into a
// float64, the shape the readers have to cope with.
func params(t *testing.T, own map[string]any) *structpb.Struct {
	t.Helper()
	m := validConn()
	for k, v := range own {
		m[k] = v
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("build params: %v", err)
	}
	return s
}
