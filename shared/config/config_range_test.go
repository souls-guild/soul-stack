package config

import (
	"fmt"
	"testing"
)

// O5: numeric fields without range validation used to accept negative values
// silently before the hardening (reaper.batch_size, keeper.retry.max_attempts,
// logging.rotation.max_size_mb/max_files). They all now yield value_out_of_range.
// Base — keeperBaseRequired (semantic_test.go).

func TestKeeperRange_ReaperBatchSizeNegative(t *testing.T) {
	src := keeperBaseRequired + `reaper:
  enabled: true
  batch_size: -5
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.reaper.batch_size") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for negative reaper.batch_size")
	}
}

func TestKeeperRange_ReaperBatchSizePositive_OK(t *testing.T) {
	src := keeperBaseRequired + `reaper:
  enabled: true
  batch_size: 500
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("positive batch_size must not trigger value_out_of_range")
	}
}

func TestKeeperRange_AcolytesNegative(t *testing.T) {
	src := keeperBaseRequired + `acolytes: -1
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.acolytes") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for negative acolytes")
	}
}

func TestKeeperRange_AcolytesDefaultZero_OK(t *testing.T) {
	// Omitted key → default 0 (feature-flag off), no range diagnostics.
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(keeperBaseRequired), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("default (omitted) acolytes must not trigger value_out_of_range")
	}
}

func TestKeeperRange_AcolytesPositive_OK(t *testing.T) {
	src := keeperBaseRequired + `acolytes: 4
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("positive acolytes must not trigger value_out_of_range")
	}
}

func TestKeeperConfig_AcolytesDefaultZero(t *testing.T) {
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(keeperBaseRequired), ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("unexpected range error on base keeper.yml")
	}
	if cfg.Acolytes != 0 {
		t.Fatalf("expected default Acolytes 0, got %d", cfg.Acolytes)
	}
}

func TestKeeperRange_AcolyteBatchNegative(t *testing.T) {
	src := keeperBaseRequired + `acolyte_batch: -1
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.acolyte_batch") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for negative acolyte_batch")
	}
}

func TestKeeperRange_AcolyteBatchPositive_OK(t *testing.T) {
	src := keeperBaseRequired + `acolyte_batch: 25
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("positive acolyte_batch must not trigger value_out_of_range")
	}
}

func TestKeeperRange_AcolyteLeaseInvalidDuration(t *testing.T) {
	src := keeperBaseRequired + `acolyte_lease: not-a-duration
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "duration_invalid", "$.acolyte_lease") {
		dump(t, diags)
		t.Fatalf("expected duration_invalid for malformed acolyte_lease")
	}
}

func TestKeeperRange_AcolytePollIntervalInvalidDuration(t *testing.T) {
	src := keeperBaseRequired + `acolyte_poll_interval: 5x
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "duration_invalid", "$.acolyte_poll_interval") {
		dump(t, diags)
		t.Fatalf("expected duration_invalid for malformed acolyte_poll_interval")
	}
}

func TestKeeperRange_SigilAnchorsReloadIntervalInvalidDuration(t *testing.T) {
	src := keeperBaseRequired + `sigil_anchors_reload_interval: 5x
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "duration_invalid", "$.sigil_anchors_reload_interval") {
		dump(t, diags)
		t.Fatalf("expected duration_invalid for malformed sigil_anchors_reload_interval")
	}
}

func TestKeeperRange_SigilAnchorsReloadIntervalValid_OK(t *testing.T) {
	src := keeperBaseRequired + `sigil_anchors_reload_interval: 45s
`
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("valid sigil_anchors_reload_interval must not trigger duration_invalid")
	}
	if cfg.SigilAnchorsReloadInterval != "45s" {
		t.Fatalf("sigil_anchors_reload_interval not parsed: got %q", cfg.SigilAnchorsReloadInterval)
	}
}

func TestKeeperConfig_AcolyteParamsDefaultsOmitted(t *testing.T) {
	// With an empty config the fields are zero-value; the daemon applies DefaultAcolyte*.
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(keeperBaseRequired), ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if hasCode(diags, "value_out_of_range") || hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("base keeper.yml must not trigger acolyte diagnostics")
	}
	if cfg.AcolyteLease != "" || cfg.AcolyteBatch != 0 || cfg.AcolytePollInterval != "" {
		t.Fatalf("expected zero-value acolyte params, got lease=%q batch=%d poll=%q",
			cfg.AcolyteLease, cfg.AcolyteBatch, cfg.AcolytePollInterval)
	}
}

func TestKeeperConfig_AcolyteParamsParsed(t *testing.T) {
	src := keeperBaseRequired + `acolyte_lease: 45s
acolyte_batch: 32
acolyte_poll_interval: 500ms
`
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if hasCode(diags, "value_out_of_range") || hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("valid acolyte params must not trigger diagnostics")
	}
	if cfg.AcolyteLease != "45s" || cfg.AcolyteBatch != 32 || cfg.AcolytePollInterval != "500ms" {
		t.Fatalf("acolyte params not parsed: lease=%q batch=%d poll=%q",
			cfg.AcolyteLease, cfg.AcolyteBatch, cfg.AcolytePollInterval)
	}
}

func TestKeeperRange_OracleCircuitWindowInvalidDuration(t *testing.T) {
	src := keeperBaseRequired + `oracle_circuit_window: 5x
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "duration_invalid", "$.oracle_circuit_window") {
		dump(t, diags)
		t.Fatalf("expected duration_invalid for malformed oracle_circuit_window")
	}
}

