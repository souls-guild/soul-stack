package config

import "testing"

// Toll extensions (ADR-038 amendment 2026-05-27): per_coven_thresholds +
// webhook. Tests only for the NEW fields; the shared validate infrastructure
// keeperBaseRequired / hasCode* is from semantic_test.go.

func TestKeeperToll_PerCovenThreshold_Valid(t *testing.T) {
	src := keeperBaseRequired + `toll:
  per_coven_thresholds:
    production-eu: 0.15
    production-us: 0.25
`
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("valid per_coven_thresholds must not trigger range error")
	}
	if cfg.Toll == nil || cfg.Toll.PerCovenThresholds["production-eu"] != 0.15 {
		t.Fatalf("expected per_coven_thresholds[production-eu]=0.15, got %v", cfg.Toll)
	}
}

func TestKeeperToll_PerCovenThreshold_OutOfRange(t *testing.T) {
	src := keeperBaseRequired + `toll:
  per_coven_thresholds:
    production-eu: 1.5
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for per_coven threshold > 1")
	}
}

func TestKeeperToll_PerCovenThreshold_ZeroRejected(t *testing.T) {
	// Threshold = 0 is bizdev-meaningless (fires on the first disconnect),
	// the schema phase must reject it (range (0, 1]).
	src := keeperBaseRequired + `toll:
  per_coven_thresholds:
    production-eu: 0
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for per_coven threshold = 0")
	}
}

func TestKeeperToll_Webhook_RequiresURLRef(t *testing.T) {
	src := keeperBaseRequired + `toll:
  webhook:
    enabled: true
    format: generic
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for webhook.enabled=true without url_ref")
	}
}

func TestKeeperToll_Webhook_FormatEnum(t *testing.T) {
	src := keeperBaseRequired + `toll:
  webhook:
    enabled: true
    url_ref: "https://hooks.slack.com/x"
    format: junk
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "value_not_in_enum") {
		dump(t, diags)
		t.Fatalf("expected value_not_in_enum for webhook.format=junk")
	}
}

func TestKeeperToll_Webhook_ValidFormats(t *testing.T) {
	for _, f := range []string{"generic", "pagerduty_v2", "slack"} {
		t.Run(f, func(t *testing.T) {
			src := keeperBaseRequired + `toll:
  webhook:
    enabled: true
    url_ref: "vault:secret/keeper/toll-webhook"
    format: ` + f + `
`
			_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
			if hasCode(diags, "value_not_in_enum") || hasCode(diags, "missing_required_field") {
				dump(t, diags)
				t.Fatalf("valid webhook format %q must not trigger errors", f)
			}
		})
	}
}

func TestKeeperToll_Webhook_DisabledNoValidation(t *testing.T) {
	// With enabled: false the url_ref is optional (the notifier won't start).
	src := keeperBaseRequired + `toll:
  webhook:
    enabled: false
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "missing_required_field") || hasCode(diags, "value_not_in_enum") {
		dump(t, diags)
		t.Fatalf("disabled webhook must not require url_ref / format")
	}
}

func TestKeeperToll_Webhook_TimeoutFormat(t *testing.T) {
	src := keeperBaseRequired + `toll:
  webhook:
    enabled: true
    url_ref: "https://h/x"
    format: generic
    timeout: "not-a-duration"
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	// checkDuration emits invalid_duration or value_out_of_range — both are
	// acceptable as a "timeout invalid" signal. The point is: not green.
	if !hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("expected duration-error for invalid webhook.timeout")
	}
}
