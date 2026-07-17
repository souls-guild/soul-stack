package utilization

import (
	"context"
	"testing"
)

// fakeSource — детерминированный Source для проверки Collector без касания /proc.
type fakeSource struct {
	load   LoadAvg
	mem    MemInfo
	disks  []Disk
	uptime int64
	cpu    CPUSample
}

func (f fakeSource) Load(context.Context) LoadAvg        { return f.load }
func (f fakeSource) Memory(context.Context) MemInfo      { return f.mem }
func (f fakeSource) Disks(context.Context) []Disk        { return f.disks }
func (f fakeSource) Uptime(context.Context) int64        { return f.uptime }
func (f fakeSource) CPUSample(context.Context) CPUSample { return f.cpu }

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
	// Первый сбор — прошлого сэмпла нет, cpu% обязан быть 0 (не bogus из tot-0).
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

// cpu% — дельта busy/total между двумя последовательными сэмплами.
func TestCpuPct_Delta(t *testing.T) {
	cases := []struct {
		name       string
		prev, cur  CPUSample
		wantFirst  float64 // результат для первого (одиночного) вызова
		wantSecond float64
	}{
		// busyΔ=60, totalΔ=100 → 60%.
		{"normal 60pct", CPUSample{Total: 100, Idle: 50}, CPUSample{Total: 200, Idle: 90}, 0, 60},
		// idle упал (аномалия счётчика) busy>total → clamp 100.
		{"clamp high", CPUSample{Total: 100, Idle: 50}, CPUSample{Total: 200, Idle: 40}, 0, 100},
		// счётчик не двинулся totalΔ=0 → 0.
		{"no movement", CPUSample{Total: 200, Idle: 90}, CPUSample{Total: 200, Idle: 90}, 0, 0},
		// reset счётчика totalΔ<0 → 0 (без отрицательного %).
		{"counter reset", CPUSample{Total: 500, Idle: 100}, CPUSample{Total: 10, Idle: 5}, 0, 0},
		// полностью idle busyΔ=0 → 0.
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

// cpu% через полный Collect: два сбора на одном Collector дают дельту.
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

// Collectors gating (NIM-87): выключенный коллектор → его поля нулевые, disk
// пропускает statfs; включённый — собирается. Неизвестное имя в наборе — no-op.
func TestCollect_CollectorsGating(t *testing.T) {
	src := fakeSource{
		load:   LoadAvg{One: 0.5, Five: 1.5, Fifteen: 2.5},
		mem:    MemInfo{UsedMB: 8000, TotalMB: 16000, SwapUsedMB: 512},
		disks:  []Disk{{Mount: "/", UsedMB: 20000, TotalMB: 50000}},
		uptime: 98765,
		cpu:    CPUSample{Total: 1000, Idle: 900},
	}

	// Только cpu → load/mem/disk/uptime зануляются, statfs (Disks) не зовётся.
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

	// Обратное направление + неизвестное имя игнорируется: mem включён (собран),
	// disk выключен (nil), "bogus" не влияет.
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

// Пустой/sparse Source → нули и nil-disks, без паники.
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
