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

// rotateCertsSystemAID — AID, от имени которого спавнится фоновый rotate_tls-
// Voyage. Совпадает с посеянным system-оператором (миграция 086,
// push.AutoImportSystemAID); дублируется здесь константой, чтобы reaper не тянул
// push-пакет ради одного идентификатора.
const rotateCertsSystemAID = "archon-system"

// rotateTLSScenario — имя day-2-сценария, доставляющего новый TLS-материал на
// хосты инкарнации (examples/service/redis/scenario/rotate_tls). Voyage
// kind=scenario с этим именем таргетит инкарнацию целиком (rotate_tls: `on`
// опущен → весь incarnation).
const rotateTLSScenario = "rotate_tls"

// defaultMaxRotationsPerTick — потолок ротаций за тик по умолчанию (anti-lavina
// при массовом истечении сертов).
const defaultMaxRotationsPerTick = 20

// PKISigner — узкая поверхность vault.Client.SignCSR (Vault PKI sign-RPC),
// нужная ротатору. Сужение допускает fake без Vault в unit-тестах.
type PKISigner interface {
	SignCSR(ctx context.Context, mount, role, csrPEM string) (*SignedCert, error)
}

// SignedCert — результат PKI-подписи (зеркало vault.SignedCertificate; reaper не
// импортирует vault-пакет ради типа, adapter в wire-up конвертирует).
type SignedCert struct {
	CertificatePEM []byte
	CAChainPEM     []byte
	SerialNumber   string
	NotAfter       time.Time
}

// CertVaultWriter — узкая поверхность vault.Client.WriteKV, нужная ротатору.
type CertVaultWriter interface {
	WriteKV(ctx context.Context, path string, data map[string]any) error
}

// certRotatorDB — узкое подмножество pgxpool.Pool: открыть tx, в которой
// due-серты берутся FOR UPDATE SKIP LOCKED и ротируются атомарно.
type certRotatorDB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// CertRotatorConfig — политика правила `rotate_due_certs`. Резолвится из
// keeper.yml::reaper.rules.rotate_due_certs (Runner) на каждый тик (hot-reload).
type CertRotatorConfig struct {
	// Threshold — за сколько до not_after серт считается due. Zero → правило не
	// ротирует (защита от кривого конфига: без порога сканировать нечего).
	Threshold time.Duration
	// JitterWindow — ширина разброса эффективного порога: каждый серт получает
	// -hash(cert_id)%jitter, чтобы серты с одинаковым not_after не ротировались в
	// один тик (anti-thundering-herd; тик Reaper общий — размазываем по порогу).
	// Zero → без разброса.
	JitterWindow time.Duration
	// MaxRotationsPerTick — потолок ротаций за тик. <=0 → defaultMaxRotationsPerTick.
	MaxRotationsPerTick int
	// DefaultPKIMount / DefaultPKIRole — PKI mount/role для SignCSR, если у
	// warrant-строки не задан свой pki_mount/pki_role. Оба пусты (и в строке, и в
	// config) → ротация серта падает (нечем подписать) с пометкой failed.
	DefaultPKIMount string
	DefaultPKIRole  string
}

// CertRotatorDeps — зависимости конструктора.
type CertRotatorDeps struct {
	Signer PKISigner
	Vault  CertVaultWriter
	// CSRGen генерит keypair+CSR (keeper-side, R2). Прод — обёртка над
	// vault.GenerateServiceCSR. Отдельным полем (не жёсткой зависимостью на
	// vault-пакет), чтобы unit-тесты подставляли детерминированный fake.
	CSRGen func(commonName string, dnsNames []string) (privateKeyPEM, csrPEM []byte, err error)
	// Cfg — провайдер политики (hot-reload: читается на каждом Run).
	Cfg    func() CertRotatorConfig
	Audit  audit.Writer
	Logger *slog.Logger
	KID    string
}

// CertRotator — реализация правила `rotate_due_certs` (cert-rotation Вар1).
// Скан истекающих active-сертов (`not_after` с jitter) → per-cert цепочка
// ротации (CAS active→rotating single-winner → csrgen → SignCSR → WriteKV →
// supersede+insert warrant → спавн Voyage(rotate_tls)) → audit.
//
// Сигнатура Run совместима с runDurationRule-вызовом Runner-а. Default OFF +
// обязательный dry_run — на уровне dispatch (правило map-driven, требует явного
// enabled:true; dry_run по умолчанию — см. Runner-ветку).
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

// NewCertRotator конструирует ротатор. signer/vault/csrgen/cfg обязательны (без
// них ротация невозможна — Run вернёт ошибку). audit/logger nil-safe.
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

// newCertRotatorFromDB — внутренний конструктор для unit-тестов (fake pool).
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

