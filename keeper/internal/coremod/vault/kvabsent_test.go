package vault_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	coremodvault "github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// absentVault models a KV mount as a flat set of mount-relative entry names and
// records every list and delete.
//
// Its LIST is a plain string-prefix match, the way an object store lists, not the
// folder semantics Vault has. That is deliberate: it is the backend on which a
// prefix without its trailing `/` reaches a sibling (`prod-1` → `prod-10/…`), so a
// step that forgot the boundary is caught here. Real Vault does not need it — LIST
// answers a folder's children, and vault/api drops the trailing `/` from the
// request anyway — but the walk builds every child path as `dir + name`, and that
// join is only inside the subtree while `dir` ends with `/`.
type absentVault struct {
	mount   string
	entries map[string]bool

	lists   []string
	deletes []string

	failDeleteOf string
	listOverride map[string][]string
}

func newAbsentVault(entries ...string) *absentVault {
	v := &absentVault{mount: "secret", entries: map[string]bool{}}
	for _, e := range entries {
		v.entries[e] = true
	}
	return v
}

func (v *absentVault) rel(p string) string { return strings.TrimPrefix(p, v.mount+"/") }

func (v *absentVault) ReadKV(context.Context, string) (map[string]any, error) {
	return nil, errors.New("kv-absent must not read secret values")
}

func (v *absentVault) WriteKV(context.Context, string, map[string]any) error {
	return errors.New("kv-absent must not write")
}

