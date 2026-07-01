package reaper

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// certBarrierYAML — keeper.yml с ГЛОБАЛЬНЫМ reaper.dry_run: false (типичный прод:
// боевой purge включён) и правилом rotate_due_certs с плейсхолдерами под подмену.
// Барьер R1: боевая ротация TLS не должна зависеть от глобального reaper.dry_run —
// у rotate_due_certs собственный per-rule dry_run, default true.
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

// buildBarrierRunner собирает Runner с реальным CertRotator поверх fakeCertDB.
// Возвращает runner + fake-пул (для ассерта casCalls: CAS active→rotating зовётся
// только в боевом проходе rotateOne, поэтому casCalls>0 ⟺ боевая ротация была).
func buildBarrierRunner(t *testing.T, cfgYAML string, db *fakeCertDB) (*Runner, *config.Store[config.KeeperConfig]) {
	t.Helper()
	store := newTestStore(t, cfgYAML)
	rc := newTestRedis(t)
	rot := buildRotator(db, &fakeSigner{cert: makeTestCertPEM(t)}, &fakeVaultWriter{}, CertRotatorConfig{
		Threshold:           30 * 24 * time.Hour,
		DefaultPKIMount:     "pki",
		DefaultPKIRole:      "service-tls",
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

// TestRotateDueCerts_Barrier_DefaultDryRunSkipsRotation — R1-барьер: при
// enabled:true БЕЗ явного per-rule dry_run:false боевая ротация НЕ идёт, даже если
// глобальный reaper.dry_run:false (тот включает боевой purge). Индикатор — CAS
// active→rotating не вызывался.
func TestRotateDueCerts_Barrier_DefaultDryRunSkipsRotation(t *testing.T) {
	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{1},
	}
	rn, store := buildBarrierRunner(t, certBarrierYAML, db)

	rn.dispatch(context.Background(), store.Get())

	if db.casCalls != 0 {
		t.Errorf("боевая ротация под default dry_run: casCalls = %d, want 0 (барьер держит)", db.casCalls)
	}
}

// TestRotateDueCerts_Barrier_ExplicitFalseRotates — снятие барьера: явный
// rotate_due_certs.dry_run:false включает боевую ротацию (due-cert проходит CAS).
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
		t.Errorf("явный dry_run:false: casCalls = 0, want >=1 (боевая ротация должна идти)")
	}
}

// TestRotateDueCerts_Barrier_ExplicitTrueSkips — явный dry_run:true идентичен
// дефолту: ротации нет.
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
		t.Errorf("явный dry_run:true: casCalls = %d, want 0", db.casCalls)
	}
}
