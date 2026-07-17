package essence

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

func ptrBool(b bool) *bool    { return &b }
func ptrStr(s string) *string { return &s }
func tel(en *bool, iv *string, col []string) *config.TelemetryConfig {
	return &config.TelemetryConfig{Enabled: en, Interval: iv, Collectors: col}
}

func TestResolveEffectiveTelemetry(t *testing.T) {
	allFive := []string{"cpu", "mem", "disk", "load", "uptime"}

	cases := []struct {
		name        string
		manifest    *config.TelemetryConfig
		essence     map[string]any
		wantEnabled bool
		wantSec     int32
		wantCollect []string
	}{
		{
			name:        "manifest nil → дефолты (enabled, 30s, все 5)",
			manifest:    nil,
			essence:     nil,
			wantEnabled: true,
			wantSec:     30,
			wantCollect: allFive,
		},
		{
			name:        "пустой essence → значения манифеста",
			manifest:    tel(ptrBool(false), ptrStr("45s"), []string{"cpu", "mem"}),
			essence:     map[string]any{},
			wantEnabled: false,
			wantSec:     45,
			wantCollect: []string{"cpu", "mem"},
		},
		{
			name:        "essence переопределяет interval и collectors",
			manifest:    tel(nil, ptrStr("60s"), []string{"cpu"}),
			essence:     map[string]any{"telemetry_interval": "120s", "telemetry_collectors": []string{"mem", "disk"}},
			wantEnabled: true,
			wantSec:     120,
			wantCollect: []string{"mem", "disk"},
		},
		{
			name:        "essence collectors как []any (YAML-форма)",
			manifest:    nil,
			essence:     map[string]any{"telemetry_collectors": []any{"cpu", "load"}},
			wantEnabled: true,
			wantSec:     30,
			wantCollect: []string{"cpu", "load"},
		},
		{
			name:        "clamp floor: interval < 10s → 10s",
			manifest:    nil,
			essence:     map[string]any{"telemetry_interval": "5s"},
			wantEnabled: true,
			wantSec:     10,
			wantCollect: allFive,
		},
		{
			name:        "clamp ceiling: interval > 3600s → 3600s",
			manifest:    nil,
			essence:     map[string]any{"telemetry_interval": "2h"},
			wantEnabled: true,
			wantSec:     3600,
			wantCollect: allFive,
		},
		{
			name:        "неизвестный collector отфильтрован (REPLACE)",
			manifest:    nil,
			essence:     map[string]any{"telemetry_collectors": []string{"cpu", "bogus", "mem"}},
			wantEnabled: true,
			wantSec:     30,
			wantCollect: []string{"cpu", "mem"},
		},
		{
			name:        "битый essence-interval → default 30s (не floor)",
			manifest:    tel(nil, ptrStr("60s"), nil),
			essence:     map[string]any{"telemetry_interval": "не-длительность"},
			wantEnabled: true,
			wantSec:     30,
			wantCollect: allFive,
		},
		{
			name:        "essence НЕ переопределяет enabled (manifest false остаётся)",
			manifest:    tel(ptrBool(false), nil, nil),
			essence:     map[string]any{"telemetry_interval": "20s"},
			wantEnabled: false,
			wantSec:     20,
			wantCollect: allFive,
		},
		{
			name:        "essence collectors не-строковый список → fallback на манифест",
			manifest:    tel(nil, nil, []string{"cpu"}),
			essence:     map[string]any{"telemetry_collectors": []any{"cpu", 42}},
			wantEnabled: true,
			wantSec:     30,
			wantCollect: []string{"cpu"},
		},
		{
			name:        "все essence-collectors неизвестны + manifest nil → откат на дефолт (все 5, НЕ пусто)",
			manifest:    nil,
			essence:     map[string]any{"telemetry_collectors": []string{"bogus", "nope"}},
			wantEnabled: true,
			wantSec:     30,
			wantCollect: allFive,
		},
		{
			name:        "все essence-collectors неизвестны + manifest задан → откат на манифест",
			manifest:    tel(nil, nil, []string{"cpu", "mem"}),
			essence:     map[string]any{"telemetry_collectors": []string{"bogus"}},
			wantEnabled: true,
			wantSec:     30,
			wantCollect: []string{"cpu", "mem"},
		},
		{
			name:        "manifest interval на границе floor (10s) проходит",
			manifest:    tel(nil, ptrStr("10s"), nil),
			essence:     nil,
			wantEnabled: true,
			wantSec:     10,
			wantCollect: allFive,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveEffectiveTelemetry(tc.manifest, tc.essence)
			if got.GetEnabled() != tc.wantEnabled {
				t.Errorf("enabled = %v, want %v", got.GetEnabled(), tc.wantEnabled)
			}
			if got.GetIntervalSec() != tc.wantSec {
				t.Errorf("interval_sec = %d, want %d", got.GetIntervalSec(), tc.wantSec)
			}
			if !reflect.DeepEqual(got.GetCollectors(), tc.wantCollect) {
				t.Errorf("collectors = %v, want %v", got.GetCollectors(), tc.wantCollect)
			}
		})
	}
}

// TestResolveEffectiveTelemetry_NoManifestMutation — CollectorsOrDefault отдаёт
// слайс манифеста напрямую; фильтр не должен мутировать исходный список.
func TestResolveEffectiveTelemetry_NoManifestMutation(t *testing.T) {
	m := tel(nil, nil, []string{"cpu", "mem"})
	_ = ResolveEffectiveTelemetry(m, nil)
	if !reflect.DeepEqual(m.Collectors, []string{"cpu", "mem"}) {
		t.Errorf("манифест мутирован: %v", m.Collectors)
	}
}

// TestUnknownTelemetryCollectors — observability-хелпер: возвращает только
// неизвестные имена; ключ отсутствует / не-список / всё известно → nil.
func TestUnknownTelemetryCollectors(t *testing.T) {
	cases := []struct {
		name    string
		essence map[string]any
		want    []string
	}{
		{"ключ отсутствует", map[string]any{}, nil},
		{"nil essence", nil, nil},
		{"все известны", map[string]any{"telemetry_collectors": []string{"cpu", "mem"}}, nil},
		{"часть неизвестна", map[string]any{"telemetry_collectors": []string{"cpu", "bogus", "nope"}}, []string{"bogus", "nope"}},
		{"все неизвестны ([]any)", map[string]any{"telemetry_collectors": []any{"x", "y"}}, []string{"x", "y"}},
		{"не список строк → nil", map[string]any{"telemetry_collectors": []any{"cpu", 42}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnknownTelemetryCollectors(tc.essence); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("UnknownTelemetryCollectors = %v, want %v", got, tc.want)
			}
		})
	}
}
