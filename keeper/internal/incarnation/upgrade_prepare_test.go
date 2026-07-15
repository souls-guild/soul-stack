package incarnation

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakePrepResolver / fakePrepLoader — minimal ServiceResolver /
// ServiceSnapshotLoader mocks for PrepareUpgrade unit tests (no real git or
// service registry involved).
type fakePrepResolver struct {
	ok bool
}

func (f fakePrepResolver) Resolve(service string) (artifact.ServiceRef, bool) {
	return artifact.ServiceRef{Name: service, Git: "file:///repo", Ref: "current"}, f.ok
}

type fakePrepLoader struct {
	targetSchema int
	loadErr      error

	chain    statemigrate.Chain
	chainErr error

	chainCalls int

	// upgrades / upgradesErr — ListUpgrades response (ADR-0068): a non-empty
	// list with FromVersions containing the current pin → found. upgradesErr
	// → fail-open legacy.
	upgrades    []artifact.Scenario
	upgradesErr error
}

func (f *fakePrepLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return &artifact.ServiceArtifact{
		Ref:      ref,
		Manifest: &config.ServiceManifest{StateSchemaVersion: f.targetSchema},
	}, nil
}

func (f *fakePrepLoader) LoadMigrationChain(_ *artifact.ServiceArtifact, _, _ int) (statemigrate.Chain, error) {
	f.chainCalls++
	if f.chainErr != nil {
		return nil, f.chainErr
	}
	return f.chain, nil
}

func (f *fakePrepLoader) ListUpgrades(_ *artifact.ServiceArtifact) ([]artifact.Scenario, error) {
	if f.upgradesErr != nil {
		return nil, f.upgradesErr
	}
	return f.upgrades, nil
}

func prepInc(serviceVersion string, schema int) *Incarnation {
	return &Incarnation{Name: "redis-prod", Service: "redis", ServiceVersion: serviceVersion, StateSchemaVersion: schema}
}

func TestPrepareUpgrade_Happy(t *testing.T) {
	mig, err := statemigrate.Parse([]byte("from_version: 1\nto_version: 2\ntransform:\n  - set:\n      path: state.foo\n      value: bar\n"))
	if err != nil {
		t.Fatalf("parse migration: %v", err)
	}
	loader := &fakePrepLoader{targetSchema: 2, chain: statemigrate.Chain{mig}}
	changedBy := "archon-alice"

	in, err := PrepareUpgrade(context.Background(), fakePrepResolver{ok: true}, loader,
		prepInc("v1", 1), "v2", "01ARZ3NDEKTSV4RRFFQ69G5FAV", &changedBy)
	if err != nil {
		t.Fatalf("PrepareUpgrade: %v", err)
	}
	if in.Name != "redis-prod" || in.TargetServiceVer != "v2" || in.TargetSchemaVer != 2 {
		t.Errorf("UpgradeInput = %+v", in)
	}
	if len(in.Chain) != 1 {
		t.Errorf("chain len = %d, want 1", len(in.Chain))
	}
	if in.Evaluator == nil {
		t.Error("Evaluator is nil")
	}
	if in.ChangedByAID == nil || *in.ChangedByAID != "archon-alice" {
		t.Errorf("ChangedByAID = %v", in.ChangedByAID)
	}
}

func TestPrepareUpgrade_RefBump(t *testing.T) {
	// Same schema, different ref → empty chain, no error.
	loader := &fakePrepLoader{targetSchema: 1, chain: statemigrate.Chain{}}
	in, err := PrepareUpgrade(context.Background(), fakePrepResolver{ok: true}, loader,
		prepInc("v1", 1), "v1-hotfix", "01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	if err != nil {
		t.Fatalf("PrepareUpgrade ref-bump: %v", err)
	}
	if len(in.Chain) != 0 || in.TargetSchemaVer != 1 {
		t.Errorf("UpgradeInput = %+v, want empty chain / schema 1", in)
	}
}

func TestPrepareUpgrade_Noop(t *testing.T) {
	// Same ref AND same schema → ErrUpgradeNoop, LoadMigrationChain isn't called.
	loader := &fakePrepLoader{targetSchema: 2}
	_, err := PrepareUpgrade(context.Background(), fakePrepResolver{ok: true}, loader,
		prepInc("v2", 2), "v2", "01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	if !errors.Is(err, ErrUpgradeNoop) {
		t.Fatalf("err = %v, want ErrUpgradeNoop", err)
	}
	if loader.chainCalls != 0 {
		t.Errorf("chainCalls = %d, want 0 (no-op must short-circuit)", loader.chainCalls)
	}
}

