package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// soulBaseWithMetrics assembles a minimally valid soul.yml with an arbitrary
// metrics: block body for basic_auth validation tests.
func soulBaseWithMetrics(metricsBlock string) []byte {
	return []byte(`sid: redis-01.prod.example.com
keeper:
  endpoints:
    - host: k1.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
` + metricsBlock)
}

func TestSoulMetricsBasicAuth_Valid(t *testing.T) {
	src := soulBaseWithMetrics(`metrics:
  enabled: true
  listen: "127.0.0.1:9191"
  basic_auth:
    enabled: true
    username: scrape
    password_file: /etc/soul/metrics-password
`)
	cfg, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for valid metrics.basic_auth")
	}
	if cfg.Metrics == nil || cfg.Metrics.BasicAuth == nil {
		t.Fatal("metrics.basic_auth not parsed")
	}
	b := cfg.Metrics.BasicAuth
	if !b.Enabled || b.Username != "scrape" || b.PasswordFile != "/etc/soul/metrics-password" {
		t.Errorf("parsed basic_auth = %+v, unexpected", b)
	}
}

func TestSoulMetricsBasicAuth_EnabledMissingUsername(t *testing.T) {
	src := soulBaseWithMetrics(`metrics:
  enabled: true
  basic_auth:
    enabled: true
    password_file: /etc/soul/metrics-password
`)
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.metrics.basic_auth.username") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for username")
	}
}

func TestSoulMetricsBasicAuth_EnabledMissingPasswordFile(t *testing.T) {
	src := soulBaseWithMetrics(`metrics:
  enabled: true
  basic_auth:
    enabled: true
    username: scrape
`)
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.metrics.basic_auth.password_file") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for password_file")
	}
}

func TestSoulMetricsBasicAuth_DisabledNoFieldsOK(t *testing.T) {
	src := soulBaseWithMetrics(`metrics:
  enabled: true
  basic_auth:
    enabled: false
`)
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for disabled basic_auth with no fields")
	}
}
