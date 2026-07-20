package reaper

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// certBarrierYAML is keeper.yml with GLOBAL reaper.dry_run: false (typical
// production setup: live purge is enabled) and a rotate_due_certs rule with
// replacement placeholders. R1 barrier: live TLS rotation must not depend on
// global reaper.dry_run because rotate_due_certs has its own per-rule dry_run,
// default true.
const certBarrierYAML = `
kid: keeper-test-01

listen:
  grpc:
    bootstrap:    { addr: "127.0.0.1:19442", tls: { cert: /tmp/c, key: /tmp/k } }
    event_stream: { addr: "127.0.0.1:18443", tls: { cert: /tmp/c, key: /tmp/k, ca: /tmp/ca } }
  openapi: { addr: "127.0.0.1:18080" }
  mcp:     { addr: "127.0.0.1:18081" }
  metrics: { addr: "127.0.0.1:19090" }

postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }

redis:
  addr: "127.0.0.1:6379"

vault:
  addr: "http://127.0.0.1:8200"
  auth: { method: token }
  pki_mount: "pki/soulstack"

auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-test-01
    ttl_default: 24h
    ttl_bootstrap: 30d

logging:
  level: info
  format: json

reaper:
  enabled: true
  interval: 50ms
  dry_run: false
  batch_size: 200
  lock_ttl: 300ms
  rules:
    rotate_due_certs:
      enabled: true
      rotate_threshold: 30d
`

// buildBarrierRunner builds Runner with a real CertRotator over fakeCertDB.
// It returns runner plus fake pool. For the casCalls assert, CAS active->rotating
// is called only in the live rotateOne path, so casCalls>0 means live rotation
// happened.
func buildBarrierRunner(t *testing.T, cfgYAML string, db *fakeCertDB) (*Runner, *config.Store[config.KeeperConfig]) {
	t.Helper()
	store := newTestStore(t, cfgYAML)
	rc := newTestRedis(t)
	rot := buildRotator(db, &fakeSigner{cert: makeTestCertPEM(t)}, &fakeVaultWriter{}, CertRotatorConfig{
		Threshold:           30 * 24 * time.Hour,
		DefaultPKIMount:     "pki",
		MaxRotationsPerTick: 20,
	})
	rn, err := NewRunner(Deps{
		Purger: &fakePurger{}, Redis: rc, Store: store, Holder: "keeper-test-01",
		Logger: silentLogger(), CertRotator: rot,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return rn, store
}

// TestRotateDueCerts_Barrier_DefaultDryRunSkipsRotation is the R1 barrier:
// with enabled:true but WITHOUT explicit per-rule dry_run:false, live rotation
// does NOT run even when global reaper.dry_run:false enables live purge. The
// indicator is that CAS active->rotating was not called.
func TestRotateDueCerts_Barrier_DefaultDryRunSkipsRotation(t *testing.T) {
	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{1},
	}
	rn, store := buildBarrierRunner(t, certBarrierYAML, db)

	rn.dispatch(context.Background(), store.Get())

	if db.casCalls != 0 {
		t.Errorf("live rotation under default dry_run: casCalls = %d, want 0 (barrier holds)", db.casCalls)
	}
}

// TestRotateDueCerts_Barrier_ExplicitFalseRotates removes the barrier: explicit
// rotate_due_certs.dry_run:false enables live rotation, so the due cert passes
// CAS.
func TestRotateDueCerts_Barrier_ExplicitFalseRotates(t *testing.T) {
	body := replaceOnce(t, certBarrierYAML,
		"rotate_due_certs:\n      enabled: true\n      rotate_threshold: 30d",
		"rotate_due_certs:\n      enabled: true\n      dry_run: false\n      rotate_threshold: 30d")

	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{1},
	}
	rn, store := buildBarrierRunner(t, body, db)

	rn.dispatch(context.Background(), store.Get())

	if db.casCalls == 0 {
		t.Errorf("explicit dry_run:false: casCalls = 0, want >=1 (live rotation must run)")
	}
}

// TestRotateDueCerts_Barrier_ExplicitTrueSkips: explicit dry_run:true matches
// the default, so there is no rotation.
func TestRotateDueCerts_Barrier_ExplicitTrueSkips(t *testing.T) {
	body := replaceOnce(t, certBarrierYAML,
		"rotate_due_certs:\n      enabled: true\n      rotate_threshold: 30d",
		"rotate_due_certs:\n      enabled: true\n      dry_run: true\n      rotate_threshold: 30d")

	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{1},
	}
	rn, store := buildBarrierRunner(t, body, db)

	rn.dispatch(context.Background(), store.Get())

	if db.casCalls != 0 {
		t.Errorf("explicit dry_run:true: casCalls = %d, want 0", db.casCalls)
	}
}
