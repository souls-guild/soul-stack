package reaper

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/certissue"
	"github.com/souls-guild/soul-stack/keeper/internal/certpolicy"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- fake pool/tx для CertRotator ---
//
// fakeCertDB моделирует минимальный набор SQL, который дёргает CertRotator:
//   - SELECT ... FROM warrant ... FOR UPDATE SKIP LOCKED (скан due) → Query.
//   - UPDATE warrant SET status ... WHERE cert_id ... AND status = ... (CAS) → Exec.
//   - INSERT INTO warrant ... RETURNING cert_id, issued_at → QueryRow.
//   - INSERT INTO voyages ... RETURNING created_at → QueryRow.
//   - INSERT INTO voyage_targets ... → Exec.
//   - UPDATE warrant SET status='superseded'/... (Supersede) → Exec.
type fakeCertDB struct {
	dueRows [][]any // строки скана due (см. selectDueCertsSQL колонки)

	// casResults: последовательность RowsAffected для UPDATE ... status CAS.
	// Первый Exec с "AND status = $3" берёт casResults[0] и т.д. Пустой → 1
	// (always won). Моделирует single-winner: 0 = проиграл гонку.
	casResults []int64
	casIdx     int

	// счётчики фактов для ассертов.
	insertedWarrants int
	insertedVoyages  int
	insertedTargets  int
	supersedes       int
	casCalls         int
	markFailedCalls  int

	execErr    error // если задан — все Exec падают (кроме скан-Query)
	insertVErr error // ошибка INSERT INTO voyages
}

func (f *fakeCertDB) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &fakeCertTx{db: f}, nil
}

func (f *fakeCertDB) exec(sql string) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "AND status = $3"):
		// CAS-переход по cert_id. Различаем целевой статус для счётчиков.
		f.casCalls++
		n := int64(1)
		if f.casIdx < len(f.casResults) {
			n = f.casResults[f.casIdx]
		}
		f.casIdx++
		if strings.Contains(sql, "SET status = $2") {
			// markFailed использует тот же markStatusSQL; отличим по контексту нельзя,
			// поэтому markFailedCalls считаем в отдельном Exec-пути ниже (rotating→failed
			// проходит именно как CAS). Здесь просто отдаём RowsAffected.
		}
		return pgconn.NewCommandTag("UPDATE " + itoaForTag(n)), nil
	case strings.Contains(sql, "SET status = 'superseded'"):
		f.supersedes++
		if f.execErr != nil {
			return pgconn.CommandTag{}, f.execErr
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	// voyage_targets вставляются через CopyFrom (см. fakeCertTx.CopyFrom), не Exec.
	return pgconn.CommandTag{}, nil
}

func (f *fakeCertDB) queryRow(sql string) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO warrant"):
		f.insertedWarrants++
		return certStaticRow{values: []any{"new-cert-id", time.Now()}}
	case strings.Contains(sql, "INSERT INTO voyages"):
		f.insertedVoyages++
		if f.insertVErr != nil {
			return certErrRow{err: f.insertVErr}
		}
		return certStaticRow{values: []any{time.Now()}}
	}
	return certErrRow{err: pgx.ErrNoRows}
}

func (f *fakeCertDB) query(sql string) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM warrant") && strings.Contains(sql, "FOR UPDATE SKIP LOCKED") {
		return &fakeCertRows{rows: f.dueRows}, nil
	}
	return &fakeCertRows{}, nil
}

// fakeCertTx — pgx.Tx-обёртка (delegate в fakeCertDB).
type fakeCertTx struct{ db *fakeCertDB }

