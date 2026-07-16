package sigil

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
)

const cloudManifestYAML = `kind: cloud_driver
protocol_version: 1
namespace: cloud
name: hetzner
spec:
  profile_schema:
    type: object
`

// fakeSlotReader returns a preset slot (or error) and commit_sha
// of the active slot (or commitErr). commit / commitErr are independent of slot / err:
// tests A1-S4 check the branch "slot reads but current does not carry commit_sha".
type fakeSlotReader struct {
	slot      *pluginhost.SlotContents
	err       error
	commit    string
	commitErr error
}

func (f fakeSlotReader) ReadSlot(string, string) (*pluginhost.SlotContents, error) {
	return f.slot, f.err
}

func (f fakeSlotReader) SlotCommitSHA(string, string) (string, error) {
	return f.commit, f.commitErr
}

// fakeStore captures the passed record and returns preset errors/list.
type fakeStore struct {
	inserted   *Sigil
	insertErr  error
	revokedKey [4]string
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

func (s *fakeStore) Revoke(_ context.Context, ns, name, ref, by string) error {
	s.revokedKey = [4]string{ns, name, ref, by}
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
	digest := sha256.Sum256([]byte("cloud-binary"))
	return &pluginhost.SlotContents{
		BinaryPath:    "/cache/cloud-hetzner/soul-cloud-hetzner",
		ManifestBytes: []byte(cloudManifestYAML),
		BinarySHA256:  hex.EncodeToString(digest[:]),
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
		Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0", CallerAID: "archon-a",
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
	if got.Namespace != "cloud" || got.Name != "hetzner" || got.Ref != "v1.0.0" {
		t.Errorf("inserted key = (%q,%q,%q)", got.Namespace, got.Name, got.Ref)
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
	// Signature A1-S4 does NOT change: commit_sha is outside signed block (DST v1).
	// Allow signature MUST match byte-for-byte with direct Sign over the same block
	// (ns, name, ref, binary_sha256, manifest_bytes) — without commit_sha.
	wantSig, err := signer.Sign("cloud", "hetzner", "v1.0.0", slot.BinarySHA256, slot.ManifestBytes)
	if err != nil {
		t.Fatalf("Sign (control): %v", err)
	}
	if !bytes.Equal(got.Signature, wantSig) {
		t.Error("Allow signature diverged from direct Sign — commit_sha leaked into signed block")
	}
	// Manifest is stored as JSON (not raw YAML).
	var m map[string]any
	if err := json.Unmarshal(got.Manifest, &m); err != nil {
		t.Fatalf("inserted manifest not JSON: %v (%q)", err, got.Manifest)
	}
	if m["kind"] != "cloud_driver" {
		t.Errorf("manifest JSON kind = %v, want cloud_driver", m["kind"])
	}
	// ManifestRaw is byte-exact raw bytes of the slot (CANON), not a JSONB projection:
	// same bytes that went into Sign (single ReadSlot).
	if !bytes.Equal(got.ManifestRaw, slot.ManifestBytes) {
		t.Errorf("inserted manifest_raw is not byte-equal slot.ManifestBytes:\n raw=%q\nslot=%q", got.ManifestRaw, slot.ManifestBytes)
	}
	if bytes.Equal(got.ManifestRaw, got.Manifest) {
		t.Error("manifest_raw matched JSONB projection — raw MUST carry raw YAML, not JSON")
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
	_, err = svc.Allow(context.Background(), AllowInput{Namespace: "cloud", Name: "absent", Ref: "v1"})
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
	_, err = svc.Allow(context.Background(), AllowInput{Namespace: "cloud", Name: "hetzner", Ref: "v1"})
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
		Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0", CallerAID: "archon-a",
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
	if err := svc.Revoke(context.Background(), "cloud", "hetzner", "v1.0.0", "archon-b"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if store.revokedKey != [4]string{"cloud", "hetzner", "v1.0.0", "archon-b"} {
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
	err = svc.Revoke(context.Background(), "cloud", "hetzner", "v1", "archon-b")
	if !errors.Is(err, ErrSigilNotFound) {
		t.Fatalf("err = %v, want ErrSigilNotFound", err)
	}
}

func TestService_List_NoSignatureNoManifest(t *testing.T) {
	now := time.Now()
	store := &fakeStore{listResult: []*Sigil{
		{
			Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0",
			SHA256:       "deadbeef",
			Signature:    []byte("secret-sig-bytes"),
			Manifest:     []byte(`{"kind":"cloud_driver"}`),
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
	if v.Namespace != "cloud" || v.Name != "hetzner" || v.Ref != "v1.0.0" || v.SHA256 != "deadbeef" {
		t.Errorf("view = %+v", v)
	}
	if v.AllowedByAID != "archon-a" || !v.AllowedAt.Equal(now) {
		t.Errorf("view audit-fields = %+v", v)
	}
	// SigilView does not carry signature/manifest by design — structural guarantee. Test
	// ensures List returns exactly SigilView (without these fields).
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
		Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0", CallerAID: "archon-a",
	}); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	got := store.inserted.Signature

	wantNew, err := newSigner.Sign("cloud", "hetzner", "v1.0.0", slot.BinarySHA256, slot.ManifestBytes)
	if err != nil {
		t.Fatalf("Sign (new): %v", err)
	}
	if !bytes.Equal(got, wantNew) {
		t.Error("Allow signed with NOT new primary after SetSigner")
	}
	wantOld, _ := oldSigner.Sign("cloud", "hetzner", "v1.0.0", slot.BinarySHA256, slot.ManifestBytes)
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
		Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0", CallerAID: "archon-a",
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
			Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0", CallerAID: "archon-a",
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
