// Package soulprint is the Soul-side collector of host facts per the ADR-018
// typed schema.
//
// Collector gathers [keeperv1.SoulprintFacts] (os/kernel/cpu/memory/network +
// root sid/hostname) from an injected [Source] and fills a
// [keeperv1.SoulprintReport] to send to Keeper over EventStream
// (cmd/soul → StreamSession.SendSoulprintReport).
//
// Primary target is Linux (/proc, /etc/os-release). On macOS (dev machine)
// and other OSes values are best-effort/partial — Collect never panics and
// never returns an error: a missing fact stays zero-value, Keeper tolerates
// sparse fields (ADR-018).
//
// pkg_mgr / init_system are NOT detected via `command -v` (that's the job of
// runtime binary selection in core modules, see coremod/util.DetectPkgMgr) —
// here they're derived from family+distro via a fixed mapping table
// (osrelease.go), as ADR-018 requires.
package soulprint

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Collector gathers Soulprint facts. Source is injected for testability
// (production — [NewSystemSource], unit tests — fake). Stateless across
// calls: each Collect is an independent snapshot.
type Collector struct {
	src     Source
	metrics *SoulprintMetrics
}

// NewCollector builds a Collector over the given Source. For production —
// soulprint.NewCollector(soulprint.NewSystemSource(), metrics).
//
// metrics feeds soul_soulprint_*-collectors (ADR-024); nil → instrumentation
// disabled (nil-safe [SoulprintMetrics] methods — no-op): push mode and unit
// tests can run without an obs stack.
func NewCollector(src Source, metrics *SoulprintMetrics) *Collector {
	return &Collector{src: src, metrics: metrics}
}

// Collect takes one snapshot of host facts and wraps it in a SoulprintReport
// with collected_at = now (Soul-side timestamp, ADR-018). sid is an echo for
// logs (authority is the mTLS peer cert), comes from cmd/soul
// (config.sid > hostname).
//
// Never returns an error: any unavailable fact stays zero-value. Deliberate
// — a partial report beats none, and Keeper doesn't require completeness
// (sparse JSONB, ADR-018).
func (c *Collector) Collect(ctx context.Context, sid string) *keeperv1.SoulprintReport {
	start := time.Now()
	defer func() { c.metrics.ObserveCollectDuration(time.Since(start).Seconds()) }()

	rep := &keeperv1.SoulprintReport{
		CollectedAt: timestamppb.Now(),
		TypedFacts:  c.collectFacts(ctx, sid),
	}
	// Collect is best-effort and never returns an error (ADR-018): a missing
	// fact is zero-value, not a failure. So always `ok`; `failed` is reserved
	// for future fatal collection scenarios.
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