func (t *fakeCertTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (t *fakeCertTx) Commit(context.Context) error          { return nil }
func (t *fakeCertTx) Rollback(context.Context) error        { return nil }

// CopyFrom — voyage.InsertTargets батчит targets через CopyFrom. Дренируем
// source, считаем строки (одна на target) и фиксируем факт вставки.
func (t *fakeCertTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, src pgx.CopyFromSource) (int64, error) {
	var n int64
	for src.Next() {
		if _, err := src.Values(); err != nil {
			return n, err
		}
		n++
	}
	t.db.insertedTargets += int(n)
	return n, t.db.execErr
}
func (t *fakeCertTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("unexpected SendBatch")
}
func (t *fakeCertTx) LargeObjects() pgx.LargeObjects { panic("unexpected LargeObjects") }
func (t *fakeCertTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected Prepare")
}
func (t *fakeCertTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	return t.db.exec(sql)
}
func (t *fakeCertTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	return t.db.query(sql)
}
func (t *fakeCertTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return t.db.queryRow(sql)
}
func (t *fakeCertTx) Conn() *pgx.Conn { return nil }

// fakeCertRows — pgx.Rows над [][]any.
type fakeCertRows struct {
	rows [][]any
	idx  int
}

func (r *fakeCertRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeCertRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	for i, d := range dest {
		switch dst := d.(type) {
		case *string:
			*dst = row[i].(string)
		case *time.Time:
			*dst = row[i].(time.Time)
		case **string:
			if row[i] == nil {
				*dst = nil
				continue
			}
			s := row[i].(string)
			*dst = &s
		}
	}
	return nil
}
func (r *fakeCertRows) Close()                                       {}
func (r *fakeCertRows) Err() error                                   { return nil }
func (r *fakeCertRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeCertRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeCertRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeCertRows) RawValues() [][]byte                          { return nil }
func (r *fakeCertRows) Conn() *pgx.Conn                              { return nil }

type certStaticRow struct{ values []any }

func (r certStaticRow) Scan(dest ...any) error {
	for i, d := range dest {
		switch dst := d.(type) {
		case *string:
			*dst = r.values[i].(string)
		case *time.Time:
			*dst = r.values[i].(time.Time)
		}
	}
	return nil
}

type certErrRow struct{ err error }

func (r certErrRow) Scan(_ ...any) error { return r.err }

// itoaForTag — переиспользуется из errand_purge_test.go (тот же пакет).

// --- fake PKI signer / vault writer / csrgen ---

type fakeSigner struct {
	err      error
	cert     []byte
	gotMount string // захват аргументов последнего SignCSR (для ассерта mount/role)
	gotRole  string
}

func (s *fakeSigner) SignCSR(_ context.Context, mount, role, _ string) (*certissue.SignedCert, error) {
	s.gotMount, s.gotRole = mount, role
	if s.err != nil {
		return nil, s.err
	}
	return &certissue.SignedCert{
		CertificatePEM: s.cert,
		SerialNumber:   "0a0b0c",
		NotAfter:       time.Now().Add(365 * 24 * time.Hour),
	}, nil
}

type fakeVaultWriter struct {
	err    error
	writes []string // пути записи
}

func (w *fakeVaultWriter) WriteKV(_ context.Context, path string, _ map[string]any) error {
	w.writes = append(w.writes, path)
	return w.err
}

// makeTestCertPEM генерит self-signed cert (для fakeSigner.cert / fingerprint).
func makeTestCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "svc.tls"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// fakeCSRGen — детерминированный keypair+CSR (реальный, чтобы SignCSR-mock не
// парсил; приватник не используется в ассертах).
func fakeCSRGen(_ string, _ []string) (privateKeyPEM, csrPEM []byte, err error) {
	return []byte("-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----"),
		[]byte("-----BEGIN CERTIFICATE REQUEST-----\nfake\n-----END CERTIFICATE REQUEST-----"), nil
}

// dueRow строит строку скана due (колонки selectDueCertsSQL).
func dueRow(certID, incarnation string, notAfter time.Time) []any {
	return []any{certID, incarnation, "cert", "secret/redis/x/tls/cert#cert", "old-serial", strings.Repeat("a", 64), notAfter, nil}
}

func testRotatorCfg() CertRotatorConfig {
	return CertRotatorConfig{
		Threshold:           30 * 24 * time.Hour,
		DefaultPKIMount:     "pki",
		MaxRotationsPerTick: 20,
	}
}

// fakePolicyResolver отдаёт заданную политику/ошибку (замена certpolicy.Resolver).
type fakePolicyResolver struct {
	pol certpolicy.Policy
	err error
}

func (f *fakePolicyResolver) Resolve(_ context.Context, _ string) (certpolicy.Policy, error) {
	return f.pol, f.err
}

// enabledCertPolicy — включённая политика по умолчанию: сценарий = rotateTLSScenario
// (и он в KnownScenarios), pki_role задан. Happy-путь ротатора проходит все скипы.
func enabledCertPolicy() certpolicy.Policy {
	return certpolicy.Policy{
		Service:        "redis",
		Present:        true,
		Enabled:        true,
		Scenario:       rotateTLSScenario,
		PKIRole:        "service-tls",
		KnownScenarios: []string{rotateTLSScenario},
	}
}

// buildRotator собирает CertRotator поверх fake-ов с включённой политикой.
func buildRotator(db *fakeCertDB, signer certissue.Signer, vw CertVaultWriter, cfg CertRotatorConfig) *CertRotator {
	return newCertRotatorFromDB(db, CertRotatorDeps{
		Signer: signer,
		Vault:  vw,
		CSRGen: fakeCSRGen,
		Cfg:    func() CertRotatorConfig { return cfg },
		Policy: &fakePolicyResolver{pol: enabledCertPolicy()},
		Logger: silentLogger(),
	})
}

// --- guard-тесты ---

// TestCertRotator_HappyRotation — due-cert проходит полную цепочку: CAS won →
// SignCSR → WriteKV cert+key → insert новых warrant (cert+key) → спавн Voyage.
func TestCertRotator_HappyRotation(t *testing.T) {
	certPEM := makeTestCertPEM(t)
	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{1}, // CAS active→rotating won
	}
	signer := &fakeSigner{cert: certPEM}
	vw := &fakeVaultWriter{}
	r := buildRotator(db, signer, vw, testRotatorCfg())

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 1 {
		t.Fatalf("rotated = %d, want 1", n)
	}
	if db.insertedVoyages != 1 {
		t.Errorf("Voyage(rotate_tls) must be spawned exactly once, got %d", db.insertedVoyages)
	}
	if db.insertedTargets != 1 {
		t.Errorf("voyage target must be inserted, got %d", db.insertedTargets)
	}
	// cert+key записаны в Vault (E3-пути).
	if len(vw.writes) != 2 {
		t.Errorf("expected 2 Vault writes (cert+key), got %d: %v", len(vw.writes), vw.writes)
	}
	// cert+key warrant-строки вписаны.
	if db.insertedWarrants != 2 {
		t.Errorf("expected 2 warrant inserts (cert+key), got %d", db.insertedWarrants)
	}
}