func TestKeeperRange_OracleCircuitMaxFiresNegative(t *testing.T) {
	src := keeperBaseRequired + `oracle_circuit_max_fires: -1
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.oracle_circuit_max_fires") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for negative oracle_circuit_max_fires")
	}
}

// Distinguishing "omitted" (nil → default in the daemon) vs "explicit 0" (breaker
// OFF): the field is *int. Omitted → nil; explicit 0 → non-nil 0.
func TestKeeperConfig_OracleCircuitMaxFiresOmittedVsZero(t *testing.T) {
	cfgOmitted, _, _, err := LoadKeeperFromBytes("keeper.yml", []byte(keeperBaseRequired), ValidateOptions{})
	if err != nil {
		t.Fatalf("load omitted: %v", err)
	}
	if cfgOmitted.OracleCircuitMaxFires != nil {
		t.Fatalf("omitted oracle_circuit_max_fires should be nil, got %v", *cfgOmitted.OracleCircuitMaxFires)
	}

	src := keeperBaseRequired + `oracle_circuit_max_fires: 0
`
	cfgZero, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("load zero: %v", err)
	}
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("explicit 0 (breaker OFF) is valid, should not yield value_out_of_range")
	}
	if cfgZero.OracleCircuitMaxFires == nil || *cfgZero.OracleCircuitMaxFires != 0 {
		t.Fatalf("explicit 0 should parse into non-nil 0, got %v", cfgZero.OracleCircuitMaxFires)
	}
}

func TestKeeperRange_LoggingMaxSizeNegative(t *testing.T) {
	src := keeperBaseRequired + `logging:
  level: info
  rotation:
    max_size_mb: -1
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.logging.rotation.max_size_mb") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for negative logging.rotation.max_size_mb")
	}
}

func TestKeeperRange_LoggingMaxFilesNegative(t *testing.T) {
	src := keeperBaseRequired + `logging:
  level: info
  rotation:
    max_files: -3
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.logging.rotation.max_files") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for negative logging.rotation.max_files")
	}
}

// soulRangeBase — a minimal valid soul.yml without the field under test.
const soulRangeBase = `sid: redis-01.prod.example.com
keeper:
  endpoints:
    - host: k1.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
`

func TestSoulRange_RetryMaxAttemptsNegative(t *testing.T) {
	src := soulRangeBase + `  retry:
    max_attempts: -2
`
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.keeper.retry.max_attempts") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for negative keeper.retry.max_attempts")
	}
}

func TestSoulRange_RetryMaxAttemptsPositive_OK(t *testing.T) {
	src := soulRangeBase + `  retry:
    max_attempts: 5
`
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("positive max_attempts must not trigger value_out_of_range")
	}
}

// soulEndpointWithPriority — a minimal valid soul.yml where the first endpoint has
// a priority set. priority is an endpoint field (6-space indent), so it can't be
// appended to soulRangeBase (which ends with keeper.tls at the 2-space level); we
// build the whole endpoint block.
func soulEndpointWithPriority(priorityLine string) string {
	return `sid: redis-01.prod.example.com
keeper:
  endpoints:
    - host: k1.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
` + priorityLine + `  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
`
}

func TestSoulRange_EndpointPriorityNegative(t *testing.T) {
	src := soulEndpointWithPriority("      priority: -1\n")
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.keeper.endpoints[0].priority") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for negative keeper.endpoints[0].priority")
	}
}

func TestSoulRange_EndpointPriorityDefaultZero_OK(t *testing.T) {
	// priority: 0 (explicit) = "unset" → normalizedPriority maps it to 1.
	// KEY case: 0 is allowed, there must be no range diagnostic.
	src := soulEndpointWithPriority("      priority: 0\n")
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("priority 0 (unset -> default 1) must not trigger value_out_of_range")
	}
}

func TestSoulRange_EndpointPriorityOmitted_OK(t *testing.T) {
	// Omitted key → zero-value 0 → default 1, no range diagnostics.
	src := soulEndpointWithPriority("")
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("omitted endpoint priority must not trigger value_out_of_range")
	}
}

func TestSoulRange_EndpointPriorityPositive_OK(t *testing.T) {
	for _, p := range []int{1, 2} {
		src := soulEndpointWithPriority(fmt.Sprintf("      priority: %d\n", p))
		_, _, diags, _ := LoadSoulFromBytes("soul.yml", []byte(src), ValidateOptions{})
		if hasCode(diags, "value_out_of_range") {
			dump(t, diags)
			t.Fatalf("positive endpoint priority %d must not trigger value_out_of_range", p)
		}
	}
}

func TestSoulRange_LoggingMaxSizeNegative(t *testing.T) {
	src := soulRangeBase + `logging:
  level: info
  rotation:
    max_size_mb: -10
`
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.logging.rotation.max_size_mb") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for negative soul logging.rotation.max_size_mb")
	}
}
