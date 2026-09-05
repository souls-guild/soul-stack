package handlers

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// fakeSigilStore — narrow mock of [sigil.Store] for SigilHandler unit tests.
type fakeSigilStore struct {
	inserted   *sigil.Sigil
	insertErr  error
	revokeErr  error
	listResult []*sigil.Sigil
}

func (s *fakeSigilStore) Insert(_ context.Context, rec *sigil.Sigil) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted = rec
	return nil
}

func (s *fakeSigilStore) Revoke(context.Context, string, string) error {
	return s.revokeErr
}

func (s *fakeSigilStore) GetActive(_ context.Context, alias string) (*sigil.Sigil, error) {
	for _, rec := range s.listResult {
		if rec.Alias == alias {
			return rec, nil
		}
	}
	return nil, sigil.ErrSigilNotFound
}

func (s *fakeSigilStore) ListActive(context.Context) ([]*sigil.Sigil, error) {
	return s.listResult, nil
}

// fakeSigilSlots — mock of [sigil.SlotReader].
type fakeSigilSlots struct {
	slot      *pluginhost.SlotContents
	err       error
	commit    string
	commitErr error
}

func (f fakeSigilSlots) ReadSlot(string) (*pluginhost.SlotContents, error) {
	return f.slot, f.err
}

func (f fakeSigilSlots) ArtifactByDigest(string, string) (string, error) {
	return "", pluginhost.ErrSlotNotFound
}

func (f fakeSigilSlots) SlotCommitSHA(string) (string, error) {
	if f.commitErr != nil {
		return "", f.commitErr
	}
	// Default: a successful slot carries a synthetic commit_sha (A1-S4 — current-
	// target). An empty commit is kept only when explicitly set.
	if f.commit == "" && f.slot != nil {
		return "0123456789abcdef0123456789abcdef01234567", nil
	}
	return f.commit, nil
}

// sigilTestSource is the git remote the fixture grants are issued on — with no
// self-name in the artifact, this and the ref are the whole signed identity.
const sigilTestSource = "https://example.com/soul-cloud-hetzner.git"

// listFixtureSHA is a well-formed digest for the list-feed fixture: the store projects
// rows through CanonicalArtifacts, which refuses anything no signature could cover.
const listFixtureSHA = "de4dbeef00000000000000000000000000000000000000000000000000000000"

func sigilSlotFixture() *pluginhost.SlotContents {
	digest := sha256.Sum256([]byte("cloud-binary"))
	return &pluginhost.SlotContents{
		Kind: sharedplugin.SourceKindGit,
		Artifacts: []pluginhost.SlotArtifact{{
			OS: sharedhost.AnyPlatform, Arch: sharedhost.AnyPlatform,
			SHA256: hex.EncodeToString(digest[:]), BinaryPath: "/cache/hetzner/current/hetzner",
		}},
		SchemaBytes: []byte(`{"kind":"ssh_provider","protocol_version":1,"provider_kind":"static_key"}`),
	}
}

func newSigilHandler(t *testing.T, store *fakeSigilStore, slots fakeSigilSlots) *SigilHandler {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer, err := sigil.NewSigner(priv)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	svc, err := sigil.NewService(sigil.ServiceDeps{Signer: signer, Store: store, Slots: slots})
	if err != nil {
		t.Fatalf("sigil.NewService: %v", err)
	}
	return NewSigilHandler(svc, nil)
}

// --- AllowTyped ---

func TestSigilHandler_Allow_201(t *testing.T) {
	slot := sigilSlotFixture()
	store := &fakeSigilStore{}
	h := newSigilHandler(t, store, fakeSigilSlots{slot: slot})

	reply, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
		SigilAllowInput{Alias: "hetzner", Source: sigilTestSource, Ref: "v1.0.0"})
	if err != nil {
		t.Fatalf("AllowTyped: %v", err)
	}
	// The 201 describes a release: kind plus every approved file, not one digest.
	if reply.View.Kind != sharedplugin.SourceKindGit {
		t.Errorf("reply.kind = %q, want %q", reply.View.Kind, sharedplugin.SourceKindGit)
	}
	if len(reply.View.Artifacts) != 1 || reply.View.Artifacts[0].SHA256 != slot.Artifacts[0].SHA256 {
		t.Errorf("reply.artifacts = %+v, want the slot's one row", reply.View.Artifacts)
	}
	if reply.View.Alias != "hetzner" || reply.View.Source != sigilTestSource || reply.View.Ref != "v1.0.0" {
		t.Errorf("reply view = %+v", reply.View)
	}
	if store.inserted == nil || store.inserted.AllowedByAID != "archon-alice" {
		t.Errorf("inserted = %+v", store.inserted)
	}
}

func TestSigilHandler_Allow_EmptyAlias_422(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{}, fakeSigilSlots{slot: sigilSlotFixture()})
	_, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
		SigilAllowInput{Alias: "", Source: sigilTestSource, Ref: "v1.0.0"})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestSigilHandler_Allow_EmptySource_422(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{}, fakeSigilSlots{slot: sigilSlotFixture()})
	_, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
		SigilAllowInput{Alias: "hetzner", Source: "", Ref: "v1.0.0"})
	wantProblem(t, err, problem.TypeValidationFailed)
}

