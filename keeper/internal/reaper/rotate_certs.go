package reaper

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/certissue"
	"github.com/souls-guild/soul-stack/keeper/internal/certpolicy"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// rotateCertsSystemAID — AID on behalf of which the background rotate_tls-
// Voyage is spawned. Matches the seeded system operator (migration 086,
// push.AutoImportSystemAID); duplicated here as a constant so reaper doesn't
// need to pull the push package for one identifier.
const rotateCertsSystemAID = "archon-system"

// rotateTLSScenario — дефолтное имя сценария доставки TLS-материала. Источник
// имени теперь манифест (certpolicy.Policy.Scenario); const остаётся якорем
// контрактного теста, что имя сценария не переименовано.
const rotateTLSScenario = "rotate_tls"

// defaultMaxRotationsPerTick — maximum rotations per tick by default (anti-thundering-herd
// when certificates expire in bulk).
const defaultMaxRotationsPerTick = 20

// CertVaultWriter — узкая поверхность vault.Client.WriteKV, нужная ротатору
// (совпадает с certissue.KVWriter — r.vault проходит как KVWriter).
type CertVaultWriter interface {
	WriteKV(ctx context.Context, path string, data map[string]any) error
}

// certPolicyResolver — резолвер эффективной cert-rotation-политики инкарнации
// (certpolicy.Resolver). Узкий интерфейс — fake без БД в unit-тестах.
type certPolicyResolver interface {
	Resolve(ctx context.Context, incarnationName string) (certpolicy.Policy, error)
}

// certRotatorDB — узкое подмножество pgxpool.Pool: открыть tx, в которой
// due-серты берутся FOR UPDATE SKIP LOCKED и ротируются атомарно.
type certRotatorDB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// CertRotatorConfig — policy of `rotate_due_certs` rule. Resolved from
// keeper.yml::reaper.rules.rotate_due_certs (Runner) per tick (hot-reload).
type CertRotatorConfig struct {
	// Threshold — how long before not_after cert is considered due. Zero → rule does not
	// rotate (protection from bad config: without threshold nothing to scan).
	Threshold time.Duration
	// JitterWindow — width of spread of effective threshold: each cert gets
	// -hash(cert_id)%jitter so certs with same not_after don't rotate in
	// one tick (anti-thundering-herd; Reaper tick is global — we spread by threshold).
	// Zero → no spread.
	JitterWindow time.Duration
	// MaxRotationsPerTick — cap of rotations per tick. <=0 → defaultMaxRotationsPerTick.
	MaxRotationsPerTick int
	// DefaultPKIMount — PKI mount для SignCSR, если у warrant-строки не задан свой
	// pki_mount. PKI role приходит из манифеста (certpolicy.Policy.PKIRole), не из config.
	DefaultPKIMount string
}

// CertRotatorDeps — constructor dependencies.
type CertRotatorDeps struct {
	Signer certissue.Signer
	Vault  CertVaultWriter
	// CSRGen generates keypair+CSR (keeper-side, R2). Prod — wrapper over
	// vault.GenerateServiceCSR. Separate field (not hard dependency on
	// vault package) so unit tests can provide deterministic fake.
	CSRGen func(commonName string, dnsNames []string) (privateKeyPEM, csrPEM []byte, err error)
	// Cfg — провайдер политики (hot-reload: читается на каждом Run).
	Cfg func() CertRotatorConfig
	// Policy — резолвер cert-rotation-политики инкарнации из манифеста сервиса.
	Policy certPolicyResolver
	Audit  audit.Writer
	Logger *slog.Logger
	KID    string
}

