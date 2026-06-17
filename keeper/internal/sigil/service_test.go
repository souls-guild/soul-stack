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

// fakeSlotReader возвращает заранее заданный слот (или ошибку) и commit_sha
// активного слота (или commitErr). commit / commitErr независимы от slot / err:
// тесты A1-S4 проверяют ветку «слот читается, но current не несёт commit_sha».
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

// fakeStore фиксирует переданную запись и отдаёт заранее заданные ошибки/ленту.
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
		t.Fatal("Insert не вызван")
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
	// Подпись A1-S4 НЕ изменилась: commit_sha вне подписываемого блока (DST v1).
	// Подпись Allow обязана совпасть byte-в-byte с прямым Sign над тем же блоком
	// (ns, name, ref, binary_sha256, manifest_bytes) — без commit_sha.
	wantSig, err := signer.Sign("cloud", "hetzner", "v1.0.0", slot.BinarySHA256, slot.ManifestBytes)
	if err != nil {
		t.Fatalf("Sign (контрольная): %v", err)
	}
	if !bytes.Equal(got.Signature, wantSig) {
		t.Error("подпись Allow разошлась с прямым Sign — commit_sha просочился в подписываемый блок")
	}
	// Manifest хранится JSON-ом (не сырым YAML).
	var m map[string]any
	if err := json.Unmarshal(got.Manifest, &m); err != nil {
		t.Fatalf("inserted manifest не JSON: %v (%q)", err, got.Manifest)
	}
	if m["kind"] != "cloud_driver" {
		t.Errorf("manifest JSON kind = %v, want cloud_driver", m["kind"])
	}
	// ManifestRaw — byte-exact сырые байты слота (КАНОН), а НЕ JSONB-проекция:
	// те же байты, что ушли в Sign (единый ReadSlot).
	if !bytes.Equal(got.ManifestRaw, slot.ManifestBytes) {
		t.Errorf("inserted manifest_raw не byte-equal slot.ManifestBytes:\n raw=%q\nslot=%q", got.ManifestRaw, slot.ManifestBytes)
	}
	if bytes.Equal(got.ManifestRaw, got.Manifest) {
		t.Error("manifest_raw совпал с JSONB-проекцией — raw обязан нести сырой YAML, не JSON")
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

// TestService_Allow_NoCommitSHA_FailClosed — слот читается (бинарь+manifest
// валидны), но current не несёт commit_sha (legacy-слот без current / битый
// symlink → SlotCommitSHA даёт ErrSlotNotFound). Allow обязан fail-closed —
// ErrPluginNotInCache, БЕЗ записи в реестр: допуск с неизвестным происхождением
// не фиксируется (ADR-026(g), commit_sha — обязательная audit-метка при allow).
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
		t.Error("Insert не должен вызываться при нерезолвленном commit_sha (fail-closed)")
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
		t.Errorf("view audit-поля = %+v", v)
	}
	// SigilView не несёт signature/manifest по типу — структурная гарантия. Тест
	// фиксирует, что List возвращает именно SigilView (без этих полей).
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
		t.Error("List должен возвращать non-nil slice")
	}
}

// TestService_SetSigner_AllowUsesNewPrimary — keeper Signer hot-reload (R3-S6):
// после SetSigner новые Allow подписываются СВЕЖИМ primary, а не стартовым.
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
		t.Error("Allow подписал НЕ новым primary после SetSigner")
	}
	wantOld, _ := oldSigner.Sign("cloud", "hetzner", "v1.0.0", slot.BinarySHA256, slot.ManifestBytes)
	if bytes.Equal(got, wantOld) {
		t.Error("Allow всё ещё подписывает старым primary — SetSigner не применился")
	}
}

// TestService_SetSigner_NilIgnored — подмена на nil игнорируется (Allow остаётся
// рабочим со стартовым Signer-ом).
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
		t.Fatalf("Allow после SetSigner(nil): %v", err)
	}
	if store.inserted == nil || len(store.inserted.Signature) != ed25519.SignatureSize {
		t.Error("Allow со стартовым Signer-ом сломался после SetSigner(nil)")
	}
}

// TestService_SetSigner_RaceWithAllow — конкурентные SetSigner и Allow без гонок
// данных (atomic.Pointer). Запускать с -race.
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
				t.Errorf("NewService(%s) должен вернуть ошибку", name)
			}
		})
	}
}