// TestCertRotator_EmitsRotatedAudit_NoSecretLeak — GUARD (NIM-99 QA G3): happy-путь
// ротации эмитит cert.rotated с НЕ-секретной нагрузкой (incarnation/fingerprint/
// serial/not_after/voyage/superseded), но БЕЗ приватника или PEM (симметрия с
// issued-аудитом).
func TestCertRotator_EmitsRotatedAudit_NoSecretLeak(t *testing.T) {
	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{1},
	}
	fa := &fakeAuditWriter{}
	r := newCertRotatorFromDB(db, CertRotatorDeps{
		Signer: &fakeSigner{cert: makeTestCertPEM(t)},
		Vault:  &fakeVaultWriter{},
		CSRGen: fakeCSRGen,
		Cfg:    func() CertRotatorConfig { return testRotatorCfg() },
		Policy: &fakePolicyResolver{pol: enabledCertPolicy()},
		Audit:  fa,
		Logger: silentLogger(),
	})

	if _, err := r.Run(context.Background(), 0, 0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fa.events) != 1 {
		t.Fatalf("ожидалось 1 cert.rotated событие, got %d", len(fa.events))
	}
	ev := fa.events[0]
	if ev.EventType != audit.EventCertRotated {
		t.Errorf("EventType = %q, want %q", ev.EventType, audit.EventCertRotated)
	}
	if ev.Source != audit.SourceKeeperInternal {
		t.Errorf("Source = %q, want keeper_internal", ev.Source)
	}
	for _, k := range []string{"incarnation", "fingerprint", "serial_number", "not_after", "voyage_id", "superseded_cert_id"} {
		if _, ok := ev.Payload[k]; !ok {
			t.Errorf("payload не содержит не-секретный ключ %q", k)
		}
	}
	// Приватник/PEM в payload течь не должны.
	for _, k := range []string{"key", "key_pem", "KeyPEM", "cert_pem", "cert", "private_key", "pem"} {
		if _, leaked := ev.Payload[k]; leaked {
			t.Errorf("payload не должен нести секрет %q", k)
		}
	}
}

