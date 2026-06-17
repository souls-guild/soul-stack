package obs

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

// resourceValue достаёт строковое значение attr-ключа из OTel-resource.
func resourceValue(t *testing.T, cfg OTelConfig, key attribute.Key) (string, bool) {
	t.Helper()
	res, err := buildResource(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildResource: %v", err)
	}
	v, ok := res.Set().Value(key)
	return v.AsString(), ok
}

// keeper-resource несёт service.name="keeper" + кастомный soulstack.kid.
func TestBuildResource_KeeperCarriesKID(t *testing.T) {
	cfg := OTelConfig{
		ServiceName:   "keeper",
		ResourceAttrs: map[string]string{"soulstack.kid": "keeper-01"},
	}
	if got, ok := resourceValue(t, cfg, "service.name"); !ok || got != "keeper" {
		t.Errorf("service.name = %q (ok=%v), want \"keeper\"", got, ok)
	}
	if got, ok := resourceValue(t, cfg, "soulstack.kid"); !ok || got != "keeper-01" {
		t.Errorf("soulstack.kid = %q (ok=%v), want \"keeper-01\"", got, ok)
	}
}

// soul-resource несёт service.name="soul" + кастомный soulstack.sid.
func TestBuildResource_SoulCarriesSID(t *testing.T) {
	cfg := OTelConfig{
		ServiceName:   "soul",
		ResourceAttrs: map[string]string{"soulstack.sid": "host-7.example.com"},
	}
	if got, ok := resourceValue(t, cfg, "service.name"); !ok || got != "soul" {
		t.Errorf("service.name = %q (ok=%v), want \"soul\"", got, ok)
	}
	if got, ok := resourceValue(t, cfg, "soulstack.sid"); !ok || got != "host-7.example.com" {
		t.Errorf("soulstack.sid = %q (ok=%v), want \"host-7.example.com\"", got, ok)
	}
}

// SetupOTel при Enabled=false должен вернуть не-nil провайдер (main не
// ветвится), а Shutdown — отработать без ошибки.
func TestSetupOTel_DisabledNoOp(t *testing.T) {
	p, err := SetupOTel(context.Background(), OTelConfig{
		Enabled:     false,
		ServiceName: "keeper",
	})
	if err != nil {
		t.Fatalf("SetupOTel(disabled): %v", err)
	}
	if p == nil {
		t.Fatal("SetupOTel вернул nil provider при Enabled=false")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown(disabled): %v", err)
	}
}

// Shutdown на nil-провайдере безопасен (defer-цепочка main не должна
// проверять nil).
func TestOTelProvider_ShutdownNil(t *testing.T) {
	var p *OTelProvider
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown(nil): %v", err)
	}
}

// Enabled без Endpoint поднимает TracerProvider без OTLP-exporter
// (span-ы никуда не уходят, но API не ломается). Кастомные ResourceAttrs
// (soulstack.kid) принимаются без ошибки.
func TestSetupOTel_EnabledNoEndpoint(t *testing.T) {
	p, err := SetupOTel(context.Background(), OTelConfig{
		Enabled:       true,
		ServiceName:   "keeper",
		ResourceAttrs: map[string]string{"soulstack.kid": "keeper-01"},
	})
	if err != nil {
		t.Fatalf("SetupOTel(enabled, no endpoint): %v", err)
	}
	if p == nil {
		t.Fatal("SetupOTel вернул nil provider")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}
