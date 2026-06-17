//go:build integration

// Integration-тесты keys-CRUD-а (sigil_signing_keys, миграция 037) через
// testcontainers-go. TestMain/run/integrationPool/requireDocker — общие на
// пакет (объявлены в store_integration_test.go); здесь только свой reset для
// таблицы ключей и проверки инвариантов на реальном PG.

package sigil

import (
	"context"
	"errors"
	"testing"
)

// keysTestAID — bootstrap-оператор (created_by_aid IS NULL) для FK keys-тестов.
// ОБЩИЙ с store_integration_test.go (reset): partial unique
// operators_first_archon_idx допускает лишь ОДНУ NULL-строку на всю таблицу
// operators, а пакетная БД одна (TestMain). Завести второй NULL-оператор нельзя
// — поэтому keys- и store-тесты делят один bootstrap-AID (ON CONFLICT DO NOTHING
// по PK делает оба seed-а идемпотентными независимо от порядка тестов).
const keysTestAID = "archon-sigil-test"

// resetKeys стирает sigil_signing_keys и идемпотентно гарантирует общий
// bootstrap-оператор для FK introduced_by_aid / retired_by_aid.
func resetKeys(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx, `TRUNCATE TABLE sigil_signing_keys RESTART IDENTITY`); err != nil {
		t.Fatalf("TRUNCATE sigil_signing_keys: %v", err)
	}
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO operators (aid, display_name, auth_method, created_by_aid)
		 VALUES ($1, 'Sigil Test', 'jwt', NULL)
		 ON CONFLICT (aid) DO NOTHING`, keysTestAID)
	if err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	return keysTestAID
}

// introduce — хелпер: вводит active-ключ с уникальным key_id.
func introduce(t *testing.T, keyID string, makePrimary bool, aid string) *SigningKey {
	t.Helper()
	by := aid
	k, err := Introduce(context.Background(), integrationPool, keyID,
		"-----BEGIN PUBLIC KEY-----\n"+keyID+"\n-----END PUBLIC KEY-----\n",
		"vault:secret/keeper/sigil-"+keyID, makePrimary, &by)
	if err != nil {
		t.Fatalf("Introduce(%s, primary=%v): %v", keyID, makePrimary, err)
	}
	return k
}

func TestIntegration_Introduce_Basic(t *testing.T) {
	aid := resetKeys(t)

	k := introduce(t, "k1", true, aid)
	if k.ID == 0 {
		t.Error("Introduce did not populate ID")
	}
	if k.IntroducedAt.IsZero() {
		t.Error("Introduce did not populate IntroducedAt")
	}
	if !k.IsPrimary {
		t.Error("makePrimary=true: ключ должен быть primary")
	}
	if k.Status != "active" {
		t.Errorf("status = %q, want active", k.Status)
	}
	if k.IntroducedByAID == nil || *k.IntroducedByAID != aid {
		t.Errorf("introduced_by_aid = %v, want %q", k.IntroducedByAID, aid)
	}
}

// TestIntegration_Introduce_Duplicate — повторный key_id → ErrKeyAlreadyExists.
func TestIntegration_Introduce_Duplicate(t *testing.T) {
	aid := resetKeys(t)
	introduce(t, "dup", false, aid)

	by := aid
	_, err := Introduce(context.Background(), integrationPool, "dup",
		"PEM", "vault:secret/keeper/sigil-dup", false, &by)
	if !errors.Is(err, ErrKeyAlreadyExists) {
		t.Fatalf("повторный key_id: err = %v, want ErrKeyAlreadyExists", err)
	}
}

// TestIntegration_Introduce_MakePrimary_DemotesPrevious — второй makePrimary
// снимает primary с первого: ровно один primary среди active.
func TestIntegration_Introduce_MakePrimary_DemotesPrevious(t *testing.T) {
	aid := resetKeys(t)
	introduce(t, "first", true, aid)
	introduce(t, "second", true, aid)

	prim, err := GetPrimaryKey(context.Background(), integrationPool)
	if err != nil {
		t.Fatalf("GetPrimary: %v", err)
	}
	if prim.KeyID != "second" {
		t.Errorf("primary = %q, want second (последний makePrimary)", prim.KeyID)
	}
	assertExactlyOnePrimary(t)
}

func TestIntegration_SetPrimary_SwitchesAtomically(t *testing.T) {
	aid := resetKeys(t)
	introduce(t, "a", true, aid)
	introduce(t, "b", false, aid)

	if err := SetPrimary(context.Background(), integrationPool, "b", aid); err != nil {
		t.Fatalf("SetPrimary(b): %v", err)
	}
	prim, err := GetPrimaryKey(context.Background(), integrationPool)
	if err != nil {
		t.Fatalf("GetPrimary: %v", err)
	}
	if prim.KeyID != "b" {
		t.Errorf("primary = %q, want b", prim.KeyID)
	}
	assertExactlyOnePrimary(t)
}

// TestIntegration_SetPrimary_NotFound — несуществующий ключ → ErrKeyNotFound.
func TestIntegration_SetPrimary_NotFound(t *testing.T) {
	aid := resetKeys(t)
	introduce(t, "only", true, aid)

	err := SetPrimary(context.Background(), integrationPool, "ghost", aid)
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("SetPrimary(ghost): err = %v, want ErrKeyNotFound", err)
	}
}

// TestIntegration_SetPrimary_Retired — retired-ключ primary стать не может.
func TestIntegration_SetPrimary_Retired(t *testing.T) {
	aid := resetKeys(t)
	introduce(t, "p", true, aid)
	introduce(t, "x", false, aid)
	if err := Retire(context.Background(), integrationPool, "x", aid); err != nil {
		t.Fatalf("Retire(x): %v", err)
	}
	err := SetPrimary(context.Background(), integrationPool, "x", aid)
	if !errors.Is(err, ErrKeyRetired) {
		t.Fatalf("SetPrimary(retired): err = %v, want ErrKeyRetired", err)
	}
}

// TestIntegration_Retire_LastActive — нельзя retire последний active.
func TestIntegration_Retire_LastActive(t *testing.T) {
	aid := resetKeys(t)
	// Единственный active, не primary (чтобы дойти до проверки last-active,
	// а не упереться в ErrRetirePrimary).
	introduce(t, "solo", false, aid)

	err := Retire(context.Background(), integrationPool, "solo", aid)
	if !errors.Is(err, ErrLastActiveKey) {
		t.Fatalf("Retire(solo): err = %v, want ErrLastActiveKey", err)
	}
}

// TestIntegration_Retire_Primary — нельзя retire primary напрямую.
func TestIntegration_Retire_Primary(t *testing.T) {
	aid := resetKeys(t)
	introduce(t, "prim", true, aid)
	introduce(t, "spare", false, aid)

	err := Retire(context.Background(), integrationPool, "prim", aid)
	if !errors.Is(err, ErrRetirePrimary) {
		t.Fatalf("Retire(primary): err = %v, want ErrRetirePrimary", err)
	}
}

// TestIntegration_Retire_Success — retire не-primary при наличии других active.
func TestIntegration_Retire_Success(t *testing.T) {
	aid := resetKeys(t)
	introduce(t, "keep", true, aid)
	introduce(t, "drop", false, aid)

	if err := Retire(context.Background(), integrationPool, "drop", aid); err != nil {
		t.Fatalf("Retire(drop): %v", err)
	}
	active, err := ListActiveKeys(context.Background(), integrationPool)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 1 || active[0].KeyID != "keep" {
		t.Fatalf("ListActive после retire = %v, want [keep]", keyIDs(active))
	}
	// retired-ключ повторно retire → ErrKeyNotFound (active-записи нет).
	if err := Retire(context.Background(), integrationPool, "drop", aid); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("повторный Retire(drop): err = %v, want ErrKeyNotFound", err)
	}
}

// TestIntegration_ListActive_Order — primary первым, далее по introduced_at.
func TestIntegration_ListActive_Order(t *testing.T) {
	aid := resetKeys(t)
	introduce(t, "alpha", false, aid)
	introduce(t, "beta", true, aid) // primary
	introduce(t, "gamma", false, aid)

	active, err := ListActiveKeys(context.Background(), integrationPool)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 3 {
		t.Fatalf("ListActive len = %d, want 3", len(active))
	}
	if !active[0].IsPrimary || active[0].KeyID != "beta" {
		t.Errorf("первый = %q (primary=%v), want beta primary", active[0].KeyID, active[0].IsPrimary)
	}
}

// TestIntegration_GetPrimary_Empty — пустой набор → ErrKeyNotFound.
func TestIntegration_GetPrimary_Empty(t *testing.T) {
	resetKeys(t)
	_, err := GetPrimaryKey(context.Background(), integrationPool)
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("GetPrimaryKey(empty): err = %v, want ErrKeyNotFound", err)
	}
}

// TestIntegration_OnePrimaryIndex — partial unique index не даёт двум active быть
// primary одновременно (прямой INSERT в обход CRUD-tx).
func TestIntegration_OnePrimaryIndex(t *testing.T) {
	aid := resetKeys(t)
	introduce(t, "primA", true, aid)

	ctx := context.Background()
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO sigil_signing_keys (key_id, pubkey_pem, vault_ref, is_primary)
		 VALUES ('primB', 'PEM', 'vault:x', true)`)
	if err == nil {
		t.Fatal("второй active-primary должен нарушить partial unique index")
	}
}

