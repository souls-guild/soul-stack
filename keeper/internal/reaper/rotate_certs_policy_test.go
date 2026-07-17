package reaper

import (
	"context"
	"errors"
	"testing"
	"time"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/certissue"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// newPolicyRotator — ротатор поверх fake-ов с ЗАДАННЫМ резолвером политики.
func newPolicyRotator(db *fakeCertDB, signer certissue.Signer, vw CertVaultWriter, res *fakePolicyResolver) *CertRotator {
	return newCertRotatorFromDB(db, CertRotatorDeps{
		Signer: signer,
		Vault:  vw,
		CSRGen: fakeCSRGen,
		Cfg:    func() CertRotatorConfig { return testRotatorCfg() },
		Policy: res,
		Logger: silentLogger(),
	})
}

func dueCertFor(certID, incarnation string) dueCert {
	return dueCert{
		certID:        certID,
		incarnationID: incarnation,
		kind:          keepercert.KindCert,
		notAfter:      time.Now().Add(24 * time.Hour),
	}
}

// TestCertRotator_Policy_Disabled_SkipsNoFail — GUARD (NIM-99 Slice B): нет секции
// certificate_rotation / enable:false → rotateOne скипает БЕЗ fallback на
// rotate_tls: Voyage не вставлен, markFailed НЕ вызван, серт остаётся active
// (casCalls==0 ⟺ ни CAS active→rotating, ни CAS rotating→failed не было).
func TestCertRotator_Policy_Disabled_SkipsNoFail(t *testing.T) {
	db := &fakeCertDB{casResults: []int64{1}}
	vw := &fakeVaultWriter{}
	pol := enabledCertPolicy()
	pol.Enabled = false
	r := newPolicyRotator(db, &fakeSigner{cert: makeTestCertPEM(t)}, vw, &fakePolicyResolver{pol: pol})

	did, err := r.rotateOne(context.Background(), dueCertFor("cert-1", "redis-prod"), testRotatorCfg())
	if err != nil {
		t.Fatalf("rotateOne: %v", err)
	}
	if did {
		t.Fatal("policy disabled → ротации быть не должно")
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage when disabled, got %d", db.insertedVoyages)
	}
	if db.casCalls != 0 {
		t.Errorf("серт остаётся active, markFailed НЕ вызван: casCalls=%d, want 0", db.casCalls)
	}
	if len(vw.writes) != 0 {
		t.Errorf("no Vault writes when disabled, got %d", len(vw.writes))
	}
}

// TestCertRotator_Policy_UnknownScenario_SkipsNoFail — GUARD: сценарий из манифеста
// отсутствует среди scenario/ сервиса → НЕ спавним Voyage и НЕ помечаем failed
// (casCalls==0: строка не тронута, статус не failed).
func TestCertRotator_Policy_UnknownScenario_SkipsNoFail(t *testing.T) {
	db := &fakeCertDB{casResults: []int64{1}}
	vw := &fakeVaultWriter{}
	pol := enabledCertPolicy()
	pol.Scenario = "rotate_tls"
	pol.KnownScenarios = []string{"some_other_scenario"} // сценарий ротации НЕ найден в сервисе
	r := newPolicyRotator(db, &fakeSigner{cert: makeTestCertPEM(t)}, vw, &fakePolicyResolver{pol: pol})

	did, err := r.rotateOne(context.Background(), dueCertFor("cert-1", "redis-prod"), testRotatorCfg())
	if err != nil {
		t.Fatalf("rotateOne: %v", err)
	}
	if did {
		t.Fatal("сценарий не найден → ротации быть не должно")
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on unknown scenario, got %d", db.insertedVoyages)
	}
	if db.casCalls != 0 {
		t.Errorf("статус НЕ failed (строка не тронута): casCalls=%d, want 0", db.casCalls)
	}
}

// TestCertRotator_Policy_EmptyPKIRole_SkipsNoFail — GUARD (NIM-99 QA G2): enable:true,
// сценарий валиден, но pki_role пуст (manifest-drift) → rotateOne (false,nil): Voyage
// не спавнится, casCalls==0 (ни CAS, ни markFailed), серт остаётся active.
func TestCertRotator_Policy_EmptyPKIRole_SkipsNoFail(t *testing.T) {
	db := &fakeCertDB{casResults: []int64{1}}
	vw := &fakeVaultWriter{}
	pol := enabledCertPolicy()
	pol.PKIRole = "" // роль подписи не задана в манифесте
	r := newPolicyRotator(db, &fakeSigner{cert: makeTestCertPEM(t)}, vw, &fakePolicyResolver{pol: pol})

	did, err := r.rotateOne(context.Background(), dueCertFor("cert-1", "redis-prod"), testRotatorCfg())
	if err != nil {
		t.Fatalf("rotateOne: %v", err)
	}
	if did {
		t.Fatal("пустой pki_role → ротации быть не должно")
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on empty pki_role, got %d", db.insertedVoyages)
	}
	if db.casCalls != 0 {
		t.Errorf("серт остаётся active, markFailed НЕ вызван: casCalls=%d, want 0", db.casCalls)
	}
	if len(vw.writes) != 0 {
		t.Errorf("no Vault writes on empty pki_role, got %d", len(vw.writes))
	}
}

// TestCertRotator_Policy_ResolveError_CertStaysActive — GUARD: транзиент-ошибка
// резолва политики (git/PG недоступны) → серт остаётся active, markFailed НЕ
// вызван, тик не падает (rotateOne вернул nil-ошибку → retry на следующий тик).
func TestCertRotator_Policy_ResolveError_CertStaysActive(t *testing.T) {
	db := &fakeCertDB{casResults: []int64{1}}
	vw := &fakeVaultWriter{}
	r := newPolicyRotator(db, &fakeSigner{cert: makeTestCertPEM(t)}, vw,
		&fakePolicyResolver{err: errors.New("git fetch failed (transient)")})

	did, err := r.rotateOne(context.Background(), dueCertFor("cert-1", "redis-prod"), testRotatorCfg())
	if err != nil {
		t.Fatalf("транзиент резолва не должен ронять ротацию: %v", err)
	}
	if did {
		t.Fatal("resolve error → ротации быть не должно")
	}
	if db.casCalls != 0 {
		t.Errorf("серт остаётся active, markFailed НЕ вызван: casCalls=%d, want 0", db.casCalls)
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on resolve error, got %d", db.insertedVoyages)
	}
}

// TestCertRotator_Policy_HappyPath_UsesManifest — GUARD: включённая политика с
// валидным сценарием и pki_role → полная ротация. SignCSR получает mount из config
// + pki_role ИЗ МАНИФЕСТА; WriteKV пишет по service-scoped E3-пути
// secret/<service>/<inc>/tls/cert; Voyage+target вставлены; cert+key warrant вписаны.
func TestCertRotator_Policy_HappyPath_UsesManifest(t *testing.T) {
	db := &fakeCertDB{casResults: []int64{1}}
	vw := &fakeVaultWriter{}
	signer := &fakeSigner{cert: makeTestCertPEM(t)}
	pol := enabledCertPolicy() // Service=redis, Scenario=rotate_tls∈Known, PKIRole=service-tls
	r := newPolicyRotator(db, signer, vw, &fakePolicyResolver{pol: pol})

	did, err := r.rotateOne(context.Background(), dueCertFor("cert-1", "redis-prod"), testRotatorCfg())
	if err != nil {
		t.Fatalf("rotateOne: %v", err)
	}
	if !did {
		t.Fatal("happy-path обязан ротировать")
	}
	if signer.gotMount != "pki" || signer.gotRole != "service-tls" {
		t.Errorf("SignCSR args = mount=%q role=%q, want pki/service-tls (mount=config, role=manifest)",
			signer.gotMount, signer.gotRole)
	}
	wantCertPath := "secret/redis/redis-prod/tls/cert"
	if len(vw.writes) != 2 || vw.writes[0] != wantCertPath {
		t.Errorf("Vault writes = %v, want [cert,key] с первым = %q (service-scoped)", vw.writes, wantCertPath)
	}
	if db.insertedVoyages != 1 || db.insertedTargets != 1 {
		t.Errorf("Voyage+target вставляются один раз: voyages=%d targets=%d", db.insertedVoyages, db.insertedTargets)
	}
	if db.insertedWarrants != 2 {
		t.Errorf("cert+key warrant inserts want 2, got %d", db.insertedWarrants)
	}
}

// TestBuildRotateTLSVoyage_WholeIncarnation_ScenarioFromArg — GUARD: имя сценария
// берётся из АРГУМЕНТА (не из const rotateTLSScenario), и target = вся инкарнация
// целиком (ровно один target kind=incarnation) — инвариант «вся инкарнация».
func TestBuildRotateTLSVoyage_WholeIncarnation_ScenarioFromArg(t *testing.T) {
	m := &certissue.Material{
		CertRef: "secret/redis/redis-prod/tls/cert#cert",
		KeyRef:  "secret/redis/redis-prod/tls/key#key",
	}
	v, targets := buildRotateTLSVoyage("voyage-1", "redis-prod", m, "custom_rotate")

	if v.ScenarioName == nil || *v.ScenarioName != "custom_rotate" {
		t.Errorf("ScenarioName обязан прийти из аргумента, got %v", v.ScenarioName)
	}
	if v.Kind != voyage.KindScenario {
		t.Errorf("Kind = %v, want scenario", v.Kind)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want ровно 1 (вся инкарнация)", len(targets))
	}
	if targets[0].TargetKind != voyage.TargetKindIncarnation || targets[0].TargetID != "redis-prod" {
		t.Errorf("target = {kind=%v id=%q}, want {incarnation, redis-prod}", targets[0].TargetKind, targets[0].TargetID)
	}
	if string(v.TargetResolved) != `["redis-prod"]` {
		t.Errorf("TargetResolved = %s, want [\"redis-prod\"]", v.TargetResolved)
	}
}

// TestRotateTLSScenario_ContractAnchor — контрактный якорь: имя дефолтного сценария
// НЕ переименовано (examples/service/redis/scenario/rotate_tls).
func TestRotateTLSScenario_ContractAnchor(t *testing.T) {
	if rotateTLSScenario != "rotate_tls" {
		t.Fatalf("контрактное имя сценария переименовано: %q != rotate_tls", rotateTLSScenario)
	}
}
