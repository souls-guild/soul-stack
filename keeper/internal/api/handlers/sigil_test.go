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
)

// fakeSigilStore — узкий мок [sigil.Store] для unit-тестов SigilHandler-а.
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

func (s *fakeSigilStore) Revoke(context.Context, string, string, string, string) error {
	return s.revokeErr
}

func (s *fakeSigilStore) ListActive(context.Context) ([]*sigil.Sigil, error) {
	return s.listResult, nil
}

// fakeSigilSlots — мок [sigil.SlotReader].
type fakeSigilSlots struct {
	slot      *pluginhost.SlotContents
	err       error
	commit    string
	commitErr error
}

func (f fakeSigilSlots) ReadSlot(string, string) (*pluginhost.SlotContents, error) {
	return f.slot, f.err
}

func (f fakeSigilSlots) SlotCommitSHA(string, string) (string, error) {
	if f.commitErr != nil {
		return "", f.commitErr
	}
	// Дефолт: успешный слот несёт синтетический commit_sha (A1-S4 — current-
	// target). Пустой commit оставляем только при явном задании.
	if f.commit == "" && f.slot != nil {
		return "0123456789abcdef0123456789abcdef01234567", nil
	}
	return f.commit, nil
}

func sigilSlotFixture() *pluginhost.SlotContents {
	digest := sha256.Sum256([]byte("cloud-binary"))
	return &pluginhost.SlotContents{
		BinaryPath:    "/cache/cloud-hetzner/soul-cloud-hetzner",
		ManifestBytes: []byte("kind: cloud_driver\nprotocol_version: 1\nnamespace: cloud\nname: hetzner\nspec:\n  profile_schema:\n    type: object\n"),
		BinarySHA256:  hex.EncodeToString(digest[:]),
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
		SigilAllowInput{Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0"})
	if err != nil {
		t.Fatalf("AllowTyped: %v", err)
	}
	if reply.View.SHA256 != slot.BinarySHA256 {
		t.Errorf("reply.sha256 = %q, want %q", reply.View.SHA256, slot.BinarySHA256)
	}
	if reply.View.Namespace != "cloud" || reply.View.Name != "hetzner" || reply.View.Ref != "v1.0.0" {
		t.Errorf("reply view = %+v", reply.View)
	}
	if store.inserted == nil || store.inserted.AllowedByAID != "archon-alice" {
		t.Errorf("inserted = %+v", store.inserted)
	}
}

func TestSigilHandler_Allow_EmptyName_422(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{}, fakeSigilSlots{slot: sigilSlotFixture()})
	_, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
		SigilAllowInput{Namespace: "cloud", Name: "", Ref: "v1.0.0"})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestSigilHandler_Allow_SlashInRef_422(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{}, fakeSigilSlots{slot: sigilSlotFixture()})
	_, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
		SigilAllowInput{Namespace: "cloud", Name: "hetzner", Ref: "feature/x"})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestSigilHandler_Allow_NotInCache_404(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{}, fakeSigilSlots{err: pluginhost.ErrSlotNotFound})
	_, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
		SigilAllowInput{Namespace: "cloud", Name: "absent", Ref: "v1.0.0"})
	wantProblem(t, err, problem.TypePluginNotInCache)
}

func TestSigilHandler_Allow_AlreadyActive_409(t *testing.T) {
	store := &fakeSigilStore{insertErr: sigil.ErrSigilAlreadyActive}
	h := newSigilHandler(t, store, fakeSigilSlots{slot: sigilSlotFixture()})
	_, err := h.AllowTyped(context.Background(), claimsFor("archon-alice"),
		SigilAllowInput{Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0"})
	wantProblem(t, err, problem.TypeSigilActive)
}

// --- ListTyped ---

func TestSigilHandler_List_200_NoSignatureNoManifest(t *testing.T) {
	store := &fakeSigilStore{listResult: []*sigil.Sigil{
		{
			Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0",
			SHA256:       "deadbeef",
			Signature:    []byte("secret-bytes"),
			Manifest:     []byte(`{"kind":"cloud_driver"}`),
			AllowedByAID: "archon-alice",
			AllowedAt:    time.Now(),
		},
	}}
	h := newSigilHandler(t, store, fakeSigilSlots{})

	page, err := h.ListTyped(context.Background())
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].SHA256 != "deadbeef" {
		t.Fatalf("items = %+v", page.Items)
	}
	// Доменная проекция (SigilView) НЕ несёт signature/manifest-полей — крипто-
	// материал/крупный JSONB не покидают service-границу (guard на регресс).
	it := page.Items[0]
	if it.Namespace != "cloud" || it.Name != "hetzner" || it.Ref != "v1.0.0" {
		t.Errorf("item key = %+v", it)
	}
	// Активная запись: RevokedAt nil → native-тип опустит revoked_at (omitempty).
	if it.RevokedAt != nil {
		t.Errorf("активная запись не должна нести revoked_at: %v", it.RevokedAt)
	}
}

func TestSigilHandler_List_200_EmptyNonNil(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{listResult: nil}, fakeSigilSlots{})
	page, err := h.ListTyped(context.Background())
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	// non-nil [] (не nil): native проекция сериализует как `[]`, не null.
	if page.Items == nil {
		t.Errorf("пустой список должен быть non-nil [], получен nil")
	}
	if len(page.Items) != 0 {
		t.Errorf("items = %d, want 0", len(page.Items))
	}
}

// --- RevokeTyped ---

func TestSigilHandler_Revoke_204(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{}, fakeSigilSlots{})
	if _, err := h.RevokeTyped(context.Background(), claimsFor("archon-alice"), "cloud", "hetzner", "v1.0.0"); err != nil {
		t.Fatalf("RevokeTyped: %v", err)
	}
}

func TestSigilHandler_Revoke_NotFound_404(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{revokeErr: sigil.ErrSigilNotFound}, fakeSigilSlots{})
	_, err := h.RevokeTyped(context.Background(), claimsFor("archon-alice"), "cloud", "hetzner", "v9.9.9")
	wantProblem(t, err, problem.TypeSigilNotFound)
}

func TestSigilHandler_Revoke_BadSegment_422(t *testing.T) {
	h := newSigilHandler(t, &fakeSigilStore{}, fakeSigilSlots{})
	_, err := h.RevokeTyped(context.Background(), claimsFor("archon-alice"), "cloud", "hetzner", "..")
	wantProblem(t, err, problem.TypeValidationFailed)
}