// TestIntegration_FK_SetNull — удаление оператора обнуляет introduced_by_aid
// (ON DELETE SET NULL), строка ключа сохраняется.
func TestIntegration_FK_SetNull(t *testing.T) {
	// resetKeys сеет bootstrap-оператора (created_by_aid IS NULL). FK-оператор
	// ссылается на него: partial unique operators_first_archon_idx допускает
	// только ОДНУ NULL-строку, второй bootstrap здесь невозможен.
	bootstrap := resetKeys(t)
	ctx := context.Background()
	const aid = "archon-keys-fk"
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO operators (aid, display_name, auth_method, created_by_aid)
		 VALUES ($1, 'FK Test', 'jwt', $2) ON CONFLICT (aid) DO NOTHING`, aid, bootstrap); err != nil {
		t.Fatalf("seed fk operator: %v", err)
	}
	introduce(t, "fkkey", true, aid)

	if _, err := integrationPool.Exec(ctx, `DELETE FROM operators WHERE aid = $1`, aid); err != nil {
		t.Fatalf("delete operator: %v", err)
	}
	k, err := GetPrimaryKey(ctx, integrationPool)
	if err != nil {
		t.Fatalf("GetPrimary after operator delete: %v", err)
	}
	if k.KeyID != "fkkey" {
		t.Fatalf("ключ должен пережить удаление оператора; got %q", k.KeyID)
	}
	if k.IntroducedByAID != nil {
		t.Errorf("introduced_by_aid = %v, want nil (ON DELETE SET NULL)", k.IntroducedByAID)
	}
}

// assertExactlyOnePrimary — инвариант: ровно один primary среди active.
func assertExactlyOnePrimary(t *testing.T) {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT count(*) FROM sigil_signing_keys WHERE status = 'active' AND is_primary`).Scan(&n); err != nil {
		t.Fatalf("count primary: %v", err)
	}
	if n != 1 {
		t.Errorf("active-primary count = %d, want 1", n)
	}
}

func keyIDs(ks []*SigningKey) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = k.KeyID
	}
	return out
}
