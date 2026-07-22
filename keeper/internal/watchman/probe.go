package watchman

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
)

// NamedPinger is a named dependency for probing. Name appears in error
// messages so operators see which dependency failed (postgres / redis).
type NamedPinger struct {
	Name   string
	Pinger health.Pinger
}

// depsProbe implements [HealthProbe] over a set of `health.Pinger`s.
// Same dependency contract as `/readyz` (PG + Redis required to serve requests):
// an instance that lost them is isolated. nil-Pinger is skipped
// (symmetric with health.Readyz: Redis can be off in dev — its absence then
// does not count as isolation).
//
// Probe runs SEQUENTIALLY and short-circuits on first error: Watchman only needs
// "at least one dependency unreachable", not the full list of failures (as in
// `/readyz` JSON) — this saves a ping on the second resource after the first fails.
type depsProbe struct {
	pingers []NamedPinger
}

// NewDepsProbe constructs [HealthProbe] from named Pingers (usually PG +
// Redis, same as `/readyz`). nil-Pingers are filtered out. If none remain,
// returns [ErrNoProbeDeps] (Watchman without dependencies is pointless).
func NewDepsProbe(pingers ...NamedPinger) (HealthProbe, error) {
	live := make([]NamedPinger, 0, len(pingers))
	for _, p := range pingers {
		if p.Pinger != nil {
			live = append(live, p)
		}
	}
	if len(live) == 0 {
		return nil, ErrNoProbeDeps
	}
	return &depsProbe{pingers: live}, nil
}

// Probe pings dependencies sequentially, returning first error (with dependency
// name). nil means all healthy. ctx already carries per-tick timeout from Watchman.
func (p *depsProbe) Probe(ctx context.Context) error {
	for _, np := range p.pingers {
		if err := np.Pinger.Ping(ctx); err != nil {
			return fmt.Errorf("%s: %w", np.Name, err)
		}
	}
	return nil
}