func TestPrepareUpgrade_DowngradeViaRef(t *testing.T) {
	// Target ref carries a schema lower than current → ErrDowngradeViaRef
	// before the chain loads.
	loader := &fakePrepLoader{targetSchema: 2}
	_, err := PrepareUpgrade(context.Background(), fakePrepResolver{ok: true}, loader,
		prepInc("v3", 3), "v2", "01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	if !errors.Is(err, ErrDowngradeViaRef) {
		t.Fatalf("err = %v, want ErrDowngradeViaRef", err)
	}
	if loader.chainCalls != 0 {
		t.Errorf("chainCalls = %d, want 0 (downgrade guard must short-circuit)", loader.chainCalls)
	}
}

func TestPrepareUpgrade_ServiceNotRegistered(t *testing.T) {
	_, err := PrepareUpgrade(context.Background(), fakePrepResolver{ok: false}, &fakePrepLoader{targetSchema: 2},
		prepInc("v1", 1), "v2", "01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	if !errors.Is(err, ErrServiceNotRegistered) {
		t.Fatalf("err = %v, want ErrServiceNotRegistered", err)
	}
}

func TestPrepareUpgrade_LoadFailed(t *testing.T) {
	loader := &fakePrepLoader{loadErr: errors.New("git: ref not found")}
	_, err := PrepareUpgrade(context.Background(), fakePrepResolver{ok: true}, loader,
		prepInc("v1", 1), "v99", "01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	if !errors.Is(err, ErrLoadTargetSnapshot) {
		t.Fatalf("err = %v, want ErrLoadTargetSnapshot", err)
	}
}

func TestPrepareUpgrade_ChainBroken(t *testing.T) {
	loader := &fakePrepLoader{targetSchema: 3, chainErr: artifact.ErrMigrationChainBroken}
	_, err := PrepareUpgrade(context.Background(), fakePrepResolver{ok: true}, loader,
		prepInc("v1", 1), "v3", "01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	if !errors.Is(err, artifact.ErrMigrationChainBroken) {
		t.Fatalf("err = %v, want ErrMigrationChainBroken (passed through)", err)
	}
}

// TestPrepareUpgrade_FoundUpgradeScenario — found branch (ADR-0068 §5): an
// upgrade scenario whose from ⊇ the current pin (v1) resolves into
// UpgradeSlug; TargetRef is pinned to to_version (for runner.Start autorun).
func TestPrepareUpgrade_FoundUpgradeScenario(t *testing.T) {
	loader := &fakePrepLoader{
		targetSchema: 2,
		chain:        statemigrate.Chain{setStep(1, 2)},
		upgrades:     []artifact.Scenario{{Name: "to_v2", FromVersions: []string{"v0", "v1"}}},
	}
	in, err := PrepareUpgrade(context.Background(), fakePrepResolver{ok: true}, loader,
		prepInc("v1", 1), "v2", "01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	if err != nil {
		t.Fatalf("PrepareUpgrade found: %v", err)
	}
	if in.UpgradeSlug != "to_v2" {
		t.Errorf("UpgradeSlug = %q, want to_v2 (from ⊇ v1)", in.UpgradeSlug)
	}
	if in.TargetRef.Ref != "v2" {
		t.Errorf("TargetRef.Ref = %q, want v2 (пин цели для runner.Start)", in.TargetRef.Ref)
	}
}

// TestPrepareUpgrade_LegacyNoUpgradeMatch — an upgrade scenario exists, but
// from does NOT contain the current pin → legacy (UpgradeSlug empty, §5 fail-open).
func TestPrepareUpgrade_LegacyNoUpgradeMatch(t *testing.T) {
	loader := &fakePrepLoader{
		targetSchema: 2,
		chain:        statemigrate.Chain{setStep(1, 2)},
		upgrades:     []artifact.Scenario{{Name: "to_v2", FromVersions: []string{"v0"}}},
	}
	in, err := PrepareUpgrade(context.Background(), fakePrepResolver{ok: true}, loader,
		prepInc("v1", 1), "v2", "01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	if err != nil {
		t.Fatalf("PrepareUpgrade legacy: %v", err)
	}
	if in.UpgradeSlug != "" {
		t.Errorf("UpgradeSlug = %q, want empty (from не матчит v1 → legacy)", in.UpgradeSlug)
	}
}

// TestPrepareUpgrade_ListUpgradesFailsOpenLegacy — a scan failure of upgrade/
// does NOT fail the upgrade: fail-open into legacy (§5★, so patch upgrades
// keep working).
func TestPrepareUpgrade_ListUpgradesFailsOpenLegacy(t *testing.T) {
	loader := &fakePrepLoader{
		targetSchema: 2,
		chain:        statemigrate.Chain{setStep(1, 2)},
		upgradesErr:  errors.New("fs: read upgrade dir failed"),
	}
	in, err := PrepareUpgrade(context.Background(), fakePrepResolver{ok: true}, loader,
		prepInc("v1", 1), "v2", "01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	if err != nil {
		t.Fatalf("PrepareUpgrade must not fail on ListUpgrades error (fail-open): %v", err)
	}
	if in.UpgradeSlug != "" {
		t.Errorf("UpgradeSlug = %q, want empty (fail-open legacy)", in.UpgradeSlug)
	}
}
