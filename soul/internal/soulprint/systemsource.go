package soulprint

import (
	"context"
	"net"
	"os"
	"runtime"
	"strings"
)

// systemSource — production-реализация [Source]. Linux читает /proc и
// /etc/os-release; прочие платформы (macOS dev-машина и т.п.) — best-effort
// через runtime/net + платформо-зависимые helper-ы (systemsource_*.go).
//
// osReleasePath / readFile вынесены в поля ради тестов platform-независимых
// веток (parse os-release, network) без касания реальной ФС.
type systemSource struct {
	osReleasePath string
	readFile      func(string) ([]byte, error)
	hostname      func() (string, error)
	interfaces    func() ([]net.Interface, error)
}

// NewSystemSource собирает production-Source поверх реальной ФС/сети.
func NewSystemSource() Source {
	return &systemSource{
		osReleasePath: "/etc/os-release",
		readFile:      os.ReadFile,
		hostname:      os.Hostname,
		interfaces:    net.Interfaces,
	}
}

// Hostname — короткое имя (без домена). os.Hostname на части систем возвращает
// FQDN — обрезаем по первой точке, чтобы соответствовать семантике ADR-018
// (hostname короткий, fqdn — отдельный факт в network).
func (s *systemSource) Hostname() string {
	h, err := s.hostname()
	if err != nil {
		return ""
	}
	h = strings.TrimSpace(h)
	if i := strings.IndexByte(h, '.'); i > 0 {
		return h[:i]
	}
	return h
}

// Arch — Go-нотация архитектуры (amd64 / arm64), совпадает со значениями,
// которые ждут core-модули и essence pipeline.
func (s *systemSource) Arch() string {
	return runtime.GOARCH
}

// OS — family/distro/version/codename. На Linux — из /etc/os-release. На прочих
// ОС os-release нет: family/distro выводятся из runtime.GOOS (darwin/windows),
// version best-effort через платформо-зависимый osVersion (systemsource_*.go).
func (s *systemSource) OS(ctx context.Context) OSInfo {
	if runtime.GOOS == "linux" {
		raw, err := s.readFile(s.osReleasePath)
		if err != nil {
			return OSInfo{}
		}
		r := parseOSRelease(string(raw))
		return OSInfo{
			Family:   r.family(),
			Distro:   r.id,
			Version:  r.versionID,
			Codename: r.versionCodename,
		}
	}
	return s.osNonLinux(ctx)
}

// osNonLinux — best-effort OSInfo для не-Linux. darwin → family/distro фиксируем
// (macos), version через osVersion (sw_vers/sysctl). windows → family/distro,
// version пока пусто (детект — пост-MVP). Прочие GOOS → family=GOOS.
func (s *systemSource) osNonLinux(ctx context.Context) OSInfo {
	switch runtime.GOOS {
	case "darwin":
		return OSInfo{Family: "darwin", Distro: "macos", Version: osVersion(ctx)}
	case "windows":
		return OSInfo{Family: "windows", Distro: "windows"}
	default:
		return OSInfo{Family: runtime.GOOS}
	}
}

// Kernel / CPU / Memory делегируются платформо-зависимым функциям
// (systemsource_linux.go / systemsource_darwin.go / systemsource_other.go).
func (s *systemSource) Kernel(ctx context.Context) KernelInfo { return kernelInfo(ctx) }
func (s *systemSource) CPU(ctx context.Context) CPUInfo       { return cpuInfo(ctx) }
func (s *systemSource) Memory(ctx context.Context) MemoryInfo { return memoryInfo(ctx) }

// Network — кроссплатформенно через net.Interfaces. primary_ip эвристика —
// первый non-loopback up-интерфейс с глобально-маршрутизируемым IPv4.
// (default-route detection — пост-MVP уточнение; для 90% случаев non-loopback
// global IPv4 совпадает с bind-адресом, ADR-018.)
func (s *systemSource) Network() NetworkInfo {
	out := NetworkInfo{FQDN: s.fqdn()}
	ifaces, err := s.interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		ii := InterfaceInfo{
			Name: iface.Name,
			MAC:  iface.HardwareAddr.String(),
			MTU:  int32(iface.MTU),
		}
		addrs, err := iface.Addrs()
		if err != nil {
			out.Interfaces = append(out.Interfaces, ii)
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ipnet.IP.To4() != nil {
				ii.IPv4 = append(ii.IPv4, ipnet.String())
				if out.PrimaryIP == "" && isPrimaryCandidate(iface, ipnet.IP) {
					out.PrimaryIP = ipnet.IP.String()
				}
			} else {
				ii.IPv6 = append(ii.IPv6, ipnet.String())
			}
		}
		out.Interfaces = append(out.Interfaces, ii)
	}
	return out
}

// fqdn — полный FQDN. os.Hostname отдаёт FQDN на корректно сконфигурированных
// хостах; если короткое — возвращаем как есть (reverse-DNS lookup в MVP не
// делаем, чтобы не блокироваться на сети при каждом сборе фактов).
func (s *systemSource) fqdn() string {
	h, err := s.hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(h)
}

// isPrimaryCandidate — интерфейс up, не loopback, IP глобально маршрутизируем
// (не link-local, не loopback). Первый такой → primary_ip.
func isPrimaryCandidate(iface net.Interface, ip net.IP) bool {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
		return false
	}
	return ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast()
}
