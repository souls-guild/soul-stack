package redis

// Host-vitals cache of Soul agents in Redis (NIM-86): the latest utilization
// snapshot (Hash `soul:<sid>:util`) + a short window of points (list-ring
// `soul:<sid>:util:win`) for sparklines. Redis only, no PG — telemetry is
// volatile and lives under TTL (Redis restart → data lost, this is normal).
// Keyed by the authenticated SID (mTLS peer), not by payload.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

const (
	// UtilizationWindowSize — length of the point window (cap for LTRIM).
	UtilizationWindowSize = 60
	// UtilizationTTL — FLOOR of the latest/window lifetime (NIM-87). Effective TTL
	// is scaled by [utilizationTTL] to the Soul's cadence (3x interval_sec from
	// the payload), but never drops below this floor: at 0/old soul (no
	// interval_sec) the key lives ≥3 pulses on the default 30s cadence.
	UtilizationTTL = 90 * time.Second
	// utilizationIntervalSecCeil — upper clamp on interval_sec from the payload (3x3600) against an "immortal" TTL from a rogue/old Soul (safety first).
	utilizationIntervalSecCeil = 3 * 3600
)

// utilizationTTL scales the TTL of vitals keys to the effective cadence of the
// Soul (NIM-87): 3x interval_sec, clamped to [UtilizationTTL, 3xutilizationIntervalSecCeil].
// intervalSec ≤0 (old soul without the field / failure) → floor; anomalously large (rogue/
// buggy Soul) is capped by the ceiling — otherwise the TTL key would become "immortal".
func utilizationTTL(intervalSec int32) time.Duration {
	if intervalSec < 0 {
		intervalSec = 0
	}
	if intervalSec > utilizationIntervalSecCeil {
		intervalSec = utilizationIntervalSecCeil
	}
	ttl := 3 * time.Duration(intervalSec) * time.Second
	if ttl < UtilizationTTL {
		return UtilizationTTL
	}
	return ttl
}

// UtilizationKey — Hash of the latest utilization snapshot of a SID.
func UtilizationKey(sid string) string { return "soul:" + sid + ":util" }

// UtilizationWindowKey — list-ring of the utilization point window of a SID.
func UtilizationWindowKey(sid string) string { return "soul:" + sid + ":util:win" }

// DiskUsage — usage of one mounted volume.
type DiskUsage struct {
	Mount   string `json:"mount"`
	UsedMB  int64  `json:"used_mb"`
	TotalMB int64  `json:"total_mb"`
}

// UtilizationSnapshot — the latest host-vitals snapshot of a host.
type UtilizationSnapshot struct {
	CollectedAt time.Time
	ReceivedAt  time.Time
	CPUPct      float64
	Load1       float64
	Load5       float64
	Load15      float64
	MemUsedMB   int64
	MemTotalMB  int64
	SwapUsedMB  int64
	UptimeSec   int64
	Disks       []DiskUsage
	// IntervalSec — the effective cadence on which the Soul sent the snapshot (NIM-87);
	// 0 for an old soul without the field. Reflects the telemetry interval resolved by Keeper.
	IntervalSec int32
}

// UtilizationPoint — a compact window point for sparklines.
type UtilizationPoint struct {
	CollectedAt time.Time `json:"collected_at"`
	CPUPct      float64   `json:"cpu_pct"`
	Load1       float64   `json:"load1"`
	MemUsedMB   int64     `json:"mem_used_mb"`
	MemTotalMB  int64     `json:"mem_total_mb"`
}

// WriteUtilization writes a SID's utilization snapshot to Redis in one pipeline: latest
// Hash (all fields + received_at=now, disks — as a JSON string) with TTL + window (LPUSH
// of the compact point + LTRIM cap + Expire). NOT a transaction: latest and window are independent
// (readers do not cross-check them), and a plain Pipeline routes per-key and avoids CROSSSLOT on
// Redis Cluster — same as heartbeat/soullease.
func WriteUtilization(ctx context.Context, c *Client, sid string, ev *keeperv1.HostUtilization, now time.Time) error {
	if c == nil {
		return errors.New("redis.WriteUtilization: nil client")
	}
	if sid == "" {
		return errors.New("redis.WriteUtilization: empty sid")
	}
	if ev == nil {
		return errors.New("redis.WriteUtilization: nil event")
	}
	if now.IsZero() {
		now = time.Now()
	}

	var collected time.Time
	collectedStr := ""
	if ca := ev.GetCollectedAt(); ca != nil {
		collected = ca.AsTime().UTC()
		collectedStr = collected.Format(time.RFC3339Nano)
	}

	disksJSON, err := json.Marshal(disksFromProto(ev.GetDisks()))
	if err != nil {
		return fmt.Errorf("redis.WriteUtilization: marshal disks: %w", err)
	}
	pointJSON, err := json.Marshal(UtilizationPoint{
		CollectedAt: collected,
		CPUPct:      ev.GetCpuPct(),
		Load1:       ev.GetLoad1(),
		MemUsedMB:   ev.GetMemUsedMb(),
		MemTotalMB:  ev.GetMemTotalMb(),
	})
	if err != nil {
		return fmt.Errorf("redis.WriteUtilization: marshal point: %w", err)
	}

	key := UtilizationKey(sid)
	winKey := UtilizationWindowKey(sid)

	// TTL is scaled to the effective cadence of the Soul (NIM-87): a larger interval
	// → a larger TTL (≥3 pulses), with a floor of UtilizationTTL for 0/old soul.
	ttl := utilizationTTL(ev.GetIntervalSec())

	pipe := c.underlying().Pipeline()
	pipe.HSet(ctx, key, map[string]any{
		"collected_at": collectedStr,
		"received_at":  now.UTC().Format(time.RFC3339Nano),
		"cpu_pct":      ev.GetCpuPct(),
		"load1":        ev.GetLoad1(),
		"load5":        ev.GetLoad5(),
		"load15":       ev.GetLoad15(),
		"mem_used_mb":  ev.GetMemUsedMb(),
		"mem_total_mb": ev.GetMemTotalMb(),
		"swap_used_mb": ev.GetSwapUsedMb(),
		"uptime_sec":   ev.GetUptimeSec(),
		"interval_sec": ev.GetIntervalSec(),
		"disks":        string(disksJSON),
	})
	pipe.Expire(ctx, key, ttl)
	pipe.LPush(ctx, winKey, string(pointJSON))
	pipe.LTrim(ctx, winKey, 0, UtilizationWindowSize-1)
	pipe.Expire(ctx, winKey, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis.WriteUtilization: pipeline EXEC: %w", err)
	}
	return nil
}

