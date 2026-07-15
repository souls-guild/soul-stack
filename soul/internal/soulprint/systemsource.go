package soulprint

import (
	"context"
	"net"
	"os"
	"runtime"
	"strings"
)

// systemSource is the production [Source] implementation. Linux reads /proc
// and /etc/os-release; other platforms (macOS dev machine, etc.) get
// best-effort via runtime/net plus platform-specific helpers
// (systemsource_*.go).
//
// osReleasePath / readFile are fields so platform-independent branches
// (os-release parsing, network) can be tested without touching the real
// filesystem.
type systemSource struct {
	osReleasePath string
	readFile      func(string) ([]byte, error)
	hostname      func() (string, error)
	interfaces    func() ([]net.Interface, error)
}

// NewSystemSource builds the production Source over the real filesystem/network.
func NewSystemSource() Source {
	return &systemSource{
		osReleasePath: "/etc/os-release",
		readFile:      os.ReadFile,
		hostname:      os.Hostname,
		interfaces:    net.Interfaces,
	}
}

// Hostname returns the short name (no domain). os.Hostname returns an FQDN
// on some systems — truncated at the first dot to match ADR-018 semantics
// (hostname is short; fqdn is a separate fact under network).
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

// Arch returns Go's architecture notation (amd64 / arm64), matching what
// core modules and the essence pipeline expect.
func (s *systemSource) Arch() string {
	return runtime.GOARCH
}

// OS returns family/distro/version/codename. On Linux, from /etc/os-release.
// Other OSes have no os-release: family/distro derive from runtime.GOOS
// (darwin/windows), version is best-effort via the platform-specific
// osVersion (systemsource_*.go).
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

// osNonLinux returns best-effort OSInfo for non-Linux. darwin → fixed
// family/distro (macos), version via osVersion (sw_vers/sysctl). windows →
// family/distro, version left empty for now (detection is post-MVP). Other
// GOOS → family=GOOS.
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

// Kernel / CPU / Memory delegate to platform-specific functions
// (systemsource_linux.go / systemsource_darwin.go / systemsource_other.go).
func (s *systemSource) Kernel(ctx context.Context) KernelInfo { return kernelInfo(ctx) }
func (s *systemSource) CPU(ctx context.Context) CPUInfo       { return cpuInfo(ctx) }
func (s *systemSource) Memory(ctx context.Context) MemoryInfo { return memoryInfo(ctx) }

// Network is cross-platform via net.Interfaces. primary_ip heuristic: the
// first non-loopback up interface with a globally-routable IPv4.
// (default-route detection is a post-MVP refinement; for 90% of cases,
// non-loopback global IPv4 matches the bind address, ADR-018.)
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

// fqdn returns the full FQDN. os.Hostname yields the FQDN on correctly
// configured hosts; if short, returned as-is (no reverse-DNS lookup in MVP,
// to avoid blocking on the network on every fact collection).
func (s *systemSource) fqdn() string {
	h, err := s.hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(h)
}

// isPrimaryCandidate: interface is up, not loopback, IP is globally routable
// (not link-local, not loopback). The first such interface → primary_ip.
func isPrimaryCandidate(iface net.Interface, ip net.IP) bool {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
		return false
	}
	return ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast()
}
