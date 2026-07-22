package soulprint

import (
	"context"
	"errors"
	"net"
	"runtime"
	"testing"
)

func TestSystemSource_HostnameShortened(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"redis1.cache.example", "redis1"},
		{"redis1", "redis1"},
		{"  host.with.space  ", "host"},
		{"", ""},
	}
	for _, tc := range cases {
		s := &systemSource{hostname: func() (string, error) { return tc.in, nil }}
		if got := s.Hostname(); got != tc.want {
			t.Errorf("Hostname(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestSystemSource_HostnameError(t *testing.T) {
	s := &systemSource{hostname: func() (string, error) { return "", errors.New("boom") }}
	if got := s.Hostname(); got != "" {
		t.Errorf("Hostname on error=%q want empty", got)
	}
}

func TestSystemSource_FQDNKeepsFull(t *testing.T) {
	s := &systemSource{hostname: func() (string, error) { return "redis1.cache.example", nil }}
	if got := s.fqdn(); got != "redis1.cache.example" {
		t.Errorf("fqdn=%q want full", got)
	}
}

// The non-Linux OS branch (darwin/windows/other) doesn't read os-release —
// test osNonLinux directly, independent of the test's GOOS.
func TestSystemSource_OSNonLinux(t *testing.T) {
	s := &systemSource{}
	got := s.osNonLinux(context.Background())
	switch runtime.GOOS {
	case "darwin":
		if got.Family != "darwin" || got.Distro != "macos" {
			t.Errorf("darwin OSInfo=%+v", got)
		}
	case "windows":
		if got.Family != "windows" || got.Distro != "windows" {
			t.Errorf("windows OSInfo=%+v", got)
		}
	default:
		if got.Family != runtime.GOOS {
			t.Errorf("family=%q want %q", got.Family, runtime.GOOS)
		}
	}
}

// OS on Linux reads /etc/os-release via an injected readFile.
func TestSystemSource_OSLinuxParse(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only os-release branch")
	}
	s := &systemSource{
		osReleasePath: "/etc/os-release",
		readFile: func(string) ([]byte, error) {
			return []byte("ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"22.04\"\nVERSION_CODENAME=jammy\n"), nil
		},
	}
	got := s.OS(context.Background())
	if got.Family != "debian" || got.Distro != "ubuntu" || got.Version != "22.04" || got.Codename != "jammy" {
		t.Errorf("OSInfo=%+v", got)
	}
}

func TestSystemSource_OSLinuxReadError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only os-release branch")
	}
	s := &systemSource{
		osReleasePath: "/etc/os-release",
		readFile:      func(string) ([]byte, error) { return nil, errors.New("nope") },
	}
	if got := s.OS(context.Background()); got != (OSInfo{}) {
		t.Errorf("OSInfo on read error=%+v want zero", got)
	}
}

// Arch is always non-empty (runtime.GOARCH).
func TestSystemSource_Arch(t *testing.T) {
	s := &systemSource{}
	if s.Arch() == "" {
		t.Error("Arch must be non-empty")
	}
}

// Network with an empty interface list: FQDN is populated, no interfaces, no panic.
func TestSystemSource_NetworkEmpty(t *testing.T) {
	s := &systemSource{
		hostname:   func() (string, error) { return "h.example", nil },
		interfaces: func() ([]net.Interface, error) { return nil, nil },
	}
	n := s.Network()
	if n.FQDN != "h.example" {
		t.Errorf("fqdn=%q", n.FQDN)
	}
	if len(n.Interfaces) != 0 {
		t.Errorf("interfaces=%d want 0", len(n.Interfaces))
	}
	if n.PrimaryIP != "" {
		t.Errorf("primary_ip=%q want empty (no interfaces)", n.PrimaryIP)
	}
}

func TestSystemSource_NetworkInterfacesError(t *testing.T) {
	s := &systemSource{
		hostname:   func() (string, error) { return "h", nil },
		interfaces: func() ([]net.Interface, error) { return nil, errors.New("boom") },
	}
	n := s.Network()
	if n.FQDN != "h" || len(n.Interfaces) != 0 {
		t.Errorf("network on iface error=%+v", n)
	}
}

func TestIsPrimaryCandidate(t *testing.T) {
	up := net.Interface{Flags: net.FlagUp}
	loop := net.Interface{Flags: net.FlagUp | net.FlagLoopback}
	down := net.Interface{}

	cases := []struct {
		name  string
		iface net.Interface
		ip    net.IP
		want  bool
	}{
		{"global on up iface", up, net.ParseIP("10.0.0.5"), true},
		{"loopback iface excluded", loop, net.ParseIP("127.0.0.1"), false},
		{"down iface excluded", down, net.ParseIP("10.0.0.5"), false},
		{"link-local excluded", up, net.ParseIP("169.254.1.1"), false},
		{"loopback ip excluded", up, net.ParseIP("127.0.0.1"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPrimaryCandidate(tc.iface, tc.ip); got != tc.want {
				t.Errorf("isPrimaryCandidate=%v want %v", got, tc.want)
			}
		})
	}
}

// Smoke: production NewSystemSource on the current host collects facts
// without panicking (best-effort: tolerant of values, we only check invariants).
func TestNewSystemSource_SmokeCollect(t *testing.T) {
	rep := NewCollector(NewSystemSource(), nil).Collect(context.Background(), "smoke-sid")
	tf := rep.GetTypedFacts()
	if tf == nil {
		t.Fatal("typed_facts nil")
	}
	if tf.GetSid() != "smoke-sid" {
		t.Errorf("sid=%q", tf.GetSid())
	}
	if tf.GetOs().GetArch() == "" {
		t.Error("arch must be detected on any platform")
	}
	if tf.GetCpu().GetCount() <= 0 {
		t.Errorf("cpu count=%d want >0 (runtime.NumCPU fallback)", tf.GetCpu().GetCount())
	}
}
