package pluginhost

import (
	"context"
	"errors"
	"testing"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// fakeSource is a minimal SigilRecordSource for the adapter's unit tests. It records
// every question asked, which is what makes the cost claims in NIM-814 checkable
// instead of asserted: `asked` is one entry per registry read, and each entry carries
// the alias the read was keyed on and the context it ran under.
type fakeSource struct {
	recs  map[string]*sharedhost.SigilRecord
	err   error
	asked []askedFor
}

type askedFor struct {
	alias string
	ctx   context.Context
}

func (f *fakeSource) GetActive(ctx context.Context, alias string) (*sharedhost.SigilRecord, error) {
	f.asked = append(f.asked, askedFor{alias: alias, ctx: ctx})
	if f.err != nil {
		return nil, f.err
	}
	rec, ok := f.recs[alias]
	if !ok {
		return nil, nil
	}
	return rec, nil
}

func sourceWith(recs ...*sharedhost.SigilRecord) *fakeSource {
	m := make(map[string]*sharedhost.SigilRecord, len(recs))
	for _, r := range recs {
		m[r.Alias] = r
	}
	return &fakeSource{recs: m}
}

// TestSigilLookupAdapter_Maps verifies Get resolves a record by REGISTRATION ALIAS —
// the key a host slot is named by and the only key the verify path holds.
func TestSigilLookupAdapter_Maps(t *testing.T) {
	want := &sharedhost.SigilRecord{
		Alias:     "hetzner",
		Source:    "https://example.com/soul-cloud-hetzner.git",
		Ref:       "v2.0.0",
		Kind:      sharedplugin.SourceKindGit,
		Artifacts: []sharedhost.SigilArtifact{{SHA256: "abc123"}},
		Signature: []byte{1, 2, 3, 4},
		Schema:    []byte(`{"kind":"ssh_provider","protocol_version":1}`),
	}
	a := NewSigilLookupAdapter(sourceWith(want), nil)

	rec, err := a.Get(context.Background(), "hetzner")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec != want {
		t.Fatalf("Get returned %+v, want %+v", rec, want)
	}
}

// TestSigilLookupAdapter_AbsentIsNil verifies no record for the alias → (nil, nil)
// (no_sigil): an answered question whose answer is "there is no grant".
func TestSigilLookupAdapter_AbsentIsNil(t *testing.T) {
	a := NewSigilLookupAdapter(sourceWith(&sharedhost.SigilRecord{Alias: "other"}), nil)
	rec, err := a.Get(context.Background(), "hetzner")
	if err != nil {
		t.Fatalf("an absent grant is not an error: %v", err)
	}
	if rec != nil {
		t.Fatalf("absent sigil must map to nil, got %+v", rec)
	}
}

// TestSigilLookupAdapter_AliasIsNotSource pins that the lookup keys on the alias and
// NOT on what the grant was signed over: a host holding a slot named `hetzner` has no
// idea which source it came from, so a lookup by source could never be made.
func TestSigilLookupAdapter_AliasIsNotSource(t *testing.T) {
	a := NewSigilLookupAdapter(sourceWith(&sharedhost.SigilRecord{
		Alias: "hetzner", Source: "https://example.com/a.git", Ref: "v2",
	}), nil)
	if rec, _ := a.Get(context.Background(), "https://example.com/a.git"); rec != nil {
		t.Fatalf("Get must key on the alias, not the source: got %+v", rec)
	}
	if rec, _ := a.Get(context.Background(), "hetzner"); rec == nil || rec.Ref != "v2" {
		t.Fatalf("Get by alias failed: %+v", rec)
	}
}

// TestSigilLookupAdapter_NilSource verifies a nil source doesn't panic and always
// answers "no grant" (no_sigil fail-closed on incomplete wire-up / Sigil off). Not an
// error: nothing failed, the Keeper is configured without Sigil.
func TestSigilLookupAdapter_NilSource(t *testing.T) {
	a := NewSigilLookupAdapter(nil, nil)
	rec, err := a.Get(context.Background(), "hetzner")
	if err != nil {
		t.Fatalf("a nil source is a configuration, not a failure: %v", err)
	}
	if rec != nil {
		t.Fatalf("nil source must yield nil record, got %+v", rec)
	}
}

// A registry read that FAILED must surface as an error and not as "no grant"
// (NIM-814). Both refuse the spawn — the gate is unchanged — but the two are fixed by
// opposite actions, and only one of them is an approval the operator should go and
// issue. This is the guard: flatten the error back into (nil, nil) and this test is the
// one that notices.
func TestSigilLookupAdapter_ReadErrorIsAnError(t *testing.T) {
	dbDown := errors.New("db down")
	a := NewSigilLookupAdapter(&fakeSource{err: dbDown}, nil)

	rec, err := a.Get(context.Background(), "hetzner")
	if rec != nil {
		t.Fatalf("a failed read must not produce a record, got %+v", rec)
	}
	if !errors.Is(err, dbDown) {
		t.Fatalf("err = %v, want the underlying read error — reported as no_sigil, a database outage tells the operator to issue an allow", err)
	}
}

// The cost claim of NIM-814, made checkable: one spawn's lookup is ONE registry read,
// keyed by the alias asked about. The defect was `Get` ignoring its key and issuing
// `SELECT … WHERE revoked_at IS NULL` — every active grant with its schema document,
// tens of KiB apiece — to then scan the slice for one alias, once per fork.
//
// Counting the reads is the whole test: the interface no longer offers a list, so a
// regression cannot reintroduce the full scan without changing this count or this key.
func TestSigilLookupAdapter_OneReadPerLookupKeyedByAlias(t *testing.T) {
	src := sourceWith(
		&sharedhost.SigilRecord{Alias: "redis"},
		&sharedhost.SigilRecord{Alias: "hetzner"},
		&sharedhost.SigilRecord{Alias: "teleport"},
	)
	a := NewSigilLookupAdapter(src, nil)

	if _, err := a.Get(context.Background(), "hetzner"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(src.asked) != 1 {
		t.Fatalf("registry reads = %d, want 1 per lookup", len(src.asked))
	}
	if src.asked[0].alias != "hetzner" {
		t.Errorf("read was keyed on %q, want the alias asked about (%q)", src.asked[0].alias, "hetzner")
	}

	// Three spawns, three reads — and each one still asks only about its own alias.
	for _, alias := range []string{"redis", "teleport", "redis"} {
		if _, err := a.Get(context.Background(), alias); err != nil {
			t.Fatalf("Get(%q): %v", alias, err)
		}
	}
	if len(src.asked) != 4 {
		t.Fatalf("registry reads after 4 lookups = %d, want 4", len(src.asked))
	}
	for i, want := range []string{"hetzner", "redis", "teleport", "redis"} {
		if src.asked[i].alias != want {
			t.Errorf("read %d keyed on %q, want %q", i, src.asked[i].alias, want)
		}
	}
}

// The CALLER's context reaches the registry, unreplaced. The other half of NIM-814:
// the adapter used to start from context.Background(), so the query carried neither the
// run's deadline nor its cancellation — a degraded Postgres hung the Spawn instead of
// refusing it, and cancelling the run did not reach the query.
func TestSigilLookupAdapter_CarriesTheCallersContext(t *testing.T) {
	type ctxKey struct{}
	src := sourceWith(&sharedhost.SigilRecord{Alias: "hetzner"})
	a := NewSigilLookupAdapter(src, nil)

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "spawn"))
	defer cancel()
	if _, err := a.Get(ctx, "hetzner"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(src.asked) != 1 {
		t.Fatalf("registry reads = %d, want 1", len(src.asked))
	}
	got := src.asked[0].ctx
	if got.Value(ctxKey{}) != "spawn" {
		t.Error("the registry was read on some other context — a context.Background() here outlives the run it belongs to")
	}
	// Cancellation must reach the query, which is what context.Background() cost:
	// with the run gone, the read is still running.
	cancel()
	if got.Err() == nil {
		t.Error("cancelling the caller did not cancel the context the registry was read on")
	}
}

// TestSigilLookupAdapter_AliasIsALookupKeyNotATrustClaim is the keeper-side proof of
// the property the whole re-keying rests on, checked independently of the Soul side.
//
// The alias selects WHICH grant to check. It contributes nothing to whether that
// grant is valid — the signature covers (source, kind, ref, schema_sha256, artifacts)
// and never the alias, which is why the same artifact under a second alias needs no
// second signature. Two things follow, and both are asserted here:
//
//   - the adapter answers only for the alias it was asked about, so a lookup can
//     never hand the verify path some other registration's grant. That would move an
//     approval between registrations while the digest and signature still checked
//     out — bytes approved for one alias running under another;
//   - the answer it returns is otherwise byte-identical between two registrations of
//     the same artifact: same source, same ref, same signature. Nothing about trust
//     changes with the name.
func TestSigilLookupAdapter_AliasIsALookupKeyNotATrustClaim(t *testing.T) {
	const source = "https://example.com/soul-mod-redis.git"
	sig := []byte{9, 9, 9}
	schema := []byte(`{"kind":"soul_module","protocol_version":1}`)
	// The same artifact registered twice. The rows differ ONLY in the alias — which
	// is exactly what the signed block does not cover.
	arts := []sharedhost.SigilArtifact{{SHA256: "aa"}}
	a := NewSigilLookupAdapter(sourceWith(
		&sharedhost.SigilRecord{Alias: "redis", Source: source, Ref: "v1", Artifacts: arts, Signature: sig, Schema: schema},
		&sharedhost.SigilRecord{Alias: "redis-community", Source: source, Ref: "v1", Artifacts: arts, Signature: sig, Schema: schema},
	), nil)

	for _, alias := range []string{"redis", "redis-community"} {
		got, err := a.Get(context.Background(), alias)
		if err != nil {
			t.Fatalf("Get(%q): %v", alias, err)
		}
		if got == nil {
			t.Fatalf("Get(%q) returned nil", alias)
		}
		// The selector held: the adapter never answers with a neighbouring grant.
		if got.Alias != alias {
			t.Errorf("Get(%q) answered with the grant for %q — a lookup that crosses registrations moves an approval", alias, got.Alias)
		}
		// The trust material is identical under either name.
		if got.Source != source || got.Ref != "v1" || string(got.Signature) != string(sig) {
			t.Errorf("Get(%q) trust material differs by alias: %+v", alias, got)
		}
	}

	// An alias nobody registered resolves to nothing rather than to "the closest
	// match" — fail-closed is the only safe answer to an unknown selector.
	if got, _ := a.Get(context.Background(), "redis-typo"); got != nil {
		t.Errorf("an unregistered alias resolved to %+v", got)
	}
}
