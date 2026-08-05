package push

// router.go — a 3-tier resolver for selecting the SshProvider plugin by SID
// (ADR-032 amendment 2026-05-27, P2 W-3 Multi-provider routing).
//
// Context. Before P2, the single-provider pilot held exactly one
// SshProvider plugin per keeper: the operator configured either
// `vault-bastion` or `static`, not both at once. P2 introduces a map of
// providers (W-2) and a resolver that picks one per SID — the operator can
// run several SshProviders at once and route SIDs between them (smoke a
// prod env through `static`, prod through `vault-bastion`).
//
// Selector R1 (architect-decisions 2026-05-27, 3-tier resolve):
//
//	Level 1: souls.ssh_target.ssh_provider    (per-SID explicit)
//	Level 2: push.coven_default_providers     (per-coven default)
//	Level 3: push.cluster_default_provider    (cluster fallback)
//
// Level 2 reads the host's OWN `souls.coven[]` and nothing else (NIM-281). A
// coven tag exists only where an operator attached it; belonging to an
// incarnation attaches none, so `coven_default_providers: {redis-prod: …}`
// routes the hosts an operator tagged `redis-prod`, not the members of the
// incarnation of that name. Routing a whole incarnation behind one bastion is
// done by tagging its hosts — the same explicit act as routing any other group.
//
// Tiebreak on multiple coven matches (a Soul in several covens, each configured
// with its own provider): alphabetical order of the labels (deterministic).
//
// All three levels empty → ErrProviderNotRouted → fail per-host. NO
// provider-chain fallback: different providers have different auth
// perimeters, and a silent fallback would break the trust invariant
// (security-first).

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// ProviderRouter resolves the SshProvider plugin name for a given SID.
// Used in pushorch.PushRun.executeAsync before the dispatch phase.
//
// Narrowed to a single method so unit tests can fake it without a live PG.
type ProviderRouter interface {
	RouteFor(ctx context.Context, sid string) (providerName string, source RouteSource, err error)
}

// RouteSource is which of the three resolve levels produced the answer.
// Carried in the audit summary (`push_runs.summary.hosts[sid].route_source`)
// and in the Prometheus counter
// `keeper_push_provider_routed_total{provider, decision_source}` (low
// cardinality: per-provider × 3 labels = a handful of series).
type RouteSource int

const (
	// SourceUnknown is the zero value (a programming error, not a valid
	// resolve path). Used only as a defensive default.
	SourceUnknown RouteSource = 0
	// SourceSoul is Level 1, per-SID explicit (`souls.ssh_target.ssh_provider`).
	SourceSoul RouteSource = iota
	// SourceCoven is Level 2, per-coven default
	// (`push.coven_default_providers[<coven>]`).
	SourceCoven
	// SourceCluster is Level 3, cluster fallback (`push.cluster_default_provider`).
	SourceCluster
)

// String returns a kebab-case label for logs / audit payloads.
func (s RouteSource) String() string {
	switch s {
	case SourceSoul:
		return "soul"
	case SourceCoven:
		return "coven"
	case SourceCluster:
		return "cluster"
	default:
		return "unknown"
	}
}

// ErrProviderNotRouted is a sentinel: neither Level 1, Level 2, nor Level 3
// produced an SshProvider name. The caller (pushorch) maps this to a
// per-host status="error" + error_code="provider_not_routed".
var ErrProviderNotRouted = errors.New("push: SshProvider not routed (no per-SID / per-coven / cluster default)")

// PGRouterReader is the narrow surface over the soul.* storage layer for the
// PG resolve of `ssh_target.ssh_provider` and a Soul's list of `coven`
// labels. Narrowed for the router (TargetReader from target_pg.go targets the
// full SSHTarget, but the router needs only two fields — split out so unit
// tests don't have to carry the extra baggage).
type PGRouterReader interface {
	// SelectSshTarget reads `ssh_target.ssh_provider` (Level 1).
	SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error)
	// SelectCovens reads a Soul's own `souls.coven[]` for the Level 2 lookup
	// (NIM-281: the labels an operator attached to the host, and no others).
	// May be empty.
	SelectCovens(ctx context.Context, sid string) ([]string, error)
}

