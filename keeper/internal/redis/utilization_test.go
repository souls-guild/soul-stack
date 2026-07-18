package redis

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func sampleUtilization(collected time.Time) *keeperv1.HostUtilization {
	return &keeperv1.HostUtilization{
		CollectedAt: timestamppb.New(collected),
		CpuPct:      42.5,
		Load1:       0.15,
		Load5:       0.30,
		Load15:      0.45,
		MemUsedMb:   2048,
		MemTotalMb:  8192,
		SwapUsedMb:  128,
		UptimeSec:   987654,
		NetRxBps:    1000000,
		NetTxBps:    2000000,
		NetErrPs:    7,
		Disks: []*keeperv1.DiskUtilization{
			{Mount: "/", UsedMb: 10240, TotalMb: 51200, InodesUsed: 5000, InodesTotal: 100000},
			{Mount: "/data", UsedMb: 40960, TotalMb: 102400, InodesUsed: 12000, InodesTotal: 200000},
		},
	}
}

func TestUtilizationKey(t *testing.T) {
	if got, want := UtilizationKey("host.example.com"), "soul:host.example.com:util"; got != want {
		t.Errorf("UtilizationKey = %q, want %q", got, want)
	}
	if got, want := UtilizationWindowKey("host.example.com"), "soul:host.example.com:util:win"; got != want {
		t.Errorf("UtilizationWindowKey = %q, want %q", got, want)
	}
}

func TestWriteReadUtilization_RoundTrip(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"
	collected := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	now := collected.Add(2 * time.Second)

	if err := WriteUtilization(ctx, c, sid, sampleUtilization(collected), now); err != nil {
		t.Fatalf("WriteUtilization: %v", err)
	}

	snap, ok, err := ReadUtilization(ctx, c, sid)
	if err != nil {
		t.Fatalf("ReadUtilization: %v", err)
	}
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if !snap.CollectedAt.Equal(collected) {
		t.Errorf("CollectedAt = %v, want %v", snap.CollectedAt, collected)
	}
	if !snap.ReceivedAt.Equal(now.UTC()) {
		t.Errorf("ReceivedAt = %v, want %v", snap.ReceivedAt, now.UTC())
	}
	if snap.CPUPct != 42.5 || snap.Load1 != 0.15 || snap.Load5 != 0.30 || snap.Load15 != 0.45 {
		t.Errorf("cpu/load mismatch: %+v", snap)
	}
	if snap.MemUsedMB != 2048 || snap.MemTotalMB != 8192 || snap.SwapUsedMB != 128 || snap.UptimeSec != 987654 {
		t.Errorf("mem/uptime mismatch: %+v", snap)
	}
	if len(snap.Disks) != 2 || snap.Disks[0].Mount != "/" || snap.Disks[1].UsedMB != 40960 {
		t.Errorf("disks mismatch: %+v", snap.Disks)
	}
	if snap.NetRxBps != 1000000 || snap.NetTxBps != 2000000 || snap.NetErrPs != 7 {
		t.Errorf("net mismatch: rx=%d tx=%d err=%d", snap.NetRxBps, snap.NetTxBps, snap.NetErrPs)
	}
	if snap.Disks[0].InodesUsed != 5000 || snap.Disks[0].InodesTotal != 100000 || snap.Disks[1].InodesUsed != 12000 {
		t.Errorf("inode mismatch: %+v", snap.Disks)
	}

	// net rides the window point (sparkline); inode/swap/err stay latest-only.
	pts, err := ReadUtilizationWindow(ctx, c, sid, 1)
	if err != nil {
		t.Fatalf("ReadUtilizationWindow: %v", err)
	}
	if len(pts) != 1 || pts[0].NetRxBps != 1000000 || pts[0].NetTxBps != 2000000 {
		t.Errorf("point net not round-tripped: %+v", pts)
	}
}

// TestReadUtilization_Missing — nonexistent sid → ok=false, no error.
func TestReadUtilization_Missing(t *testing.T) {
	c, _ := newClientMR(t)
	_, ok, err := ReadUtilization(context.Background(), c, "ghost.example.com")
	if err != nil {
		t.Fatalf("ReadUtilization: %v", err)
	}
	if ok {
		t.Error("ok=true on missing key")
	}
}

// TestWriteUtilization_SetsTTL — after a write, TTL of latest and the window ∈ (0, UtilizationTTL].
func TestWriteUtilization_SetsTTL(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"
	if err := WriteUtilization(ctx, c, sid, sampleUtilization(time.Now()), time.Now()); err != nil {
		t.Fatalf("WriteUtilization: %v", err)
	}
	for _, key := range []string{UtilizationKey(sid), UtilizationWindowKey(sid)} {
		ttl := mr.TTL(key)
		if ttl <= 0 || ttl > UtilizationTTL {
			t.Errorf("TTL(%q) = %v, want (0, %v]", key, ttl, UtilizationTTL)
		}
	}
}

// TestWriteUtilization_WindowCaps — LPUSH N+10 records → window length == UtilizationWindowSize.
func TestWriteUtilization_WindowCaps(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"
	for i := 0; i < UtilizationWindowSize+10; i++ {
		if err := WriteUtilization(ctx, c, sid, sampleUtilization(time.Now()), time.Now()); err != nil {
			t.Fatalf("WriteUtilization #%d: %v", i, err)
		}
	}
	llen := c.underlying().LLen(ctx, UtilizationWindowKey(sid)).Val()
	if llen != int64(UtilizationWindowSize) {
		t.Errorf("window LLEN = %d, want %d (LTRIM cap)", llen, UtilizationWindowSize)
	}
	pts, err := ReadUtilizationWindow(ctx, c, sid, UtilizationWindowSize)
	if err != nil {
		t.Fatalf("ReadUtilizationWindow: %v", err)
	}
	if len(pts) != UtilizationWindowSize {
		t.Errorf("window points = %d, want %d", len(pts), UtilizationWindowSize)
	}
}