// CertRotator — реализация правила `rotate_due_certs` (cert-rotation Вар1).
// Скан истекающих active-сертов (`not_after` с jitter) → per-cert цепочка
// ротации (резолв политики манифеста → CAS active→rotating → certissue.Issue →
// supersede+insert warrant → спавн Voyage(<scenario>)) → audit.
//
// Run signature is compatible with runDurationRule call of Runner. Default OFF +
// mandatory dry_run — at dispatch level (rule is map-driven, requires explicit
// enabled:true; dry_run by default — see Runner branch).
type CertRotator struct {
	pool    certRotatorDB
	signer  certissue.Signer
	vault   CertVaultWriter
	csrgen  func(commonName string, dnsNames []string) (privateKeyPEM, csrPEM []byte, err error)
	cfg     func() CertRotatorConfig
	policy  certPolicyResolver
	audit   audit.Writer
	logger  *slog.Logger
	kid     string
	nowFunc func() time.Time
}

// NewCertRotator конструирует ротатор. signer/vault/csrgen/cfg/policy обязательны
// (без них ротация невозможна — Run вернёт ошибку). audit/logger nil-safe.
func NewCertRotator(pool *pgxpool.Pool, d CertRotatorDeps) *CertRotator {
	return &CertRotator{
		pool:   pool,
		signer: d.Signer,
		vault:  d.Vault,
		csrgen: d.CSRGen,
		cfg:    d.Cfg,
		policy: d.Policy,
		audit:  d.Audit,
		logger: d.Logger,
		kid:    d.KID,
	}
}

// newCertRotatorFromDB — internal constructor for unit tests (fake pool).
func newCertRotatorFromDB(pool certRotatorDB, d CertRotatorDeps) *CertRotator {
	return &CertRotator{
		pool:   pool,
		signer: d.Signer,
		vault:  d.Vault,
		csrgen: d.CSRGen,
		cfg:    d.Cfg,
		policy: d.Policy,
		audit:  d.Audit,
		logger: d.Logger,
		kid:    d.KID,
	}
}

// selectDueCertsSQL — scan of expiring active cert drivers. FOR UPDATE SKIP
// LOCKED protects from race with concurrent tick of another Keeper instance.
//
// Rotation driver — kind='cert' (server certificate): its private key kind='key'
// is rotated as COMPANION within same chain (one cert+key pair), itself not
// scanned. kind='ca' in MVP is NOT rotated automatically (update of
// trusted root — separate, more careful process; CA lives longer than
// server cert). auto_rotate=true — per-cert opt-in (registry column, NOT
// incarnation.spec — doesn't require state_schema migration).
//
// Effective threshold with jitter: cert is due when
//
//	not_after - (hash(cert_id) % jitter_window) < NOW() + threshold.
//
// hash — per-row (fnv of cert_id), so jitter is applied AFTER candidate selection:
// rough predicate `not_after < NOW()+threshold+jitter_window` in SQL
// catches SUPERSET (everyone who MIGHT be due at maximum jitter), exact
// jitter-filter — in Go (isDue). SQL conservatively wider — correct.
//
// $1 = NOW()+threshold+jitter_window (upper bound-superset). LIMIT $2 — cap.
const selectDueCertsSQL = `
SELECT cert_id, incarnation_id, kind, vault_ref, serial_number, fingerprint,
       not_after, pki_mount
FROM warrant
WHERE status = 'active'
  AND auto_rotate = true
  AND kind = 'cert'
  AND not_after < $1
ORDER BY not_after ASC
LIMIT $2
FOR UPDATE SKIP LOCKED
`

// dueCert — rotation candidate from scan.
type dueCert struct {
	certID        string
	incarnationID string
	kind          keepercert.Kind
	vaultRef      string
	serialNumber  string
	fingerprint   string
	notAfter      time.Time
	pkiMount      *string
}

