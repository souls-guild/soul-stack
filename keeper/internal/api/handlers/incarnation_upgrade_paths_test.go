package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
)

// --- Upgrade-paths (ADR-0068 §6) --------------------------------------

func alwaysInScope(*incarnation.Incarnation) bool { return true }
func neverInScope(*incarnation.Incarnation) bool  { return false }

// newUpPathsHandler builds a handler for upgrade-paths: db + resolver(ok) + loader,
// refs — late-binding via SetServiceRefs (nil → not wired, cheap 500 mode).
func newUpPathsHandler(db *fakeIncDB, loader *fakeLoader, refs ServiceRefsLister) *IncarnationHandler {
	h := NewIncarnationHandler(db, nil, nil, nil, &fakeResolver{ok: true}, loader, nil, nil, nil)
	if refs != nil {
		h.SetServiceRefs(refs)
	}
	return h
}

// wantUpPathsProblem checks that err is a problem with the expected status/type.
func wantUpPathsProblem(t *testing.T, err error, status int, typ string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected problem error, got nil")
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("error is not a problem: %v", err)
	}
	if d.Status != status {
		t.Errorf("status = %d, want %d (type %q, detail %q)", d.Status, status, d.Type, d.Detail)
	}
	if d.Type != typ {
		t.Errorf("type = %q, want %q", d.Type, typ)
	}
}

func upPathsDB() *fakeIncDB {
	return &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncRowVer(name, "v1", 1) }}
}

// TestUpgradePaths_Cheap_ListsRefs_IsCurrent — cheap mode (no ?to=): returns the
// service registry tags; the current pin is marked is_current, the rest are not. Target empty.
func TestUpgradePaths_Cheap_ListsRefs_IsCurrent(t *testing.T) {
	refs := &fakeRefsLister{refs: []artifact.GitRef{
		{Name: "v1", Type: artifact.GitRefTypeTag, Commit: "aaa"},
		{Name: "v2", Type: artifact.GitRefTypeTag, Commit: "bbb"},
		{Name: "main", Type: artifact.GitRefTypeBranch, Commit: "ccc", IsDefault: true},
	}}
	loader := &fakeLoader{}
	h := newUpPathsHandler(upPathsDB(), loader, refs)

	view, err := h.UpgradePathsTyped(context.Background(), "redis-prod", "", alwaysInScope)
	if err != nil {
		t.Fatalf("UpgradePathsTyped: %v", err)
	}
	if view.CurrentVersion != "v1" || view.CurrentStateSchemaVersion != 1 {
		t.Errorf("current = %q/%d, want v1/1", view.CurrentVersion, view.CurrentStateSchemaVersion)
	}
	if view.Target != nil {
		t.Errorf("Target = %+v, want nil in cheap mode", view.Target)
	}
	if len(view.Paths) != 3 {
		t.Fatalf("Paths len = %d, want 3", len(view.Paths))
	}
	byRef := map[string]UpgradePathRefView{}
	for _, p := range view.Paths {
		byRef[p.Ref] = p
	}
	if !byRef["v1"].IsCurrent {
		t.Errorf("v1.IsCurrent = false, want true (current pin)")
	}
	if byRef["v2"].IsCurrent || byRef["main"].IsCurrent {
		t.Errorf("non-current refs marked is_current: v2=%v main=%v", byRef["v2"].IsCurrent, byRef["main"].IsCurrent)
	}
	if byRef["main"].Type != artifact.GitRefTypeBranch {
		t.Errorf("main.Type = %q, want branch", byRef["main"].Type)
	}
	// Cheap mode does not load snapshots (ADR-0068 §6: only ls-remote).
	if loader.loadCalls != 0 {
		t.Errorf("loadCalls = %d, want 0 (cheap mode does not load a snapshot)", loader.loadCalls)
	}
	if refs.called != 1 {
		t.Errorf("refs.called = %d, want 1", refs.called)
	}
}

// TestUpgradePaths_Cheap_NoRefsLister_500 — refs-lister not wired → 500.
func TestUpgradePaths_Cheap_NoRefsLister_500(t *testing.T) {
	h := newUpPathsHandler(upPathsDB(), &fakeLoader{}, nil) // refs nil
	_, err := h.UpgradePathsTyped(context.Background(), "redis-prod", "", alwaysInScope)
	wantUpPathsProblem(t, err, http.StatusInternalServerError, problem.TypeInternalError)
}

