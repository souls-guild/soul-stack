package handlers

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
)

// The survey is a projection of the incarnations the caller may see, so its
// scope gate has to behave exactly like the incarnation list's: fail-closed.
// An undefined scope must yield an EMPTY survey rather than the estate — and it
// must do so without touching the store, since reaching the DB with no scope is
// how a leak starts.
func TestDeprecationsTyped_EmptyScopeFailsClosed(t *testing.T) {
	db := &fakeIncDB{}
	// services/loader are non-nil via the stub below so the wiring check passes
	// and the scope gate is what actually decides.
	h := NewIncarnationHandler(db, nil, nil, nil, stubServiceResolver{}, stubSnapshotLoader{}, nil,
		fakeIncScoper{}, nil)

	reply, err := h.DeprecationsTyped(context.Background(), runsClaims())
	if err != nil {
		t.Fatalf("DeprecationsTyped: %v", err)
	}
	if len(reply.Items) != 0 || len(reply.Gaps) != 0 {
		t.Errorf("empty scope produced a non-empty survey: %+v", reply)
	}
	if reply.ScannedIncarnations != 0 {
		t.Errorf("scanned = %d, want 0 - nothing may be scanned without a scope", reply.ScannedIncarnations)
	}
}

// A mis-wired scoper (nil) is the same fail-closed case: it must not degrade
// into "unrestricted".
func TestDeprecationsTyped_NilScoperFailsClosed(t *testing.T) {
	db := &fakeIncDB{}
	h := NewIncarnationHandler(db, nil, nil, nil, stubServiceResolver{}, stubSnapshotLoader{}, nil, nil, nil)

	reply, err := h.DeprecationsTyped(context.Background(), runsClaims())
	if err != nil {
		t.Fatalf("DeprecationsTyped: %v", err)
	}
	if len(reply.Items) != 0 || reply.ScannedIncarnations != 0 {
		t.Errorf("nil scoper produced a survey: %+v", reply)
	}
}

// Without a service registry the survey cannot resolve a single definition.
// That is a wiring fault and must surface as an error rather than as an empty
// list, which would read as "the estate is clean".
func TestDeprecationsTyped_UnwiredRegistryIsAnError(t *testing.T) {
	h := NewIncarnationHandler(&fakeIncDB{}, nil, nil, nil, nil, nil, nil, fakeIncScoper{}, nil)

	if _, err := h.DeprecationsTyped(context.Background(), runsClaims()); err == nil {
		t.Fatal("an unwired registry returned a clean survey instead of an error")
	}
}

// stubServiceResolver / stubSnapshotLoader — non-nil deps so the wiring check
// passes and the scope gate is the thing under test. Neither is reached in
// these cases: a fail-closed scope returns before any definition is resolved.
type stubServiceResolver struct{}

func (stubServiceResolver) Resolve(string) (artifact.ServiceRef, bool) {
	return artifact.ServiceRef{}, false
}

type stubSnapshotLoader struct{}

func (stubSnapshotLoader) Load(context.Context, artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	return nil, errServiceNotRegistered
}

func (stubSnapshotLoader) LoadMigrationChain(*artifact.ServiceArtifact, int, int) (statemigrate.Chain, error) {
	return statemigrate.Chain{}, nil
}

func (stubSnapshotLoader) ReadFile(*artifact.ServiceArtifact, string) ([]byte, error) {
	return nil, errServiceNotRegistered
}

func (stubSnapshotLoader) ListUpgrades(*artifact.ServiceArtifact) ([]artifact.Scenario, error) {
	return nil, nil
}
