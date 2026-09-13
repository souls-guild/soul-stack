package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// [sigilRecordSource] is the wire-up between the plugin_sigils registry and the verify
// contract, and the only production site that decides which registry outcome is a FACT
// and which is a FAILURE (NIM-814):
//
//   - [sigil.ErrSigilNotFound] — asked and answered, there is no grant for this alias →
//     (nil, nil) → verify reports no_sigil, whose hint tells the operator to issue one;
//   - anything else — could not ask → the error propagates → verify reports
//     lookup_unavailable, which says the answer is UNKNOWN.
//
// Collapsing the two in either direction is invisible one layer up, where the adapter's
// own tests run against a fake source: an unconditional `return nil, nil` restores the
// original defect exactly — a Postgres outage reaching the operator as "plugin is not
// allowed; run keeper.plugin.allow …", an instruction to widen a supply-chain gate over
// a database being down.

// storeStub is a sigil.Store that answers GetActive from fixed values and records the
// context it was called on. The other three methods are never reached here.
type storeStub struct {
	rec  *sigil.Sigil
	err  error
	seen context.Context
}

func (s *storeStub) Insert(context.Context, *sigil.Sigil) error   { panic("not reached") }
func (s *storeStub) Revoke(context.Context, string, string) error { panic("not reached") }
func (s *storeStub) ListActive(context.Context) ([]*sigil.Sigil, error) {
	panic("the verify path must not read the whole active set")
}

func (s *storeStub) GetActive(ctx context.Context, _ string) (*sigil.Sigil, error) {
	s.seen = ctx
	if s.err != nil {
		return nil, s.err
	}
	return s.rec, nil
}

func TestSigilRecordSource_AbsentGrantIsNotAFailure(t *testing.T) {
	src := sigilRecordSource{store: &storeStub{err: sigil.ErrSigilNotFound}}

	rec, err := src.GetActive(context.Background(), "hetzner")
	if err != nil {
		t.Fatalf("a grant that does not exist is a fact, not a failure: %v", err)
	}
	if rec != nil {
		t.Fatalf("rec = %+v, want nil (verify reads it as no_sigil)", rec)
	}
}

// The half the ticket is about: a registry that could not answer must NOT look like a
// registry that answered "no". Both refuse the spawn — fail-closed is unchanged — but
// only one of them is fixed by issuing an approval.
func TestSigilRecordSource_ReadFailurePropagates(t *testing.T) {
	dbDown := errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
	src := sigilRecordSource{store: &storeStub{err: dbDown}}

	rec, err := src.GetActive(context.Background(), "hetzner")
	if rec != nil {
		t.Fatalf("a failed read must not produce a record, got %+v", rec)
	}
	if !errors.Is(err, dbDown) {
		t.Fatalf("err = %v, want the underlying read error — swallowed here, a database outage reaches the operator as `plugin is not allowed; run keeper.plugin.allow …`", err)
	}
	// And the reverse collapse is a defect too: a genuinely missing grant reported as
	// "unknown" would tell an operator to go look at a database that is fine.
	if errors.Is(err, sigil.ErrSigilNotFound) {
		t.Error("a transport failure must not be reported as ErrSigilNotFound")
	}
}

// The projection carries every field verify needs, byte for byte. Schema and Signature
// are the two that would fail silently: dropping either leaves a record that looks
// well-formed and fails as bad_signature on a live Keeper, which reads as tampering.
func TestSigilRecordSource_ProjectsEveryVerifyField(t *testing.T) {
	want := &sigil.Sigil{
		ID:     42,
		Alias:  "redis",
		Source: "https://example.com/redis.git",
		Ref:    "v2.0.0",
		Kind:   "artifact",
		Artifacts: []sharedhost.SigilArtifact{
			{OS: "linux", Arch: "amd64", Path: "redis_linux_amd64", SHA256: "aa"},
			{OS: "linux", Arch: "arm64", Path: "redis_linux_arm64", SHA256: "bb"},
		},
		Signature: []byte{1, 2, 3, 4},
		Schema:    []byte(`{"kind":"soul_module","protocol_version":1}`),
	}
	src := sigilRecordSource{store: &storeStub{rec: want}}

	got, err := src.GetActive(context.Background(), "redis")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if got == nil {
		t.Fatal("GetActive returned nil for a present grant")
	}
	if got.Alias != want.Alias || got.Source != want.Source || got.Ref != want.Ref || got.Kind != want.Kind {
		t.Errorf("identity mismatch: %+v", got)
	}
	// The WHOLE artifact list crosses: the signature is over every row, so dropping the
	// other platforms' rows would leave a record that cannot verify at all.
	if len(got.Artifacts) != len(want.Artifacts) {
		t.Fatalf("Artifacts = %d, want %d", len(got.Artifacts), len(want.Artifacts))
	}
	for i := range want.Artifacts {
		if got.Artifacts[i] != want.Artifacts[i] {
			t.Errorf("Artifacts[%d] = %+v, want %+v", i, got.Artifacts[i], want.Artifacts[i])
		}
	}
	if !bytes.Equal(got.Signature, want.Signature) {
		t.Errorf("Signature = %v, want %v", got.Signature, want.Signature)
	}
	// Byte-exact: verify hashes exactly these bytes via SchemaDigest (S3↔S6 invariant).
	if !bytes.Equal(got.Schema, want.Schema) {
		t.Errorf("Schema not byte-exact: %q vs %q", got.Schema, want.Schema)
	}
}

// The caller's context reaches the query. A context.Background() here would ignore the
// run's deadline and survive its cancellation, which is what made a degraded Postgres
// hang the Spawn instead of refusing it.
func TestSigilRecordSource_CarriesTheCallersContext(t *testing.T) {
	type ctxKey struct{}
	store := &storeStub{rec: &sigil.Sigil{Alias: "redis"}}
	src := sigilRecordSource{store: store}

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "spawn"))
	defer cancel()
	if _, err := src.GetActive(ctx, "redis"); err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if store.seen == nil {
		t.Fatal("the store was never called")
	}
	if store.seen.Value(ctxKey{}) != "spawn" {
		t.Error("the query ran on some other context — it would neither see the run's deadline nor notice its cancellation")
	}
	cancel()
	if store.seen.Err() == nil {
		t.Error("cancelling the caller did not cancel the context the query ran on")
	}
}