// selectDueCertsSQL — скан истекающих active-сертов-драйверов. FOR UPDATE SKIP
// LOCKED защищает от гонки с конкурентным тиком другого Keeper-инстанса.
//
// Драйвер ротации — kind='cert' (серверный сертификат): его приватник kind='key'
// ротируется как СПУТНИК внутри той же цепочки (одна пара cert+key), сам не
// сканируется. kind='ca' в MVP НЕ ротируется автоматически (обновление
// доверенного корня — отдельный, более осторожный процесс; CA живёт дольше
// серверного серта). auto_rotate=true — per-cert opt-out (колонка реестра, НЕ
// incarnation.spec — не тянет state_schema-миграцию).
//
// Эффективный порог с jitter: серт due, когда
//
//	not_after - (hash(cert_id) % jitter_window) < NOW() + threshold.
//
// hash — per-строка (fnv по cert_id), поэтому jitter применяется ПОСЛЕ выборки
// кандидатов: грубый предикат `not_after < NOW()+threshold+jitter_window` в SQL
// ловит СУПЕРСЕТ (все, кто МОЖЕТ быть due при максимальном jitter), точный
// jitter-фильтр — в Go (isDue). SQL консервативно шире — корректно.
//
// $1 = NOW()+threshold+jitter_window (верхняя граница-суперсет). LIMIT $2 — cap.
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

// dueCert — кандидат ротации из скана.
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

