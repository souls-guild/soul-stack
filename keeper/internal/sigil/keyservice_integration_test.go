//go:build integration

// Integration test for KeyService (R3-S7) over real PG: complete operator flow
// introduce → list → set-primary → retire with key-gen and Vault-write (Vault is mocked
// with fake-writer — no Vault in integration stack; here we verify the
// key-gen+PG+publisher coupling, not Vault itself). CRUD invariants (ErrLastActiveKey/
// ErrRetirePrimary/one_primary) — keys_integration_test.go.

package sigil

import (
	"context"
	"testing"
)

// recordingPublisher counts calls to Publish (anchors-changed after mutation).
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

	// 1. Introduce first key as primary.
	r1, err := svc.Introduce(ctx, true, aid)
	if err != nil {
		t.Fatalf("Introduce#1: %v", err)
	}
	if !r1.IsPrimary {
		t.Error("first key must be primary")
	}
	if _, ok := vw.writes["secret/keeper/sigil-keys/"+r1.KeyID]; !ok {
		t.Errorf("Vault did not receive first key's private key")
	}
	if pub.n != 1 {
		t.Errorf("publisher called %d times after Introduce#1, want 1", pub.n)
	}

	// 2. Introduce second key (not primary).
	r2, err := svc.Introduce(ctx, false, aid)
	if err != nil {
		t.Fatalf("Introduce#2: %v", err)
	}

	// 3. List — two active, primary first (r1).
	keys, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("active keys = %d, want 2", len(keys))
	}
	if keys[0].KeyID != r1.KeyID || !keys[0].IsPrimary {
		t.Errorf("primary first: got %s (primary=%v)", keys[0].KeyID, keys[0].IsPrimary)
	}

	// 4. SetPrimary on second key.
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

	// 5. Retire old (now non-primary) first key.
	if err := svc.Retire(ctx, r1.KeyID, aid); err != nil {
		t.Fatalf("Retire(r1): %v", err)
	}
	keys, _ = svc.List(ctx)
	if len(keys) != 1 || keys[0].KeyID != r2.KeyID {
		t.Errorf("after retire: active = %v, want [r2]", keys)
	}

	// Publisher called on each of 4 mutations (2 introduce + set-primary + retire).
	if pub.n != 4 {
		t.Errorf("publisher called %d times, want 4 (2 introduce + set-primary + retire)", pub.n)
	}
}
