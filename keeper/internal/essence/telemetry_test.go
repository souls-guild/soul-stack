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
	allDefault := []string{"cpu", "mem", "disk", "load", "uptime", "net"}

	cases := []struct {
		name        string
		manifest    *config.TelemetryConfig
		essence     map[string]any
		wantEnabled bool
		wantSec     int32
		wantCollect []string
	}{
		{
			name:        "manifest nil -> defaults (enabled, 30s, all 6)",
			manifest:    nil,
			essence:     nil,
			wantEnabled: true,
			wantSec:     30,
			wantCollect: allDefault,
		},
		{
			name:        "empty essence -> manifest values",
			manifest:    tel(ptrBool(false), ptrStr("45s"), []string{"cpu", "mem"}),
			essence:     map[string]any{},
			wantEnabled: false,
			wantSec:     45,
			wantCollect: []string{"cpu", "mem"},
		},
		{
			name:        "essence overrides interval and collectors",
			manifest:    tel(nil, ptrStr("60s"), []string{"cpu"}),
			essence:     map[string]any{"telemetry_interval": "120s", "telemetry_collectors": []string{"mem", "disk"}},
			wantEnabled: true,
			wantSec:     120,
			wantCollect: []string{"mem", "disk"},
		},
		{
			name:        "essence collectors as []any (YAML form)",
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
			wantCollect: allDefault,
		},
		{
			name:        "clamp ceiling: interval > 3600s → 3600s",
			manifest:    nil,
			essence:     map[string]any{"telemetry_interval": "2h"},
			wantEnabled: true,
			wantSec:     3600,
			wantCollect: allDefault,
		},
		{
			name:        "unknown collector filtered out (REPLACE)",
			manifest:    nil,
			essence:     map[string]any{"telemetry_collectors": []string{"cpu", "bogus", "mem"}},
			wantEnabled: true,
			wantSec:     30,
			wantCollect: []string{"cpu", "mem"},
		},
		{
			name:        "broken essence-interval -> default 30s (not floor)",
			manifest:    tel(nil, ptrStr("60s"), nil),
			essence:     map[string]any{"telemetry_interval": "not-a-duration"},
			wantEnabled: true,
			wantSec:     30,
			wantCollect: allDefault,
		},
		{
			name:        "essence does NOT override enabled (manifest false stays)",
			manifest:    tel(ptrBool(false), nil, nil),
			essence:     map[string]any{"telemetry_interval": "20s"},
			wantEnabled: false,
			wantSec:     20,
			wantCollect: allDefault,
		},
		{
			name:        "essence collectors not a string list -> fallback to manifest",
			manifest:    tel(nil, nil, []string{"cpu"}),
			essence:     map[string]any{"telemetry_collectors": []any{"cpu", 42}},
			wantEnabled: true,
			wantSec:     30,
			wantCollect: []string{"cpu"},
		},
		{
			name:        "all essence-collectors unknown + manifest nil -> fallback to default (all 6, NOT empty)",
			manifest:    nil,
			essence:     map[string]any{"telemetry_collectors": []string{"bogus", "nope"}},
			wantEnabled: true,
			wantSec:     30,
			wantCollect: allDefault,
		},
		{
			name:        "all essence-collectors unknown + manifest set -> fallback to manifest",
			manifest:    tel(nil, nil, []string{"cpu", "mem"}),
			essence:     map[string]any{"telemetry_collectors": []string{"bogus"}},
			wantEnabled: true,
			wantSec:     30,
			wantCollect: []string{"cpu", "mem"},
		},
		{
			name:        "manifest interval at floor boundary (10s) passes",
			manifest:    tel(nil, ptrStr("10s"), nil),
			essence:     nil,
			wantEnabled: true,
			wantSec:     10,
			wantCollect: allDefault,
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

// TestResolveEffectiveTelemetry_NoManifestMutation — CollectorsOrDefault returns
// the manifest slice directly; the filter must not mutate the source list.
func TestResolveEffectiveTelemetry_NoManifestMutation(t *testing.T) {
	m := tel(nil, nil, []string{"cpu", "mem"})
	_ = ResolveEffectiveTelemetry(m, nil)
	if !reflect.DeepEqual(m.Collectors, []string{"cpu", "mem"}) {
		t.Errorf("manifest mutated: %v", m.Collectors)
	}
}

// TestUnknownTelemetryCollectors — observability helper: returns only
// unknown names; key absent / not a list / all known -> nil.
func TestUnknownTelemetryCollectors(t *testing.T) {
	cases := []struct {
		name    string
		essence map[string]any
		want    []string
	}{
		{"key absent", map[string]any{}, nil},
		{"nil essence", nil, nil},
		{"all known", map[string]any{"telemetry_collectors": []string{"cpu", "mem"}}, nil},
		{"some unknown", map[string]any{"telemetry_collectors": []string{"cpu", "bogus", "nope"}}, []string{"bogus", "nope"}},
		{"all unknown ([]any)", map[string]any{"telemetry_collectors": []any{"x", "y"}}, []string{"x", "y"}},
		{"not a string list -> nil", map[string]any{"telemetry_collectors": []any{"cpu", 42}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnknownTelemetryCollectors(tc.essence); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("UnknownTelemetryCollectors = %v, want %v", got, tc.want)
			}
		})
	}
}