// ReadUtilization returns the latest snapshot of a SID. If the key is absent —
// (zero, false, nil), not an error (stale/never-reported is treated as "no
// data").
func ReadUtilization(ctx context.Context, c *Client, sid string) (UtilizationSnapshot, bool, error) {
	var snap UtilizationSnapshot
	if c == nil {
		return snap, false, errors.New("redis.ReadUtilization: nil client")
	}
	if sid == "" {
		return snap, false, errors.New("redis.ReadUtilization: empty sid")
	}
	res, err := c.underlying().HGetAll(ctx, UtilizationKey(sid)).Result()
	if err != nil {
		return snap, false, fmt.Errorf("redis.ReadUtilization: HGETALL %q: %w", UtilizationKey(sid), err)
	}
	if len(res) == 0 {
		return snap, false, nil
	}
	snap.CollectedAt = parseUtilTime(res["collected_at"])
	snap.ReceivedAt = parseUtilTime(res["received_at"])
	snap.CPUPct = parseUtilFloat(res["cpu_pct"])
	snap.Load1 = parseUtilFloat(res["load1"])
	snap.Load5 = parseUtilFloat(res["load5"])
	snap.Load15 = parseUtilFloat(res["load15"])
	snap.MemUsedMB = parseUtilInt(res["mem_used_mb"])
	snap.MemTotalMB = parseUtilInt(res["mem_total_mb"])
	snap.SwapUsedMB = parseUtilInt(res["swap_used_mb"])
	snap.UptimeSec = parseUtilInt(res["uptime_sec"])
	snap.IntervalSec = int32(parseUtilInt(res["interval_sec"]))
	if raw := res["disks"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &snap.Disks); err != nil {
			return snap, false, fmt.Errorf("redis.ReadUtilization: parse disks: %w", err)
		}
	}
	return snap, true, nil
}

// ReadUtilizationWindow returns up to limit of the latest window points (newest-first,
// as LPUSH laid them out). limit outside (0, UtilizationWindowSize] is capped to the window.
func ReadUtilizationWindow(ctx context.Context, c *Client, sid string, limit int) ([]UtilizationPoint, error) {
	if c == nil {
		return nil, errors.New("redis.ReadUtilizationWindow: nil client")
	}
	if sid == "" {
		return nil, errors.New("redis.ReadUtilizationWindow: empty sid")
	}
	if limit <= 0 || limit > UtilizationWindowSize {
		limit = UtilizationWindowSize
	}
	raws, err := c.underlying().LRange(ctx, UtilizationWindowKey(sid), 0, int64(limit-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("redis.ReadUtilizationWindow: LRANGE %q: %w", UtilizationWindowKey(sid), err)
	}
	pts := make([]UtilizationPoint, 0, len(raws))
	for _, raw := range raws {
		var p UtilizationPoint
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, fmt.Errorf("redis.ReadUtilizationWindow: parse point: %w", err)
		}
		pts = append(pts, p)
	}
	return pts, nil
}

func disksFromProto(in []*keeperv1.DiskUtilization) []DiskUsage {
	if len(in) == 0 {
		return nil
	}
	out := make([]DiskUsage, 0, len(in))
	for _, d := range in {
		if d == nil {
			continue
		}
		out = append(out, DiskUsage{Mount: d.GetMount(), UsedMB: d.GetUsedMb(), TotalMB: d.GetTotalMb()})
	}
	return out
}

// parseUtilTime — best-effort parse of RFC3339Nano; an empty/broken string → zero-time
// (telemetry is volatile, we treat a half-broken record leniently).
func parseUtilTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func parseUtilFloat(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
func parseUtilInt(s string) int64     { n, _ := strconv.ParseInt(s, 10, 64); return n }
