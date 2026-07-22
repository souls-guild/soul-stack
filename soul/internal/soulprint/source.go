package soulprint

import "context"

// Source is the access layer for OS facts. Separated from Collector for
// testability: the production implementation ([systemSource]) reads
// /proc, /etc/os-release, net.*; unit tests substitute a fake with fixed
// values.
//
// All methods are best-effort: an unavailable fact returns a zero value
// (empty string / 0 / empty slice), never an error or panic. ADR-018: a
// partial fact beats a missing one (Keeper tolerates sparse fields).
type Source interface {
	// Hostname is the short host name (no domain), uname -n / gethostname().
	Hostname() string
	// Arch is the target OS architecture in Go notation (amd64 / arm64).
	Arch() string
	// OS returns OS facts (family/distro/version/codename). pkg_mgr/init_system
	// are derived by Collector from family+distro, not by Source.
	OS(ctx context.Context) OSInfo
	// Kernel returns kernel version and release.
	Kernel(ctx context.Context) KernelInfo
	// CPU returns logical CPU count + model/vendor.
	CPU(ctx context.Context) CPUInfo
	// Memory returns RAM/available/swap in MB.
	Memory(ctx context.Context) MemoryInfo
	// Network returns primary_ip, fqdn, and the interface list.
	Network() NetworkInfo
}

// OSInfo holds OS facts from Source. pkg_mgr/init_system are NOT here: they're
// derived from family+distro (osrelease.go::pkgMgrInitSystem).
type OSInfo struct {
	Family   string // debian / rhel / alpine / darwin / windows
	Distro   string // ubuntu / rocky / alpine / macos
	Version  string // "22.04" / "9.3" / "14.4"
	Codename string // "jammy" / "" (not all distros have one)
}

// KernelInfo holds kernel facts.
type KernelInfo struct {
	Version string // full version with distro suffix (5.15.0-101-generic)
	Release string // kernel version only (5.15.0)
}

// CPUInfo holds CPU facts.
type CPUInfo struct {
	Count  int32  // logical CPUs (including HT/SMT)
	Model  string // marketing name (Intel Xeon E5-2670 / Apple M2)
	Vendor string // GenuineIntel / AuthenticAMD / Apple
}

// MemoryInfo holds memory facts. Everything in MB (not bytes) — conversion
// happens in Source so Collector doesn't need to know /proc/meminfo units.
type MemoryInfo struct {
	TotalMB     int64
	AvailableMB int64
	SwapMB      int64
}

// NetworkInfo holds network facts.
type NetworkInfo struct {
	PrimaryIP  string // primary IPv4 (the interface with the default route)
	FQDN       string // full FQDN (usually == SID)
	Interfaces []InterfaceInfo
}

// InterfaceInfo describes one network interface.
type InterfaceInfo struct {
	Name string
	IPv4 []string // CIDR notation (10.0.0.1/24)
	IPv6 []string
	MAC  string
	MTU  int32
}
