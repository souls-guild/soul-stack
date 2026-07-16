package reaper

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// rotateCertsSystemAID — AID on behalf of which the background rotate_tls-
// Voyage is spawned. Matches the seeded system operator (migration 086,
// push.AutoImportSystemAID); duplicated here as a constant so reaper doesn't
// need to pull the push package for one identifier.
const rotateCertsSystemAID = "archon-system"

// rotateTLSScenario — name of the day-2 scenario that delivers new TLS material to
// incarnation hosts (examples/service/redis/scenario/rotate_tls). Voyage
// kind=scenario with this name targets the entire incarnation (rotate_tls: `on`
// omitted → entire incarnation).
const rotateTLSScenario = "rotate_tls"

// defaultMaxRotationsPerTick — maximum rotations per tick by default (anti-thundering-herd
// when certificates expire in bulk).
const defaultMaxRotationsPerTick = 20

// PKISigner — narrow interface to vault.Client.SignCSR (Vault PKI sign-RPC),
// needed by rotator. Narrowing allows fake without Vault in unit tests.
type PKISigner interface {
	SignCSR(ctx context.Context, mount, role, csrPEM string) (*SignedCert, error)
}

// SignedCert — result of PKI signing (mirror of vault.SignedCertificate; reaper does not
// import vault package for the type, adapter in wire-up converts it).
type SignedCert struct {
	CertificatePEM []byte
	CAChainPEM     []byte
	SerialNumber   string
	NotAfter       time.Time
}

// CertVaultWriter — narrow interface to vault.Client.WriteKV, needed by rotator.
type CertVaultWriter interface {
	WriteKV(ctx context.Context, path string, data map[string]any) error
}

// certRotatorDB — narrow subset of pgxpool.Pool: begin tx where
// due certs are taken FOR UPDATE SKIP LOCKED and rotated atomically.
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
	// DefaultPKIMount / DefaultPKIRole — PKI mount/role for SignCSR, if
	// warrant row doesn't have its own pki_mount/pki_role. Both empty (in row and in
	// config) → cert rotation fails (nothing to sign) with failed mark.
	DefaultPKIMount string
	DefaultPKIRole  string
}

// CertRotatorDeps — constructor dependencies.
type CertRotatorDeps struct {
	Signer PKISigner
	Vault  CertVaultWriter
	// CSRGen generates keypair+CSR (keeper-side, R2). Prod — wrapper over
	// vault.GenerateServiceCSR. Separate field (not hard dependency on
	// vault package) so unit tests can provide deterministic fake.
	CSRGen func(commonName string, dnsNames []string) (privateKeyPEM, csrPEM []byte, err error)
	// Cfg — policy provider (hot-reload: read on each Run).
	Cfg    func() CertRotatorConfig
	Audit  audit.Writer
	Logger *slog.Logger
	KID    string
}

// CertRotator — implementation of `rotate_due_certs` rule (cert-rotation Var1).
// Scan of expiring active certs (`not_after` with jitter) → per-cert rotation chain
// (CAS active→rotating single-winner → csrgen → SignCSR → WriteKV →
// supersede+insert warrant → spawn Voyage(rotate_tls)) → audit.
//
// Run signature is compatible with runDurationRule call of Runner. Default OFF +
// mandatory dry_run — at dispatch level (rule is map-driven, requires explicit
// enabled:true; dry_run by default — see Runner branch).
type CertRotator struct {
	pool    certRotatorDB
	signer  PKISigner
	vault   CertVaultWriter
	csrgen  func(commonName string, dnsNames []string) (privateKeyPEM, csrPEM []byte, err error)
	cfg     func() CertRotatorConfig
	audit   audit.Writer
	logger  *slog.Logger
	kid     string
	nowFunc func() time.Time
}

