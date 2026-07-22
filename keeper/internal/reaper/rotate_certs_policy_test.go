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

// newPolicyRotator — rotator on top of fakes with a GIVEN policy resolver.
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

// TestCertRotator_Policy_Disabled_SkipsNoFail — GUARD (NIM-99 Slice B): no
// certificate_rotation section / enable:false → rotateOne skips WITHOUT fallback to
// rotate_tls: Voyage is not inserted, markFailed is NOT called, the cert stays active
// (casCalls==0 <=> neither CAS active->rotating nor CAS rotating->failed happened).
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
		t.Fatal("policy disabled -> should not rotate")
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage when disabled, got %d", db.insertedVoyages)
	}
	if db.casCalls != 0 {
		t.Errorf("cert stays active, markFailed NOT called: casCalls=%d, want 0", db.casCalls)
	}
	if len(vw.writes) != 0 {
		t.Errorf("no Vault writes when disabled, got %d", len(vw.writes))
	}
}

// TestCertRotator_Policy_UnknownScenario_SkipsNoFail — GUARD: the scenario from the
// manifest is absent from the service's scenario/ dir -> do NOT spawn Voyage and do NOT
// mark failed (casCalls==0: row untouched, status not failed).
func TestCertRotator_Policy_UnknownScenario_SkipsNoFail(t *testing.T) {
	db := &fakeCertDB{casResults: []int64{1}}
	vw := &fakeVaultWriter{}
	pol := enabledCertPolicy()
	pol.Scenario = "rotate_tls"
	pol.KnownScenarios = []string{"some_other_scenario"} // rotation scenario NOT found in the service
	r := newPolicyRotator(db, &fakeSigner{cert: makeTestCertPEM(t)}, vw, &fakePolicyResolver{pol: pol})

	did, err := r.rotateOne(context.Background(), dueCertFor("cert-1", "redis-prod"), testRotatorCfg())
	if err != nil {
		t.Fatalf("rotateOne: %v", err)
	}
	if did {
		t.Fatal("scenario not found -> should not rotate")
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on unknown scenario, got %d", db.insertedVoyages)
	}
	if db.casCalls != 0 {
		t.Errorf("status NOT failed (row untouched): casCalls=%d, want 0", db.casCalls)
	}
}

// TestCertRotator_Policy_EmptyPKIRole_SkipsNoFail — GUARD (NIM-99 QA G2): enable:true,
// scenario is valid, but pki_role is empty (manifest-drift) -> rotateOne (false,nil): Voyage
// is not spawned, casCalls==0 (neither CAS nor markFailed), cert stays active.
func TestCertRotator_Policy_EmptyPKIRole_SkipsNoFail(t *testing.T) {
	db := &fakeCertDB{casResults: []int64{1}}
	vw := &fakeVaultWriter{}
	pol := enabledCertPolicy()
	pol.PKIRole = "" // signing role not set in the manifest
	r := newPolicyRotator(db, &fakeSigner{cert: makeTestCertPEM(t)}, vw, &fakePolicyResolver{pol: pol})

	did, err := r.rotateOne(context.Background(), dueCertFor("cert-1", "redis-prod"), testRotatorCfg())
	if err != nil {
		t.Fatalf("rotateOne: %v", err)
	}
	if did {
		t.Fatal("empty pki_role -> should not rotate")
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on empty pki_role, got %d", db.insertedVoyages)
	}
	if db.casCalls != 0 {
		t.Errorf("cert stays active, markFailed NOT called: casCalls=%d, want 0", db.casCalls)
	}
	if len(vw.writes) != 0 {
		t.Errorf("no Vault writes on empty pki_role, got %d", len(vw.writes))
	}
}