// TestSigilHandler_Allow_ReservedAlias_422 — the reserved list reaches the transport, so
// an operator naming a plugin `core` is told in the 422 rather than discovering it when
// `core.file.present` starts meaning somebody else's code.
func TestSigilHandler_Allow_ReservedAlias_422(t *testing.T) {
	for _, alias := range []string{"core", "keeper", "soul", "soul-stack"} {
		store := &fakeSigilStore{}
		h := newSigilHandler(t, store, fakeSigilSlots{slot: sigilSlotFixture()})
		_, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
			SigilAllowInput{Alias: alias, Source: sigilTestSource, Ref: "v1.0.0"})
		wantProblem(t, err, problem.TypeValidationFailed)
		if store.inserted != nil {
			t.Errorf("alias %q reached the registry", alias)
		}
	}
}

func TestSigilHandler_Allow_SlashInRef_422(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{}, fakeSigilSlots{slot: sigilSlotFixture()})
	_, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
		SigilAllowInput{Alias: "hetzner", Source: sigilTestSource, Ref: "feature/x"})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestSigilHandler_Allow_NotInCache_404(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{}, fakeSigilSlots{err: pluginhost.ErrSlotNotFound})
	_, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
		SigilAllowInput{Alias: "absent", Source: sigilTestSource, Ref: "v1.0.0"})
	wantProblem(t, err, problem.TypePluginNotInCache)
}

func TestSigilHandler_Allow_AlreadyActive_409(t *testing.T) {
	store := &fakeSigilStore{insertErr: sigil.ErrSigilAlreadyActive}
	h := newSigilHandler(t, store, fakeSigilSlots{slot: sigilSlotFixture()})
	_, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
		SigilAllowInput{Alias: "hetzner", Source: sigilTestSource, Ref: "v1.0.0"})
	wantProblem(t, err, problem.TypeSigilActive)
}

// TestSigilHandler_Allow_AliasTaken_409 — the other live-collision sentinel. Same HTTP
// status, different sentence: the operator's fix is a different alias, not a revoke.
func TestSigilHandler_Allow_AliasTaken_409(t *testing.T) {
	store := &fakeSigilStore{insertErr: sigil.ErrAliasAlreadyRegistered}
	h := newSigilHandler(t, store, fakeSigilSlots{slot: sigilSlotFixture()})
	_, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
		SigilAllowInput{Alias: "hetzner", Source: sigilTestSource, Ref: "v1.0.0"})
	wantProblem(t, err, problem.TypeSigilActive)
}

// --- ListTyped ---

func TestSigilHandler_List_200_NoSignatureNoSchema(t *testing.T) {
	store := &fakeSigilStore{listResult: []*sigil.Sigil{
		{
			Alias: "hetzner", Source: sigilTestSource, Ref: "v1.0.0",
			Kind:         sharedplugin.SourceKindGit,
			Artifacts:    []sharedhost.SigilArtifact{{SHA256: listFixtureSHA}},
			Signature:    []byte("secret-bytes"),
			Schema:       []byte(`{"kind":"ssh_provider","protocol_version":1}`),
			AllowedByAID: "archon-alice",
			AllowedAt:    time.Now(),
		},
	}}
	h := newSigilHandler(t, store, fakeSigilSlots{})

	page, err := h.ListTyped(context.Background())
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if len(page.Items) != 1 || len(page.Items[0].Artifacts) != 1 ||
		page.Items[0].Artifacts[0].SHA256 != listFixtureSHA {
		t.Fatalf("items = %+v", page.Items)
	}
	// The domain projection (SigilView) carries neither the signature nor the schema —
	// crypto material and a large document do not leave the service boundary
	// (regression guard).
	it := page.Items[0]
	if it.Alias != "hetzner" || it.Source != sigilTestSource || it.Ref != "v1.0.0" {
		t.Errorf("item key = %+v", it)
	}
	// Active record: RevokedAt nil → the native type omits revoked_at (omitempty).
	if it.RevokedAt != nil {
		t.Errorf("active entry must not carry revoked_at: %v", it.RevokedAt)
	}
}

func TestSigilHandler_List_200_EmptyNonNil(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{listResult: nil}, fakeSigilSlots{})
	page, err := h.ListTyped(context.Background())
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	// non-nil [] (not nil): the native projection serializes as `[]`, not null.
	if page.Items == nil {
		t.Errorf("empty list must be non-nil [], got nil")
	}
	if len(page.Items) != 0 {
		t.Errorf("items = %d, want 0", len(page.Items))
	}
}

// --- RevokeTyped ---

func TestSigilHandler_Revoke_204(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{}, fakeSigilSlots{})
	if _, err := h.RevokeTyped(context.Background(), claimsFor("archon-alice"), "hetzner"); err != nil {
		t.Fatalf("RevokeTyped: %v", err)
	}
}

func TestSigilHandler_Revoke_NotFound_404(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{revokeErr: sigil.ErrSigilNotFound}, fakeSigilSlots{})
	_, err := h.RevokeTyped(context.Background(), claimsFor("archon-alice"), "hetzner")
	wantProblem(t, err, problem.TypeSigilNotFound)
}

func TestSigilHandler_Revoke_BadSegment_422(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{}, fakeSigilSlots{})
	for _, alias := range []string{"", "..", "a/b", "Hetzner", "core"} {
		_, err := h.RevokeTyped(context.Background(), claimsFor("archon-alice"), alias)
		wantProblem(t, err, problem.TypeValidationFailed)
	}
}
