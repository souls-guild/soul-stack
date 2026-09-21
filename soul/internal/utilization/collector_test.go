package utilization

import (
	"context"
	"testing"
	"time"
)

// fakeSource — a deterministic Source for testing Collector without touching /proc.
type fakeSource struct {
	load     LoadAvg
	mem      MemInfo
	disks    []Disk
	uptime   int64
	cpu      CPUSample
	net      NetSample
	netCalls *int // when non-nil, incremented on each Network() call
}

func (f fakeSource) Load(context.Context) LoadAvg        { return f.load }
func (f fakeSource) Memory(context.Context) MemInfo      { return f.mem }
func (f fakeSource) Disks(context.Context) []Disk        { return f.disks }
func (f fakeSource) Uptime(context.Context) int64        { return f.uptime }
func (f fakeSource) CPUSample(context.Context) CPUSample { return f.cpu }
func (f fakeSource) Network(context.Context) NetSample {
	if f.netCalls != nil {
		*f.netCalls++
	}
	return f.net
}

func TestCollect_FillsAllFields(t *testing.T) {
	src := fakeSource{
		load:   LoadAvg{One: 0.5, Five: 1.5, Fifteen: 2.5},
		mem:    MemInfo{UsedMB: 8000, TotalMB: 16000, SwapUsedMB: 512},
		disks:  []Disk{{Mount: "/", UsedMB: 20000, TotalMB: 50000}, {Mount: "/data", UsedMB: 1, TotalMB: 2}},
		uptime: 98765,
		cpu:    CPUSample{Total: 1000, Idle: 900},
	}

	u := NewCollector(src).Collect(context.Background(), "redis1.example", nil)

	if u.GetCollectedAt() == nil {
		t.Fatal("collected_at must be set (Soul-side timestamp)")
	}
	if err := u.GetCollectedAt().CheckValid(); err != nil {
		t.Fatalf("collected_at invalid: %v", err)
	}
	// First collect — there is no previous sample, cpu% must be 0 (not bogus from tot-0).
	if u.GetCpuPct() != 0 {
		t.Errorf("first-sample cpu_pct=%v want 0", u.GetCpuPct())
	}
	if u.GetLoad1() != 0.5 || u.GetLoad5() != 1.5 || u.GetLoad15() != 2.5 {
		t.Errorf("load mismatch: %v/%v/%v", u.GetLoad1(), u.GetLoad5(), u.GetLoad15())
	}
	if u.GetMemUsedMb() != 8000 || u.GetMemTotalMb() != 16000 || u.GetSwapUsedMb() != 512 {
		t.Errorf("mem mismatch: %+v", u)
	}
	if u.GetUptimeSec() != 98765 {
		t.Errorf("uptime=%d want 98765", u.GetUptimeSec())
	}
	if len(u.GetDisks()) != 2 {
		t.Fatalf("disks=%d want 2", len(u.GetDisks()))
	}
	d0 := u.GetDisks()[0]
	if d0.GetMount() != "/" || d0.GetUsedMb() != 20000 || d0.GetTotalMb() != 50000 {
		t.Errorf("disk[0] mismatch: %+v", d0)
	}
}

// cpu% — delta busy/total between two consecutive samples.
func TestCpuPct_Delta(t *testing.T) {
	cases := []struct {
		name       string
		prev, cur  CPUSample
		wantFirst  float64 // result for the first (single) call
		wantSecond float64
	}{
		// busyΔ=60, totalΔ=100 → 60%.
		{"normal 60pct", CPUSample{Total: 100, Idle: 50}, CPUSample{Total: 200, Idle: 90}, 0, 60},
		// idle dropped (counter anomaly) busy>total → clamp 100.
		{"clamp high", CPUSample{Total: 100, Idle: 50}, CPUSample{Total: 200, Idle: 40}, 0, 100},
		// counter did not move totalΔ=0 → 0.
		{"no movement", CPUSample{Total: 200, Idle: 90}, CPUSample{Total: 200, Idle: 90}, 0, 0},
		// counter reset totalΔ<0 → 0 (no negative %).
		{"counter reset", CPUSample{Total: 500, Idle: 100}, CPUSample{Total: 10, Idle: 5}, 0, 0},
		// fully idle busyΔ=0 → 0.
		{"fully idle", CPUSample{Total: 100, Idle: 50}, CPUSample{Total: 200, Idle: 150}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCollector(fakeSource{})
			if got := c.cpuPct(tc.prev); got != tc.wantFirst {
				t.Fatalf("first sample cpu%%=%v want %v", got, tc.wantFirst)
			}
			if got := c.cpuPct(tc.cur); got != tc.wantSecond {
				t.Errorf("second sample cpu%%=%v want %v", got, tc.wantSecond)
			}
		})
	}
}