// Run executes one iteration of the rule. Returns count of actually spawned
// rotations (skips/lost-CAS don't count). dry_run is handled by Runner BEFORE
// Run call (like other rules) — here always "combat" pass.
//
// Arguments duration/batchSize from common runner contract are ignored: threshold/
// jitter/cap are taken from cfg() (rule-specific policy richer than single duration).
func (r *CertRotator) Run(ctx context.Context, _ time.Duration, _ int) (int64, error) {
	if r.pool == nil {
		return 0, fmt.Errorf("reaper.rotate_due_certs: pool is nil")
	}
	if r.signer == nil || r.vault == nil || r.csrgen == nil || r.cfg == nil || r.policy == nil {
		return 0, fmt.Errorf("reaper.rotate_due_certs: signer/vault/csrgen/cfg/policy are required")
	}

	cfg := r.cfg()
	if cfg.Threshold <= 0 {
		if r.logger != nil {
			r.logger.Info("reaper.rotate_due_certs: threshold=0, skip (nothing to scan)")
		}
		return 0, nil
	}
	limit := cfg.MaxRotationsPerTick
	if limit <= 0 {
		limit = defaultMaxRotationsPerTick
	}

	now := r.now()
	upper := now.Add(cfg.Threshold + cfg.JitterWindow)

	due, err := r.selectDue(ctx, upper, limit)
	if err != nil {
		return 0, err
	}

	var rotated int64
	for _, c := range due {
		if !r.isDue(c, now, cfg) {
			continue
		}
		did, perr := r.rotateOne(ctx, c, cfg)
		if perr != nil {
			// Error of one rotation does NOT drop tick: each rotation — own tx,
			// independent. Log and continue (partial progress better than
			// full rollback on bulk expiration).
			if r.logger != nil {
				r.logger.Error("reaper.rotate_due_certs: cert rotation failed",
					slog.String("cert_id", c.certID),
					slog.String("incarnation", c.incarnationID),
					slog.Any("error", perr))
			}
			continue
		}
		if did {
			rotated++
		}
	}
	return rotated, nil
}

// selectDue opens short tx only for scanning candidates and materializes them
// into slice (tx is rolled back — locks are released). Real rotation of each cert
// goes in its OWN tx (rotateOne), which re-captures row via CAS
// active→rotating.
//
// Why not one big tx for whole tick: rotation of one cert includes external
// I/O (Vault PKI sign, Vault write) — keeping PG-tx open for all this = long
// locks. single-winner per cert gives CAS active→rotating inside rotateOne
// (we don't rely on scan-tx lock holding).
func (r *CertRotator) selectDue(ctx context.Context, upper time.Time, limit int) ([]dueCert, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("reaper.rotate_due_certs: begin scan tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	rows, err := tx.Query(ctx, selectDueCertsSQL, upper, limit)
	if err != nil {
		return nil, fmt.Errorf("reaper.rotate_due_certs: scan: %w", err)
	}
	defer rows.Close()

	var out []dueCert
	for rows.Next() {
		var c dueCert
		var kindStr string
		if serr := rows.Scan(&c.certID, &c.incarnationID, &kindStr, &c.vaultRef,
			&c.serialNumber, &c.fingerprint, &c.notAfter, &c.pkiMount); serr != nil {
			return nil, fmt.Errorf("reaper.rotate_due_certs: scan row: %w", serr)
		}
		c.kind = keepercert.Kind(kindStr)
		out = append(out, c)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("reaper.rotate_due_certs: rows: %w", rerr)
	}
	return out, nil
}

// isDue applies individual jitter: cert is due when its not_after, shifted
// back by hash%jitter, falls under threshold. Without jitter (window=0) — direct threshold
// not_after < now+threshold.
func (r *CertRotator) isDue(c dueCert, now time.Time, cfg CertRotatorConfig) bool {
	jitter := time.Duration(0)
	if cfg.JitterWindow > 0 {
		jitter = time.Duration(hashMod(c.certID, int64(cfg.JitterWindow)))
	}
	effective := c.notAfter.Add(-jitter)
	return effective.Before(now.Add(cfg.Threshold))
}

// hashMod — deterministic fnv-hash of string modulo m (for jitter). m>0.
func hashMod(s string, m int64) int64 {
	if m <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64() % uint64(m))
}

func (r *CertRotator) now() time.Time {
	if r.nowFunc != nil {
		return r.nowFunc()
	}
	return time.Now().UTC()
}

