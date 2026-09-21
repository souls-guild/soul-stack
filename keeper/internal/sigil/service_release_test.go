package sigil

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// artifactSlotSource is the publication root the fixture slot was fetched from. The
// slot records it, and `plugin.allow` checks the operator's asserted `source` against
// it before signing.
const artifactSlotSource = "https://nexus.internal/plugins/redis"

// artifactSlotFixture is a slot as the artifact provider leaves it: one file per
// platform, each with its own digest and published path, one disclosure.
func artifactSlotFixture() *pluginhost.SlotContents {
	amd := sha256.Sum256([]byte("redis-linux-amd64"))
	arm := sha256.Sum256([]byte("redis-linux-arm64"))
	return &pluginhost.SlotContents{
		Kind:   sharedplugin.SourceKindArtifact,
		Source: artifactSlotSource,
		Artifacts: []pluginhost.SlotArtifact{
			{OS: "linux", Arch: "amd64", Path: "redis_linux_amd64",
				SHA256: hex.EncodeToString(amd[:]), BinaryPath: "/cache/redis/current/linux-amd64/redis"},
			{OS: "linux", Arch: "arm64", Path: "redis_linux_arm64",
				SHA256: hex.EncodeToString(arm[:]), BinaryPath: "/cache/redis/current/linux-arm64/redis"},
		},
		SchemaBytes: []byte(sshSchemaJSON),
	}
}

