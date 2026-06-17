package main

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// resolveMetricsBasicAuth без настроенного блока / при disabled возвращает
// (nil, nil) — listener поднимается без auth, vault не дёргается (vc=nil ok).
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
			// vc=nil намеренно: ни одна из этих веток не должна обращаться к vault.
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

// resolveMetricsBasicAuth при enabled, но nil vault-клиенте — ошибка
// (резолв password_ref невозможен). Пароль/ref в сообщение не попадают.
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
