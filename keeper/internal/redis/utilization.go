package redis

// Host-vitals кэш Soul-агентов в Redis (NIM-86): последний снимок утилизации
// (Hash `soul:<sid>:util`) + короткое окно точек (list-ring
// `soul:<sid>:util:win`) для спарклайнов. Только Redis, без PG — телеметрия
// волатильна и живёт под TTL (рестарт Redis → данные теряются, это норма).
// Ключуется по аутентифицированному SID (mTLS peer), не по payload.

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
	// UtilizationWindowSize — длина окна точек (cap для LTRIM).
	UtilizationWindowSize = 60
	// UtilizationTTL — ФЛОР времени жизни latest/окна (NIM-87). Эффективный TTL
	// масштабируется [utilizationTTL] под каденс Soul-а (3× interval_sec из
	// payload), но не опускается ниже этого флора: при 0/старом soul (нет
	// interval_sec) ключ живёт ≥3 pulse-а на дефолтном каденсе 30s.
	UtilizationTTL = 90 * time.Second
	// utilizationIntervalSecCeil — верхний кламп interval_sec из payload (3×3600) против «бессмертного» TTL от rogue/старого Soul (безопасность на первом месте).
	utilizationIntervalSecCeil = 3 * 3600
)

// utilizationTTL масштабирует TTL vitals-ключей под эффективный каденс Soul-а
// (NIM-87): 3× interval_sec, кламп в [UtilizationTTL, 3×utilizationIntervalSecCeil].
// intervalSec ≤0 (старый soul без поля / сбой) → флор; аномально большой (rogue/
// забагованный Soul) капается потолком — иначе TTL-ключ стал бы «бессмертным».
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

// UtilizationKey — Hash последнего снимка утилизации SID-а.
func UtilizationKey(sid string) string { return "soul:" + sid + ":util" }

// UtilizationWindowKey — list-ring окна точек утилизации SID-а.
func UtilizationWindowKey(sid string) string { return "soul:" + sid + ":util:win" }

// DiskUsage — использование одного примонтированного тома.
type DiskUsage struct {
	Mount   string `json:"mount"`
	UsedMB  int64  `json:"used_mb"`
	TotalMB int64  `json:"total_mb"`
}

// UtilizationSnapshot — последний снимок host-vitals хоста.
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
	// IntervalSec — эффективный каденс, на котором Soul прислал снимок (NIM-87);
	// 0 для старого soul без поля. Отражает решённый Keeper-ом telemetry-интервал.
	IntervalSec int32
}

// UtilizationPoint — компактная точка окна для спарклайнов.
type UtilizationPoint struct {
	CollectedAt time.Time `json:"collected_at"`
	CPUPct      float64   `json:"cpu_pct"`
	Load1       float64   `json:"load1"`
	MemUsedMB   int64     `json:"mem_used_mb"`
	MemTotalMB  int64     `json:"mem_total_mb"`
}

// WriteUtilization кладёт снимок утилизации SID-а в Redis одним pipeline: latest
// Hash (все поля + received_at=now, disks — JSON-строкой) с TTL + окно (LPUSH
// компактной точки + LTRIM cap + Expire). НЕ транзакция: latest и окно независимы
// (readers их не сверяют), а plain Pipeline роутит по-ключу и не даёт CROSSSLOT на
// Redis Cluster — как heartbeat/soullease.
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

	// TTL масштабируется под эффективный каденс Soul-а (NIM-87): больший интервал
	// → больший TTL (≥3 pulse-а), с флором UtilizationTTL для 0/старого soul.
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

// ReadUtilization возвращает последний снимок SID-а. На отсутствие ключа —
// (zero, false, nil), не ошибка (stale/never-reported трактуется как «нет
// данных»).
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

// ReadUtilizationWindow возвращает до limit последних точек окна (newest-first,
// как их сложил LPUSH). limit вне (0, UtilizationWindowSize] капается к окну.
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

// parseUtilTime — best-effort парс RFC3339Nano; пустая/битая строка → zero-time
// (телеметрия волатильна, полу-битую запись трактуем мягко).
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
