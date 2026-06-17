package main

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

func TestFailbackInterval_NoSpray(t *testing.T) {
	t.Parallel()
	got := failbackInterval(time.Hour, 0)
	if got != time.Hour {
		t.Errorf("failbackInterval(1h, 0) = %s, want 1h", got)
	}
}

func TestFailbackInterval_BoundedRange(t *testing.T) {
	t.Parallel()
	// 100 итераций должны жить в [interval/2, interval+spray]. spray не
	// растягивает интервал, но clamp снизу — interval/2 (защита от
	// отрицательного значения при interval ≤ spray).
	interval := 100 * time.Millisecond
	spray := 30 * time.Millisecond
	for i := 0; i < 100; i++ {
		got := failbackInterval(interval, spray)
		if got < interval/2 || got > interval+spray {
			t.Fatalf("iter %d: got %s, want in [%s, %s]", i, got, interval/2, interval+spray)
		}
	}
}

func TestLoadFailback_Defaults(t *testing.T) {
	t.Parallel()
	fb, err := loadFailback(&config.SoulConfig{})
	if err != nil {
		t.Fatalf("loadFailback: %v", err)
	}
	if !fb.enabled {
		t.Errorf("default enabled = false, want true")
	}
	if fb.interval != time.Hour {
		t.Errorf("default interval = %s, want 1h", fb.interval)
	}
	if fb.spray != 10*time.Minute {
		t.Errorf("default spray = %s, want 10m", fb.spray)
	}
}

func TestLoadFailback_FromConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.SoulConfig{
		Keeper: config.SoulKeeper{
			Failback: &config.SoulKeeperFailback{
				Enabled:  true,
				Interval: "30m",
				Spray:    "5m",
			},
		},
	}
	fb, err := loadFailback(cfg)
	if err != nil {
		t.Fatalf("loadFailback: %v", err)
	}
	if !fb.enabled || fb.interval != 30*time.Minute || fb.spray != 5*time.Minute {
		t.Errorf("loadFailback = %+v", fb)
	}
}

func TestLoadFailback_InvalidInterval(t *testing.T) {
	t.Parallel()
	cfg := &config.SoulConfig{
		Keeper: config.SoulKeeper{
			Failback: &config.SoulKeeperFailback{Interval: "bogus"},
		},
	}
	if _, err := loadFailback(cfg); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

// priority=0 (опущенный в YAML) трактуется как дефолтный высший приоритет (1),
// а не как «выше единицы». orderedByPriority нормализует 0→1, так что endpoint
// без явного priority встаёт в одну группу с priority:1, не падает и не
// уезжает вперёд priority:1-хостов.
func TestOrderedByPriority_ZeroNormalized(t *testing.T) {
	t.Parallel()
	in := []config.SoulKeeperEndpoint{
		{Host: "p2", Priority: 2},
		{Host: "p0", Priority: 0}, // опущен → норм. в 1
		{Host: "p1", Priority: 1},
	}
	out := orderedByPriority(in)
	// p0 (норм. 1) и p1 (1) идут перед p2 (2); SliceStable сохраняет исходный
	// относительный порядок p0/p1 (p0 объявлен раньше p1).
	gotHosts := []string{out[0].Host, out[1].Host, out[2].Host}
	want := []string{"p0", "p1", "p2"}
	for i := range want {
		if gotHosts[i] != want[i] {
			t.Fatalf("orderedByPriority order = %v, want %v", gotHosts, want)
		}
	}
	// Не мутирует исходный slice.
	if in[1].Host != "p0" || in[1].Priority != 0 {
		t.Errorf("input mutated: %+v", in)
	}
}
