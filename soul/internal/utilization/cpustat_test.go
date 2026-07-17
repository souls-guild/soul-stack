package utilization

import "testing"

// TestParseCPUStatLine — guest(idx8)/guest_nice(idx9) НЕ учитываются в Total
// (ядро уже включило их в user/nice); Idle = idle+iowait.
func TestParseCPUStatLine(t *testing.T) {
	// user nice system idle iowait irq softirq steal guest guest_nice
	s := parseCPUStatLine("cpu 10 2 3 80 5 0 0 0 100 50")
	if s.Total != 100 {
		t.Errorf("Total=%d, want 100 (guest/guest_nice не суммируются)", s.Total)
	}
	if s.Idle != 85 {
		t.Errorf("Idle=%d, want 85 (idle+iowait)", s.Idle)
	}
}

func TestParseCPUStatLine_ShortAndGarbage(t *testing.T) {
	if s := parseCPUStatLine("garbage"); s != (CPUSample{}) {
		t.Errorf("не-cpu строка → zero, got %+v", s)
	}
	if s := parseCPUStatLine("cpu 1 2 3 4"); s.Total != 10 || s.Idle != 4 {
		t.Errorf("частичная строка: Total=%d Idle=%d, want 10/4", s.Total, s.Idle)
	}
}
