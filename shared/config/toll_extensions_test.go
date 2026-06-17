package config

import "testing"

// Toll-extensions (ADR-038 amendment 2026-05-27): per_coven_thresholds +
// webhook. Тесты только на НОВЫЕ поля; общая validate-инфраструктура
// keeperBaseRequired / hasCode* — из semantic_test.go.

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
	// Threshold = 0 — bizdev-бессмыслен (срабатывает на первом disconnect),
	// schema-фаза должна отвергнуть (диапазон (0, 1]).
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
	// При enabled: false url_ref необязателен (notifier не поднимется).
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
	// checkDuration выдаёт invalid_duration или value_out_of_range — оба
	// допустимы как сигнал «timeout invalid». Главное — не зелёный.
	if !hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("expected duration-error for invalid webhook.timeout")
	}
}