func (v *absentVault) ListKVEntries(_ context.Context, prefix string) ([]string, error) {
	v.lists = append(v.lists, prefix)
	if names, ok := v.listOverride[prefix]; ok {
		return names, nil
	}
	p := v.rel(prefix)
	seen := map[string]bool{}
	for e := range v.entries {
		rest, ok := strings.CutPrefix(e, p)
		if !ok || rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seen[rest[:i+1]] = true
		} else {
			seen[rest] = true
		}
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (v *absentVault) DeleteKV(_ context.Context, p string) (bool, error) {
	v.deletes = append(v.deletes, p)
	if p == v.failDeleteOf {
		return false, errors.New("permission denied")
	}
	r := v.rel(p)
	existed := v.entries[r]
	delete(v.entries, r)
	return existed, nil
}

func (v *absentVault) remaining() []string {
	out := []string{}
	for e := range v.entries {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// runCtx is the scope keeper_dispatch puts on a keeper task of the destroy run of
// incarnation prod-1 of service dragonfly.
func runCtx() context.Context {
	return destroyRun(util.WithService(util.WithIncarnation(context.Background(), "prod-1"), "dragonfly"))
}

func destroyRun(ctx context.Context) context.Context {
	return util.WithRunScope(ctx, util.RunScope{Scenario: "destroy", ApplyID: "01JDESTROY", Destroy: true})
}

func applyAbsent(t *testing.T, ctx context.Context, v *absentVault, fa *fakeAudit, params map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	m := coremodvault.New(v, fa)
	m.Mount = "secret"
	stream := internaltest.NewApplyStreamCtx(ctx)
	req := &pluginv1.ApplyRequest{State: coremodvault.StateAbsent, Params: mustStruct(t, params)}
	if err := m.Apply(req, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev == nil {
		t.Fatal("no final event")
	}
	return ev
}

var scopeIncarnation = map[string]any{"scope": "incarnation"}

func deletedOutput(t *testing.T, ev *pluginv1.ApplyEvent) []string {
	t.Helper()
	raw, ok := ev.GetOutput().AsMap()["deleted"].([]any)
	if !ok {
		t.Fatalf("output has no `deleted` list: %v", ev.GetOutput().AsMap())
	}
	out := make([]string, len(raw))
	for i, x := range raw {
		out[i] = x.(string)
	}
	return out
}

// fleet is a mount holding the incarnation's subtree and everything a wrong
// derivation would reach instead: the service root itself, a sibling incarnation
// whose name extends this one, another incarnation, another service, the Keeper's
// own keys.
func fleet() *absentVault {
	return newAbsentVault(
		"dragonfly/prod-1/admin_password",
		"dragonfly/prod-1/users/alice",
		"dragonfly/prod-1/users/bob",
		"dragonfly/prod-1/tls/cert",
		"dragonfly/prod-1/tls/key",
		"dragonfly/prod-1",
		"dragonfly/prod-10/users/alice",
		"dragonfly/prod-1-bar/users/alice",
		"dragonfly/prod-2/users/alice",
		"dragonfly/shared",
		"redis/prod-1/users/alice",
		"keeper/sigil-keys/k1",
	)
}

// TestAbsent_DeletesExactlyTheIncarnationSubtree — every entry under
// `<mount>/<service>/<incarnation>/` goes, permanently; nothing beside it: not the
// service root, not a sibling incarnation whose name shares the prefix (`prod-10`,
// `prod-1-bar`), not another service, not the Keeper's keys.
func TestAbsent_DeletesExactlyTheIncarnationSubtree(t *testing.T) {
	v := fleet()
	fa := &fakeAudit{}

	ev := applyAbsent(t, runCtx(), v, fa, scopeIncarnation)
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if !ev.GetChanged() {
		t.Error("changed=false although the subtree held entries")
	}
	want := []string{
		"secret/dragonfly/prod-1/admin_password",
		"secret/dragonfly/prod-1/tls/cert",
		"secret/dragonfly/prod-1/tls/key",
		"secret/dragonfly/prod-1/users/alice",
		"secret/dragonfly/prod-1/users/bob",
	}
	if got := deletedOutput(t, ev); !reflect.DeepEqual(got, want) {
		t.Errorf("deleted = %v, want %v", got, want)
	}
	wantLeft := []string{
		"dragonfly/prod-1",
		"dragonfly/prod-1-bar/users/alice",
		"dragonfly/prod-10/users/alice",
		"dragonfly/prod-2/users/alice",
		"dragonfly/shared",
		"keeper/sigil-keys/k1",
		"redis/prod-1/users/alice",
	}
	if got := v.remaining(); !reflect.DeepEqual(got, wantLeft) {
		t.Errorf("remaining = %v, want %v", got, wantLeft)
	}
	if len(fa.events) != 1 || fa.events[0].EventType != audit.EventVaultKVAbsent {
		t.Fatalf("audit events = %+v, want one vault.kv-absent", fa.events)
	}
	p := fa.events[0].Payload
	if len(p["paths"].([]any)) != len(want) || p["service"] != "dragonfly" || p["incarnation"] != "prod-1" {
		t.Errorf("audit payload = %v", p)
	}
}

// TestAbsent_ListsTheIncarnationWithItsBoundary — the walk starts at exactly
// `<mount>/<service>/<incarnation>/`: never the service root, and never the
// incarnation name as a bare prefix.
func TestAbsent_ListsTheIncarnationWithItsBoundary(t *testing.T) {
	v := fleet()
	applyAbsent(t, runCtx(), v, &fakeAudit{}, scopeIncarnation)
	if len(v.lists) == 0 || v.lists[0] != "secret/dragonfly/prod-1/" {
		t.Fatalf("first list = %v, want exactly secret/dragonfly/prod-1/", v.lists)
	}
	for _, l := range v.lists {
		if !strings.HasPrefix(l, "secret/dragonfly/prod-1/") || !strings.HasSuffix(l, "/") {
			t.Errorf("list %q is outside the subtree or lacks its trailing /", l)
		}
	}
}

// TestAbsent_EmptySubtreeIsNoOp — nothing under the incarnation: changed=false,
// nothing deleted, nothing audited. A destroy re-run lands here.
func TestAbsent_EmptySubtreeIsNoOp(t *testing.T) {
	v := newAbsentVault("dragonfly/prod-2/users/alice", "dragonfly/prod-10/x")
	fa := &fakeAudit{}
	ev := applyAbsent(t, runCtx(), v, fa, scopeIncarnation)
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if ev.GetChanged() {
		t.Error("changed=true over an empty subtree")
	}
	if got := deletedOutput(t, ev); len(got) != 0 {
		t.Errorf("deleted = %v, want empty", got)
	}
	if len(v.deletes) != 0 || len(fa.events) != 0 {
		t.Errorf("deletes = %v, audit = %d; want none", v.deletes, len(fa.events))
	}
}

// TestAbsent_RerunIsIdempotent — the second run of the same step finds the subtree
// empty and succeeds without change.
func TestAbsent_RerunIsIdempotent(t *testing.T) {
	v := fleet()
	if first := applyAbsent(t, runCtx(), v, &fakeAudit{}, scopeIncarnation); first.GetFailed() || !first.GetChanged() {
		t.Fatalf("first run: failed=%v changed=%v msg=%s", first.GetFailed(), first.GetChanged(), first.GetMessage())
	}
	fa := &fakeAudit{}
	second := applyAbsent(t, runCtx(), v, fa, scopeIncarnation)
	if second.GetFailed() || second.GetChanged() {
		t.Fatalf("second run: failed=%v changed=%v msg=%s", second.GetFailed(), second.GetChanged(), second.GetMessage())
	}
	if len(fa.events) != 0 {
		t.Errorf("second run wrote audit: %+v", fa.events)
	}
}

// TestAbsent_NoOwnerFailsClosed — without the run's service or incarnation on the
// context the namespace cannot be derived, and Vault is not asked anything.
func TestAbsent_NoOwnerFailsClosed(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"neither":        destroyRun(context.Background()),
		"no incarnation": destroyRun(util.WithService(context.Background(), "dragonfly")),
		"no service":     destroyRun(util.WithIncarnation(context.Background(), "prod-1")),
	} {
		t.Run(name, func(t *testing.T) {
			v := fleet()
			ev := applyAbsent(t, ctx, v, &fakeAudit{}, scopeIncarnation)
			if !ev.GetFailed() {
				t.Fatal("kv-absent ran without a run owner")
			}
			if len(v.lists) != 0 || len(v.deletes) != 0 {
				t.Errorf("reached Vault without an owner: lists=%v deletes=%v", v.lists, v.deletes)
			}
		})
	}
}

// TestAbsent_UnsafeOwnerSegmentFailsClosed — an incarnation or service name that
// could move the delete (`..`, a `/`) or a reserved service is refused before Vault
// is asked anything.
func TestAbsent_UnsafeOwnerSegmentFailsClosed(t *testing.T) {
	for _, c := range []struct{ service, incarnation string }{
		{"dragonfly", ".."},
		{"dragonfly", "prod-1/../prod-2"},
		{"..", "prod-1"},
		{"keeper", "sigil-keys"},
	} {
		v := fleet()
		ctx := destroyRun(util.WithService(util.WithIncarnation(context.Background(), c.incarnation), c.service))
		ev := applyAbsent(t, ctx, v, &fakeAudit{}, scopeIncarnation)
		if !ev.GetFailed() {
			t.Errorf("service=%q incarnation=%q was accepted", c.service, c.incarnation)
		}
		if len(v.lists) != 0 || len(v.deletes) != 0 {
			t.Errorf("service=%q incarnation=%q reached Vault: lists=%v deletes=%v", c.service, c.incarnation, v.lists, v.deletes)
		}
	}
}

// TestAbsent_RefusedOutsideTheDestroyRun — in any run but the incarnation's
// destroy (an `update`, a scenario merely NAMED destroy, a run whose kind is
// unknown) the step refuses before Vault is asked anything: otherwise
// `incarnation.run` could delete every declared secret of a live incarnation.
func TestAbsent_RefusedOutsideTheDestroyRun(t *testing.T) {
	owner := util.WithService(util.WithIncarnation(context.Background(), "prod-1"), "dragonfly")
	for name, ctx := range map[string]context.Context{
		"update run": util.WithRunScope(owner, util.RunScope{Scenario: "update", ApplyID: "01JAPPLY"}),
		"scenario named destroy, not the destroy run": util.WithRunScope(owner, util.RunScope{Scenario: "destroy", ApplyID: "01JAPPLY"}),
		"run kind unknown":                            owner,
	} {
		t.Run(name, func(t *testing.T) {
			v := fleet()
			before := v.remaining()
			ev := applyAbsent(t, ctx, v, &fakeAudit{}, scopeIncarnation)
			if !ev.GetFailed() || !strings.Contains(ev.GetMessage(), "destroy run") {
				t.Fatalf("failed=%v msg=%q, want a refusal naming the destroy run", ev.GetFailed(), ev.GetMessage())
			}
			if len(v.lists) != 0 || len(v.deletes) != 0 {
				t.Errorf("reached Vault outside destroy: lists=%v deletes=%v", v.lists, v.deletes)
			}
			if got := v.remaining(); !reflect.DeepEqual(got, before) {
				t.Errorf("entries changed: %v -> %v", before, got)
			}
		})
	}
}

// TestAbsent_ListedNameCannotLeadOut — a name Vault lists is trusted as one segment
// only; a listed `..` stops the walk before any delete.
func TestAbsent_ListedNameCannotLeadOut(t *testing.T) {
	v := fleet()
	v.listOverride = map[string][]string{"secret/dragonfly/prod-1/": {"admin_password", "../"}}
	ev := applyAbsent(t, runCtx(), v, &fakeAudit{}, scopeIncarnation)
	if !ev.GetFailed() {
		t.Fatal("a listed `..` was walked")
	}
	if len(v.deletes) != 0 {
		t.Errorf("deleted after a bad listing: %v", v.deletes)
	}
}

// TestAbsent_FailureMidwayNamesWhatIsGone — a delete failing after others succeeded
// fails the step, names what is already gone, and audits it.
func TestAbsent_FailureMidwayNamesWhatIsGone(t *testing.T) {
	v := newAbsentVault("dragonfly/prod-1/a", "dragonfly/prod-1/b")
	v.failDeleteOf = "secret/dragonfly/prod-1/b"
	fa := &fakeAudit{}
	ev := applyAbsent(t, runCtx(), v, fa, scopeIncarnation)
	if !ev.GetFailed() {
		t.Fatal("a failed delete did not fail the step")
	}
	if !strings.Contains(ev.GetMessage(), "already deleted: secret/dragonfly/prod-1/a") {
		t.Errorf("message does not name what is gone: %s", ev.GetMessage())
	}
	if len(fa.events) != 1 || !reflect.DeepEqual(fa.events[0].Payload["paths"], []any{"secret/dragonfly/prod-1/a"}) {
		t.Errorf("audit = %+v, want the one deleted path", fa.events)
	}
}

// TestAbsent_ScopeIsTheOnlyParam — `scope: incarnation` is required and is the only
// thing the step accepts; in particular no path, so no author spelling reaches it.
func TestAbsent_ScopeIsTheOnlyParam(t *testing.T) {
	m := coremodvault.New(newAbsentVault(), nil)
	bad := map[string]map[string]any{
		"no scope":      {},
		"other scope":   {"scope": "service"},
		"scope not str": {"scope": true},
		"a path":        {"scope": "incarnation", "path": "secret/dragonfly/prod-1"},
		"targets":       {"scope": "incarnation", "targets": []any{map[string]any{"path": "secret/redis"}}},
	}
	for name, params := range bad {
		t.Run(name, func(t *testing.T) {
			rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{State: coremodvault.StateAbsent, Params: mustStruct(t, params)})
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if rep.GetOk() {
				t.Errorf("Validate accepted %v", params)
			}
			v := fleet()
			if ev := applyAbsent(t, runCtx(), v, &fakeAudit{}, params); !ev.GetFailed() || len(v.deletes) != 0 {
				t.Errorf("Apply with %v: failed=%v deletes=%v", params, ev.GetFailed(), v.deletes)
			}
		})
	}
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{State: coremodvault.StateAbsent, Params: mustStruct(t, scopeIncarnation)})
	if !rep.GetOk() {
		t.Errorf("Validate refused scope: incarnation: %v", rep.GetErrors())
	}
}
