//go:build e2e

package harness

import (
	"strings"
	"testing"
)

// AllSoulIndexes — every soul-stub index, for the common "bind the whole stack
// to one incarnation" case of [Stack.CreateIncarnationOnRoster].
func (s *Stack) AllSoulIndexes() []int {
	idx := make([]int, len(s.souls))
	for i := range s.souls {
		idx[i] = i
	}
	return idx
}

// CreateIncarnationOnRoster bootstraps an incarnation onto an ALREADY-connected
// roster of soul-stubs and returns (incarnationName, applyID of the create run).
//
// THE BOOTSTRAP ORDER LIVES HERE (NIM-210) — one place, not per test:
//
//  1. seed the `incarnation` row (status='ready', spec/state empty) — direct SQL;
//  2. bind each soul-stub as a member (incarnation_membership);
//  3. run the create scenario as an ordinary explicit run.
//
// Why not POST /v1/incarnations. Since NIM-124 membership is a first-class
// relation guarded by FK incarnation_membership_incarnation_fk (migration 099),
// a host CANNOT be bound before the incarnation row exists. POST
// /v1/incarnations inserts that row AND starts the create run in the same call
// (lifecycle.auto_create), leaving no window in between — and a create run
// resolves its roster at run start, so an unbound roster aborts with `no_hosts`
// before dispatch (run.go §3). Neither "bind first" nor "create first" is
// reachable through that endpoint; seeding the row first is the only order that
// satisfies both constraints. It is the same direct-SQL escape the harness
// already uses for entry points unreachable through the API (see
// [Stack.SeedIncarnationReady]).
//
// Identical to the L3b helper of the same name
// (tests/e2e-live/harness/seed.go), deliberately: one bootstrap mechanic across
// both tiers, so an ordering regression is found once rather than twice. The
// only tier difference is step 3's precondition — L3b waits for each member's
// first SoulprintReport, whereas an L3a soul-stub reports nothing: the caller
// connects the stub with [Stack.ConnectSoulStub] (the roster counts connected
// hosts) and, for services whose render reads facts keeper-side, writes them
// with [Stack.SeedSoulprint] BEFORE calling this.
//
// Coverage note: this path does NOT exercise POST /v1/incarnations with a
// `create_scenario` (its input validation, create-plan resolution and the
// `incarnation.created` audit event). The bare create path — POST without a
// starting scenario — is covered by hello_world / noop / long_runner /
// coven_probe, and the 422 surface by [Stack.CreateIncarnationRaw]; the
// auto-started create run has no L3a coverage left, since every test that
// exercised it also needs a bound roster. Here create is an explicit run, so it
// writes `incarnation.scenario_started`, not `incarnation.created`.
//
// ★ That sentence was written while it was false. Those four tests had been
// failing at CreateIncarnation with 422 "not registered" since ADR-029, so the
// coverage this note leaned on to justify its own scope did not exist — nothing
// re-read the note against a run, because a red L3a and an unwritten L3a look
// the same from here. They were repaired in NIM-317 (and no longer need no
// roster: they bind one after create, which is why they can stay on the bare
// path). Before trusting a claim like this one, run the tests it names.
//
// serviceRef — `<service>@<ref>`; the ref is stored in
// incarnation.service_version for readability only (the run path resolves the
// service ref from the registry, incarnation_typed.go::RunTyped). createScenario
// must carry `create: true`; scenarios that compose their own name via
// id_template (ADR-0079) are NOT usable here — the name is fixed by the seed.
func (s *Stack) CreateIncarnationOnRoster(t *testing.T, name, serviceRef, createScenario string, soulIndexes []int, input map[string]any) (string, string) {
	t.Helper()
	if len(soulIndexes) == 0 {
		t.Fatalf("CreateIncarnationOnRoster(%s): no soul indexes — an empty roster aborts the create run with no_hosts", name)
	}
	s.SeedIncarnationReady(t, name, stripServiceRef(serviceRef), serviceRefVersion(serviceRef), map[string]any{})
	for _, idx := range soulIndexes {
		s.AddMember(t, idx, name)
	}
	return name, s.RunScenario(t, name, createScenario, input)
}

// serviceRefVersion — the `<ref>` half of `<service>@<ref>`, defaulting to the
// branch RegisterService publishes on. Only [Stack.CreateIncarnationOnRoster]
// needs it, to fill incarnation.service_version the way the create handler would.
func serviceRefVersion(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 && i+1 < len(ref) {
		return ref[i+1:]
	}
	return "main"
}
