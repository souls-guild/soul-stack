// Package beaconaddr is the single source of canonical addresses for built-in
// core beacons ([ADR-030], slice S1).
//
// A beacon address (`core.beacon.<name>`, VigilDef.check) is needed by BOTH sides:
//   - Keeper (`keeper/internal/oracle`) — closed enum validating VigilDef.check
//     (unknown check → validation error, not a silently non-executable Vigil);
//   - Soul (`soul/internal/beacon`) — static registry of check bodies addressed
//     by these same addresses.
//
// ADR-011 forbids a keeper→soul import, so the lists used to be duplicated and
// drifted apart (S3 bug: keeper enum lagged by 4 addresses → false 422 on a valid
// Vigil). The canonical list lives here in neutral `shared/` (imported by both
// keeper and soul, but not by each other), removing the duplicated source of truth.
//
// A beacon is a Vigil check (read-only observer), not an apply module, so the
// addresses live in their own package rather than in shared/coremanifest (the
// registry of apply-module input manifests).
//
// Plugin kind `soul_beacon` (community checks, S5) is not introduced yet — until
// then the set is closed to this list.
package beaconaddr

// Addresses of built-in core beacons MVP (`core.beacon.<name>`, VigilDef.check).
// When adding a new core beacon, add a constant here and to [All]; the invariant
// test keeper-enum == soul-registry == this list catches drift.
const (
	ServiceDown   = "core.beacon.service_down"
	FileChanged   = "core.beacon.file_changed"
	PortClosed    = "core.beacon.port_closed"
	DiskFull      = "core.beacon.disk_full"
	ProcessAbsent = "core.beacon.process_absent"
	HTTPUnhealthy = "core.beacon.http_unhealthy"
	// Inotify is a Linux-only kernel inotify syscall (V5-3, ADR-030 amendment
	// 2026-05-26). On non-Linux platforms the beacon returns an explicit
	// "platform not supported" error; the address constant is available everywhere
	// for a single source of truth across keeper-enum / soul-registry.
	Inotify = "core.beacon.inotify"
)

// All returns all canonical core-beacon MVP addresses. A fresh slice on each
// call — the caller cannot silently mutate the shared list.
func All() []string {
	return []string{
		ServiceDown,
		FileChanged,
		PortClosed,
		DiskFull,
		ProcessAbsent,
		HTTPUnhealthy,
		Inotify,
	}
}
