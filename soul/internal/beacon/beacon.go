// Package beacon — Soul-side event-driven monitoring (ADR-030, slice S1).
//
// Contents:
//   - [Beacon]: read-only interface for a check's body (`core.beacon.<name>`,
//     parallels core modules). Beacon observes host state and does NOT mutate
//     it — a construction invariant (ADR-030).
//   - [Registry]: static registry of built-in core beacons (like
//     coremod.Default for modules). Covers the full canonical address set
//     [beaconaddr.All] (service_down / file_changed / port_closed / disk_full /
//     process_absent / http_unhealthy); [Default] panics on a mismatch with
//     it — a build bug, not bad input. soul_beacon plugins are S5, only
//     built-ins exist so far.
//   - [Scheduler] (scheduler.go): per-process scheduler for the active Vigil
//     set, edge-triggered Portent emission.
//
// Soul-safe isolation (ADR-012(d)): the package doesn't pull in Vault/cel-go —
// beacon checks only read local host state.
package beacon

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"google.golang.org/protobuf/types/known/structpb"
)

// State — the result of one beacon check: an opaque host-state string.
// Scheduler compares it against the previous value (edge-triggered): a State
// change → Portent. String semantics are up to the individual beacon
// (`core.beacon.service_down` → "up"/"down"; `core.beacon.file_changed` →
// file hash or "missing").
type State = string

// Beacon — the body of one check. Read-only by construction (ADR-030): Check
// observes host state and returns it, but does NOT change the system.
//
// Returns:
//   - state: current state (see [State]);
//   - data:  facts for PortentEvent.data (file path, service name, hash, etc.);
//     may be nil, in which case Portent only carries the scheduler's base fields;
//   - err:   the check couldn't run (e.g. an invalid param). On error the
//     scheduler skips the tick — baseline/last-state are untouched, no Portent
//     is emitted (a check error ≠ a host state change).
type Beacon interface {
	Check(ctx context.Context, params *structpb.Struct) (state State, data *structpb.Struct, err error)
}

// BeaconLookup — narrow interface for resolving a beacon by VigilDef.check
// address. Implemented by the static [Registry] (core beacons) and
// [CompositeRegistry] (core + plugin beacons, ADR-030 V5-2). Scheduler only
// operates through this interface — it doesn't distinguish built-in from
// plugin beacons (the Composite resolver handles dispatch).
type BeaconLookup interface {
	Lookup(name string) (Beacon, bool)
}

// Registry — static set of built-in core beacons, addressed by name
// (`core.beacon.service_down` / `core.beacon.file_changed`). Immutable after
// [Default] builds it; Lookup is the only operation the scheduler needs.
type Registry struct {
	beacons map[string]Beacon
}

// Default builds the registry of all built-in core beacons for MVP (ADR-030 S1).
//
// Coverage of the canonical [beaconaddr.All] set is checked right here: the
// registry must contain exactly one impl per address, no more, no less. A
// mismatch is a build-time programmer bug (forgot to register a new beacon,
// or an address drifted from the shared source), not user input → panic at
// init time rather than a silently incomplete registry (the same bug class
// that caused S3/OpenRC issues before the move to shared).
func Default() *Registry {
	beacons := map[string]Beacon{
		ServiceDownName:   NewServiceDown(),
		FileChangedName:   NewFileChanged(),
		PortClosedName:    NewPortClosed(),
		DiskFullName:      NewDiskFull(),
		ProcessAbsentName: NewProcessAbsent(),
		HTTPUnhealthyName: NewHTTPUnhealthy(),
		InotifyName:       NewInotify(),
	}
	canonical := beaconaddr.All()
	if len(beacons) != len(canonical) {
		panic(fmt.Sprintf("beacon: реестр (%d) рассинхронен с beaconaddr.All (%d)", len(beacons), len(canonical)))
	}
	for _, addr := range canonical {
		if _, ok := beacons[addr]; !ok {
			panic(fmt.Sprintf("beacon: канонический адрес %q не зарегистрирован в Default", addr))
		}
	}
	return &Registry{beacons: beacons}
}

// Lookup returns the beacon for a `core.beacon.<name>` address (VigilDef.check).
// A false second result means no such built-in beacon exists (scheduler
// logs and skips the Vigil instead of failing).
func (r *Registry) Lookup(name string) (Beacon, bool) {
	b, ok := r.beacons[name]
	return b, ok
}

// Names returns the addresses of all registered beacons (for startup logs).
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.beacons))
	for name := range r.beacons {
		out = append(out, name)
	}
	return out
}