// rotateOne conducts one server cert rotation. Returns (spawn-was, err).
//
// Резолв политики манифеста идёт ДО CAS: транзиент-ошибка резолва / выключенная
// секция / неизвестный сценарий / пустой pki_role → серт остаётся active БЕЗ
// markFailed (следующий тик повторит либо корректно пропустит). Только после
// захвата single-winner (CAS active→rotating) внешние фейлы (issue/commit) ведут
// к CAS rotating→failed (серт не возвращается в active, тик не зациклится).
func (r *CertRotator) rotateOne(ctx context.Context, c dueCert, cfg CertRotatorConfig) (bool, error) {
	pol, perr := r.policy.Resolve(ctx, c.incarnationID)
	if perr != nil {
		// Транзиент (git/PG): серт остаётся active, БЕЗ markFailed — retry на след. тик.
		if r.logger != nil {
			r.logger.Warn("reaper.rotate_due_certs: резолв политики провален, серт остаётся active",
				slog.String("cert_id", c.certID),
				slog.String("incarnation", c.incarnationID),
				slog.Any("error", perr))
		}
		return false, nil
	}
	if !pol.Enabled {
		return false, nil // нет секции / enable:false — авто-ротация выключена
	}
	scenarioOK := false
	for _, s := range pol.KnownScenarios {
		if s == pol.Scenario {
			scenarioOK = true
			break
		}
	}
	if !scenarioOK {
		if r.logger != nil {
			r.logger.Warn("reaper.rotate_due_certs: сценарий ротации не найден в сервисе, спавн пропущен",
				slog.String("incarnation", c.incarnationID),
				slog.String("scenario", pol.Scenario))
		}
		return false, nil
	}
	mount, role := r.resolvePKI(c, cfg, pol)
	if role == "" {
		if r.logger != nil {
			r.logger.Warn("reaper.rotate_due_certs: пустой pki_role (manifest-drift), серт пропущен",
				slog.String("incarnation", c.incarnationID))
		}
		return false, nil
	}

	// (1) Capture single-winner.
	won, err := r.casToRotating(ctx, c.certID)
	if err != nil {
		return false, fmt.Errorf("cas active→rotating: %w", err)
	}
	if !won {
		// Lost race (another tick/instance already rotating this row) —
		// idempotent no-op. Not an error.
		return false, nil
	}

	// (2) Внешние I/O вне tx. Любой фейл → failed + err.
	material, ierr := r.issueMaterial(ctx, c, pol.Service, mount, role)
	if ierr != nil {
		r.markFailed(ctx, c.certID)
		return false, ierr
	}

	// (3) Атомарно: supersede+insert warrant (cert+key) + спавн Voyage.
	voyageID, cerr := r.commitRotation(ctx, c, material, pol.Scenario, role)
	if cerr != nil {
		r.markFailed(ctx, c.certID)
		return false, cerr
	}

	// (4) Audit after commit (best-effort).
	r.emitRotated(ctx, c, material, voyageID)
	return true, nil
}

// resolvePKI выбирает mount/role: role — из манифеста (pol.PKIRole, обязателен);
// mount — config-дефолт с warrant-override (c.pkiMount бьёт cfg.DefaultPKIMount).
func (r *CertRotator) resolvePKI(c dueCert, cfg CertRotatorConfig, pol certpolicy.Policy) (mount, role string) {
	mount, role = cfg.DefaultPKIMount, pol.PKIRole
	if c.pkiMount != nil && *c.pkiMount != "" {
		mount = *c.pkiMount
	}
	return mount, role
}

// casToRotating — CAS active→rotating by cert_id. true = captured.
func (r *CertRotator) casToRotating(ctx context.Context, certID string) (bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	n, err := keepercert.MarkStatus(ctx, tx, certID, keepercert.StatusActive, keepercert.StatusRotating)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return n == 1, nil
}

