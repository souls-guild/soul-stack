package config

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestCadenceScheduler_EnabledFootgunGuard — ADR-048 §5: default-ON при наличии
// Redis. Не задано (nil блок / nil поле) → ON (Cadence без планировщика молча не
// спавнит); явный false → OFF; явный true → ON.
func TestCadenceScheduler_EnabledFootgunGuard(t *testing.T) {
	tru, fal := true, false
	cases := []struct {
		name string
		cs   *KeeperCadenceScheduler
		want bool
	}{
		{"nil block → ON", nil, true},
		{"nil enabled field → ON", &KeeperCadenceScheduler{}, true},
		{"explicit true → ON", &KeeperCadenceScheduler{Enabled: &tru}, true},
		{"explicit false → OFF", &KeeperCadenceScheduler{Enabled: &fal}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cs.CadenceSchedulerEnabled(); got != tc.want {
				t.Errorf("CadenceSchedulerEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCadenceScheduler_ResolvedLockTTL — пустое/невалидное → дефолт 5m; валидное →
// оно само.
func TestCadenceScheduler_ResolvedLockTTL(t *testing.T) {
	cases := []struct {
		name string
		cs   *KeeperCadenceScheduler
		want time.Duration
	}{
		{"nil block → default", nil, DefaultCadenceSchedulerLockTTL},
		{"empty → default", &KeeperCadenceScheduler{}, DefaultCadenceSchedulerLockTTL},
		{"explicit 2m", &KeeperCadenceScheduler{LockTTL: "2m"}, 2 * time.Minute},
		{"invalid → default", &KeeperCadenceScheduler{LockTTL: "nope"}, DefaultCadenceSchedulerLockTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cs.ResolvedLockTTL(); got != tc.want {
				t.Errorf("ResolvedLockTTL() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoadKeeper_CadenceScheduler_Parse — блок cadence_scheduler парсится из
// keeper.yml (enabled/interval/lock_ttl) и проходит semantic-валидацию формата
// duration.
func TestLoadKeeper_CadenceScheduler_Parse(t *testing.T) {
	src := keeperBaseRequired + `cadence_scheduler:
  enabled: true
  interval: 15s
  lock_ttl: 5m
`
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	if hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatal("валидный cadence_scheduler не должен давать duration_invalid")
	}
	if cfg.CadenceScheduler == nil {
		t.Fatal("cadence_scheduler не распарсился")
	}
	if !cfg.CadenceScheduler.CadenceSchedulerEnabled() {
		t.Error("enabled: true должно дать ON")
	}
	if cfg.CadenceScheduler.Interval != "15s" {
		t.Errorf("interval (alias-источник) = %q, want 15s", cfg.CadenceScheduler.Interval)
	}
	if got := cfg.CadenceScheduler.ResolvedLockTTL(); got != 5*time.Minute {
		t.Errorf("lock_ttl = %v, want 5m", got)
	}
}

// TestLoadKeeper_CadenceScheduler_BadDuration — невалидный формат interval
// отвергается semantic-фазой (стиль reaper.interval / acolyte_*).
func TestLoadKeeper_CadenceScheduler_BadDuration(t *testing.T) {
	src := keeperBaseRequired + `cadence_scheduler:
  interval: not-a-duration
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "duration_invalid", "$.cadence_scheduler.interval") {
		dump(t, diags)
		t.Fatal("ожидался duration_invalid на $.cadence_scheduler.interval")
	}
}

// TestCadenceScheduler_ResolvedPollFloor — пустое/невалидное → дефолт 30s;
// валидное → оно само (профиль «Спокойный», ADR-048 «Adaptive interval»).
func TestCadenceScheduler_ResolvedPollFloor(t *testing.T) {
	cases := []struct {
		name string
		cs   *KeeperCadenceScheduler
		want time.Duration
	}{
		{"nil block → default", nil, DefaultCadenceSchedulerPollFloor},
		{"empty → default", &KeeperCadenceScheduler{}, DefaultCadenceSchedulerPollFloor},
		{"explicit 30s", &KeeperCadenceScheduler{PollFloor: "30s"}, 30 * time.Second},
		{"invalid → default", &KeeperCadenceScheduler{PollFloor: "garbage"}, DefaultCadenceSchedulerPollFloor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cs.ResolvedPollFloor(); got != tc.want {
				t.Errorf("ResolvedPollFloor() = %v, want %v", got, tc.want)
			}
		})
	}
	if DefaultCadenceSchedulerPollFloor != 30*time.Second {
		t.Errorf("профиль «Спокойный» floor=30s; got %v", DefaultCadenceSchedulerPollFloor)
	}
}

// TestCadenceScheduler_ResolvedPollCeiling — пустое → дефолт 60s; backcompat-alias:
// если interval задан, а poll_ceiling нет → ceiling = max(interval, poll_floor)
// (clamp ВВЕРХ до floor — старый суб-30s interval не роняет инвариант floor≤ceiling).
func TestCadenceScheduler_ResolvedPollCeiling(t *testing.T) {
	cases := []struct {
		name string
		cs   *KeeperCadenceScheduler
		want time.Duration
	}{
		{"nil block → default", nil, DefaultCadenceSchedulerPollCeiling},
		{"empty → default", &KeeperCadenceScheduler{}, DefaultCadenceSchedulerPollCeiling},
		{"explicit poll_ceiling 60s", &KeeperCadenceScheduler{PollCeiling: "60s"}, 60 * time.Second},
		{"backcompat: interval alias выше floor → ceiling", &KeeperCadenceScheduler{Interval: "45s"}, 45 * time.Second},
		{"backcompat: interval alias ниже floor → clamp до floor 30", &KeeperCadenceScheduler{Interval: "20s"}, 30 * time.Second},
		{"backcompat: dev 5s alias → clamp до floor 30", &KeeperCadenceScheduler{Interval: "5s"}, 30 * time.Second},
		{"explicit ceiling wins over interval", &KeeperCadenceScheduler{Interval: "20s", PollCeiling: "45s"}, 45 * time.Second},
		{"invalid interval alias → default", &KeeperCadenceScheduler{Interval: "garbage"}, DefaultCadenceSchedulerPollCeiling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cs.ResolvedPollCeiling(); got != tc.want {
				t.Errorf("ResolvedPollCeiling() = %v, want %v", got, tc.want)
			}
		})
	}
	if DefaultCadenceSchedulerPollCeiling != 60*time.Second {
		t.Errorf("профиль «Спокойный» ceiling=60s; got %v", DefaultCadenceSchedulerPollCeiling)
	}
}

// TestCadenceScheduler_ResolvedPollIdle — пустое → дефолт 120s; валидное → оно.
func TestCadenceScheduler_ResolvedPollIdle(t *testing.T) {
	cases := []struct {
		name string
		cs   *KeeperCadenceScheduler
		want time.Duration
	}{
		{"nil block → default", nil, DefaultCadenceSchedulerPollIdle},
		{"empty → default", &KeeperCadenceScheduler{}, DefaultCadenceSchedulerPollIdle},
		{"explicit 120s", &KeeperCadenceScheduler{PollIdle: "120s"}, 120 * time.Second},
		{"invalid → default", &KeeperCadenceScheduler{PollIdle: "nope"}, DefaultCadenceSchedulerPollIdle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cs.ResolvedPollIdle(); got != tc.want {
				t.Errorf("ResolvedPollIdle() = %v, want %v", got, tc.want)
			}
		})
	}
	if DefaultCadenceSchedulerPollIdle != 120*time.Second {
		t.Errorf("профиль «Спокойный» idle=120s; got %v", DefaultCadenceSchedulerPollIdle)
	}
}

// TestLoadKeeper_CadenceScheduler_PollProfile — профиль «Спокойный» парсится из
// keeper.yml и проходит валидацию.
func TestLoadKeeper_CadenceScheduler_PollProfile(t *testing.T) {
	src := keeperBaseRequired + `cadence_scheduler:
  enabled: true
  poll_floor: 30s
  poll_ceiling: 60s
  poll_idle: 120s
`
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	for _, code := range []string{"duration_invalid", "value_out_of_range"} {
		if hasCode(diags, code) {
			dump(t, diags)
			t.Fatalf("валидный профиль не должен давать %s", code)
		}
	}
	cs := cfg.CadenceScheduler
	if cs == nil {
		t.Fatal("cadence_scheduler не распарсился")
	}
	if cs.ResolvedPollFloor() != 30*time.Second || cs.ResolvedPollCeiling() != 60*time.Second || cs.ResolvedPollIdle() != 120*time.Second {
		t.Errorf("профиль = (%v,%v,%v), want (30s,60s,120s)", cs.ResolvedPollFloor(), cs.ResolvedPollCeiling(), cs.ResolvedPollIdle())
	}
}

// TestLoadKeeper_CadenceScheduler_BackcompatInterval — старый keeper.yml с одним
// interval НЕ падает, даже если interval < poll_floor (dev ставил 5s): alias
// clamp-ит ceiling вверх до floor, конфиг грузится без error-диагностик, эмитится
// warning. Проверяет именно суб-floor значения (5s/15s/29s) — прежний тест на 45s
// был выше floor и скрывал дефект (interval < floor роняло конфиг через
// floor ≤ ceiling).
func TestLoadKeeper_CadenceScheduler_BackcompatInterval(t *testing.T) {
	for _, iv := range []string{"5s", "15s", "29s"} {
		t.Run("interval "+iv+" грузится с warning", func(t *testing.T) {
			src := keeperBaseRequired + `cadence_scheduler:
  interval: ` + iv + `
`
			cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
			if err != nil {
				t.Fatalf("LoadKeeperFromBytes: %v (суб-floor interval не должен ронять конфиг)", err)
			}
			// 0 error-диагностик: конфиг ГРУЗИТСЯ (backcompat-инвариант).
			if diag.HasErrors(diags) {
				dump(t, diags)
				t.Fatalf("interval=%s alias не должен давать error-диагностик (clamp вверх до floor)", iv)
			}
			// ceiling поднят до floor (30s).
			if got := cfg.CadenceScheduler.ResolvedPollCeiling(); got != 30*time.Second {
				t.Errorf("backcompat ceiling = %v, want 30s (clamp до floor)", got)
			}
			// Warning о подъёме эмитится.
			if !hasCodeAt(diags, "value_clamped", "$.cadence_scheduler.interval") {
				dump(t, diags)
				t.Errorf("ожидался warning value_clamped на $.cadence_scheduler.interval (interval %s < floor 30s)", iv)
			}
		})
	}
}

// TestLoadKeeper_CadenceScheduler_BackcompatIntervalAboveFloor — interval >= floor
// (45s) грузится без warning: подъёма нет, ceiling = interval.
func TestLoadKeeper_CadenceScheduler_BackcompatIntervalAboveFloor(t *testing.T) {
	src := keeperBaseRequired + `cadence_scheduler:
  interval: 45s
`
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("interval=45s alias не должен ломать валидацию (floor 30 <= 45 <= idle 120)")
	}
	if hasCode(diags, "value_clamped") {
		dump(t, diags)
		t.Error("interval=45s выше floor — warning о подъёме не должен эмититься")
	}
	if got := cfg.CadenceScheduler.ResolvedPollCeiling(); got != 45*time.Second {
		t.Errorf("backcompat ceiling = %v, want 45s (alias из interval, выше floor)", got)
	}
}

// TestLoadKeeper_CadenceScheduler_ExplicitFloorAboveCeiling — ЯВНЫЙ poll_floor >
// poll_ceiling — это реальная конфиг-ошибка оператора (не alias-случай) → error.
// Отличие от backcompat: alias-clamp здесь не применяется (poll_ceiling задан явно).
func TestLoadKeeper_CadenceScheduler_ExplicitFloorAboveCeiling(t *testing.T) {
	src := keeperBaseRequired + `cadence_scheduler:
  poll_floor: 40s
  poll_ceiling: 30s
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.cadence_scheduler.poll_floor") {
		dump(t, diags)
		t.Fatal("явный poll_floor 40s > poll_ceiling 30s — реальная ошибка, ожидался value_out_of_range")
	}
}

// TestLoadKeeper_CadenceScheduler_PollFloorBelowMinimum — poll_floor < 30s
// отвергается (абсолютный минимум, DB-CHECK Pass B тоже 30).
func TestLoadKeeper_CadenceScheduler_PollFloorBelowMinimum(t *testing.T) {
	src := keeperBaseRequired + `cadence_scheduler:
  poll_floor: 10s
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.cadence_scheduler.poll_floor") {
		dump(t, diags)
		t.Fatal("ожидался value_out_of_range на $.cadence_scheduler.poll_floor (< 30s)")
	}
}

// TestLoadKeeper_CadenceScheduler_FloorGreaterThanCeiling — poll_floor > poll_ceiling
// отвергается (коридор вырожден).
func TestLoadKeeper_CadenceScheduler_FloorGreaterThanCeiling(t *testing.T) {
	src := keeperBaseRequired + `cadence_scheduler:
  poll_floor: 90s
  poll_ceiling: 60s
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.cadence_scheduler.poll_floor") {
		dump(t, diags)
		t.Fatal("ожидался value_out_of_range на $.cadence_scheduler.poll_floor (> ceiling)")
	}
}

// TestLoadKeeper_CadenceScheduler_IdleBelowCeiling — poll_idle < poll_ceiling
// отвергается (idle не должен быть чаще обычного опроса).
func TestLoadKeeper_CadenceScheduler_IdleBelowCeiling(t *testing.T) {
	src := keeperBaseRequired + `cadence_scheduler:
  poll_ceiling: 60s
  poll_idle: 45s
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.cadence_scheduler.poll_idle") {
		dump(t, diags)
		t.Fatal("ожидался value_out_of_range на $.cadence_scheduler.poll_idle (< ceiling)")
	}
}
