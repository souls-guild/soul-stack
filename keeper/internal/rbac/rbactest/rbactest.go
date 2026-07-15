// Package rbactest provides RBAC test fixtures for the keeper packages (api /
// mcp / middleware / handlers / rbac). After the config-RBAC hard cut
// (ADR-028(g)), the only source of an enforcer is a DB snapshot
// [rbac.Snapshot]; tests that used to rely on `config.KeeperRBAC` now build
// an equivalent snapshot by hand through this helper.
//
// The [Config]/[Role] shape mirrors the old config-RBAC
// (default_policy + roles[].{name,operators,permissions}) — same permission
// cases, just sourced from a Snapshot instead of keeper.yml. Lives in a
// regular (non-`_test.go`) file because the fixture is shared across several
// test packages.
package rbactest

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// Config is the test-form RBAC catalog: a flat list of roles with bindings.
// Analogous to the removed `config.KeeperRBAC`; `DefaultPolicy` is kept for
// fixture readability (deny is the only MVP mode) and has no effect on the
// built Snapshot.
//
// Revoked is a fixture for ADR-014 Amendment 2026-05-27 (JWT immediate
// revoke): AID → revoked_at. Independent of Roles/Operators — an operator
// can be in both Membership and Revoked at once (models "fired, but still
// has an active role in the catalog").
type Config struct {
	DefaultPolicy string
	Roles         []Role
	Revoked       map[string]time.Time
}

// Role is a single fixture role: name, bound AIDs, raw permission strings.
type Role struct {
	Name        string
	Operators   []string
	Permissions []string
}

// Snapshot builds an [rbac.Snapshot] from the test config: role → permissions
// (Roles) and AID → roles (Membership). A nil config yields an empty snapshot
// (default deny), same as a nil snapshot in [rbac.NewEnforcerFromSnapshot].
func Snapshot(cfg *Config) *rbac.Snapshot {
	if cfg == nil {
		return nil
	}
	snap := &rbac.Snapshot{
		Roles:      make(map[string][]string, len(cfg.Roles)),
		Membership: make(map[string][]string),
		Revoked:    make(map[string]time.Time, len(cfg.Revoked)),
	}
	for _, r := range cfg.Roles {
		snap.Roles[r.Name] = r.Permissions
		for _, aid := range r.Operators {
			snap.Membership[aid] = append(snap.Membership[aid], r.Name)
		}
	}
	for aid, at := range cfg.Revoked {
		snap.Revoked[aid] = at
	}
	return snap
}

// NewEnforcer builds an [rbac.Enforcer] from the test config via the DB
// snapshot path. Returns permission-string parse errors (the same
// [rbac.ParsePermission] as the prod path) — tests for "unknown permission →
// fatal" rely on this.
func NewEnforcer(cfg *Config) (*rbac.Enforcer, error) {
	return rbac.NewEnforcerFromSnapshot(Snapshot(cfg))
}

// MustEnforcer is NewEnforcer with t.Fatalf on error. Convenient for most
// fixtures, where an invalid permission is a test bug, not a case under test.
func MustEnforcer(t *testing.T, cfg *Config) *rbac.Enforcer {
	t.Helper()
	e, err := NewEnforcer(cfg)
	if err != nil {
		t.Fatalf("rbactest.NewEnforcer: %v", err)
	}
	return e
}