// markFailed — best-effort transition rotating→failed (row doesn't return to
// active so next tick doesn't count it due). Error is logged, doesn't drop
// tick (cert outside active — worst case hangs in rotating until manual triage).
func (r *CertRotator) markFailed(ctx context.Context, certID string) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("reaper.rotate_due_certs: markFailed begin tx", slog.String("cert_id", certID), slog.Any("error", err))
		}
		return
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, merr := keepercert.MarkStatus(ctx, tx, certID, keepercert.StatusRotating, keepercert.StatusFailed); merr != nil {
		if r.logger != nil {
			r.logger.Warn("reaper.rotate_due_certs: markFailed", slog.String("cert_id", certID), slog.Any("error", merr))
		}
		return
	}
	_ = tx.Commit(ctx)
}

// issueMaterial выпускает новый cert+key через certissue.Issue (csrgen → SignCSR
// → WriteKV cert+key по service-scoped E3-путям secret/<service>/<inc>/tls/{cert,key}).
//
// ★ R2: приватник генерится Keeper-ом и пишется только в Vault — он НИКОГДА не
// логируется / не кладётся в audit / не попадает в текст ошибки (см. certissue).
func (r *CertRotator) issueMaterial(ctx context.Context, c dueCert, service, mount, role string) (*certissue.Material, error) {
	cn := c.incarnationID + ".tls"
	return certissue.Issue(ctx, r.signer, r.vault, r.csrgen, certissue.Params{
		CommonName: cn,
		DNSNames:   []string{cn, c.incarnationID},
		Mount:      mount,
		Role:       role,
		CertPath:   certissue.VaultPath(service, c.incarnationID, keepercert.KindCert),
		KeyPath:    certissue.VaultPath(service, c.incarnationID, keepercert.KindKey),
	})
}