// `plugin.allow` on an artifact release approves the WHOLE release in one gesture: one
// row per platform, all under one signature. This is the operator-surface half of the
// decision — an Archon confirms a release, not a hash.
func TestService_Allow_ArtifactReleaseApprovesEveryPlatform(t *testing.T) {
	slot := artifactSlotFixture()
	store := &fakeStore{}
	signer := testSigner(t)
	svc, err := NewService(ServiceDeps{
		Signer: signer,
		Store:  store,
		// commit is set, and must be ignored: this kind has no commit to pin.
		Slots: fakeSlotReader{slot: slot, commit: testCommitSHA},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const base = "https://nexus.internal/plugins/redis"
	approved, err := svc.Allow(context.Background(), AllowInput{
		Alias: "redis", Source: base, Ref: "v1.4.0", CallerAID: "archon-a",
	})
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if approved.Kind != sharedplugin.SourceKindArtifact {
		t.Errorf("kind = %q, want artifact", approved.Kind)
	}
	if len(approved.Artifacts) != 2 {
		t.Fatalf("approved artifacts = %+v, want both platforms", approved.Artifacts)
	}

	got := store.inserted
	if got == nil {
		t.Fatal("Insert was not called")
	}
	if got.Kind != sharedplugin.SourceKindArtifact || len(got.Artifacts) != 2 {
		t.Fatalf("inserted = kind %q, %d artifacts", got.Kind, len(got.Artifacts))
	}
	for i, want := range slot.Artifacts {
		a := got.Artifacts[i]
		if a.OS != want.OS || a.Arch != want.Arch || a.Path != want.Path || a.SHA256 != want.SHA256 {
			t.Errorf("inserted artifact %d = %+v, want %+v", i, a, want)
		}
	}

	// ★ No commit_sha. An artifact release has no commit to pin; writing the slot's
	// directory name into a column an operator reads as provenance would put a value
	// there that resolves to nothing.
	if got.CommitSHA != "" {
		t.Errorf("inserted commit_sha = %q, want empty (an artifact release has no commit)", got.CommitSHA)
	}

	// The signature is over the whole list, byte for byte the same one a direct Sign
	// produces — nothing about the slot leaks into the block beyond what is signed.
	wantSig, err := signer.Sign(base, sharedplugin.SourceKindArtifact, "v1.4.0",
		slot.SigilArtifacts(), slot.SchemaBytes)
	if err != nil {
		t.Fatalf("Sign (control): %v", err)
	}
	if string(got.Signature) != string(wantSig) {
		t.Error("Allow signature diverged from a direct Sign over the release")
	}
	if len(got.Signature) != ed25519.SignatureSize {
		t.Errorf("signature len = %d, want %d", len(got.Signature), ed25519.SignatureSize)
	}
}

// The git kind keeps its audit provenance and keeps failing closed without it. That
// half is unchanged by NIM-793: only the artifact kind is allowed to have no commit.
func TestService_Allow_GitStillRequiresACommit(t *testing.T) {
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t),
		Store:  &fakeStore{},
		Slots:  fakeSlotReader{slot: slotFixture(), commitErr: pluginhost.ErrSlotNotFound},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.Allow(context.Background(), AllowInput{
		Alias: "hetzner", Source: testSource, Ref: "v1.0.0", CallerAID: "archon-a",
	}); err == nil {
		t.Fatal("Allow approved a git slot with no resolvable commit_sha")
	}
}

// A soul_module fetch is answered from ANY row of the grant: an amd64 Soul and an
// arm64 Soul ask for different digests under one approval, and both are legitimate.
func TestService_LookupModuleBinary_AnyPlatformRowOfTheRelease(t *testing.T) {
	slot := artifactSlotFixture()
	slot.SchemaBytes = []byte(soulModuleSchemaJSON)
	rec := &Sigil{
		Alias:     "redis",
		Source:    "https://nexus.internal/plugins/redis",
		Ref:       "v1.4.0",
		Kind:      sharedplugin.SourceKindArtifact,
		Artifacts: slot.SigilArtifacts(),
		Schema:    []byte(soulModuleSchemaJSON),
	}
	svc := lookupService(t,
		&fakeStore{listResult: []*Sigil{rec}},
		mapSlotReader{slots: map[string]*pluginhost.SlotContents{"redis": slot}},
	)

	for _, want := range slot.Artifacts {
		path, err := svc.LookupModuleBinary(context.Background(), want.SHA256)
		if err != nil {
			t.Fatalf("LookupModuleBinary(%s/%s): %v", want.OS, want.Arch, err)
		}
		if path != want.BinaryPath {
			t.Errorf("path = %q, want %q", path, want.BinaryPath)
		}
	}

	// A digest that is in no row of any grant stays refused.
	stray := sha256.Sum256([]byte("never-approved"))
	if _, err := svc.LookupModuleBinary(context.Background(), hex.EncodeToString(stray[:])); err == nil {
		t.Error("LookupModuleBinary served a digest no grant approves")
	}
}

// A projection of the grant for the broadcast has to survive a round trip through the
// artifacts column: what a Soul verifies against is what the column holds, so a
// storage format that reordered or dropped a row would break every grant it touched.
func TestArtifactsColumn_RoundTrips(t *testing.T) {
	slot := artifactSlotFixture()
	raw, err := marshalArtifacts(slot.SigilArtifacts())
	if err != nil {
		t.Fatalf("marshalArtifacts: %v", err)
	}
	back, err := unmarshalArtifacts(raw)
	if err != nil {
		t.Fatalf("unmarshalArtifacts: %v", err)
	}
	want := slot.SigilArtifacts()
	if len(back) != len(want) {
		t.Fatalf("round trip gave %d artifacts, want %d", len(back), len(want))
	}
	for i := range want {
		if back[i] != want[i] {
			t.Errorf("artifact %d = %+v, want %+v", i, back[i], want[i])
		}
	}

	// A column holding a list no signature could exist over is refused rather than
	// repaired: the grant is unusable, and every read path treats it that way.
	for _, bad := range []string{`[]`, `not json`, `[{"os":"linux","arch":"amd64","sha256":"zz"}]`} {
		if _, err := unmarshalArtifacts([]byte(bad)); err == nil {
			t.Errorf("unmarshalArtifacts accepted %q", bad)
		}
	}
}

// The Keeper refuses to sign a `source` it did not fetch from.
//
// `source` arrives as free operator text and goes straight into the signed block, and
// it is half of the address a Soul will later download from. For a release the Keeper
// downloaded, the assertion is checkable — so it is checked, and a mismatch is a 422
// rather than a signature over a statement the Keeper knows to be false. Signing
// `kind` to stop a rewritten catalog redirecting a fetch would buy nothing while the
// other half of the address stayed unverified.
func TestService_Allow_RefusesASourceItDidNotFetchFrom(t *testing.T) {
	slot := artifactSlotFixture()
	store := &fakeStore{}
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t), Store: store,
		Slots: fakeSlotReader{slot: slot, commit: testCommitSHA},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	_, err = svc.Allow(context.Background(), AllowInput{
		Alias: "redis", Source: "http://evil.example/plugins/redis", Ref: "v1.4.0", CallerAID: "archon-a",
	})
	if !errors.Is(err, ErrSourceMismatch) {
		t.Fatalf("err = %v, want ErrSourceMismatch", err)
	}
	if store.inserted != nil {
		t.Error("a grant was written for a source the Keeper never fetched from")
	}

	// The honest assertion still passes — the check is a comparison, not a ban.
	if _, err := svc.Allow(context.Background(), AllowInput{
		Alias: "redis", Source: artifactSlotSource, Ref: "v1.4.0", CallerAID: "archon-a",
	}); err != nil {
		t.Fatalf("Allow with the fetched source: %v", err)
	}
}

// A GIT slot records no source, and there the operator's assertion stays unchecked —
// unchanged behaviour for that kind, which is what this ticket must not disturb. The
// git layout carries no descriptor and nothing in it records the remote, so there is
// nothing to compare against; pretending otherwise would be worse than saying so.
func TestService_Allow_GitSourceStaysOperatorAsserted(t *testing.T) {
	slot := slotFixture()
	if slot.Source != "" {
		t.Fatalf("a git slot must record no source, got %q", slot.Source)
	}
	store := &fakeStore{}
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t), Store: store,
		Slots: fakeSlotReader{slot: slot, commit: testCommitSHA},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.Allow(context.Background(), AllowInput{
		Alias: "hetzner", Source: "https://example.com/anything-the-operator-says.git",
		Ref: "v1.0.0", CallerAID: "archon-a",
	}); err != nil {
		t.Fatalf("git allow must keep accepting an operator-asserted source: %v", err)
	}
	if store.inserted == nil {
		t.Fatal("git allow wrote no grant")
	}
}
