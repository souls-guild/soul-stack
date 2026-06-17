package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

func TestSoulOTelEndpoint(t *testing.T) {
	if got := soulOTelEndpoint(nil); got != "" {
		t.Errorf("soulOTelEndpoint(nil) = %q, want empty", got)
	}
	if got := soulOTelEndpoint(&config.SoulOTel{Endpoint: "otel:4317"}); got != "otel:4317" {
		t.Errorf("soulOTelEndpoint = %q, want otel:4317", got)
	}
}

// Дефолт loopback-адреса метрик зафиксирован и совпадает с docs/soul/config.md.
func TestDefaultSoulMetricsListen(t *testing.T) {
	if defaultSoulMetricsListen != "127.0.0.1:9091" {
		t.Errorf("defaultSoulMetricsListen = %q, want 127.0.0.1:9091", defaultSoulMetricsListen)
	}
}

func TestResolveSoulMetricsBasicAuth(t *testing.T) {
	t.Run("nil block → no auth", func(t *testing.T) {
		got, err := resolveSoulMetricsBasicAuth(nil)
		if err != nil || got != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("disabled → no auth", func(t *testing.T) {
		got, err := resolveSoulMetricsBasicAuth(&config.SoulMetricsBasicAuth{Enabled: false})
		if err != nil || got != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("enabled reads password from file, trims newline", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "pw")
		if err := os.WriteFile(f, []byte("s3cr3t\n"), 0o400); err != nil {
			t.Fatal(err)
		}
		got, err := resolveSoulMetricsBasicAuth(&config.SoulMetricsBasicAuth{
			Enabled: true, Username: "scrape", PasswordFile: f,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || got.Username != "scrape" || got.Password != "s3cr3t" {
			t.Fatalf("got %+v, want {scrape s3cr3t}", got)
		}
	})

	t.Run("missing file → error", func(t *testing.T) {
		_, err := resolveSoulMetricsBasicAuth(&config.SoulMetricsBasicAuth{
			Enabled: true, Username: "scrape", PasswordFile: "/no/such/file",
		})
		if err == nil {
			t.Fatal("expected error for missing password file")
		}
	})

	t.Run("empty file → error", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "pw")
		if err := os.WriteFile(f, []byte("\n"), 0o400); err != nil {
			t.Fatal(err)
		}
		_, err := resolveSoulMetricsBasicAuth(&config.SoulMetricsBasicAuth{
			Enabled: true, Username: "scrape", PasswordFile: f,
		})
		if err == nil {
			t.Fatal("expected error for empty password file")
		}
	})
}
