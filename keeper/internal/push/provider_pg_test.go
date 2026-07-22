package push

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
)

// fakePushProviderReader is a mock [PushProviderResolver] for unit tests.
type fakePushProviderReader struct {
	provider *pushprovider.PushProvider
	err      error
}

func (r *fakePushProviderReader) SelectByName(_ context.Context, _ string) (*pushprovider.PushProvider, error) {
	return r.provider, r.err
}

// fakeLegacyFallback is a mock [LegacyPushProvidersFallback].
type fakeLegacyFallback struct {
	params map[string]any
	ok     bool
}

func (f *fakeLegacyFallback) ResolveParams(_ string) (map[string]any, bool) {
	return f.params, f.ok
}

func TestPGFallback_PGFirstReturnsPGParams(t *testing.T) {
	reader := &fakePushProviderReader{
		provider: &pushprovider.PushProvider{
			Name:   "vault-bastion",
			Params: map[string]any{"vault_addr": "https://vault.example.com"},
		},
	}
	fallback := &fakeLegacyFallback{
		params: map[string]any{"vault_addr": "old-yaml"},
		ok:     true,
	}
	r := &PGFallbackProviderResolver{
		Reader:      reader,
		Fallback:    fallback,
		AllowLegacy: true, // enabled, but shouldn't trigger (PG row exists)
	}
	params, err := r.ResolveParams(context.Background(), "vault-bastion")
	if err != nil {
		t.Fatalf("ResolveParams: %v", err)
	}
	if params["vault_addr"] != "https://vault.example.com" {
		t.Errorf("PG params lost, got: %v", params)
	}
}

func TestPGFallback_PGEmptyParamsReturnedAsEmptyMap(t *testing.T) {
	reader := &fakePushProviderReader{
		provider: &pushprovider.PushProvider{Name: "vault", Params: nil},
	}
	r := &PGFallbackProviderResolver{Reader: reader}
	params, err := r.ResolveParams(context.Background(), "vault")
	if err != nil {
		t.Fatalf("ResolveParams: %v", err)
	}
	if params == nil || len(params) != 0 {
		t.Errorf("params = %v, want empty map", params)
	}
}

func TestPGFallback_NotFoundAllowLegacyFalse(t *testing.T) {
	reader := &fakePushProviderReader{err: pushprovider.ErrPushProviderNotFound}
	r := &PGFallbackProviderResolver{Reader: reader, AllowLegacy: false}
	_, err := r.ResolveParams(context.Background(), "missing")
	if !errors.Is(err, ErrPushProviderNotConfigured) {
		t.Errorf("err = %v, want ErrPushProviderNotConfigured", err)
	}
}

func TestPGFallback_NotFoundAllowLegacyTrueUsesYAML(t *testing.T) {
	reader := &fakePushProviderReader{err: pushprovider.ErrPushProviderNotFound}
	fallback := &fakeLegacyFallback{
		params: map[string]any{"vault_addr": "from-yaml"},
		ok:     true,
	}
	var buf bytes.Buffer
	r := &PGFallbackProviderResolver{
		Reader:      reader,
		Fallback:    fallback,
		AllowLegacy: true,
		Logger:      slog.New(slog.NewTextHandler(&buf, nil)),
	}
	params, err := r.ResolveParams(context.Background(), "vault-bastion")
	if err != nil {
		t.Fatalf("ResolveParams: %v", err)
	}
	if params["vault_addr"] != "from-yaml" {
		t.Errorf("legacy params lost: %v", params)
	}
	if !strings.Contains(buf.String(), "S7-2 deprecation") {
		t.Errorf("deprecation WARN not logged: %s", buf.String())
	}
}

func TestPGFallback_WARNOnceOnly(t *testing.T) {
	reader := &fakePushProviderReader{err: pushprovider.ErrPushProviderNotFound}
	fallback := &fakeLegacyFallback{params: map[string]any{}, ok: true}
	var buf bytes.Buffer
	r := &PGFallbackProviderResolver{
		Reader:      reader,
		Fallback:    fallback,
		AllowLegacy: true,
		Logger:      slog.New(slog.NewTextHandler(&buf, nil)),
	}
	for i := 0; i < 3; i++ {
		_, _ = r.ResolveParams(context.Background(), "vault")
	}
	warnCount := strings.Count(buf.String(), "S7-2 deprecation")
	if warnCount != 1 {
		t.Errorf("WARN logged %d times, want 1", warnCount)
	}
}

func TestPGFallback_LegacyNotFoundReturnsNotConfigured(t *testing.T) {
	reader := &fakePushProviderReader{err: pushprovider.ErrPushProviderNotFound}
	fallback := &fakeLegacyFallback{ok: false} // not in yaml either
	r := &PGFallbackProviderResolver{
		Reader: reader, Fallback: fallback, AllowLegacy: true,
	}
	_, err := r.ResolveParams(context.Background(), "missing")
	if !errors.Is(err, ErrPushProviderNotConfigured) {
		t.Errorf("err = %v, want ErrPushProviderNotConfigured", err)
	}
}

func TestPGFallback_TransportErrorPropagates(t *testing.T) {
	dbErr := errors.New("connection refused")
	reader := &fakePushProviderReader{err: dbErr}
	r := &PGFallbackProviderResolver{Reader: reader, AllowLegacy: true}
	_, err := r.ResolveParams(context.Background(), "vault")
	if err == nil || errors.Is(err, ErrPushProviderNotConfigured) {
		t.Errorf("err = %v, want wrap of dbErr", err)
	}
}
