// Package soulprint — Soul-side сбор фактов о хосте по typed-схеме ADR-018.
//
// Collector собирает [keeperv1.SoulprintFacts] (os/kernel/cpu/memory/network +
// корневые sid/hostname) из инъецируемого [Source] и заполняет
// [keeperv1.SoulprintReport] для отправки Keeper-у через EventStream
// (cmd/soul → StreamSession.SendSoulprintReport).
//
// Основная цель — Linux (/proc, /etc/os-release). На macOS (dev-машина) и
// прочих ОС значения best-effort/частичные — Collect никогда не паникует и не
// возвращает error: отсутствующий факт остаётся zero-value, Keeper толерантен
// к sparse-полям (ADR-018).
//
// pkg_mgr / init_system НЕ детектятся через `command -v` (это задача
// рантайм-выбора бинаря в core-модулях, см. coremod/util.DetectPkgMgr) —
// здесь они выводятся из family+distro по фиксированной таблице маппинга
// (osrelease.go), как требует ADR-018.
package soulprint

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Collector собирает Soulprint-факты. Source инъецируется ради тестируемости
// (production — [NewSystemSource], unit-тесты — fake). Без состояния между
// вызовами: каждый Collect — независимый снимок.
type Collector struct {
	src     Source
	metrics *SoulprintMetrics
}

// NewCollector собирает Collector над переданным Source. Для production —
// soulprint.NewCollector(soulprint.NewSystemSource(), metrics).
//
// metrics — soul_soulprint_*-collectors (ADR-024); nil → инструментация
// выключена (nil-safe методы [SoulprintMetrics] — no-op): push-режим и
// unit-тесты поднимаются без obs-стека.
func NewCollector(src Source, metrics *SoulprintMetrics) *Collector {
	return &Collector{src: src, metrics: metrics}
}

// Collect делает один снимок фактов хоста и заворачивает его в SoulprintReport
// с collected_at = now (Soul-side timestamp, ADR-018). sid — echo для логов
// (authority — mTLS peer cert), приходит из cmd/soul (config.sid > hostname).
//
// Ошибок не возвращает: любой недоступный факт остаётся zero-value. Это
// сознательно — частичный отчёт полезнее отсутствующего, а Keeper не требует
// заполненности (sparse JSONB, ADR-018).
func (c *Collector) Collect(ctx context.Context, sid string) *keeperv1.SoulprintReport {
	start := time.Now()
	defer func() { c.metrics.ObserveCollectDuration(time.Since(start).Seconds()) }()

	rep := &keeperv1.SoulprintReport{
		CollectedAt: timestamppb.Now(),
		TypedFacts:  c.collectFacts(ctx, sid),
	}
	// Collect best-effort и не возвращает error (ADR-018): отсутствующий факт —
	// zero-value, не сбой. Поэтому всегда `ok`; `failed` зарезервирован под
	// будущие fatal-сценарии сбора.
	c.metrics.ObserveCollection(collectResultOK)
	return rep
}

func (c *Collector) collectFacts(ctx context.Context, sid string) *keeperv1.SoulprintFacts {
	os := c.src.OS(ctx)
	pkgMgr, initSystem := pkgMgrInitSystem(os.Family, os.Distro)

	return &keeperv1.SoulprintFacts{
		Sid:      sid,
		Hostname: c.src.Hostname(),
		Os: &keeperv1.OsFacts{
			Family:     os.Family,
			Distro:     os.Distro,
			Version:    os.Version,
			Codename:   os.Codename,
			Arch:       c.src.Arch(),
			PkgMgr:     pkgMgr,
			InitSystem: initSystem,
		},
		Kernel:  c.collectKernel(ctx),
		Cpu:     c.collectCPU(ctx),
		Memory:  c.collectMemory(ctx),
		Network: c.collectNetwork(),
	}
}

func (c *Collector) collectKernel(ctx context.Context) *keeperv1.KernelFacts {
	k := c.src.Kernel(ctx)
	return &keeperv1.KernelFacts{
		Version: k.Version,
		Release: k.Release,
	}
}

func (c *Collector) collectCPU(ctx context.Context) *keeperv1.CpuFacts {
	cpu := c.src.CPU(ctx)
	return &keeperv1.CpuFacts{
		Count:  cpu.Count,
		Model:  cpu.Model,
		Vendor: cpu.Vendor,
	}
}

func (c *Collector) collectMemory(ctx context.Context) *keeperv1.MemoryFacts {
	m := c.src.Memory(ctx)
	return &keeperv1.MemoryFacts{
		TotalMb:     m.TotalMB,
		AvailableMb: m.AvailableMB,
		SwapMb:      m.SwapMB,
	}
}

func (c *Collector) collectNetwork() *keeperv1.NetworkFacts {
	n := c.src.Network()
	ifaces := make([]*keeperv1.NetworkInterface, 0, len(n.Interfaces))
	for _, iface := range n.Interfaces {
		ifaces = append(ifaces, &keeperv1.NetworkInterface{
			Name: iface.Name,
			Ipv4: iface.IPv4,
			Ipv6: iface.IPv6,
			Mac:  iface.MAC,
			Mtu:  iface.MTU,
		})
	}
	return &keeperv1.NetworkFacts{
		PrimaryIp:  n.PrimaryIP,
		Fqdn:       n.FQDN,
		Interfaces: ifaces,
	}
}