// NewCertRotator constructs rotator. signer/vault/csrgen/cfg are required (without
// them rotation is impossible — Run will return error). audit/logger are nil-safe.
func NewCertRotator(pool *pgxpool.Pool, d CertRotatorDeps) *CertRotator {
	return &CertRotator{
		pool:   pool,
		signer: d.Signer,
		vault:  d.Vault,
		csrgen: d.CSRGen,
		cfg:    d.Cfg,
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
       not_after, pki_mount, pki_role
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
	pkiRole       *string
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
	if r.signer == nil || r.vault == nil || r.csrgen == nil || r.cfg == nil {
		return 0, fmt.Errorf("reaper.rotate_due_certs: signer/vault/csrgen/cfg are required")
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
			&c.serialNumber, &c.fingerprint, &c.notAfter, &c.pkiMount, &c.pkiRole); serr != nil {
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
// Chain (design.md):
//  1. CAS active→rotating (single-winner barrier: exactly one tick/instance
//     captures row; loser gets rows=0 and exits without work — against
//     double Voyage spawn). Didn't pass CAS → (false, nil): idempotency.
//  2. External I/O OUTSIDE tx (long round-trips don't hold PG-lock): csrgen (R2) →
//     SignCSR (Vault PKI) → WriteKV cert+key to Vault (E3-paths secret/redis/<inc>/
//     tls/{cert,key}). Any fail → CAS rotating→failed (row doesn't return to
//     active, next tick doesn't count it due and doesn't loop) + return err.
//  3. PG-tx: supersede rotating-row cert + insert new active cert +
//     supersede/insert active key + insert Voyage(rotate_tls)+targets. All in one
//     tx (canonical conductor.processOne): if crash before commit, nothing fixed,
//     cert stays rotating. commit.
//  4. Audit cert.rotated AFTER commit (best-effort).
func (r *CertRotator) rotateOne(ctx context.Context, c dueCert, cfg CertRotatorConfig) (bool, error) {
	mount, role := r.resolvePKI(c, cfg)
	if mount == "" || role == "" {
		r.markFailed(ctx, c.certID)
		return false, fmt.Errorf("no PKI mount/role for cert %s (warrant + config both empty)", c.certID)
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

	// (2) External I/O outside tx. Any fail → failed + err.
	material, ierr := r.issueMaterial(ctx, c, mount, role)
	if ierr != nil {
		r.markFailed(ctx, c.certID)
		return false, ierr
	}

	// (3) Atomically: supersede+insert warrant (cert+key) + spawn Voyage.
	voyageID, cerr := r.commitRotation(ctx, c, material)
	if cerr != nil {
		r.markFailed(ctx, c.certID)
		return false, cerr
	}

	// (4) Audit after commit (best-effort).
	r.emitRotated(ctx, c, material, voyageID)
	return true, nil
}

// resolvePKI selects mount/role: warrant-row beats config default.
func (r *CertRotator) resolvePKI(c dueCert, cfg CertRotatorConfig) (mount, role string) {
	mount, role = cfg.DefaultPKIMount, cfg.DefaultPKIRole
	if c.pkiMount != nil && *c.pkiMount != "" {
		mount = *c.pkiMount
	}
	if c.pkiRole != nil && *c.pkiRole != "" {
		role = *c.pkiRole
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

// rotatedMaterial — result of issueMaterial: new server cert+key + refs.
type rotatedMaterial struct {
	certPEM      []byte
	keyPEM       []byte
	serialNumber string
	fingerprint  string
	notAfter     time.Time
	certRef      string // E3-path+field of new cert (secret/redis/<inc>/tls/cert#cert)
	keyRef       string
}

// issueMaterial generates keypair+CSR (R2), signs via Vault PKI and puts
// cert+key in Vault by E3-paths. Returns material for commitRotation.
//
// ★ R2: private key (privPEM) is generated by Keeper and written to Vault. It is NEVER
// logged / placed in audit / returned outside reaper — only in
// Vault (conscious exception from identity invariant, see vault/csrgen.go).
func (r *CertRotator) issueMaterial(ctx context.Context, c dueCert, mount, role string) (*rotatedMaterial, error) {
	cn := c.incarnationID + ".tls"
	privPEM, csrPEM, err := r.csrgen(cn, []string{cn, c.incarnationID})
	if err != nil {
		return nil, fmt.Errorf("csrgen: %w", err)
	}

	signed, err := r.signer.SignCSR(ctx, mount, role, string(csrPEM))
	if err != nil {
		return nil, fmt.Errorf("sign csr: %w", err)
	}
	// signed.CAChainPEM is intentionally discarded: CA is not rotated here, hosts
	// continue to trust current root from state.tls.ca_ref (see buildRotateTLSVoyage).

	certPath := certVaultPath(c.incarnationID, keepercert.KindCert)
	keyPath := certVaultPath(c.incarnationID, keepercert.KindKey)

	// cert-PEM in field `cert`, key-PEM in field `key` (parity with essence convention
	// tls_cert_ref "<path>#cert"). WriteKV doesn't put values in error text
	// (vault.Client invariant).
	if werr := r.vault.WriteKV(ctx, certPath, map[string]any{"cert": string(signed.CertificatePEM)}); werr != nil {
		return nil, fmt.Errorf("write cert to vault: %w", werr)
	}
	if werr := r.vault.WriteKV(ctx, keyPath, map[string]any{"key": string(privPEM)}); werr != nil {
		return nil, fmt.Errorf("write key to vault: %w", werr)
	}

	fingerprint, err := fingerprintFromPEM(signed.CertificatePEM)
	if err != nil {
		return nil, fmt.Errorf("fingerprint new cert: %w", err)
	}

	return &rotatedMaterial{
		certPEM:      signed.CertificatePEM,
		keyPEM:       privPEM,
		serialNumber: signed.SerialNumber,
		fingerprint:  fingerprint,
		notAfter:     signed.NotAfter.UTC(),
		certRef:      certPath + "#cert",
		keyRef:       keyPath + "#key",
	}, nil
}

// commitRotation in one tx: supersede old active cert+key + insert new +
// spawn Voyage(rotate_tls). Returns voyage_id.
func (r *CertRotator) commitRotation(ctx context.Context, c dueCert, m *rotatedMaterial) (string, error) {
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
		VaultRef:             m.certRef,
		SerialNumber:         m.serialNumber,
		Fingerprint:          m.fingerprint,
		NotAfter:             m.notAfter,
		Status:               keepercert.StatusActive,
		AutoRotate:           true,
		LastRotationVoyageID: &voyageID,
	}
	if c.pkiMount != nil {
		newCert.PKIMount = c.pkiMount
	}
	if c.pkiRole != nil {
		newCert.PKIRole = c.pkiRole
	}
	if r.kid != "" {
		newCert.IssuedByKID = &r.kid
	}
	// Insert directly into current tx (not RegisterActive — that would open its own tx);
	// supersede of old cert-row already done above.
	if ierr := keepercert.Insert(ctx, tx, newCert); ierr != nil {
		return "", fmt.Errorf("insert new active cert: %w", ierr)
	}

	// Companion key: private key is updated together with cert. Supersede of previous active
	// key-row (if was) + insert new with same not_after. fingerprint =
	// SHA-256(SubjectPublicKeyInfo) of pair — public key of cert and key is same,
	// so we take already calculated m.fingerprint (second PEM parse unnecessary).
	if serr := keepercert.SupersedeActive(ctx, tx, c.incarnationID, keepercert.KindKey); serr != nil {
		return "", fmt.Errorf("supersede key: %w", serr)
	}
	newKey := &keepercert.Warrant{
		IncarnationID:        c.incarnationID,
		Kind:                 keepercert.KindKey,
		VaultRef:             m.keyRef,
		SerialNumber:         m.serialNumber,
		Fingerprint:          m.fingerprint,
		NotAfter:             m.notAfter,
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

	// Voyage(rotate_tls) — delivery of new PEM to incarnation hosts.
	v, targets := buildRotateTLSVoyage(voyageID, c.incarnationID, m)
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

// buildRotateTLSVoyage assembles Voyage kind=scenario (rotate_tls) for incarnation
// + one target (incarnation-name). Input carries new cert_ref/key_ref
// (ca_ref rotate_tls takes from state.tls.ca_ref — CA is not rotated here).
func buildRotateTLSVoyage(voyageID, incarnation string, m *rotatedMaterial) (*voyage.Voyage, []voyage.VoyageTarget) {
	scenario := rotateTLSScenario
	input, _ := json.Marshal(map[string]any{
		"cert_ref": m.certRef,
		"key_ref":  m.keyRef,
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

// emitRotated writes cert.rotated (best-effort, nil-safe). source=keeper_internal,
// correlation_id=voyage_id. Secrets (PEM/private key) are NOT placed in payload.
func (r *CertRotator) emitRotated(ctx context.Context, c dueCert, m *rotatedMaterial, voyageID string) {
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
			"fingerprint":        m.fingerprint,
			"serial_number":      m.serialNumber,
			"not_after":          m.notAfter.Format(time.RFC3339),
			"superseded_cert_id": c.certID,
			"superseded_serial":  c.serialNumber,
		},
	}
	if err := r.audit.Write(ctx, ev); err != nil && r.logger != nil {
		r.logger.Warn("reaper.rotate_due_certs: audit write failed",
			slog.String("cert_id", c.certID), slog.Any("error", err))
	}
}

// certVaultPath builds E3-path of material: secret/redis/<inc>/tls/<kind>.
// Synchronization with rotate_tls is required (scenario reads vault(cert_ref) from same
// branch). ★ Convention is fixed as E3 default in ADR cert-rotation; when
// supporting non-redis services, path will become service-parametrized.
func certVaultPath(incarnation string, kind keepercert.Kind) string {
	return "secret/redis/" + incarnation + "/tls/" + string(kind)
}

// fingerprintFromPEM parses the first CERTIFICATE block of PEM and calculates fingerprint
// SHA-256(SubjectPublicKeyInfo) — same method as keepercert.FingerprintFromCert
// / E1-module (fingerprint is tied to key).
func fingerprintFromPEM(pemBytes []byte) (string, error) {
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return "", fmt.Errorf("no CERTIFICATE block in PEM")
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return "", fmt.Errorf("parse certificate: %w", err)
			}
			return keepercert.FingerprintFromCert(cert), nil
		}
	}
}
