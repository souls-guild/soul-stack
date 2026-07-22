package beacon

import (
	"context"
	"fmt"
	"syscall"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

// DiskFullName is the core-beacon address (`core.beacon.<name>`, VigilDef.check).
const DiskFullName = beaconaddr.DiskFull

const (
	stateDiskOK   State = "ok"
	stateDiskFull State = "full"
)

// diskFullDefaultThreshold is the default filesystem usage threshold
// (percent). "full" triggers when usage ≥ threshold.
const diskFullDefaultThreshold = 90.0

// diskUsage is a filesystem usage snapshot: percent of space used. Computed
// via statfs (read-only syscall), not by parsing `df` output — more accurate
// and independent of locale/tool output format.
type diskUsage struct {
	usedPercent float64
}

// DiskFull is the core-beacon for observing filesystem fill level (ADR-030).
// Read-only: a single statfs call, no writes. State is "full" when usage ≥
// threshold_percent, otherwise "ok". The ok↔full transition is
// edge-triggered → Portent.
//
// Params:
//   - `path` (string, required) — mount point or any path within the filesystem;
//   - `threshold_percent` (int, optional, default 90) — "full" threshold, 1..100.
type DiskFull struct {
	// Usage is a field so unit tests can swap in a deterministic snapshot
	// (real statfs depends on host free space — flaky). In production it's
	// statfsUsage over syscall.Statfs.
	Usage func(path string) (diskUsage, error)
}

// NewDiskFull builds a beacon with the production sampler (syscall.Statfs).
func NewDiskFull() *DiskFull { return &DiskFull{Usage: statfsUsage} }

func (b *DiskFull) Check(_ context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	path, err := util.StringParam(params, "path")
	if err != nil {
		return "", nil, err
	}
	threshold, err := optThresholdPercent(params, diskFullDefaultThreshold)
	if err != nil {
		return "", nil, err
	}

	u, err := b.Usage(path)
	if err != nil {
		return "", nil, fmt.Errorf("statfs %s: %v", path, err)
	}

	state := stateDiskOK
	if u.usedPercent >= threshold {
		state = stateDiskFull
	}
	return state, diskData(path, u.usedPercent, threshold), nil
}

// statfsUsage computes percent used via statfs (read-only). used = total -
// Bavail; percent of total. Bavail (not Bfree) is blocks available to an
// unprivileged process: root-reserved space (~5% by default on ext-family
// filesystems) counts as used, matching plain `df`. Using Bfree instead would
// undercount used_percent relative to `df` and delay the beacon's "full"
// trigger. Total/avail are in Bsize blocks — the percentage cancels Bsize
// out. An empty filesystem (Blocks == 0) → 0%, avoiding division by zero.
func statfsUsage(path string) (diskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return diskUsage{}, err
	}
	total := st.Blocks
	if total == 0 {
		return diskUsage{usedPercent: 0}, nil
	}
	used := total - st.Bavail
	return diskUsage{usedPercent: float64(used) / float64(total) * 100}, nil
}

func diskData(path string, usedPercent, threshold float64) *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"path":         path,
		"used_percent": usedPercent,
		"threshold":    threshold,
	})
	return s
}

// optThresholdPercent parses the optional threshold_percent (1..100). Empty
// param → def. proto-json marshals numbers as float64 (OptIntParam requires an integer).
func optThresholdPercent(params *structpb.Struct, def float64) (float64, error) {
	n, ok, err := util.OptIntParam(params, "threshold_percent")
	if err != nil {
		return 0, err
	}
	if !ok {
		return def, nil
	}
	if n < 1 || n > 100 {
		return 0, fmt.Errorf("param %q: must be 1..100, got %d", "threshold_percent", n)
	}
	return float64(n), nil
}