// Run выполняет одну итерацию правила. Возвращает число фактически заспавненных
// ротаций (skip/lost-CAS в счётчик не идут). dry_run обрабатывается Runner-ом ДО
// вызова Run (как у прочих правил) — здесь всегда «боевой» проход.
//
// Аргументы duration/batchSize из общего runner-контракта игнорируются: порог/
// jitter/cap берутся из cfg() (правило-специфичная политика богаче одного duration).
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
			r.logger.Info("reaper.rotate_due_certs: threshold=0, пропуск (нечего сканировать)")
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
			// Ошибка одной ротации НЕ роняет тик: каждая ротация — своя tx,
			// независимая. Логируем и продолжаем (частичный прогресс полезнее
			// полного отката при массовом истечении).
			if r.logger != nil {
				r.logger.Error("reaper.rotate_due_certs: ротация серта провалена",
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

// selectDue открывает короткую tx только для скана кандидатов и материализует их
// в срез (tx откатывается — locks отпускаются). Реальная ротация каждого серта
// идёт в СВОЕЙ tx (rotateOne), которая заново захватывает строку через CAS
// active→rotating.
//
// Почему не одна большая tx на весь тик: ротация одного серта включает внешние
// I/O (Vault PKI sign, Vault write) — держать PG-tx открытой на всё это = долгие
// locks. single-winner на каждый серт даёт CAS active→rotating внутри rotateOne
// (не полагаемся на удержание lock скан-tx).
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

// isDue применяет индивидуальный jitter: серт due, когда его not_after, сдвинутый
// назад на hash%jitter, попадает под порог. Без jitter (window=0) — прямой порог
// not_after < now+threshold.
func (r *CertRotator) isDue(c dueCert, now time.Time, cfg CertRotatorConfig) bool {
	jitter := time.Duration(0)
	if cfg.JitterWindow > 0 {
		jitter = time.Duration(hashMod(c.certID, int64(cfg.JitterWindow)))
	}
	effective := c.notAfter.Add(-jitter)
	return effective.Before(now.Add(cfg.Threshold))
}

// hashMod — детерминированный fnv-хеш строки по модулю m (для jitter). m>0.
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

// rotateOne проводит одну ротацию серверного серта. Возвращает (спавн-был, err).
//
// Цепочка (design.md):
//  1. CAS active→rotating (single-winner-барьер: строку захватывает ровно один
//     тик/инстанс; проигравший получает rows=0 и уходит без работы — против
//     двойного спавна Voyage). НЕ прошёл CAS → (false, nil): idempotency.
//  2. Внешние I/O ВНЕ tx (долгие round-trip-ы не держат PG-lock): csrgen (R2) →
//     SignCSR (Vault PKI) → WriteKV cert+key в Vault (E3-пути secret/redis/<inc>/
//     tls/{cert,key}). Любой фейл → CAS rotating→failed (строка не возвращается в
//     active, следующий тик её не считает due и не зациклится) + return err.
//  3. PG-tx: supersede rotating-строки cert + insert новой active cert +
//     supersede/insert active key + insert Voyage(rotate_tls)+targets. Всё в одной
//     tx (эталон conductor.processOne): при крэше до commit ничего не зафиксировано,
//     серт остаётся rotating. commit.
//  4. Audit cert.rotated ПОСЛЕ commit (best-effort).
func (r *CertRotator) rotateOne(ctx context.Context, c dueCert, cfg CertRotatorConfig) (bool, error) {
	mount, role := r.resolvePKI(c, cfg)
	if mount == "" || role == "" {
		r.markFailed(ctx, c.certID)
		return false, fmt.Errorf("no PKI mount/role for cert %s (warrant + config both empty)", c.certID)
	}

	// (1) Захват single-winner.
	won, err := r.casToRotating(ctx, c.certID)
	if err != nil {
		return false, fmt.Errorf("cas active→rotating: %w", err)
	}
	if !won {
		// Проиграли гонку (другой тик/инстанс уже ротирует эту строку) —
		// idempotent no-op. Не ошибка.
		return false, nil
	}

	// (2) Внешние I/O вне tx. Любой фейл → failed + err.
	material, ierr := r.issueMaterial(ctx, c, mount, role)
	if ierr != nil {
		r.markFailed(ctx, c.certID)
		return false, ierr
	}

	// (3) Атомарно: supersede+insert warrant (cert+key) + спавн Voyage.
	voyageID, cerr := r.commitRotation(ctx, c, material)
	if cerr != nil {
		r.markFailed(ctx, c.certID)
		return false, cerr
	}

	// (4) Audit после commit (best-effort).
	r.emitRotated(ctx, c, material, voyageID)
	return true, nil
}

// resolvePKI выбирает mount/role: warrant-строка бьёт config-дефолт.
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

// casToRotating — CAS active→rotating по cert_id. true = захватили.
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

// markFailed — best-effort перевод rotating→failed (строка не возвращается в
// active, чтобы следующий тик не считал её due). Ошибка логируется, не роняет
// тик (серт вне active — worst case повиснет в rotating до ручного триажа).
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

// rotatedMaterial — результат issueMaterial: новый серверный cert+key + refs.
type rotatedMaterial struct {
	certPEM      []byte
	keyPEM       []byte
	serialNumber string
	fingerprint  string
	notAfter     time.Time
	certRef      string // E3-путь+field нового cert (secret/redis/<inc>/tls/cert#cert)
	keyRef       string
}

// issueMaterial генерит keypair+CSR (R2), подписывает через Vault PKI и кладёт
// cert+key в Vault по E3-путям. Возвращает материал для commitRotation.
//
// ★ R2: приватник (privPEM) генерится Keeper-ом и пишется в Vault. Он НИКОГДА не
// логируется / не кладётся в audit / не возвращается наружу reaper-а — только в
// Vault (осознанное исключение из identity-инварианта, см. vault/csrgen.go).
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
	// signed.CAChainPEM намеренно отбрасывается: CA здесь не ротируется, хосты
	// продолжают доверять текущему корню из state.tls.ca_ref (см. buildRotateTLSVoyage).

	certPath := certVaultPath(c.incarnationID, keepercert.KindCert)
	keyPath := certVaultPath(c.incarnationID, keepercert.KindKey)

	// cert-PEM в поле `cert`, key-PEM в поле `key` (парити essence-конвенции
	// tls_cert_ref "<path>#cert"). WriteKV значения в текст ошибки не кладёт
	// (vault.Client-инвариант).
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

// commitRotation в одной tx: supersede старых active cert+key + insert новых +
// спавн Voyage(rotate_tls). Возвращает voyage_id.
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

	// Захваченную rotating-строку (тот же cert_id) переводим в superseded (история
	// ротаций). Active cert после этого нет — insert новой active не нарушит
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
	// Insert напрямую в текущую tx (не RegisterActive — тот открыл бы свою tx);
	// supersede старой cert-строки уже сделан выше.
	if ierr := keepercert.Insert(ctx, tx, newCert); ierr != nil {
		return "", fmt.Errorf("insert new active cert: %w", ierr)
	}

	// Спутник key: приватник обновился вместе с cert. Supersede прежней active
	// key-строки (если была) + insert новой с тем же not_after. fingerprint =
	// SHA-256(SubjectPublicKeyInfo) пары — публичный ключ у cert и key один и тот
	// же, поэтому берём уже посчитанный m.fingerprint (второй parse PEM излишен).
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
		AutoRotate:           false, // key ротируется как спутник cert, сам не драйвер
		LastRotationVoyageID: &voyageID,
	}
	if r.kid != "" {
		newKey.IssuedByKID = &r.kid
	}
	if ierr := keepercert.Insert(ctx, tx, newKey); ierr != nil {
		return "", fmt.Errorf("insert new active key: %w", ierr)
	}

	// Voyage(rotate_tls) — доставка нового PEM на хосты инкарнации.
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

// buildRotateTLSVoyage собирает Voyage kind=scenario (rotate_tls) для инкарнации
// + один target (incarnation-name). Input несёт новые cert_ref/key_ref
// (ca_ref rotate_tls возьмёт из state.tls.ca_ref — CA здесь не ротируется).
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

// emitRotated пишет cert.rotated (best-effort, nil-safe). source=keeper_internal,
// correlation_id=voyage_id. Секреты (PEM/приватник) в payload НЕ кладутся.
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

// certVaultPath строит E3-путь материала: secret/redis/<inc>/tls/<kind>.
// Синхронность с rotate_tls обязательна (сценарий читает vault(cert_ref) той же
// ветки). ★ Конвенция зафиксирована как E3-дефолт в ADR cert-rotation; при
// поддержке не-redis-сервисов путь станет service-параметризованным.
func certVaultPath(incarnation string, kind keepercert.Kind) string {
	return "secret/redis/" + incarnation + "/tls/" + string(kind)
}

// fingerprintFromPEM парсит первый CERTIFICATE-блок PEM и считает fingerprint
// SHA-256(SubjectPublicKeyInfo) — тот же способ, что keepercert.FingerprintFromCert
// / E1-модуль (fingerprint привязан к ключу).
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