// cpu% via full Collect: two collects on one Collector give a delta.
func TestCollect_CpuDeltaAcrossCollects(t *testing.T) {
	c := NewCollector(fakeSource{cpu: CPUSample{Total: 100, Idle: 50}})
	if got := c.Collect(context.Background(), "h", nil).GetCpuPct(); got != 0 {
		t.Fatalf("first Collect cpu_pct=%v want 0", got)
	}
	c.src = fakeSource{cpu: CPUSample{Total: 200, Idle: 90}}
	if got := c.Collect(context.Background(), "h", nil).GetCpuPct(); got != 60 {
		t.Errorf("second Collect cpu_pct=%v want 60", got)
	}
}

// netBps — per-second rate of a monotonic counter over dt.
func TestNetBps(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur uint64
		dt        time.Duration
		want      int64
	}{
		{"normal", 1000, 2000, 2 * time.Second, 500},
		{"first sample dt zero", 1000, 2000, 0, 0},
		{"negative dt", 1000, 2000, -time.Second, 0},
		{"counter reset", 2000, 500, time.Second, 0},
		{"no movement", 2000, 2000, time.Second, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := netBps(tc.prev, tc.cur, tc.dt); got != tc.want {
				t.Errorf("netBps(%d,%d,%v)=%d want %d", tc.prev, tc.cur, tc.dt, got, tc.want)
			}
		})
	}
}

// net rate: first Collect has no baseline → all-zero; a second Collect with an
// advanced counter yields a positive rate. dt is real wall-clock (time.Now), so
// we only assert the sign, not the exact value — the math is covered by TestNetBps.
func TestCollect_NetFirstSampleZeroThenPositive(t *testing.T) {
	c := NewCollector(fakeSource{net: NetSample{RxBytes: 1000, TxBytes: 2000, ErrDrops: 3}})
	u := c.Collect(context.Background(), "h", CollectorSet{"net": true})
	if u.GetNetRxBps() != 0 || u.GetNetTxBps() != 0 || u.GetNetErrPs() != 0 {
		t.Fatalf("first Collect net must be zero: rx=%d tx=%d err=%d",
			u.GetNetRxBps(), u.GetNetTxBps(), u.GetNetErrPs())
	}
	time.Sleep(time.Millisecond) // guarantee dt>0 for the second sample
	c.src = fakeSource{net: NetSample{RxBytes: 1_000_000, TxBytes: 2_000_000, ErrDrops: 100}}
	u2 := c.Collect(context.Background(), "h", CollectorSet{"net": true})
	if u2.GetNetRxBps() <= 0 || u2.GetNetTxBps() <= 0 || u2.GetNetErrPs() <= 0 {
		t.Errorf("second Collect net must be positive: rx=%d tx=%d err=%d",
			u2.GetNetRxBps(), u2.GetNetTxBps(), u2.GetNetErrPs())
	}
}

// net disabled → rates stay zero and Network() is never called.
func TestCollect_NetDisabledNotCalled(t *testing.T) {
	var calls int
	src := fakeSource{net: NetSample{RxBytes: 1000}, netCalls: &calls}
	u := NewCollector(src).Collect(context.Background(), "h", CollectorSet{"cpu": true})
	if calls != 0 {
		t.Errorf("Network() called %d times when net disabled, want 0", calls)
	}
	if u.GetNetRxBps() != 0 || u.GetNetTxBps() != 0 || u.GetNetErrPs() != 0 {
		t.Errorf("net fields must be zero when disabled: %+v", u)
	}
}

