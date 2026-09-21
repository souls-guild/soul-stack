package main

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// resolveMetricsBasicAuth with no configured block / when disabled returns
// (nil, nil) -- the listener comes up without auth, vault is not touched
// (vc=nil ok).
func TestResolveMetricsBasicAuth_DisabledOrAbsent(t *testing.T) {
	cases := []struct {
		name string
		m    *config.KeeperMetrics
	}{
		{"nil-block", nil},
		{"nil-auth", &config.KeeperMetrics{}},
		{"nil-basic", &config.KeeperMetrics{Auth: &config.KeeperMetricsAuth{}}},
		{"disabled", &config.KeeperMetrics{Auth: &config.KeeperMetricsAuth{
			Basic: &config.KeeperMetricsBasicAuth{Enabled: false, Username: "u", PasswordRef: "vault:secret/x"},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// vc=nil deliberately: none of these branches should touch vault.
			auth, err := resolveMetricsBasicAuth(context.Background(), nil, tc.m)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if auth != nil {
				t.Errorf("auth = %+v, want nil", auth)
			}
		})
	}
}

// resolveMetricsBasicAuth with enabled but a nil vault client -- error
// (password_ref resolution is impossible). The password/ref never end up
// in the message.
func TestResolveMetricsBasicAuth_EnabledNilVault(t *testing.T) {
	m := &config.KeeperMetrics{Auth: &config.KeeperMetricsAuth{
		Basic: &config.KeeperMetricsBasicAuth{
			Enabled: true, Username: "scrape", PasswordRef: "vault:secret/keeper/metrics-password",
		},
	}}
	_, err := resolveMetricsBasicAuth(context.Background(), nil, m)
	if err == nil {
		t.Fatal("err = nil, want error for enabled basic-auth with nil vault client")
	}
}

func TestOTelEndpoint(t *testing.T) {
	if got := otelEndpoint(nil); got != "" {
		t.Errorf("otelEndpoint(nil) = %q, want empty", got)
	}
	if got := otelEndpoint(&config.KeeperOTel{Endpoint: "otel:4317"}); got != "otel:4317" {
		t.Errorf("otelEndpoint = %q, want otel:4317", got)
	}
}