// TestCertRotator_Policy_ResolveError_CertStaysActive — GUARD: a transient policy
// resolve error (git/PG unavailable) -> cert stays active, markFailed is NOT
// called, tick doesn't fail (rotateOne returned a nil error -> retry on next tick).
func TestCertRotator_Policy_ResolveError_CertStaysActive(t *testing.T) {
	db := &fakeCertDB{casResults: []int64{1}}
	vw := &fakeVaultWriter{}
	r := newPolicyRotator(db, &fakeSigner{cert: makeTestCertPEM(t)}, vw,
		&fakePolicyResolver{err: errors.New("git fetch failed (transient)")})

	did, err := r.rotateOne(context.Background(), dueCertFor("cert-1", "redis-prod"), testRotatorCfg())
	if err != nil {
		t.Fatalf("transient resolve error must not fail rotation: %v", err)
	}
	if did {
		t.Fatal("resolve error -> should not rotate")
	}
	if db.casCalls != 0 {
		t.Errorf("cert stays active, markFailed NOT called: casCalls=%d, want 0", db.casCalls)
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on resolve error, got %d", db.insertedVoyages)
	}
}

// TestCertRotator_Policy_HappyPath_UsesManifest — GUARD: an enabled policy with a
// valid scenario and pki_role -> full rotation. SignCSR gets mount from config
// + pki_role FROM THE MANIFEST; WriteKV writes to the service-scoped E3 path
// secret/<service>/<inc>/tls/cert; Voyage+target are inserted; cert+key warrant recorded.
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
		t.Fatal("happy-path must rotate")
	}
	if signer.gotMount != "pki" || signer.gotRole != "service-tls" {
		t.Errorf("SignCSR args = mount=%q role=%q, want pki/service-tls (mount=config, role=manifest)",
			signer.gotMount, signer.gotRole)
	}
	wantCertPath := "secret/redis/redis-prod/tls/cert"
	if len(vw.writes) != 2 || vw.writes[0] != wantCertPath {
		t.Errorf("Vault writes = %v, want [cert,key] with the first = %q (service-scoped)", vw.writes, wantCertPath)
	}
	if db.insertedVoyages != 1 || db.insertedTargets != 1 {
		t.Errorf("Voyage+target inserted exactly once: voyages=%d targets=%d", db.insertedVoyages, db.insertedTargets)
	}
	if db.insertedWarrants != 2 {
		t.Errorf("cert+key warrant inserts want 2, got %d", db.insertedWarrants)
	}
}

// TestBuildRotateTLSVoyage_WholeIncarnation_ScenarioFromArg — GUARD: the scenario name
// comes from the ARGUMENT (not from const rotateTLSScenario), and target = the whole
// incarnation (exactly one target kind=incarnation) - the "whole incarnation" invariant.
func TestBuildRotateTLSVoyage_WholeIncarnation_ScenarioFromArg(t *testing.T) {
	m := &certissue.Material{
		CertRef: "secret/redis/redis-prod/tls/cert#cert",
		KeyRef:  "secret/redis/redis-prod/tls/key#key",
	}
	v, targets := buildRotateTLSVoyage("voyage-1", "redis-prod", m, "custom_rotate")

	if v.ScenarioName == nil || *v.ScenarioName != "custom_rotate" {
		t.Errorf("ScenarioName must come from the argument, got %v", v.ScenarioName)
	}
	if v.Kind != voyage.KindScenario {
		t.Errorf("Kind = %v, want scenario", v.Kind)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want exactly 1 (whole incarnation)", len(targets))
	}
	if targets[0].TargetKind != voyage.TargetKindIncarnation || targets[0].TargetID != "redis-prod" {
		t.Errorf("target = {kind=%v id=%q}, want {incarnation, redis-prod}", targets[0].TargetKind, targets[0].TargetID)
	}
	if string(v.TargetResolved) != `["redis-prod"]` {
		t.Errorf("TargetResolved = %s, want [\"redis-prod\"]", v.TargetResolved)
	}
}

// TestRotateTLSScenario_ContractAnchor — contract anchor: the default scenario name
// is NOT renamed (examples/service/redis/scenario/rotate_tls).
func TestRotateTLSScenario_ContractAnchor(t *testing.T) {
	if rotateTLSScenario != "rotate_tls" {
		t.Fatalf("contract scenario name renamed: %q != rotate_tls", rotateTLSScenario)
	}
}