// TestCertRotator_SingleWinner_LostCAS — GUARD single-winner (design.md): если
// CAS active→rotating вернул 0 (другой тик/инстанс перехватил), ротация НЕ
// происходит — ни Voyage, ни Vault-записи, ни insert-ов. Два тика на один
// due-cert не спавнят две ротации.
func TestCertRotator_SingleWinner_LostCAS(t *testing.T) {
	certPEM := makeTestCertPEM(t)
	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{0}, // проиграли гонку
	}
	vw := &fakeVaultWriter{}
	r := buildRotator(db, &fakeSigner{cert: certPEM}, vw, testRotatorCfg())

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 {
		t.Fatalf("rotated = %d, want 0 (lost CAS)", n)
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on lost CAS, got %d", db.insertedVoyages)
	}
	if len(vw.writes) != 0 {
		t.Errorf("no Vault writes on lost CAS, got %d", len(vw.writes))
	}
	if db.insertedWarrants != 0 {
		t.Errorf("no warrant inserts on lost CAS, got %d", db.insertedWarrants)
	}
}

// TestCertRotator_FailedPath_SignerDown — GUARD failed-path (design.md): SignCSR
// упал → серт помечается failed (CAS rotating→failed), НЕ остаётся active, Voyage
// не спавнится. Ошибка одной ротации не роняет тик (Run возвращает nil).
func TestCertRotator_FailedPath_SignerDown(t *testing.T) {
	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{1, 1}, // [0]=active→rotating won, [1]=rotating→failed
	}
	vw := &fakeVaultWriter{}
	r := buildRotator(db, &fakeSigner{err: errors.New("vault pki down")}, vw, testRotatorCfg())

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run must not fail the whole tick on one cert error: %v", err)
	}
	if n != 0 {
		t.Fatalf("rotated = %d, want 0 (signer down)", n)
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on signer failure, got %d", db.insertedVoyages)
	}
	// Второй CAS-вызов = markFailed (rotating→failed): цепочка захватила строку
	// (casCalls>=2), но не вписала active.
	if db.casCalls < 2 {
		t.Errorf("expected CAS to rotating THEN markFailed (>=2 CAS calls), got %d", db.casCalls)
	}
	if db.insertedWarrants != 0 {
		t.Errorf("no new active warrant on failure, got %d", db.insertedWarrants)
	}
}

// TestCertRotator_FailClosed_VaultDown — GUARD fail-closed (design.md): Vault
// WriteKV упал → markFailed, Voyage не спавнится, Run НЕ падает (тик выживает).
func TestCertRotator_FailClosed_VaultDown(t *testing.T) {
	certPEM := makeTestCertPEM(t)
	db := &fakeCertDB{
		dueRows:    [][]any{dueRow("cert-1", "redis-prod", time.Now().Add(24*time.Hour))},
		casResults: []int64{1, 1},
	}
	vw := &fakeVaultWriter{err: errors.New("vault write denied")}
	r := buildRotator(db, &fakeSigner{cert: certPEM}, vw, testRotatorCfg())

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run must survive Vault-down: %v", err)
	}
	if n != 0 {
		t.Fatalf("rotated = %d, want 0 (vault down)", n)
	}
	if db.insertedVoyages != 0 {
		t.Errorf("no Voyage on Vault failure, got %d", db.insertedVoyages)
	}
}

