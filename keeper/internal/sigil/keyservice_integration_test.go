//go:build integration

// Integration-тест KeyService (R3-S7) поверх реального PG: полный operator-flow
// introduce → list → set-primary → retire с key-gen и Vault-write (Vault мокается
// fake-writer-ом — у integration-стека Vault нет; здесь проверяется связка
// key-gen+PG+publisher, не сам Vault). CRUD-инварианты (ErrLastActiveKey/
// ErrRetirePrimary/one_primary) — keys_integration_test.go.

package sigil

import (
	"context"
	"testing"
)

// recordingPublisher считает вызовы Publish (anchors-changed после мутации).
type recordingPublisher struct{ n int }

func (p *recordingPublisher) Publish(context.Context) { p.n++ }

func newKeyServiceIT(t *testing.T) (*KeyService, *captureVaultWriter, *recordingPublisher) {
	t.Helper()
	vw := newCaptureVaultWriter()
	svc, err := NewKeyService(KeyServiceDeps{
		Pool:          integrationPool,
		Vault:         vw,
		VaultKeyMount: "secret/keeper/sigil-keys",
	})
	if err != nil {
		t.Fatalf("NewKeyService: %v", err)
	}
	pub := &recordingPublisher{}
	svc.SetPublisher(pub)
	return svc, vw, pub
}

func TestIntegration_KeyService_FullRotationFlow(t *testing.T) {
	aid := resetKeys(t)
	svc, vw, pub := newKeyServiceIT(t)
	ctx := context.Background()

	// 1. Introduce первого ключа primary.
	r1, err := svc.Introduce(ctx, true, aid)
	if err != nil {
		t.Fatalf("Introduce#1: %v", err)
	}
	if !r1.IsPrimary {
		t.Error("первый ключ должен быть primary")
	}
	if _, ok := vw.writes["secret/keeper/sigil-keys/"+r1.KeyID]; !ok {
		t.Errorf("Vault не получил приватник первого ключа")
	}
	if pub.n != 1 {
		t.Errorf("publisher вызван %d раз после Introduce#1, want 1", pub.n)
	}

	// 2. Introduce второго ключа (не primary).
	r2, err := svc.Introduce(ctx, false, aid)
	if err != nil {
		t.Fatalf("Introduce#2: %v", err)
	}

	// 3. List — два active, primary первым (r1).
	keys, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("active keys = %d, want 2", len(keys))
	}
	if keys[0].KeyID != r1.KeyID || !keys[0].IsPrimary {
		t.Errorf("primary первым: got %s (primary=%v)", keys[0].KeyID, keys[0].IsPrimary)
	}

	// 4. SetPrimary на второй ключ.
	if err := svc.SetPrimary(ctx, r2.KeyID, aid); err != nil {
		t.Fatalf("SetPrimary(r2): %v", err)
	}
	prim, err := GetPrimaryKey(ctx, integrationPool)
	if err != nil {
		t.Fatalf("GetPrimary: %v", err)
	}
	if prim.KeyID != r2.KeyID {
		t.Errorf("primary = %s, want r2 %s", prim.KeyID, r2.KeyID)
	}

	// 5. Retire старого (теперь не primary) первого ключа.
	if err := svc.Retire(ctx, r1.KeyID, aid); err != nil {
		t.Fatalf("Retire(r1): %v", err)
	}
	keys, _ = svc.List(ctx)
	if len(keys) != 1 || keys[0].KeyID != r2.KeyID {
		t.Errorf("после retire: active = %v, want [r2]", keys)
	}

	// Publisher вызван на каждой из 4 мутаций (2 introduce + set-primary + retire).
	if pub.n != 4 {
		t.Errorf("publisher вызван %d раз, want 4 (2 introduce + set-primary + retire)", pub.n)
	}
}
