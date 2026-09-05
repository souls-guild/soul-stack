package sigil

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
)

// sshSchemaJSON is a canonical ssh_provider document as the serializer produces it —
// no namespace, no name, no publisher, because the format has nowhere to put one.
const sshSchemaJSON = `{"kind":"ssh_provider","protocol_version":1,"provider_kind":"static_key"}`

// fakeSlotReader returns a preset slot (or error) and the active slot's commit_sha (or
// commitErr). commit / commitErr are independent of slot / err: the A1-S4 tests cover
// the branch "the slot reads, but current carries no commit_sha".
type fakeSlotReader struct {
	slot      *pluginhost.SlotContents
	err       error
	commit    string
	commitErr error
}

func (f fakeSlotReader) ReadSlot(string) (*pluginhost.SlotContents, error) {
	return f.slot, f.err
}

func (f fakeSlotReader) SlotCommitSHA(string) (string, error) {
	return f.commit, f.commitErr
}

// fakeStore captures the passed record and returns preset errors/list.
type fakeStore struct {
	inserted   *Sigil
	insertErr  error
	revokedKey [2]string
	revokeErr  error
	listResult []*Sigil
	listErr    error
}

func (s *fakeStore) Insert(_ context.Context, rec *Sigil) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted = rec
	return nil
}

func (s *fakeStore) Revoke(_ context.Context, alias, by string) error {
	s.revokedKey = [2]string{alias, by}
	return s.revokeErr
}

func (s *fakeStore) ListActive(context.Context) ([]*Sigil, error) {
	return s.listResult, s.listErr
}

func testSigner(t *testing.T) *Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	s, err := NewSigner(priv)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func slotFixture() *pluginhost.SlotContents {
	digest := sha256.Sum256([]byte("ssh-binary"))
	return &pluginhost.SlotContents{
		BinaryPath:   "/cache/hetzner/current/hetzner",
		SchemaBytes:  []byte(sshSchemaJSON),
		BinarySHA256: hex.EncodeToString(digest[:]),
	}
}

const testCommitSHA = "0123456789abcdef0123456789abcdef01234567"

