package soulprint

import "context"

// Source — слой доступа к фактам ОС. Отделён от Collector ради тестируемости:
// production-реализация ([systemSource]) читает /proc, /etc/os-release, net.*;
// unit-тесты подменяют её на fake с фиксированными значениями.
//
// Все методы best-effort: при недоступности факта возвращают zero-value
// (пустую строку / 0 / пустой slice), не error и не panic. Решение «частичный
// факт полезнее отсутствующего» принято на уровне ADR-018 (Keeper толерантен к
// sparse-полям).
type Source interface {
	// Hostname — короткое имя хоста (без домена), uname -n / gethostname().
	Hostname() string
	// Arch — архитектура target ОС в Go-нотации (amd64 / arm64).
	Arch() string
	// OS — факты об операционной системе (family/distro/version/codename).
	// pkg_mgr/init_system выводятся Collector-ом из family+distro, не Source.
	OS(ctx context.Context) OSInfo
	// Kernel — версия и release ядра.
	Kernel(ctx context.Context) KernelInfo
	// CPU — число logical CPU + model/vendor.
	CPU(ctx context.Context) CPUInfo
	// Memory — RAM/available/swap в МБ.
	Memory(ctx context.Context) MemoryInfo
	// Network — primary_ip, fqdn, список интерфейсов.
	Network() NetworkInfo
}

// OSInfo — факты ОС из Source. pkg_mgr/init_system здесь НЕТ: они производные
// от family+distro (osrelease.go::pkgMgrInitSystem).
type OSInfo struct {
	Family   string // debian / rhel / alpine / darwin / windows
	Distro   string // ubuntu / rocky / alpine / macos
	Version  string // "22.04" / "9.3" / "14.4"
	Codename string // "jammy" / "" (не у всех distros)
}

// KernelInfo — факты ядра.
type KernelInfo struct {
	Version string // полная версия с дистрибутив-suffix (5.15.0-101-generic)
	Release string // только версия ядра (5.15.0)
}

// CPUInfo — факты процессоров.
type CPUInfo struct {
	Count  int32  // logical CPUs (с учётом HT/SMT)
	Model  string // маркетинговое имя (Intel Xeon E5-2670 / Apple M2)
	Vendor string // GenuineIntel / AuthenticAMD / Apple
}

// MemoryInfo — факты памяти. Всё в МБ (не байтах) — конвертация на стороне
// Source, чтобы Collector не знал про единицы /proc/meminfo.
type MemoryInfo struct {
	TotalMB     int64
	AvailableMB int64
	SwapMB      int64
}

// NetworkInfo — факты сети.
type NetworkInfo struct {
	PrimaryIP  string // основной IPv4 (интерфейс с default-route)
	FQDN       string // полный FQDN (обычно == SID)
	Interfaces []InterfaceInfo
}

// InterfaceInfo — один сетевой интерфейс.
type InterfaceInfo struct {
	Name string
	IPv4 []string // CIDR-нотация (10.0.0.1/24)
	IPv6 []string
	MAC  string
	MTU  int32
}
