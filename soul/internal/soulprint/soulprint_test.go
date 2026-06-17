package soulprint

import (
	"context"
	"testing"
)

// fakeSource — детерминированный Source для проверки Collect → typed_facts
// без касания реальной системы.
type fakeSource struct {
	hostname string
	arch     string
	os       OSInfo
	kernel   KernelInfo
	cpu      CPUInfo
	memory   MemoryInfo
	network  NetworkInfo
}

func (f fakeSource) Hostname() string                  { return f.hostname }
func (f fakeSource) Arch() string                      { return f.arch }
func (f fakeSource) OS(context.Context) OSInfo         { return f.os }
func (f fakeSource) Kernel(context.Context) KernelInfo { return f.kernel }
func (f fakeSource) CPU(context.Context) CPUInfo       { return f.cpu }
func (f fakeSource) Memory(context.Context) MemoryInfo { return f.memory }
func (f fakeSource) Network() NetworkInfo              { return f.network }

func TestCollect_FillsTypedFacts(t *testing.T) {
	src := fakeSource{
		hostname: "redis1",
		arch:     "amd64",
		os:       OSInfo{Family: "debian", Distro: "ubuntu", Version: "22.04", Codename: "jammy"},
		kernel:   KernelInfo{Version: "5.15.0-101-generic", Release: "5.15.0"},
		cpu:      CPUInfo{Count: 4, Model: "Intel Xeon E5-2670", Vendor: "GenuineIntel"},
		memory:   MemoryInfo{TotalMB: 16000, AvailableMB: 8000, SwapMB: 2000},
		network: NetworkInfo{
			PrimaryIP: "10.0.0.5",
			FQDN:      "redis1.cache.example",
			Interfaces: []InterfaceInfo{
				{Name: "eth0", IPv4: []string{"10.0.0.5/24"}, IPv6: []string{"fe80::1/64"}, MAC: "aa:bb:cc:dd:ee:ff", MTU: 1500},
			},
		},
	}

	rep := NewCollector(src, nil).Collect(context.Background(), "redis1.cache.example")

	if rep.GetCollectedAt() == nil {
		t.Fatal("collected_at must be set (Soul-side timestamp, ADR-018)")
	}
	tf := rep.GetTypedFacts()
	if tf == nil {
		t.Fatal("typed_facts must be populated")
	}
	if tf.GetSid() != "redis1.cache.example" {
		t.Errorf("sid=%q want redis1.cache.example", tf.GetSid())
	}
	if tf.GetHostname() != "redis1" {
		t.Errorf("hostname=%q want redis1", tf.GetHostname())
	}

	os := tf.GetOs()
	if os.GetFamily() != "debian" || os.GetDistro() != "ubuntu" || os.GetVersion() != "22.04" || os.GetCodename() != "jammy" {
		t.Errorf("os mismatch: %+v", os)
	}
	if os.GetArch() != "amd64" {
		t.Errorf("arch=%q want amd64", os.GetArch())
	}
	// pkg_mgr/init_system выводятся из family+distro по таблице ADR-018.
	if os.GetPkgMgr() != "apt" || os.GetInitSystem() != "systemd" {
		t.Errorf("pkg_mgr/init_system=%q/%q want apt/systemd", os.GetPkgMgr(), os.GetInitSystem())
	}

	if k := tf.GetKernel(); k.GetVersion() != "5.15.0-101-generic" || k.GetRelease() != "5.15.0" {
		t.Errorf("kernel mismatch: %+v", k)
	}
	if c := tf.GetCpu(); c.GetCount() != 4 || c.GetModel() != "Intel Xeon E5-2670" || c.GetVendor() != "GenuineIntel" {
		t.Errorf("cpu mismatch: %+v", c)
	}
	if m := tf.GetMemory(); m.GetTotalMb() != 16000 || m.GetAvailableMb() != 8000 || m.GetSwapMb() != 2000 {
		t.Errorf("memory mismatch: %+v", m)
	}

	n := tf.GetNetwork()
	if n.GetPrimaryIp() != "10.0.0.5" || n.GetFqdn() != "redis1.cache.example" {
		t.Errorf("network mismatch: primary=%q fqdn=%q", n.GetPrimaryIp(), n.GetFqdn())
	}
	if len(n.GetInterfaces()) != 1 {
		t.Fatalf("interfaces=%d want 1", len(n.GetInterfaces()))
	}
	iface := n.GetInterfaces()[0]
	if iface.GetName() != "eth0" || iface.GetMac() != "aa:bb:cc:dd:ee:ff" || iface.GetMtu() != 1500 {
		t.Errorf("interface meta mismatch: %+v", iface)
	}
	if len(iface.GetIpv4()) != 1 || iface.GetIpv4()[0] != "10.0.0.5/24" {
		t.Errorf("ipv4 mismatch: %v", iface.GetIpv4())
	}
	if len(iface.GetIpv6()) != 1 || iface.GetIpv6()[0] != "fe80::1/64" {
		t.Errorf("ipv6 mismatch: %v", iface.GetIpv6())
	}
}

// Collect на пустом Source (нераспознанная ОС / недоступные факты) не должен
// паниковать и обязан вернуть непустой report с zero-value полями — Keeper
// толерантен к sparse-фактам (ADR-018).
func TestCollect_EmptySourceNoPanic(t *testing.T) {
	rep := NewCollector(fakeSource{}, nil).Collect(context.Background(), "host-x")
	if rep.GetTypedFacts() == nil {
		t.Fatal("typed_facts must be non-nil even for empty source")
	}
	if rep.GetTypedFacts().GetSid() != "host-x" {
		t.Errorf("sid=%q want host-x", rep.GetTypedFacts().GetSid())
	}
	// Нераспознанный family → пустые pkg_mgr/init_system, не паника.
	if pm := rep.GetTypedFacts().GetOs().GetPkgMgr(); pm != "" {
		t.Errorf("pkg_mgr=%q want empty for unknown OS", pm)
	}
}

// Контракт: collected_at — момент сбора (proto Timestamp валиден).
func TestCollect_CollectedAtIsValid(t *testing.T) {
	rep := NewCollector(fakeSource{}, nil).Collect(context.Background(), "h")
	if err := rep.GetCollectedAt().CheckValid(); err != nil {
		t.Fatalf("collected_at invalid: %v", err)
	}
}
