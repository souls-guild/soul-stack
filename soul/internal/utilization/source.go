package utilization

import "context"

// Source — the access layer for live host utilization. Separated from Collector
// for testability: the production implementation ([systemSource]) reads /proc +
// statfs, unit tests swap it with a fake.
//
// All methods are best-effort: when a fact is unavailable they return a
// zero-value (0 / empty slice), not an error and not a panic (symmetry with
// soulprint.Source, ADR-018/ADR-072 — Keeper tolerates sparse fields).
type Source interface {
	// Load — load average over 1/5/15 minutes.
	Load(ctx context.Context) LoadAvg
	// Memory — used/total RAM + used swap in MB.
	Memory(ctx context.Context) MemInfo
	// Disks — usage of non-virtual mount points.
	Disks(ctx context.Context) []Disk
	// Uptime — host uptime in seconds.
	Uptime(ctx context.Context) int64
	// CPUSample — raw /proc/stat ticks for delta cpu% computation (Collector-side).
	CPUSample(ctx context.Context) CPUSample
}

// LoadAvg — load average.
type LoadAvg struct {
	One, Five, Fifteen float64
}

// MemInfo — memory in MB (used = total - available).
type MemInfo struct {
	UsedMB, TotalMB, SwapUsedMB int64
}

// Disk — usage of one mount point, volumes in MB.
type Disk struct {
	Mount           string
	UsedMB, TotalMB int64
}

// CPUSample — a snapshot of /proc/stat's `cpu ` line counters. Idle includes
// iowait, Total is the sum of all fields; cpu% = delta busy/total between two
// samples.
type CPUSample struct {
	Total, Idle uint64
}