// RouterConfig is a read-only snapshot of the cluster-defaults config. Passed
// as a snapshot function so hot-reload (config.Store.OnReload) can swap the
// map without recreating the PGRouter: routing decisions take a fresh
// snapshot on every RouteFor call.
//
// CovenDefaultProviders maps coven name → provider name.
// ClusterDefaultProvider is the fallback when there's no match.
type RouterConfig struct {
	CovenDefaultProviders  map[string]string
	ClusterDefaultProvider string
}

// RouterConfigSource is the source of a fresh config snapshot. Implemented
// by daemon wire-up as a wrapper over config.Store. A nil pointer is
// dangerous (a router without config effectively only has Level 1 / fail) —
// the caller must pass a non-nil value.
type RouterConfigSource interface {
	Snapshot() RouterConfig
}

// staticRouterConfig is a static snapshot for unit tests and a one-off init
// snapshot when hot-reload isn't in play.
type staticRouterConfig struct {
	cfg RouterConfig
}

// NewStaticRouterConfigSource is a wrapper for tests.
func NewStaticRouterConfigSource(cfg RouterConfig) RouterConfigSource {
	return &staticRouterConfig{cfg: cfg}
}

func (s *staticRouterConfig) Snapshot() RouterConfig { return s.cfg }

// PGRouter is the production implementation of [ProviderRouter] over the
// soul.* storage layer and a snapshot of cluster-defaults config.
//
// RouteFor algorithm:
//
//  1. SELECT souls.ssh_target.ssh_provider → if non-empty → SourceSoul.
//  2. SELECT the host's own coven[] → look up CovenDefaultProviders over it
//     (alphabetical) → first match → SourceCoven.
//  3. ClusterDefaultProvider non-empty → SourceCluster.
//  4. Otherwise ErrProviderNotRouted.
type PGRouter struct {
	Reader PGRouterReader
	Config RouterConfigSource
}

// NewPGRouter validates dependencies and returns a router.
func NewPGRouter(reader PGRouterReader, cfg RouterConfigSource) (*PGRouter, error) {
	if reader == nil {
		return nil, errors.New("push: PGRouter requires Reader")
	}
	if cfg == nil {
		return nil, errors.New("push: PGRouter requires Config")
	}
	return &PGRouter{Reader: reader, Config: cfg}, nil
}

// RouteFor implements [ProviderRouter].
func (r *PGRouter) RouteFor(ctx context.Context, sid string) (string, RouteSource, error) {
	// Level 1: per-SID explicit. We propagate soul.ErrSoulNotFound — this is
	// an off-path case (the caller has usually already validated the Soul
	// row), but it's not our job to mask it.
	target, err := r.Reader.SelectSshTarget(ctx, sid)
	if err != nil {
		return "", SourceUnknown, fmt.Errorf("router: select ssh_target %s: %w", sid, err)
	}
	if target != nil && target.SSHProvider != nil && *target.SSHProvider != "" {
		return *target.SSHProvider, SourceSoul, nil
	}

	cfg := r.Config.Snapshot()

	// Level 2: per-coven default over the host's own tags, alphabetical (see the
	// tiebreak note at the top of this file). A read failure is propagated rather
	// than swallowed: falling through to Level 3 on a transient PG error would
	// move the host onto a different provider — a silent change of auth
	// perimeter.
	if len(cfg.CovenDefaultProviders) > 0 {
		covens, err := r.Reader.SelectCovens(ctx, sid)
		if err != nil {
			return "", SourceUnknown, fmt.Errorf("router: select covens %s: %w", sid, err)
		}
		if provider, ok := firstConfiguredProvider(covens, cfg.CovenDefaultProviders); ok {
			return provider, SourceCoven, nil
		}
	}

	// Level 3: cluster fallback.
	if cfg.ClusterDefaultProvider != "" {
		return cfg.ClusterDefaultProvider, SourceCluster, nil
	}

	return "", SourceUnknown, fmt.Errorf("%w: sid=%s", ErrProviderNotRouted, sid)
}

// firstConfiguredProvider returns the provider configured for the
// alphabetically first coven that has one. A local sort on a short slice (a Soul
// is usually in 1-3 covens) — the caller's slice is left alone, since it belongs
// to the reader.
func firstConfiguredProvider(covens []string, defaults map[string]string) (string, bool) {
	if len(covens) == 0 {
		return "", false
	}
	sorted := make([]string, len(covens))
	copy(sorted, covens)
	sort.Strings(sorted)
	for _, c := range sorted {
		if provider, ok := defaults[c]; ok && provider != "" {
			return provider, true
		}
	}
	return "", false
}
