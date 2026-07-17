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