// TestUpgradePaths_Cheap_LsRemoteFail_502 — ls-remote of the git source failed → 502
// (like ListRefsTyped: keeper is healthy, the external git is not).
func TestUpgradePaths_Cheap_LsRemoteFail_502(t *testing.T) {
	refs := &fakeRefsLister{err: context.DeadlineExceeded}
	h := newUpPathsHandler(upPathsDB(), &fakeLoader{}, refs)
	_, err := h.UpgradePathsTyped(context.Background(), "redis-prod", "", alwaysInScope)
	wantUpPathsProblem(t, err, http.StatusBadGateway, problem.TypeBadGateway)
}

// TestUpgradePaths_Target_Found — ?to=v2 with an upgrade scenario (from ⊇ pin) →
// mode=found + slug, direction=forward, the applied migration chain.
func TestUpgradePaths_Target_Found(t *testing.T) {
	mig, err := statemigrate.Parse([]byte("from_version: 1\nto_version: 2\ntransform:\n  - set:\n      path: state.foo\n      value: bar\n"))
	if err != nil {
		t.Fatalf("parse migration: %v", err)
	}
	loader := &fakeLoader{
		targetSchema: 2,
		chain:        statemigrate.Chain{mig},
		upgrades:     []artifact.Scenario{{Name: "to_v2", FromVersions: []string{"v1"}}},
	}
	h := newUpPathsHandler(upPathsDB(), loader, nil)

	view, err := h.UpgradePathsTyped(context.Background(), "redis-prod", "v2", alwaysInScope)
	if err != nil {
		t.Fatalf("UpgradePathsTyped: %v", err)
	}
	if view.Paths != nil {
		t.Errorf("Paths = %+v, want nil in on-demand", view.Paths)
	}
	tgt := view.Target
	if tgt == nil {
		t.Fatal("Target = nil, want populated")
	}
	if tgt.To != "v2" || tgt.TargetStateSchemaVersion != 2 {
		t.Errorf("to=%q target_schema=%d, want v2/2", tgt.To, tgt.TargetStateSchemaVersion)
	}
	if tgt.Direction != upgradeDirectionForward {
		t.Errorf("direction = %q, want forward", tgt.Direction)
	}
	if tgt.Mode != upgradeModeFound || tgt.Slug != "to_v2" {
		t.Errorf("mode/slug = %q/%q, want found/to_v2", tgt.Mode, tgt.Slug)
	}
	if tgt.Downgrade {
		t.Errorf("Downgrade = true, want false (forward)")
	}
	if len(tgt.StateMigrations) != 1 {
		t.Fatalf("StateMigrations len = %d, want 1", len(tgt.StateMigrations))
	}
	got := tgt.StateMigrations[0]
	if got.From != 1 || got.To != 2 || got.Path != "migrations/001_to_002.yml" {
		t.Errorf("migration step = %+v, want {1 2 migrations/001_to_002.yml}", got)
	}
	if !tgt.Reachable || tgt.UnreachableReason != "" {
		t.Errorf("reachable/reason = %v/%q, want true/empty (chain assembled)", tgt.Reachable, tgt.UnreachableReason)
	}
}

// TestUpgradePaths_Target_Legacy — ?to=v2 with no matching upgrade scenario →
// mode=legacy; direction=forward; migrations still apply (forward).
func TestUpgradePaths_Target_Legacy(t *testing.T) {
	mig, _ := statemigrate.Parse([]byte("from_version: 1\nto_version: 2\ntransform:\n  - set:\n      path: state.foo\n      value: bar\n"))
	loader := &fakeLoader{targetSchema: 2, chain: statemigrate.Chain{mig}} // upgrades nil → legacy
	h := newUpPathsHandler(upPathsDB(), loader, nil)

	view, err := h.UpgradePathsTyped(context.Background(), "redis-prod", "v2", alwaysInScope)
	if err != nil {
		t.Fatalf("UpgradePathsTyped: %v", err)
	}
	tgt := view.Target
	if tgt == nil {
		t.Fatal("Target = nil")
	}
	if tgt.Mode != upgradeModeLegacy || tgt.Slug != "" {
		t.Errorf("mode/slug = %q/%q, want legacy/empty", tgt.Mode, tgt.Slug)
	}
	if tgt.Direction != upgradeDirectionForward {
		t.Errorf("direction = %q, want forward", tgt.Direction)
	}
	if len(tgt.StateMigrations) != 1 {
		t.Errorf("StateMigrations len = %d, want 1 (forward loads the chain)", len(tgt.StateMigrations))
	}
	if !tgt.Reachable {
		t.Errorf("Reachable = false, want true (forward with an assembled chain)")
	}
}