// TestCertRotator_Idempotent_NoDueSkipsWork — нет due-сертов → тик no-op (0
// ротаций, ничего не спавнится).
func TestCertRotator_Idempotent_NoDueSkipsWork(t *testing.T) {
	db := &fakeCertDB{dueRows: nil}
	vw := &fakeVaultWriter{}
	r := buildRotator(db, &fakeSigner{cert: makeTestCertPEM(t)}, vw, testRotatorCfg())

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 || db.insertedVoyages != 0 {
		t.Errorf("no work without due certs; rotated=%d voyages=%d", n, db.insertedVoyages)
	}
}

// TestCertRotator_Jitter_SpreadsSameExpiry — GUARD jitter (design.md): N сертов
// с ОДНИМ not_after не все считаются due в один тик — indi­vidual jitter
// (hash%window) отсеивает часть. Проверяем, что при узком пороге и широком
// jitter-окне не все N попадают в ротацию за один тик.
func TestCertRotator_Jitter_SpreadsSameExpiry(t *testing.T) {
	// not_after ровно на границе порога: threshold=30d, серт истекает через 30d.
	// jitter сдвигает эффективный порог назад на hash%window; при window=15d
	// часть сертов «ещё не due» (их сдвинутый not_after > now+threshold... нет,
	// сдвиг НАЗАД приближает → due), поэтому конструируем обратный кейс:
	// not_after чуть ЗА порогом (now+threshold+10d), jitter возвращает часть в due.
	now := time.Now()
	cfg := CertRotatorConfig{
		Threshold:           30 * 24 * time.Hour,
		JitterWindow:        20 * 24 * time.Hour,
		DefaultPKIMount:     "pki",
		MaxRotationsPerTick: 100,
	}
	// not_after = now + threshold + 10d: без jitter НИКТО не due (not_after >
	// now+threshold). jitter сдвигает not_after назад на hash%20d; due только те,
	// у кого hash%20d > 10d (сдвинулись под порог).
	notAfter := now.Add(cfg.Threshold + 10*24*time.Hour)

	dueCandidates := 0
	total := 40
	rr := &CertRotator{nowFunc: func() time.Time { return now }}
	for i := 0; i < total; i++ {
		c := dueCert{certID: "cert-" + itoaForTag(int64(i%6)) + "-" + strings.Repeat("x", i), notAfter: notAfter}
		if rr.isDue(c, now, cfg) {
			dueCandidates++
		}
	}
	// Ключевой инвариант: НЕ все и НЕ ноль — jitter реально размазывает.
	if dueCandidates == 0 {
		t.Errorf("jitter отсёк ВСЕХ — окно/порог подобраны неверно (ожидался частичный набор)")
	}
	if dueCandidates == total {
		t.Errorf("jitter не отсёк НИКОГО (%d/%d due) — размазывания нет", dueCandidates, total)
	}
}

// TestCertRotator_ThresholdZero_NoScan — threshold=0 → правило ничего не
// сканирует (защита от кривого конфига).
func TestCertRotator_ThresholdZero_NoScan(t *testing.T) {
	db := &fakeCertDB{dueRows: [][]any{dueRow("cert-1", "x", time.Now())}}
	cfg := testRotatorCfg()
	cfg.Threshold = 0
	r := buildRotator(db, &fakeSigner{}, &fakeVaultWriter{}, cfg)

	n, err := r.Run(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 {
		t.Errorf("threshold=0 must scan nothing, rotated=%d", n)
	}
}

// TestCertRotator_MissingDeps — signer/vault/csrgen/cfg обязательны.
func TestCertRotator_MissingDeps(t *testing.T) {
	r := newCertRotatorFromDB(&fakeCertDB{}, CertRotatorDeps{}) // всё nil
	if _, err := r.Run(context.Background(), 0, 0); err == nil {
		t.Fatal("Run without deps must error")
	}
}
