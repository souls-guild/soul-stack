//go:build linux

package utilization

import (
	"context"
	"testing"
)

func TestVirtualFS(t *testing.T) {
	virtual := []string{"tmpfs", "proc", "sysfs", "cgroup2", "overlay", "devtmpfs", "fuse.portal", "fuse.sshfs"}
	real := []string{"ext4", "xfs", "btrfs", "zfs", "vfat"}
	for _, fs := range virtual {
		if !virtualFS(fs) {
			t.Errorf("virtualFS(%q)=false want true", fs)
		}
	}
	for _, fs := range real {
		if virtualFS(fs) {
			t.Errorf("virtualFS(%q)=true want false", fs)
		}
	}
}

func TestVirtualIface(t *testing.T) {
	virtual := []string{"lo", "docker0", "veth1234", "br-abcdef", "virbr0", "tap0", "tun0", "cni0", "flannel.1", "kube-ipvs0", "vnet3", "vxlan0", "dummy0", "ifb0", "bond0"}
	real := []string{"eth0", "ens3", "enp0s31f6", "wlan0", "eno1"}
	for _, n := range virtual {
		if !virtualIface(n) {
			t.Errorf("virtualIface(%q)=false want true", n)
		}
	}
	for _, n := range real {
		if virtualIface(n) {
			t.Errorf("virtualIface(%q)=true want false", n)
		}
	}
}

func TestParseUint(t *testing.T) {
	if got := parseUint("12345"); got != 12345 {
		t.Errorf("parseUint=%d want 12345", got)
	}
	if got := parseUint("-1"); got != 0 {
		t.Errorf("parseUint(-1)=%d want 0 (not a uint)", got)
	}
	if got := parseUint("bogus"); got != 0 {
		t.Errorf("parseUint(bogus)=%d want 0", got)
	}
}

func TestParseHelpers(t *testing.T) {
	if got := parseFirstInt("  16384 kB"); got != 16384 {
		t.Errorf("parseFirstInt=%d want 16384", got)
	}
	if got := parseFirstInt("bogus"); got != 0 {
		t.Errorf("parseFirstInt(bogus)=%d want 0", got)
	}
	if got := parseFirstInt(""); got != 0 {
		t.Errorf("parseFirstInt(empty)=%d want 0", got)
	}
	if got := parseFloat("1.25"); got != 1.25 {
		t.Errorf("parseFloat=%v want 1.25", got)
	}
	if got := parseFloat("nan-nan"); got != 0 {
		t.Errorf("parseFloat(bad)=%v want 0", got)
	}
}

// The real source on a linux host: does not panic, returns sane values.
// Values are not deterministic — we only check invariants (uptime>0, total>0).
func TestSystemSource_Smoke(t *testing.T) {
	src := NewSystemSource()
	ctx := context.Background()

	if up := src.Uptime(ctx); up <= 0 {
		t.Errorf("uptime=%d want >0 on a running host", up)
	}
	if m := src.Memory(ctx); m.TotalMB <= 0 {
		t.Errorf("mem total=%d want >0", m.TotalMB)
	}
	s := src.CPUSample(ctx)
	if s.Total == 0 {
		t.Error("cpu sample total=0 want >0 (/proc/stat cpu line)")
	}
	if s.Idle > s.Total {
		t.Errorf("cpu idle=%d > total=%d (invariant)", s.Idle, s.Total)
	}
	// Load / Disks — best-effort, only checking for the absence of a panic.
	_ = src.Load(ctx)
	for _, d := range src.Disks(ctx) {
		if d.TotalMB <= 0 {
			t.Errorf("disk %q total=%d must be >0 (zero-total filtered out)", d.Mount, d.TotalMB)
		}
	}
}

func TestInodeUsage(t *testing.T) {
	cases := []struct {
		name                string
		files, ffree        uint64
		wantUsed, wantTotal int64
	}{
		{"normal", 1000, 250, 750, 1000},
		{"empty-full", 1000, 0, 1000, 1000},
		{"no-inodes", 0, 0, 0, 0},
		{"drvfs-ffree-gt-files", 999, 1000000, 0, 0}, // WSL drvfs: never negative
		{"all-free", 1000, 1000, 0, 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, tot := inodeUsage(c.files, c.ffree)
			if u != c.wantUsed || tot != c.wantTotal {
				t.Fatalf("inodeUsage(%d,%d)=(%d,%d); want (%d,%d)", c.files, c.ffree, u, tot, c.wantUsed, c.wantTotal)
			}
		})
	}
}