// TestUpgradePaths_Target_Downgrade — target with a schema below the current → direction=
// downgrade, downgrade flag set, the chain is NOT loaded (forward-only, ADR-019).
func TestUpgradePaths_Target_Downgrade(t *testing.T) {
	loader := &fakeLoader{targetSchema: 0} // current schema=1 → 0<1 downgrade
	h := newUpPathsHandler(upPathsDB(), loader, nil)

	view, err := h.UpgradePathsTyped(context.Background(), "redis-prod", "v0", alwaysInScope)
	if err != nil {
		t.Fatalf("UpgradePathsTyped: %v", err)
	}
	tgt := view.Target
	if tgt == nil {
		t.Fatal("Target = nil")
	}
	if tgt.Direction != upgradeDirectionDowngrade || !tgt.Downgrade {
		t.Errorf("direction/downgrade = %q/%v, want downgrade/true", tgt.Direction, tgt.Downgrade)
	}
	if tgt.StateMigrations != nil {
		t.Errorf("StateMigrations = %+v, want nil (downgrade does not load the chain)", tgt.StateMigrations)
	}
	if loader.chainCalls != 0 {
		t.Errorf("chainCalls = %d, want 0 (downgrade does not call LoadMigrationChain)", loader.chainCalls)
	}
	// mode is meaningless for downgrade → empty, ListUpgrades is not called.
	if tgt.Mode != "" {
		t.Errorf("mode = %q, want empty (downgrade does not compute found/legacy)", tgt.Mode)
	}
	if loader.upgradesCalls != 0 {
		t.Errorf("upgradesCalls = %d, want 0 (downgrade does not call ListUpgrades)", loader.upgradesCalls)
	}
	if !tgt.Reachable {
		t.Errorf("Reachable = false, want true (downgrade is a different direction, not \"unreachable\")")
	}
}

// TestUpgradePaths_Target_Noop — ?to==pin AND schema equals the current → direction=no-op.
func TestUpgradePaths_Target_Noop(t *testing.T) {
	loader := &fakeLoader{targetSchema: 1, chain: statemigrate.Chain{}}
	h := newUpPathsHandler(upPathsDB(), loader, nil)

	view, err := h.UpgradePathsTyped(context.Background(), "redis-prod", "v1", alwaysInScope)
	if err != nil {
		t.Fatalf("UpgradePathsTyped: %v", err)
	}
	if view.Target == nil || view.Target.Direction != upgradeDirectionNoop {
		t.Errorf("direction = %+v, want no-op", view.Target)
	}
	// mode is meaningless for no-op → empty, ListUpgrades is not called.
	if view.Target.Mode != "" {
		t.Errorf("mode = %q, want empty (no-op does not compute found/legacy)", view.Target.Mode)
	}
	if loader.upgradesCalls != 0 {
		t.Errorf("upgradesCalls = %d, want 0 (no-op does not call ListUpgrades)", loader.upgradesCalls)
	}
	if !view.Target.Reachable {
		t.Errorf("Reachable = false, want true (no-op is reachable)")
	}
}

// TestUpgradePaths_Target_SameSchema — schema equals the current but the ref differs (ref-bump)
// → direction=same-schema (not no-op).
func TestUpgradePaths_Target_SameSchema(t *testing.T) {
	loader := &fakeLoader{targetSchema: 1, chain: statemigrate.Chain{}}
	h := newUpPathsHandler(upPathsDB(), loader, nil)

	view, err := h.UpgradePathsTyped(context.Background(), "redis-prod", "v1-hotfix", alwaysInScope)
	if err != nil {
		t.Fatalf("UpgradePathsTyped: %v", err)
	}
	if view.Target == nil || view.Target.Direction != upgradeDirectionSameSchema {
		t.Errorf("direction = %+v, want same-schema", view.Target)
	}
	if view.Target.Downgrade {
		t.Errorf("Downgrade = true, want false (same-schema)")
	}
	if !view.Target.Reachable {
		t.Errorf("Reachable = false, want true (same-schema is reachable)")
	}
}

