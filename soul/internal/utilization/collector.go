// Package utilization — Soul-side сбор живой утилизации хоста (CPU%/load/mem/
// disk/uptime) и заполнение [keeperv1.HostUtilization] для периодической
// отправки Keeper-у по presence-каналу (ADR-071).
//
// Волатильный слой, отдельный от статического soulprint (ADR-018): свой каденс
// (~30s), не targeting-факт, хранится в Redis (горячее, не PG). Best-effort как
// soulprint — недоступный факт остаётся zero-value, Collect не паникует и не
// возвращает error.
package utilization

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Collector собирает снимок утилизации. В отличие от soulprint.Collector —
// stateful: хранит предыдущий CPUSample, т.к. cpu% считается дельтой тиков
// между двумя сборами. Вызывается из единственного writer-а сессии (select-loop
// handleSession) — конкурентных Collect нет, синхронизация не нужна.
type Collector struct {
	src  Source
	prev CPUSample
	seen bool // был ли уже сэмпл (первый Collect → cpu%=0)
}

// NewCollector собирает Collector над переданным Source. Для production —
// utilization.NewCollector(utilization.NewSystemSource()).
func NewCollector(src Source) *Collector {
	return &Collector{src: src}
}

// Collect делает один снимок утилизации с collected_at = now. sid в message нет
// (authority — mTLS peer cert на Keeper-е, ADR-071); принимается лишь ради
// симметрии сигнатуры с soulprint.Collect. Ошибок не возвращает.
func (c *Collector) Collect(ctx context.Context, _ string) *keeperv1.HostUtilization {
	load := c.src.Load(ctx)
	mem := c.src.Memory(ctx)
	return &keeperv1.HostUtilization{
		CollectedAt: timestamppb.Now(),
		CpuPct:      c.cpuPct(c.src.CPUSample(ctx)),
		Load1:       load.One,
		Load5:       load.Five,
		Load15:      load.Fifteen,
		MemUsedMb:   mem.UsedMB,
		MemTotalMb:  mem.TotalMB,
		SwapUsedMb:  mem.SwapUsedMB,
		Disks:       c.disks(ctx),
		UptimeSec:   c.src.Uptime(ctx),
	}
}

// cpuPct — 100*(busyΔ/totalΔ), busy = Total-Idle, дельта относительно прошлого
// сэмпла. Первый сэмпл и невозрастающий totalΔ (счётчик не двинулся / reset) →
// 0; результат зажат 0..100.
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