// inode counts flow from the Source Disk through to the proto DiskUtilization.
func TestCollect_InodesFlowThrough(t *testing.T) {
	src := fakeSource{disks: []Disk{
		{Mount: "/", UsedMB: 100, TotalMB: 1000, InodesUsed: 4200, InodesTotal: 65536},
	}}
	u := NewCollector(src).Collect(context.Background(), "h", nil)
	if len(u.GetDisks()) != 1 {
		t.Fatalf("disks=%d want 1", len(u.GetDisks()))
	}
	d := u.GetDisks()[0]
	if d.GetInodesUsed() != 4200 || d.GetInodesTotal() != 65536 {
		t.Errorf("inode passthrough mismatch: used=%d total=%d", d.GetInodesUsed(), d.GetInodesTotal())
	}
}

// Collectors gating (NIM-87): a disabled collector → its fields are zero, disk
// skips statfs; an enabled one — is collected. An unknown name in the set — no-op.
func TestCollect_CollectorsGating(t *testing.T) {
	src := fakeSource{
		load:   LoadAvg{One: 0.5, Five: 1.5, Fifteen: 2.5},
		mem:    MemInfo{UsedMB: 8000, TotalMB: 16000, SwapUsedMB: 512},
		disks:  []Disk{{Mount: "/", UsedMB: 20000, TotalMB: 50000}},
		uptime: 98765,
		cpu:    CPUSample{Total: 1000, Idle: 900},
	}

	// Only cpu → load/mem/disk/uptime are zeroed, statfs (Disks) is not called.
	only := NewCollector(src).Collect(context.Background(), "h", CollectorSet{"cpu": true})
	if only.GetLoad1() != 0 || only.GetLoad5() != 0 || only.GetLoad15() != 0 {
		t.Errorf("load must be zero when disabled: %v/%v/%v", only.GetLoad1(), only.GetLoad5(), only.GetLoad15())
	}
	if only.GetMemUsedMb() != 0 || only.GetMemTotalMb() != 0 || only.GetSwapUsedMb() != 0 {
		t.Errorf("mem must be zero when disabled: %+v", only)
	}
	if only.GetDisks() != nil {
		t.Errorf("disks must be skipped when disabled, got %v", only.GetDisks())
	}
	if only.GetUptimeSec() != 0 {
		t.Errorf("uptime must be zero when disabled: %d", only.GetUptimeSec())
	}

	// Reverse direction + unknown name is ignored: mem enabled (collected),
	// disk disabled (nil), "bogus" has no effect.
	memOnly := NewCollector(src).Collect(context.Background(), "h", CollectorSet{"mem": true, "bogus": true})
	if memOnly.GetMemTotalMb() != 16000 || memOnly.GetMemUsedMb() != 8000 {
		t.Errorf("mem must be collected when enabled: %+v", memOnly)
	}
	if memOnly.GetDisks() != nil {
		t.Errorf("disks must be nil when disabled, got %v", memOnly.GetDisks())
	}
	if memOnly.GetUptimeSec() != 0 {
		t.Errorf("uptime must be zero when disabled: %d", memOnly.GetUptimeSec())
	}
}

// Empty/sparse Source → zeros and nil-disks, no panic.
func TestCollect_EmptySourceNoPanic(t *testing.T) {
	u := NewCollector(fakeSource{}).Collect(context.Background(), "h", nil)
	if u == nil {
		t.Fatal("Collect must return non-nil")
	}
	if u.GetCpuPct() != 0 || u.GetLoad1() != 0 || u.GetMemTotalMb() != 0 || u.GetUptimeSec() != 0 {
		t.Errorf("empty source must map to zeros: %+v", u)
	}
	if u.GetDisks() != nil {
		t.Errorf("empty source disks must be nil, got %v", u.GetDisks())
	}
}