func TestService_Allow_Success(t *testing.T) {
	slot := slotFixture()
	store := &fakeStore{}
	signer := testSigner(t)
	svc, err := NewService(ServiceDeps{
		Signer: signer,
		Store:  store,
		Slots:  fakeSlotReader{slot: slot, commit: testCommitSHA},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sha, err := svc.Allow(context.Background(), AllowInput{
		Alias: "hetzner", Source: testSource, Ref: "v1.0.0", CallerAID: "archon-a",
	})
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if sha != slot.BinarySHA256 {
		t.Errorf("returned sha256 = %q, want %q", sha, slot.BinarySHA256)
	}
	if store.inserted == nil {
		t.Fatal("Insert was not called")
	}
	got := store.inserted
	if got.Alias != "hetzner" || got.Source != testSource || got.Ref != "v1.0.0" {
		t.Errorf("inserted identity = (%q,%q,%q)", got.Alias, got.Source, got.Ref)
	}
	if got.SHA256 != slot.BinarySHA256 {
		t.Errorf("inserted sha256 = %q, want %q", got.SHA256, slot.BinarySHA256)
	}
	if got.AllowedByAID != "archon-a" {
		t.Errorf("inserted allowed_by_aid = %q, want archon-a", got.AllowedByAID)
	}
	if got.CommitSHA != testCommitSHA {
		t.Errorf("inserted commit_sha = %q, want %q", got.CommitSHA, testCommitSHA)
	}
	if len(got.Signature) != ed25519.SignatureSize {
		t.Errorf("signature len = %d, want %d", len(got.Signature), ed25519.SignatureSize)
	}
	// commit_sha stays OUTSIDE the signed block, so the Allow signature must equal a
	// direct Sign over (source, ref, binary_sha256, schema) byte for byte.
	wantSig, err := signer.Sign(testSource, "v1.0.0", slot.BinarySHA256, slot.SchemaBytes)
	if err != nil {
		t.Fatalf("Sign (control): %v", err)
	}
	if !bytes.Equal(got.Signature, wantSig) {
		t.Error("Allow signature diverged from direct Sign — commit_sha leaked into the signed block")
	}
	// Schema is the byte-exact slot bytes (the CANON): the SAME bytes that went into
	// Sign, from one ReadSlot. A second, re-derived copy is how the invariant "signed
	// exactly these bytes" decays.
	if !bytes.Equal(got.Schema, slot.SchemaBytes) {
		t.Errorf("inserted schema is not byte-equal slot.SchemaBytes:\n got=%q\nslot=%q", got.Schema, slot.SchemaBytes)
	}
}

// TestService_Allow_RejectsReservedAlias — the reserved list is enforced at
// REGISTRATION, and before the slot is even read: `core.file.present` in a diff must
// keep meaning the engine, not somebody's plugin.
func TestService_Allow_RejectsReservedAlias(t *testing.T) {
	for _, alias := range []string{"core", "keeper", "soul", "sigil", "soul-stack", "CORE", " core "} {
		store := &fakeStore{}
		svc, err := NewService(ServiceDeps{
			Signer: testSigner(t),
			Store:  store,
			Slots:  fakeSlotReader{slot: slotFixture(), commit: testCommitSHA},
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		_, err = svc.Allow(context.Background(), AllowInput{
			Alias: alias, Source: testSource, Ref: "v1.0.0", CallerAID: "archon-a",
		})
		if !errors.Is(err, ErrAliasReserved) {
			t.Errorf("alias %q: err = %v, want ErrAliasReserved", alias, err)
		}
		if store.inserted != nil {
			t.Errorf("alias %q: a reserved alias must not reach the registry", alias)
		}
	}
}

// TestService_Allow_RejectsMalformedAlias — the alias names a cache directory and an
// address level, so path-shaped, dotted and uppercase names are refused too.
func TestService_Allow_RejectsMalformedAlias(t *testing.T) {
	for _, alias := range []string{"", "../escape", "a/b", "Redis", "redis.acl", "9lives"} {
		svc, err := NewService(ServiceDeps{
			Signer: testSigner(t),
			Store:  &fakeStore{},
			Slots:  fakeSlotReader{slot: slotFixture(), commit: testCommitSHA},
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		_, err = svc.Allow(context.Background(), AllowInput{
			Alias: alias, Source: testSource, Ref: "v1.0.0", CallerAID: "archon-a",
		})
		if !errors.Is(err, ErrAliasReserved) {
			t.Errorf("alias %q: err = %v, want ErrAliasReserved", alias, err)
		}
	}
}

// TestService_Allow_SameArtifactTwoAliasesOneSignature is the NIM-438 property from the
// service side: the alias is not in the signed block, so registering the same artifact
// under a second alias reuses the very same signature. Trust follows (source, ref); the
// alias only says where to look it up.
func TestService_Allow_SameArtifactTwoAliasesOneSignature(t *testing.T) {
	slot := slotFixture()
	signer := testSigner(t)

	sigFor := func(alias string) []byte {
		store := &fakeStore{}
		svc, err := NewService(ServiceDeps{
			Signer: signer, Store: store,
			Slots: fakeSlotReader{slot: slot, commit: testCommitSHA},
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		if _, err := svc.Allow(context.Background(), AllowInput{
			Alias: alias, Source: testSource, Ref: "v1.0.0", CallerAID: "archon-a",
		}); err != nil {
			t.Fatalf("Allow(%s): %v", alias, err)
		}
		return store.inserted.Signature
	}

	if !bytes.Equal(sigFor("redis"), sigFor("redis-community")) {
		t.Error("two aliases over the same artifact produced different signatures — the alias leaked into the signed block")
	}
}

func TestService_Allow_PluginNotInCache(t *testing.T) {
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t),
		Store:  &fakeStore{},
		Slots:  fakeSlotReader{err: pluginhost.ErrSlotNotFound},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Allow(context.Background(), AllowInput{Alias: "absent", Source: testSource, Ref: "v1"})
	if !errors.Is(err, ErrPluginNotInCache) {
		t.Fatalf("err = %v, want ErrPluginNotInCache", err)
	}
}

func TestService_Allow_AlreadyActive(t *testing.T) {
	store := &fakeStore{insertErr: ErrSigilAlreadyActive}
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t),
		Store:  store,
		Slots:  fakeSlotReader{slot: slotFixture(), commit: testCommitSHA},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Allow(context.Background(), AllowInput{Alias: "hetzner", Source: testSource, Ref: "v1"})
	if !errors.Is(err, ErrSigilAlreadyActive) {
		t.Fatalf("err = %v, want ErrSigilAlreadyActive", err)
	}
}

// TestService_Allow_NoCommitSHA_FailClosed — slot reads (binary+manifest
// are valid), but current does not carry commit_sha (legacy slot without current / broken
// symlink → SlotCommitSHA returns ErrSlotNotFound). Allow MUST fail-closed —
// ErrPluginNotInCache, NO registry write: permission with unknown origin
// is not recorded (ADR-026(g), commit_sha is mandatory audit metadata on allow).
func TestService_Allow_NoCommitSHA_FailClosed(t *testing.T) {
	store := &fakeStore{}
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t),
		Store:  store,
		Slots:  fakeSlotReader{slot: slotFixture(), commitErr: pluginhost.ErrSlotNotFound},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Allow(context.Background(), AllowInput{
		Alias: "hetzner", Source: testSource, Ref: "v1.0.0", CallerAID: "archon-a",
	})
	if !errors.Is(err, ErrPluginNotInCache) {
		t.Fatalf("err = %v, want ErrPluginNotInCache", err)
	}
	if store.inserted != nil {
		t.Error("Insert must not be called on unresolved commit_sha (fail-closed)")
	}
}

func TestService_Revoke_PassesKey(t *testing.T) {
	store := &fakeStore{}
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t), Store: store, Slots: fakeSlotReader{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.Revoke(context.Background(), "hetzner", "archon-b"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if store.revokedKey != [2]string{"hetzner", "archon-b"} {
		t.Errorf("revoked key = %v", store.revokedKey)
	}
}

func TestService_Revoke_NotFound(t *testing.T) {
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t), Store: &fakeStore{revokeErr: ErrSigilNotFound}, Slots: fakeSlotReader{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	err = svc.Revoke(context.Background(), "hetzner", "archon-b")
	if !errors.Is(err, ErrSigilNotFound) {
		t.Fatalf("err = %v, want ErrSigilNotFound", err)
	}
}

func TestService_List_NoSignatureNoSchema(t *testing.T) {
	now := time.Now()
	store := &fakeStore{listResult: []*Sigil{
		{
			Alias: "hetzner", Source: testSource, Ref: "v1.0.0",
			SHA256:       "deadbeef",
			Signature:    []byte("secret-sig-bytes"),
			Schema:       []byte(sshSchemaJSON),
			AllowedByAID: "archon-a",
			AllowedAt:    now,
		},
	}}
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t), Store: store, Slots: fakeSlotReader{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1", len(views))
	}
	v := views[0]
	if v.Alias != "hetzner" || v.Source != testSource || v.Ref != "v1.0.0" || v.SHA256 != "deadbeef" {
		t.Errorf("view = %+v", v)
	}
	if v.AllowedByAID != "archon-a" || !v.AllowedAt.Equal(now) {
		t.Errorf("view audit-fields = %+v", v)
	}
	// SigilView carries neither the signature nor the schema by design — a structural
	// guarantee. This test pins that List returns exactly SigilView.
}

func TestService_List_NonNilEmpty(t *testing.T) {
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t), Store: &fakeStore{listResult: nil}, Slots: fakeSlotReader{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if views == nil {
		t.Error("List must return non-nil slice")
	}
}

// TestService_SetSigner_AllowUsesNewPrimary — keeper Signer hot-reload (R3-S6):
// after SetSigner, new Allow uses FRESH primary, not the initial one.
func TestService_SetSigner_AllowUsesNewPrimary(t *testing.T) {
	slot := slotFixture()
	store := &fakeStore{}
	oldSigner := testSigner(t)
	svc, err := NewService(ServiceDeps{
		Signer: oldSigner,
		Store:  store,
		Slots:  fakeSlotReader{slot: slot, commit: testCommitSHA},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	newSigner := testSigner(t)
	svc.SetSigner(newSigner)

	if _, err := svc.Allow(context.Background(), AllowInput{
		Alias: "hetzner", Source: testSource, Ref: "v1.0.0", CallerAID: "archon-a",
	}); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	got := store.inserted.Signature

	wantNew, err := newSigner.Sign(testSource, "v1.0.0", slot.BinarySHA256, slot.SchemaBytes)
	if err != nil {
		t.Fatalf("Sign (new): %v", err)
	}
	if !bytes.Equal(got, wantNew) {
		t.Error("Allow signed with NOT new primary after SetSigner")
	}
	wantOld, _ := oldSigner.Sign(testSource, "v1.0.0", slot.BinarySHA256, slot.SchemaBytes)
	if bytes.Equal(got, wantOld) {
		t.Error("Allow still signs with old primary — SetSigner was not applied")
	}
}

// TestService_SetSigner_NilIgnored — replacement with nil is ignored (Allow remains
// functional with the initial Signer).
func TestService_SetSigner_NilIgnored(t *testing.T) {
	slot := slotFixture()
	store := &fakeStore{}
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t),
		Store:  store,
		Slots:  fakeSlotReader{slot: slot, commit: testCommitSHA},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.SetSigner(nil)
	if _, err := svc.Allow(context.Background(), AllowInput{
		Alias: "hetzner", Source: testSource, Ref: "v1.0.0", CallerAID: "archon-a",
	}); err != nil {
		t.Fatalf("Allow after SetSigner(nil): %v", err)
	}
	if store.inserted == nil || len(store.inserted.Signature) != ed25519.SignatureSize {
		t.Error("Allow with initial Signer broke after SetSigner(nil)")
	}
}

// TestService_SetSigner_RaceWithAllow — concurrent SetSigner and Allow without data races
// (atomic.Pointer). Run with -race.
func TestService_SetSigner_RaceWithAllow(t *testing.T) {
	slot := slotFixture()
	svc, err := NewService(ServiceDeps{
		Signer: testSigner(t),
		Store:  &fakeStore{},
		Slots:  fakeSlotReader{slot: slot, commit: testCommitSHA},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			svc.SetSigner(testSigner(t))
		}
	}()
	for i := 0; i < 200; i++ {
		_, _ = svc.Allow(context.Background(), AllowInput{
			Alias: "hetzner", Source: testSource, Ref: "v1.0.0", CallerAID: "archon-a",
		})
	}
	<-done
}

func TestNewService_NilDeps(t *testing.T) {
	signer := testSigner(t)
	cases := map[string]ServiceDeps{
		"nil signer": {Store: &fakeStore{}, Slots: fakeSlotReader{}},
		"nil store":  {Signer: signer, Slots: fakeSlotReader{}},
		"nil slots":  {Signer: signer, Store: &fakeStore{}},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewService(d); err == nil {
				t.Errorf("NewService(%s) must return error", name)
			}
		})
	}
}
