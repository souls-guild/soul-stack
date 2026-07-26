package rbac

import (
	"errors"
	"strings"
	"testing"
)

// The `soul.console` guards (ADR-0074, rbac.md §Console). An interactive pty is
// the most privileged thing an operator can ask for — the pty inherits the Soul
// daemon's user and its commands cannot be checked against a module allow-list —
// so the boundaries of the right are pinned by tests, not by prose alone.

// TestCatalog_ConsolePermission — the right is admitted by the catalog, so a
// role carrying it loads instead of failing with unknown_permission.
func TestCatalog_ConsolePermission(t *testing.T) {
	if !IsAllowedPermission("soul", "console") {
		t.Fatal("soul.console missing from catalog")
	}
	if _, err := ParsePermission("soul.console"); err != nil {
		t.Errorf("ParsePermission(soul.console): %v", err)
	}
	if _, err := ParsePermission("soul.console on host=web-01.example.com"); err != nil {
		t.Errorf("ParsePermission(soul.console on host=…): %v", err)
	}

	// The catalog — not the grammar — is what admits the name: a near-miss is
	// still rejected, so the guard above proves catalog membership.
	_, err := ParsePermission("soul.consoles")
	if err == nil {
		t.Fatal("soul.consoles parsed; a name outside the catalog must be rejected")
	}
	if !strings.Contains(err.Error(), "unknown_permission") {
		t.Errorf("soul.consoles rejected with %v, want unknown_permission", err)
	}
}

// TestEnforcer_ConsoleHostScope — `soul.console on host=<sid>` opens a console on
// THAT host and on no other. The core scope guard of the right.
func TestEnforcer_ConsoleHostScope(t *testing.T) {
	e, err := NewEnforcerFromSnapshot(snapshotOf(fixtureRole{
		name:        "web-console",
		operators:   []string{"archon-op"},
		permissions: []string{"soul.console on host=web-01.example.com"},
	}))
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}

	if err := e.Check("archon-op", "soul", "console", map[string]string{"host": "web-01.example.com"}); err != nil {
		t.Errorf("console on the scoped host: %v, want allow", err)
	}
	if err := e.Check("archon-op", "soul", "console", map[string]string{"host": "db-01.example.com"}); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("console on a host outside scope: %v, want ErrPermissionDenied", err)
	}
}

// TestEnforcer_ConsoleIsNotErrandRun — the two rights are independent in BOTH
// directions. `errand.run` must never hand out a shell (a console is strictly
// stronger: an Errand is one named module call, a console is an arbitrary
// interactive shell), and holding a console must not imply the Errand API.
func TestEnforcer_ConsoleIsNotErrandRun(t *testing.T) {
	e, err := NewEnforcerFromSnapshot(
		snapshotOf(
			fixtureRole{name: "errand-only", operators: []string{"archon-errand"}, permissions: []string{"errand.run"}},
			fixtureRole{name: "console-only", operators: []string{"archon-console"}, permissions: []string{"soul.console"}},
		))
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}

	if err := e.Check("archon-errand", "soul", "console", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("errand.run granted soul.console: %v — a console is strictly stronger and must be granted explicitly", err)
	}
	if err := e.Check("archon-console", "errand", "run", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("soul.console granted errand.run: %v — the rights are independent", err)
	}
}

// TestEnforcer_ConsoleSocketGateIsExistenceOnly — the WebSocket `/v1/console`
// upgrade carries NO target host (the SID arrives later, in the `open` frame),
// so its gate must be the existence-gate [Enforcer.HoldsAction] and NOT a
// scope-aware Check with a nil context: an absent dimension fails closed, so
// Check(nil) denies exactly the host-scoped roles the feature exists to serve
// (ADR-047 §g G1). This test pins both halves of that asymmetry.
func TestEnforcer_ConsoleSocketGateIsExistenceOnly(t *testing.T) {
	e, err := NewEnforcerFromSnapshot(snapshotOf(fixtureRole{
		name:        "web-console",
		operators:   []string{"archon-op"},
		permissions: []string{"soul.console on host=web-01.example.com"},
	}))
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}

	if !e.HoldsAction("archon-op", "soul", "console") {
		t.Error("HoldsAction denied a host-scoped console role — the socket gate would 403 before the operator can name a host")
	}
	if err := e.Check("archon-op", "soul", "console", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("Check(nil) on a host-scoped role: %v, want ErrPermissionDenied — this is why the socket gate cannot be scope-aware", err)
	}

	// An Archon holding nothing must not pass the existence-gate either.
	if e.HoldsAction("archon-nobody", "soul", "console") {
		t.Error("HoldsAction admitted an Archon with no console right")
	}
}

// TestEnforcer_ConsoleCoveredByResourceWildcard — `soul.*` covers `soul.console`,
// like every other action of the resource. Recorded deliberately: this is a
// widening for roles that already hold `soul.*`, and the same consequence
// `incarnation.*` had when `incarnation.view-secrets` landed (ADR-0070).
// Operators who must not hand out shells enumerate actions instead of `soul.*`.
func TestEnforcer_ConsoleCoveredByResourceWildcard(t *testing.T) {
	e, err := NewEnforcerFromSnapshot(snapshotOf(fixtureRole{
		name: "soul-admin", operators: []string{"archon-wild"}, permissions: []string{"soul.* on coven=prod"},
	}))
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-wild", "soul", "console", map[string]string{"coven": "prod"}); err != nil {
		t.Errorf("soul.* did not cover soul.console: %v", err)
	}
	if err := e.Check("archon-wild", "soul", "console", map[string]string{"coven": "dev"}); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("soul.* on coven=prod reached coven=dev: %v", err)
	}
}
