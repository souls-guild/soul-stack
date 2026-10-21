package servicevars

import (
	"log/slog"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// telemetryIntervalCeilSec — the upper sanity ceiling for the interval
// (bound on the Redis TTL, ADR-072/NIM-87). Lower bound —
// config.TelemetryIntervalFloor.
const telemetryIntervalCeilSec = 3600

// Service-var keys for telemetry (flat, like node_exporter_*).
const (
	varKeyTelemetryInterval   = "telemetry_interval"
	varKeyTelemetryCollectors = "telemetry_collectors"
)

// TelemetryVars looks up, in the service view, the vars [ResolveEffectiveTelemetry]
// and [UnknownTelemetryCollectors] read — those two keys and no other, so a broken
// var elsewhere in the service is never evaluated and does not withhold the host's
// telemetry config (ADR-0082 amendment 2026-10-15, point 7). A key the service does
// not define is absent from the map.
//
// A key whose definition fails to evaluate is absent too, with a WARN naming it and
// the error: the per-key fallback [ResolveEffectiveTelemetry] already gives a value
// that does not parse. Failing the whole lookup instead withheld the entire
// config, the manifest's `enabled: false` with it, over one key. logger nil is
// [slog.Default].
func TelemetryVars(view *cel.VarView, logger *slog.Logger) map[string]any {
	if logger == nil {
		logger = slog.Default()
	}
	out := make(map[string]any, 2)
	for _, key := range [...]string{varKeyTelemetryInterval, varKeyTelemetryCollectors} {
		val, found, err := view.Lookup(key)
		if err != nil {
			logger.Warn("telemetry: a service var does not evaluate, falling back to the manifest for it",
				slog.String("key", key),
				slog.Any("error", err),
			)
			continue
		}
		if found {
			out[key] = val
		}
	}
	return out
}

// ResolveEffectiveTelemetry merges the manifest `telemetry:` policy with the
// service's own vars into the effective host-vitals config (ADR-072, NIM-87). A
// pure function: does not touch PG/IO — tested in isolation. vars is what
// [TelemetryVars] looked up, or nil for a service-level default.
//
//   - enabled  = m.EnabledOrDefault() (a service var does NOT override enabled);
//   - interval = vars[telemetry_interval] (string) else m.IntervalOrDefault();
//     ParseDuration + clamp [floor, ceil]; an unparsable value → default 30s;
//   - collectors = vars[telemetry_collectors] ([]string|[]any) else
//     m.CollectorsOrDefault(); the list is REPLACE (not union), filtered by
//     config.IsKnownCollector. An empty result after filtering (a list made
//     entirely of unknown names) → falls back to the manifest/default: a
//     non-empty explicit set ALWAYS goes on the wire (Soul treats [] as
//     fail-open "all 5", which is not the intent).
//
// m == nil (no `telemetry:` block) — valid: nil-safe getters return defaults.
func ResolveEffectiveTelemetry(m *config.TelemetryConfig, vars map[string]any) *keeperv1.TelemetryConfig {
	interval := m.IntervalOrDefault()
	if ov, ok := varString(vars, varKeyTelemetryInterval); ok {
		interval = ov
	}
	collectors := filterKnownCollectors(m.CollectorsOrDefault())
	if ov, ok := varStringSlice(vars, varKeyTelemetryCollectors); ok {
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
// [floor, ceil]. An unparsable string (service vars are NOT validated on load,
// unlike the manifest) → falls back to the built-in default 30s: a typo in a
// service var must not push the whole soul population down to the 10s floor.
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

// UnknownTelemetryCollectors returns collectors named in the service's vars (as
// [TelemetryVars] looked them up) that are not part of the closed set
// (observability: an author's typo is visible in keeper logs). nil if the key is
// missing / it is not a list of strings / all names are known.
func UnknownTelemetryCollectors(vars map[string]any) []string {
	ov, ok := varStringSlice(vars, varKeyTelemetryCollectors)
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

// varString reads a string service var: (v, true) only if the key is present AND
// it is a non-empty string (empty/non-string → fallback to the manifest).
func varString(vars map[string]any, key string) (string, bool) {
	raw, ok := vars[key]
	if !ok {
		return "", false
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// varStringSlice reads the list-shaped collectors var: []string or []any of
// strings (YAML-unmarshal gives []any). Returns (nil, false) if the key is
// absent or not recognized as a list of strings — then fallback to the manifest.
func varStringSlice(vars map[string]any, key string) ([]string, bool) {
	raw, ok := vars[key]
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