// TestReadUtilizationWindow_NewestFirst — the window is returned newest-first (as LPUSH stacked it).
func TestReadUtilizationWindow_NewestFirst(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"
	for _, cpu := range []float64{1, 2, 3} {
		ev := sampleUtilization(time.Now())
		ev.CpuPct = cpu
		if err := WriteUtilization(ctx, c, sid, ev, time.Now()); err != nil {
			t.Fatalf("WriteUtilization: %v", err)
		}
	}
	pts, err := ReadUtilizationWindow(ctx, c, sid, 3)
	if err != nil {
		t.Fatalf("ReadUtilizationWindow: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("points = %d, want 3", len(pts))
	}
	if pts[0].CPUPct != 3 || pts[1].CPUPct != 2 || pts[2].CPUPct != 1 {
		t.Errorf("order = [%v %v %v], want newest-first [3 2 1]", pts[0].CPUPct, pts[1].CPUPct, pts[2].CPUPct)
	}
}

func TestReadUtilizationWindow_Empty(t *testing.T) {
	c, _ := newClientMR(t)
	pts, err := ReadUtilizationWindow(context.Background(), c, "ghost.example.com", 10)
	if err != nil {
		t.Fatalf("ReadUtilizationWindow: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("points = %d, want 0 on missing key", len(pts))
	}
}

// TestWriteUtilization_NilCollectedAt — nil CollectedAt → zero-time, no panic.
func TestWriteUtilization_NilCollectedAt(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"
	ev := sampleUtilization(time.Now())
	ev.CollectedAt = nil
	if err := WriteUtilization(ctx, c, sid, ev, time.Now()); err != nil {
		t.Fatalf("WriteUtilization: %v", err)
	}
	snap, ok, err := ReadUtilization(ctx, c, sid)
	if err != nil || !ok {
		t.Fatalf("ReadUtilization: ok=%v err=%v", ok, err)
	}
	if !snap.CollectedAt.IsZero() {
		t.Errorf("CollectedAt = %v, want zero-time on nil CollectedAt", snap.CollectedAt)
	}
}

// TestUtilizationTTL — pure function scaling TTL to the cadence (NIM-87):
// 3×interval with a UtilizationTTL floor (90s) and a ceiling (clamp interval_sec at
// 10800 → TTL 32400s); 0/small/negative interval → floor.
func TestUtilizationTTL(t *testing.T) {
	cases := []struct {
		intervalSec int32
		want        time.Duration
	}{
		{0, 90 * time.Second},             // old soul without the field → floor
		{-5, 90 * time.Second},            // negative (failure) → 0 → floor
		{30, 90 * time.Second},            // 3×30=90 == floor
		{10, 90 * time.Second},            // 3×10=30 < floor → 90
		{60, 180 * time.Second},           // 3×60=180
		{600, 1800 * time.Second},         // 3×600=1800
		{10800, 32400 * time.Second},      // 3×10800=32400 == ceiling
		{2147483647, 32400 * time.Second}, // int32-max → clamp → ceiling 32400
	}
	for _, tc := range cases {
		if got := utilizationTTL(tc.intervalSec); got != tc.want {
			t.Errorf("utilizationTTL(%d) = %v, want %v", tc.intervalSec, got, tc.want)
		}
	}
}

// TestWriteUtilization_TTLScalesWithInterval — a larger interval_sec → higher TTL
// than the floor; interval_sec is persisted and read back.
func TestWriteUtilization_TTLScalesWithInterval(t *testing.T) {
	c, mr := newClientMR(t)
	ctx := context.Background()
	sid := "host.example.com"
	ev := sampleUtilization(time.Now())
	ev.IntervalSec = 600 // → TTL 1800s

	if err := WriteUtilization(ctx, c, sid, ev, time.Now()); err != nil {
		t.Fatalf("WriteUtilization: %v", err)
	}

	for _, key := range []string{UtilizationKey(sid), UtilizationWindowKey(sid)} {
		ttl := mr.TTL(key)
		if ttl <= UtilizationTTL || ttl > 1800*time.Second {
			t.Errorf("TTL(%q) = %v, want (%v, 1800s]", key, ttl, UtilizationTTL)
		}
	}

	snap, ok, err := ReadUtilization(ctx, c, sid)
	if err != nil || !ok {
		t.Fatalf("ReadUtilization: ok=%v err=%v", ok, err)
	}
	if snap.IntervalSec != 600 {
		t.Errorf("IntervalSec = %d, want 600 (round-trip)", snap.IntervalSec)
	}
}

func TestWriteUtilization_Rejects(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	if err := WriteUtilization(ctx, nil, "sid", sampleUtilization(time.Now()), time.Now()); err == nil {
		t.Error("nil client returned nil err")
	}
	if err := WriteUtilization(ctx, c, "", sampleUtilization(time.Now()), time.Now()); err == nil {
		t.Error("empty sid returned nil err")
	}
	if err := WriteUtilization(ctx, c, "sid", nil, time.Now()); err == nil {
		t.Error("nil event returned nil err")
	}
}
