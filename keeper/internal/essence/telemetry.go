package essence

import (
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

// telemetryIntervalCeilSec — the upper sanity ceiling for the interval
// (bound on the Redis TTL, ADR-072/NIM-87). Lower bound —
// config.TelemetryIntervalFloor.
const telemetryIntervalCeilSec = 3600

// essence override keys for telemetry (flat, like node_exporter_*).
const (
	essenceKeyTelemetryInterval   = "telemetry_interval"
	essenceKeyTelemetryCollectors = "telemetry_collectors"
)

// ResolveEffectiveTelemetry merges the manifest `telemetry:` policy with the
// essence override into the effective host-vitals config (ADR-072, NIM-87). A
// pure function: does not touch PG/IO — tested in isolation.
//
//   - enabled  = m.EnabledOrDefault() (essence does NOT override enabled);
//   - interval = essence[telemetry_interval] (string) else m.IntervalOrDefault();
//     ParseDuration + clamp [floor, ceil]; an unparsable override → default 30s;
//   - collectors = essence[telemetry_collectors] ([]string|[]any) else
//     m.CollectorsOrDefault(); the list is REPLACE (not union), filtered by
//     config.IsKnownCollector. An empty result after filtering (essence made
//     entirely of unknown names) → falls back to the manifest/default: a
//     non-empty explicit set ALWAYS goes on the wire (Soul treats [] as
//     fail-open "all 5", which is not the intent).
//
// m == nil (no `telemetry:` block) — valid: nil-safe getters return defaults.
func ResolveEffectiveTelemetry(m *config.TelemetryConfig, essence map[string]any) *keeperv1.TelemetryConfig {
	interval := m.IntervalOrDefault()
	if ov, ok := essenceString(essence, essenceKeyTelemetryInterval); ok {
		interval = ov
	}
	collectors := filterKnownCollectors(m.CollectorsOrDefault())
	if ov, ok := essenceStringSlice(essence, essenceKeyTelemetryCollectors); ok {
		if filtered := filterKnownCollectors(ov); len(filtered) > 0 {
			collectors = filtered
		}
	}
	return &keeperv1.TelemetryConfig{
		Enabled:     m.EnabledOrDefault(),
		IntervalSec: clampIntervalSec(interval),
		Collectors:  collectors,
	}
}

// clampIntervalSec parses a duration string and clamps seconds to
// [floor, ceil]. An unparsable string (essence is NOT validated on load,
// unlike the manifest) → falls back to the built-in default 30s: a typo in
// essence must not push the whole soul population down to the 10s floor.
func clampIntervalSec(s string) int32 {
	floor := int64(config.TelemetryIntervalFloor / time.Second)
	d, err := config.ParseDuration(s)
	if err != nil {
		d = 30 * time.Second
	}
	sec := int64(d / time.Second)
	if sec < floor {
		sec = floor
	}
	if sec > telemetryIntervalCeilSec {
		sec = telemetryIntervalCeilSec
	}
	return int32(sec)
}

// filterKnownCollectors keeps only collectors from the closed set
// (config.IsKnownCollector), preserving order. Always a new slice (does not
// mutate the input). An empty result is possible (a list made entirely of
// unknowns) — the caller (ResolveEffectiveTelemetry) falls back to the
// default; empty never goes on the wire.
func filterKnownCollectors(in []string) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		if config.IsKnownCollector(c) {
			out = append(out, c)
		}
	}
	return out
}

// UnknownTelemetryCollectors returns essence collectors that are not part of
// the closed set (observability: an operator typo is visible in keeper logs).
// nil if the key is missing / it is not a list of strings / all names are
// known.
func UnknownTelemetryCollectors(essence map[string]any) []string {
	ov, ok := essenceStringSlice(essence, essenceKeyTelemetryCollectors)
	if !ok {
		return nil
	}
	var unknown []string
	for _, c := range ov {
		if !config.IsKnownCollector(c) {
			unknown = append(unknown, c)
		}
	}
	return unknown
}

// essenceString reads a string essence override: (v, true) only if the key is
// present AND it is a non-empty string (empty/non-string → fallback to the
// manifest).
func essenceString(essence map[string]any, key string) (string, bool) {
	raw, ok := essence[key]
	if !ok {
		return "", false
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// essenceStringSlice reads the list-shaped essence override for collectors:
// []string or []any of strings (YAML-unmarshal gives []any). Returns
// (nil, false) if the key is absent or not recognized as a list of strings —
// then fallback to the manifest.
func essenceStringSlice(essence map[string]any, key string) ([]string, bool) {
	raw, ok := essence[key]
	if !ok {
		return nil, false
	}
	switch v := raw.(type) {
	case []string:
		return v, true
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		return nil, false
	}
}
