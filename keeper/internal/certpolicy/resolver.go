// Package certpolicy resolves the effective TLS-cert auto-rotation policy of an
// incarnation (NIM-99): reads incarnation -> its pinned service snapshot -> the
// manifest's `certificate_rotation:` section. Common input for the reaper (who to
// rotate) and the UI/coremod (whether the service's rotator is visible and enabled).
package certpolicy

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Policy — the effective cert-rotation policy of an incarnation. Present — whether
// the manifest has a `certificate_rotation:` section; Enabled — whether it's on
// (enable:true). KnownScenarios — the scenario/ names of the snapshot (for
// validating Scenario by the resolver/UI).
type Policy struct {
	Service        string
	Present        bool
	Enabled        bool
	Scenario       string
	PKIRole        string
	Threshold      time.Duration
	KnownScenarios []string
}

// IncarnationReader — the read surface of incarnation for [incarnation.SelectByName].
// Matches [incarnation.ExecQueryRower] (that one requires Exec/QueryRow/Query, not
// just QueryRow); production supplies pgxpool.Pool, tests — a fake.
type IncarnationReader interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ServiceRefResolver resolves the git coordinates of a service by name.
// [scenario.ServiceRegistry] satisfies it.
type ServiceRefResolver interface {
	Resolve(service string) (artifact.ServiceRef, bool)
}

// PolicyLister lists the cert-policy of a service snapshot.
// [serviceregistry.CertPolicyCache] satisfies it.
type PolicyLister interface {
	ListCertPolicy(ctx context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error)
}

// Resolver assembles [Policy] from the incarnation DB + its pinned version's snapshot.
type Resolver struct {
	db       IncarnationReader
	services ServiceRefResolver
	lister   PolicyLister
}

// NewResolver builds a resolver on top of the DB, service registry, and cert-policy cache.
func NewResolver(db IncarnationReader, services ServiceRefResolver, lister PolicyLister) *Resolver {
	return &Resolver{db: db, services: services, lister: lister}
}

// Resolve returns the cert-rotation policy of incarnationName.
//
// SelectByName -> services.Resolve -> pin ref to inc.ServiceVersion (not the
// registry ref) -> ListCertPolicy. No section (Rotation==nil) -> Present/Enabled
// false, no error. A Threshold parse error is swallowed as 0 (the threshold is
// currently informational, not critical).
func (r *Resolver) Resolve(ctx context.Context, incarnationName string) (Policy, error) {
	inc, err := incarnation.SelectByName(ctx, r.db, incarnationName)
	if err != nil {
		return Policy{}, fmt.Errorf("certpolicy: load incarnation %q: %w", incarnationName, err)
	}

	ref, ok := r.services.Resolve(inc.Service)
	if !ok {
		return Policy{}, fmt.Errorf("certpolicy: service %q not registered", inc.Service)
	}
	ref.Ref = inc.ServiceVersion // pinned incarnation version, NOT the registry ref

	info, err := r.lister.ListCertPolicy(ctx, inc.Service, ref.Git, ref.Ref)
	if err != nil {
		return Policy{}, fmt.Errorf("certpolicy: list cert policy for %q: %w", inc.Service, err)
	}

	p := Policy{Service: inc.Service, KnownScenarios: info.Scenarios}
	if info.Rotation == nil {
		return p, nil
	}
	p.Present = true
	p.Enabled = info.Rotation.Enable
	p.Scenario = info.Rotation.Scenario
	p.PKIRole = info.Rotation.PKIRole
	if info.Rotation.Threshold != "" {
		if d, perr := config.ParseDuration(info.Rotation.Threshold); perr == nil {
			p.Threshold = d
		}
	}
	return p, nil
}