// commitRotation в одной tx: supersede старых active cert+key + insert новых +
// спавн Voyage(<scenario>). Возвращает voyage_id. role пишется в PKIRole нового
// cert (выравнивание записи с манифестом), scenario — имя спавнимого сценария.
func (r *CertRotator) commitRotation(ctx context.Context, c dueCert, m *certissue.Material, scenario, role string) (string, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("begin rotation tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	voyageID := audit.NewULID()

	// Captured rotating-row (same cert_id) is transitioned to superseded (history
	// of rotations). No active cert after this — inserting new active won't violate
	// partial-unique.
	if _, merr := keepercert.MarkStatus(ctx, tx, c.certID, keepercert.StatusRotating, keepercert.StatusSuperseded); merr != nil {
		return "", fmt.Errorf("supersede rotating cert: %w", merr)
	}
	newCert := &keepercert.Warrant{
		IncarnationID:        c.incarnationID,
		Kind:                 keepercert.KindCert,
		VaultRef:             m.CertRef,
		SerialNumber:         m.SerialNumber,
		Fingerprint:          m.Fingerprint,
		NotAfter:             m.NotAfter,
		Status:               keepercert.StatusActive,
		AutoRotate:           true,
		LastRotationVoyageID: &voyageID,
		PKIRole:              &role, // из манифеста (certpolicy.Policy.PKIRole)
	}
	if c.pkiMount != nil {
		newCert.PKIMount = c.pkiMount
	}
	if r.kid != "" {
		newCert.IssuedByKID = &r.kid
	}
	// Insert directly into current tx (not RegisterActive — that would open its own tx);
	// supersede of old cert-row already done above.
	if ierr := keepercert.Insert(ctx, tx, newCert); ierr != nil {
		return "", fmt.Errorf("insert new active cert: %w", ierr)
	}

	// Спутник key: приватник обновился вместе с cert. Supersede прежней active
	// key-строки (если была) + insert новой с тем же not_after. fingerprint =
	// SHA-256(SubjectPublicKeyInfo) пары — публичный ключ у cert и key один и тот
	// же, поэтому берём уже посчитанный m.Fingerprint (второй parse PEM излишен).
	if serr := keepercert.SupersedeActive(ctx, tx, c.incarnationID, keepercert.KindKey); serr != nil {
		return "", fmt.Errorf("supersede key: %w", serr)
	}
	newKey := &keepercert.Warrant{
		IncarnationID:        c.incarnationID,
		Kind:                 keepercert.KindKey,
		VaultRef:             m.KeyRef,
		SerialNumber:         m.SerialNumber,
		Fingerprint:          m.Fingerprint,
		NotAfter:             m.NotAfter,
		Status:               keepercert.StatusActive,
		AutoRotate:           false, // key rotates as satellite of cert, is not driver itself
		LastRotationVoyageID: &voyageID,
	}
	if r.kid != "" {
		newKey.IssuedByKID = &r.kid
	}
	if ierr := keepercert.Insert(ctx, tx, newKey); ierr != nil {
		return "", fmt.Errorf("insert new active key: %w", ierr)
	}

	// Voyage(<scenario>) — доставка нового PEM на хосты инкарнации.
	v, targets := buildRotateTLSVoyage(voyageID, c.incarnationID, m, scenario)
	if ierr := voyage.Insert(ctx, tx, v); ierr != nil {
		return "", fmt.Errorf("insert voyage: %w", ierr)
	}
	if ierr := voyage.InsertTargets(ctx, tx, voyageID, targets); ierr != nil {
		return "", fmt.Errorf("insert voyage targets: %w", ierr)
	}

	if cerr := tx.Commit(ctx); cerr != nil {
		return "", fmt.Errorf("commit rotation: %w", cerr)
	}
	committed = true
	return voyageID, nil
}

// buildRotateTLSVoyage собирает Voyage kind=scenario (имя из scenario) для
// инкарнации + один target (incarnation-name → вся инкарнация целиком). Input
// несёт новые cert_ref/key_ref (ca_ref сценарий возьмёт из state.tls.ca_ref — CA
// здесь не ротируется).
func buildRotateTLSVoyage(voyageID, incarnation string, m *certissue.Material, scenario string) (*voyage.Voyage, []voyage.VoyageTarget) {
	input, _ := json.Marshal(map[string]any{
		"cert_ref": m.CertRef,
		"key_ref":  m.KeyRef,
	})
	resolved, _ := json.Marshal([]string{incarnation})

	v := &voyage.Voyage{
		VoyageID:       voyageID,
		Kind:           voyage.KindScenario,
		ScenarioName:   &scenario,
		Input:          input,
		TargetResolved: resolved,
		TotalBatches:   1,
		Status:         voyage.StatusPending,
		StartedByAID:   rotateCertsSystemAID,
	}
	targets := []voyage.VoyageTarget{{
		TargetKind: voyage.TargetKindIncarnation,
		TargetID:   incarnation,
		BatchIndex: 0,
		Status:     voyage.TargetStatusAwaiting,
	}}
	return v, targets
}

// emitRotated пишет cert.rotated (best-effort, nil-safe). source=keeper_internal,
// correlation_id=voyage_id. Секреты (PEM/приватник) в payload НЕ кладутся.
func (r *CertRotator) emitRotated(ctx context.Context, c dueCert, m *certissue.Material, voyageID string) {
	if r.audit == nil {
		return
	}
	ev := &audit.Event{
		EventType:     audit.EventCertRotated,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: voyageID,
		Payload: map[string]any{
			"incarnation":        c.incarnationID,
			"kind":               string(keepercert.KindCert),
			"voyage_id":          voyageID,
			"fingerprint":        m.Fingerprint,
			"serial_number":      m.SerialNumber,
			"not_after":          m.NotAfter.Format(time.RFC3339),
			"superseded_cert_id": c.certID,
			"superseded_serial":  c.serialNumber,
		},
	}
	if err := r.audit.Write(ctx, ev); err != nil && r.logger != nil {
		r.logger.Warn("reaper.rotate_due_certs: audit write failed",
			slog.String("cert_id", c.certID), slog.Any("error", err))
	}
}
