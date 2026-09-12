// The seam between the contract and libvirt.
//
// Everything above this interface is the `vm` contract — batch identity,
// idempotency, the output shape, the refusals — and everything below it is
// libvirt. The split is what lets the contract be tested without a hypervisor,
// which matters because the contract is the part two implementations have to
// agree on and the part a workstation CI cannot boot a VM to check.
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// errNoDomain is what a lookup returns for a machine that is not there. Callers
// turn it into an idempotent success (`destroyed`) or a skip (`probed`), so it
// has to be distinguishable from a hypervisor that is merely unreachable.
var errNoDomain = errors.New("no such domain")

// errVolumeSurvived marks a teardown where the DOMAIN went but its disks did
// not, or could not be accounted for.
//
// ★ It is a distinct sentinel because it is TERMINAL: once the domain is
// undefined, nothing names its volume paths any more, so a retry cannot re-derive
// the problem and the next round would come back "already gone" and look clean.
// Without it, a caller that retries until the domain is absent reports a
// confirmed teardown over a disk that is still there — and the next create under
// that name is wedged on "storage volume already exists" with nothing pointing
// back.
var errVolumeSurvived = errors.New("domain removed but its volumes may survive")

// domainInfo is everything the actions need to know about one machine. It is a
// read-back, not an echo of what was asked for: `probed` on a machine somebody
// resized by hand must report what is actually running.
type domainInfo struct {
	UUID      string
	Name      string
	Namespace string
	Labels    map[string]string

	Running     bool
	VCPUs       int64
	MemoryBytes int64
	DiskBytes   int64

	MAC       string
	NetworkID string
	ImageID   string
	CreatedAt string

	DeletionProtection bool
}

// runLabel is the batch identity this machine carries, "" if none.
func (d domainInfo) runLabel() string { return d.Labels[runLabelKey] }

// domainSpec is one machine to build.
type domainSpec struct {
	Name      string
	Namespace string
	Labels    map[string]string
	Profile   vmProfile

	ImageID     string // resolved
	ImagePath   string // resolved backing volume
	NetworkID   string // resolved canonical UUID, for metadata and lease lookups
	NetworkName string // libvirt network name, which is what the domain XML attaches by
	Seed        []byte // NoCloud seed ISO
}

// hypervisor is the libvirt surface this artifact uses, and nothing wider. A verb
// that is not here is a verb the contract does not need.
type hypervisor interface {
	Close() error

	// LookupNetwork resolves a network by UUID or name and returns BOTH: the
	// domain XML attaches an interface by name, while output.attributes and the
	// lease lookup carry the canonical UUID. Returning one and deriving the other
	// at the call site is what put a UUID into `<source network=>` once already.
	LookupNetwork(id string) (name, uuid string, err error)

	// Lease is the authoritative read for both primary_ip and sid: the guest
	// tells the network who it is, and dnsmasq records it. ok=false means the
	// machine has not taken a lease yet, which is not an error.
	Lease(ctx context.Context, networkID, mac string) (ip, hostname string, ok bool, err error)

	// ListDomains returns only machines this artifact made in this namespace.
	ListDomains(namespace string) ([]domainInfo, error)
	LookupDomain(uuid string) (domainInfo, error)

	// ResolveImage turns image_id or image_name into the backing volume, and
	// reports what is available when it cannot.
	ResolveImage(prof vmProfile) (imageID, imagePath string, err error)

	CreateDomain(ctx context.Context, spec domainSpec) (domainInfo, error)
	DestroyDomain(uuid string) error

	// StopDomain reports whether the stop had to be forced, so the caller can say
	// so rather than leaving an unclean shutdown to be discovered later.
	StopDomain(ctx context.Context, uuid string) (forced bool, err error)
	StartDomain(uuid string) error
	SetVCPUs(uuid string, n int64) error
	SetMemoryBytes(uuid string, b int64) error
	ResizeDiskBytes(uuid string, b int64) error

	// FreeMemoryBytes and PoolFreeBytes are the local quota: `resized` fails
	// closed against them before it touches the first machine.
	FreeMemoryBytes() (int64, error)
	PoolFreeBytes() (int64, error)
}

// VMLocal is the shared implementation behind the object's actions. It holds no
// connection: one is opened per Apply from that call's `endpoint`, because two
// concurrent runs may legitimately address two different hypervisors.
type VMLocal struct {
	// dial is the hypervisor constructor, swapped in tests. Nil means the real
	// libvirt one.
	dial func(endpoint string) (hypervisor, error)

	mu sync.Mutex
}

func (m *VMLocal) connect(endpoint string) (hypervisor, error) {
	m.mu.Lock()
	d := m.dial
	m.mu.Unlock()
	if d == nil {
		d = dialLibvirt
	}
	h, err := d(endpoint)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", endpoint, err)
	}
	return h, nil
}
