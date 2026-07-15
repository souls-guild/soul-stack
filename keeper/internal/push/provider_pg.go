package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ErrPushProviderNotConfigured is the sentinel error for resolving env-payload
// params: the PG table `push_providers` has no row for the plugin name, and
// legacy fallback is disabled (`push.allow_legacy_push_providers=false`).
//
// Not an error condition: pilot S6 + legacy-fallback false meant "no
// env-payload — the plugin starts with defaults," so setupPushDispatchers
// isn't required to treat this as a startup error. The sentinel exists for a
// diagnostic message in the wire-up log ("provider X not configured in PG,
// fallback disabled — plugin starts without env-payload").
var ErrPushProviderNotConfigured = errors.New("push: provider not configured in PG and legacy fallback disabled")

// PushProviderResolver is the narrow surface over push_providers storage that
// the PG resolver needs. Implemented by a wrapper over pgxpool.Pool (see
// [NewPGPushProviderReader]); unit tests substitute a fake.
type PushProviderResolver interface {
	SelectByName(ctx context.Context, name string) (*pushprovider.PushProvider, error)
}

// pgPoolPushProviderReader is the production implementation of
// [PushProviderResolver] over pgxpool.Pool / the corresponding
// pushprovider.ExecQueryRower.
type pgPoolPushProviderReader struct {
	db pushprovider.ExecQueryRower
}

// NewPGPushProviderReader adapts a pgxpool.Pool (or any
// pushprovider.ExecQueryRower) to [PushProviderResolver]. Used by
// setupPushDispatchers in daemon wire-up.
func NewPGPushProviderReader(db pushprovider.ExecQueryRower) PushProviderResolver {
	return &pgPoolPushProviderReader{db: db}
}

func (r *pgPoolPushProviderReader) SelectByName(ctx context.Context, name string) (*pushprovider.PushProvider, error) {
	return pushprovider.SelectByName(ctx, r.db, name)
}

// LegacyPushProvidersFallback is the narrow surface over resolving config
// `keeper.yml::push.providers[]`. Implemented by a thin wrapper in
// daemon wire-up over `[]config.KeeperPushProvider`.
type LegacyPushProvidersFallback interface {
	ResolveParams(name string) (map[string]any, bool)
}

// configProvidersFallback is the production implementation of
// [LegacyPushProvidersFallback] over `[]config.KeeperPushProvider` (the
// inline form from pilot S6). Lookup by name is linear: the list is short
// (1-2 plugins in a typical install), so we don't bother building a map.
type configProvidersFallback struct {
	entries []config.KeeperPushProvider
}

// NewLegacyConfigProvidersFallback wraps `keeper.yml::push.providers[]` in a
// LegacyPushProvidersFallback.
func NewLegacyConfigProvidersFallback(providers []config.KeeperPushProvider) LegacyPushProvidersFallback {
	return &configProvidersFallback{entries: providers}
}

func (f *configProvidersFallback) ResolveParams(name string) (map[string]any, bool) {
	for _, e := range f.entries {
		if e.Name == name {
			return e.Params, true
		}
	}
	return nil, false
}

// PGFallbackProviderResolver is a PG-first resolver for the SSH plugin's
// env-payload params, with an optional fallback to keeper.yml::push.providers[]
// (ADR-032 amendment 2026-05-26, S7-2).
//
// ResolveParams algorithm:
//
//  1. SELECT push_providers by name:
//     - row found → return `params` (may be an empty object).
//     - [pushprovider.ErrPushProviderNotFound] → go to step 2.
//     - other errors → propagate (PG unavailable).
//
//  2. `AllowLegacy=false` (default for S7-2) → return
//     [ErrPushProviderNotConfigured]; the caller (daemon wire-up) treats this
//     error as fine and starts the plugin without env-payload.
//     `AllowLegacy=true` → log a one-time WARN deprecation notice and
//     delegate to [Fallback] (a resolver over keeper.yml::push.providers[]).
//
// Fail-safe semantics: an unconfigured provider is NOT a startup error;
// the security invariant (sensitive params as vault-refs) is checked in
// pushprovider.Service.Create/Update, not here — there's no operator input
// here, only reads.
type PGFallbackProviderResolver struct {
	Reader       PushProviderResolver
	Fallback     LegacyPushProvidersFallback
	AllowLegacy  bool
	Logger       *slog.Logger
	legacyWarned sync.Once
}

// ResolveParams returns env-payload params for the plugin named pluginName.
// Semantics — see the type's doc comment.
func (r *PGFallbackProviderResolver) ResolveParams(ctx context.Context, pluginName string) (map[string]any, error) {
	p, err := r.Reader.SelectByName(ctx, pluginName)
	if err == nil {
		if p.Params == nil {
			return map[string]any{}, nil
		}
		return p.Params, nil
	}
	if !errors.Is(err, pushprovider.ErrPushProviderNotFound) {
		return nil, fmt.Errorf("push: read push_providers %q: %w", pluginName, err)
	}

	// No PG row: switch to legacy fallback if the flag allows it.
	if !r.AllowLegacy || r.Fallback == nil {
		return nil, fmt.Errorf("%w: %s", ErrPushProviderNotConfigured, pluginName)
	}

	r.legacyWarned.Do(func() {
		if r.Logger != nil {
			r.Logger.Warn("push: S7-2 deprecation: keeper.yml::push.providers[] используется как fallback; мигрируйте на push_providers через POST /v1/push-providers",
				slog.String("trigger_plugin", pluginName))
		}
	})
	params, ok := r.Fallback.ResolveParams(pluginName)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPushProviderNotConfigured, pluginName)
	}
	return params, nil
}
