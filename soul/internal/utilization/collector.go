// Package utilization — Soul-side collection of live host utilization (CPU%/load/mem/
// disk/uptime) and filling in [keeperv1.HostUtilization] for periodic
// sending to Keeper over the presence channel (ADR-072).
//
// A volatile layer, separate from the static soulprint (ADR-018): its own cadence
// (~30s), not a targeting fact, stored in Redis (hot, not PG). Best-effort like
// soulprint — an unavailable fact stays zero-value, Collect does not panic and does not
// return an error.
package utilization

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Collector gathers a utilization snapshot. Unlike soulprint.Collector, it is
// stateful: it keeps the previous CPUSample, since cpu% is computed as the tick delta
// between two collections. Called from the session's single writer (select-loop
// handleSession) — no concurrent Collect calls, no synchronization needed.
type Collector struct {
	src  Source
	prev CPUSample
	seen bool // whether a sample has already been taken (first Collect → cpu%=0)
}

// NewCollector builds a Collector over the given Source. For production —
// utilization.NewCollector(utilization.NewSystemSource()).
func NewCollector(src Source) *Collector {
	return &Collector{src: src}
}

// CollectorSet — the set of enabled host-vitals collectors (ADR-072, NIM-87):
// name → enabled. A disabled collector is not gathered, its fields in the snapshot are zero
// (for disk — the expensive statfs is skipped). A nil set = "all enabled" (default).
// Valid names are cpu/mem/disk/load/uptime (config.KnownCollectors); unknown names
// in the set are ignored (Collect only reads these five keys).
type CollectorSet map[string]bool

func (s CollectorSet) on(name string) bool {
	if s == nil {
		return true
	}
	return s[name]
}

// Collect takes a single utilization snapshot with collected_at = now, gathering only
// the collectors enabled in `enabled` (disabled → zero fields). There is no sid in the message
// (authority is the mTLS peer cert on Keeper, ADR-072); it is accepted for symmetry with
// soulprint.Collect. Does not return errors.
func (c *Collector) Collect(ctx context.Context, _ string, enabled CollectorSet) *keeperv1.HostUtilization {
	u := &keeperv1.HostUtilization{CollectedAt: timestamppb.Now()}
	if enabled.on("cpu") {
		u.CpuPct = c.cpuPct(c.src.CPUSample(ctx))
	}
	if enabled.on("load") {
		load := c.src.Load(ctx)
		u.Load1, u.Load5, u.Load15 = load.One, load.Five, load.Fifteen
	}
	if enabled.on("mem") {
		mem := c.src.Memory(ctx)
		u.MemUsedMb, u.MemTotalMb, u.SwapUsedMb = mem.UsedMB, mem.TotalMB, mem.SwapUsedMB
	}
	if enabled.on("disk") {
		u.Disks = c.disks(ctx)
	}
	if enabled.on("uptime") {
		u.UptimeSec = c.src.Uptime(ctx)
	}
	return u
}

// cpuPct — 100*(busyDelta/totalDelta), busy = Total-Idle, delta relative to the previous
// sample. The first sample and a non-increasing totalDelta (counter did not move / reset) →
// 0; the result is clamped to 0..100.
func (c *Collector) cpuPct(cur CPUSample) float64 {
	prev, seen := c.prev, c.seen
	c.prev, c.seen = cur, true
	if !seen {
		return 0
	}
	totalD := int64(cur.Total) - int64(prev.Total)
	if totalD <= 0 {
		return 0
	}
	busyD := totalD - (int64(cur.Idle) - int64(prev.Idle))
	return clamp(100*float64(busyD)/float64(totalD), 0, 100)
}

func (c *Collector) disks(ctx context.Context) []*keeperv1.DiskUtilization {
	src := c.src.Disks(ctx)
	if len(src) == 0 {
		return nil
	}
	out := make([]*keeperv1.DiskUtilization, 0, len(src))
	for _, d := range src {
		out = append(out, &keeperv1.DiskUtilization{Mount: d.Mount, UsedMb: d.UsedMB, TotalMb: d.TotalMB})
	}
	return out
}

func clamp(v, lo, hi float64) float64 {
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	default:
		return v
	}
}