// TestUpgradePaths_Target_BrokenChain_Unreachable_200 — a structurally broken migration
// chain to the target → NOT an HTTP error, but 200 with reachable=false + unreachable_reason
// (preview shows the unreachable target as DATA, ADR-0068 §6). direction/mode
// are STILL computed (the target is forward, found/legacy are known); state_migrations empty.
func TestUpgradePaths_Target_BrokenChain_Unreachable_200(t *testing.T) {
	loader := &fakeLoader{targetSchema: 3, chainErr: artifact.ErrMigrationChainBroken}
	h := newUpPathsHandler(upPathsDB(), loader, nil)
	view, err := h.UpgradePathsTyped(context.Background(), "redis-prod", "v3", alwaysInScope)
	if err != nil {
		t.Fatalf("UpgradePathsTyped: %v (broken chain is preview data, not an HTTP error)", err)
	}
	tgt := view.Target
	if tgt == nil {
		t.Fatal("Target = nil")
	}
	if tgt.Reachable {
		t.Errorf("Reachable = true, want false (broken chain)")
	}
	wantReason := "migration chain to v3 is broken: " + artifact.ErrMigrationChainBroken.Error()
	if tgt.UnreachableReason != wantReason {
		t.Errorf("UnreachableReason = %q, want %q", tgt.UnreachableReason, wantReason)
	}
	if tgt.Direction != upgradeDirectionForward {
		t.Errorf("direction = %q, want forward (target is an upgrade, only the chain is unreachable)", tgt.Direction)
	}
	if tgt.Mode != upgradeModeLegacy {
		t.Errorf("mode = %q, want legacy (mode is still computed on forward)", tgt.Mode)
	}
	if len(tgt.StateMigrations) != 0 {
		t.Errorf("StateMigrations = %+v, want empty (chain cannot be assembled)", tgt.StateMigrations)
	}
}

// TestUpgradePaths_Target_ChainError_500 — a non-broken LoadMigrationChain error (parsing a
// malformed migrations/NNN_to_MMM.yml of an already-materialized snapshot = keeper-internal
// defect) → 500, NOT 502 (502 is only for loader.Load, where the external git is at fault).
func TestUpgradePaths_Target_ChainError_500(t *testing.T) {
	loader := &fakeLoader{targetSchema: 2, chainErr: errors.New("parse migrations/001_to_002.yml: bad yaml")}
	h := newUpPathsHandler(upPathsDB(), loader, nil)
	_, err := h.UpgradePathsTyped(context.Background(), "redis-prod", "v2", alwaysInScope)
	wantUpPathsProblem(t, err, http.StatusInternalServerError, problem.TypeInternalError)
}

// TestUpgradePaths_Target_LoadFail_502 — the target snapshot did not materialize (git failure)
// → 502.
func TestUpgradePaths_Target_LoadFail_502(t *testing.T) {
	loader := &fakeLoader{loadErr: context.DeadlineExceeded}
	h := newUpPathsHandler(upPathsDB(), loader, nil)
	_, err := h.UpgradePathsTyped(context.Background(), "redis-prod", "v2", alwaysInScope)
	wantUpPathsProblem(t, err, http.StatusBadGateway, problem.TypeBadGateway)
}

// TestUpgradePaths_OutOfScope_404 — incarnation outside the operator scope → 404 (do not
// leak existence), in both cheap and on-demand modes.
func TestUpgradePaths_OutOfScope_404(t *testing.T) {
	for _, toRef := range []string{"", "v2"} {
		refs := &fakeRefsLister{refs: []artifact.GitRef{{Name: "v1"}}}
		h := newUpPathsHandler(upPathsDB(), &fakeLoader{targetSchema: 2}, refs)
		_, err := h.UpgradePathsTyped(context.Background(), "redis-prod", toRef, neverInScope)
		wantUpPathsProblem(t, err, http.StatusNotFound, problem.TypeNotFound)
	}
}

// TestUpgradePaths_NotFound_404 — incarnation absent → 404.
func TestUpgradePaths_NotFound_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(_ string) pgx.Row { return errRow{err: pgx.ErrNoRows} }}
	h := newUpPathsHandler(db, &fakeLoader{}, &fakeRefsLister{})
	_, err := h.UpgradePathsTyped(context.Background(), "ghost", "", alwaysInScope)
	wantUpPathsProblem(t, err, http.StatusNotFound, problem.TypeNotFound)
}

// TestUpgradePaths_InvalidName_422 — invalid name → 422 before select/resolution.
func TestUpgradePaths_InvalidName_422(t *testing.T) {
	h := newUpPathsHandler(&fakeIncDB{}, &fakeLoader{}, &fakeRefsLister{})
	_, err := h.UpgradePathsTyped(context.Background(), "Bad_Name", "", alwaysInScope)
	wantUpPathsProblem(t, err, http.StatusUnprocessableEntity, problem.TypeValidationFailed)
}
