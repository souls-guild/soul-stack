// Package bootstrap — логика `keeper init` (ADR-013).
//
// Init под PG advisory lock проверяет «реестр operators пуст», вставляет
// первого Архонта (`created_by_aid: NULL`), выпускает JWT (TTL
// `auth.jwt.ttl_bootstrap`, claim `bootstrap_initial: true`, role
// `cluster-admin`), пишет audit-event `operator.created` (source
// `keeper_internal`, `archon_aid: NULL`) и сохраняет токен в файл с
// `mode 0400`. Повторный вызов на непустом реестре → [ErrAlreadyInitialized].
//
// Пакет не управляет lifecycle Postgres-пула / Vault-клиента / JWT-issuer-а
// — caller (`keeper/cmd/keeper`) собирает зависимости и передаёт через
// [Config]; bootstrap-логика чисто оркестрационная.
package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// AdvisoryLockID — int64-литерал для PG advisory lock-а bootstrap-а.
// Значение `0x534f554c` — ASCII `"SOUL"` (по одному байту на символ).
// Все ноды Keeper-кластера видят один и тот же lock-namespace; даже при
// одновременном запуске двух `keeper init` второй блокируется до COMMIT
// первого, после чего видит непустой реестр и завершается с
// [ErrAlreadyInitialized].
const AdvisoryLockID int64 = 0x534f554c

// BootstrapRoleClusterAdmin — единственная роль, выпускаемая первому
// Архонту по ADR-013/rbac.md.
const BootstrapRoleClusterAdmin = "cluster-admin"

// vaultSigningKeyField — имя поля внутри Vault KV-secret, в котором лежит
// signing-key JWT (base64-encoded). Совпадает с golden-форматом, который
// засевают и интеграционные тесты, и `vault kv put`-команда в local-dev.
const vaultSigningKeyField = "signing_key"

// credentialFileMode — права на файл с JWT-токеном (read-only owner).
// ADR-013(c): JWT не должен быть читаем другими пользователями.
const credentialFileMode os.FileMode = 0o400

// ErrAlreadyInitialized возвращается Init-ом, когда `Count(operators) > 0`
// под удерживаемым advisory lock-ом. Caller (`keeper/cmd/keeper`)
// маппит в exit-code 1 + сообщение «already initialized».
var ErrAlreadyInitialized = errors.New("bootstrap: keeper already initialized (operators registry not empty)")

// ErrSigningKeyMissing возвращается, если в Vault KV нет поля
// `signing_key` либо оно пустое.
var ErrSigningKeyMissing = errors.New("bootstrap: signing_key field missing or empty in Vault KV")

// ErrAuditWriteFailed возвращается, если audit-write упал ПОСЛЕ
// успешного COMMIT-а insert-а operator-а. Operator уже в БД, audit
// потерян — caller должен предупредить администратора о необходимости
// ручной сверки (manual reconciliation). Error содержит обёрнутый
// оригинальный pgx-error для диагностики.
var ErrAuditWriteFailed = errors.New("bootstrap: audit write failed")

// ErrTokenFileWriteFailed возвращается, если writeTokenFile упал
// ПОСЛЕ COMMIT-а insert-а operator-а И успешного audit-write-а. То есть
// БД консистентна, аудит на месте, но JWT-файл не сохранён.
//
// Стратегия recovery (PM-decision M0.5c review:b): caller печатает JWT
// в stderr с предупреждением «токен скомпрометирован — ротировать ASAP».
// Альтернатива (write до COMMIT через TempFile+Rename) отвергнута:
// она не страхует от рантайм-проблем самого writeTokenFile (например,
// permission на target dir определяется только в момент write).
//
// Result.Token заполняется ТОЛЬКО при ErrTokenFileWriteFailed — в
// happy-path токен в Result отсутствует (он в файле).
var ErrTokenFileWriteFailed = errors.New("bootstrap: token file write failed")

// JWTIssuer — узкий интерфейс над `keeper/internal/jwt.Issuer`. Сужение
// нужно для unit-тестов (без подгрузки signing-key и golang-jwt). Реальный
// `*jwt.Issuer` удовлетворяет интерфейсу автоматически.
type JWTIssuer interface {
	Issue(aid string, roles []string, ttl time.Duration, bootstrapInitial bool) (string, error)
}

// Config — все зависимости, нужные Init. Заполняется в
// `keeper/cmd/keeper`, не парсит keeper.yml сам.
type Config struct {
	// ArchonAID — AID нового Архонта (флаг `--archon`). Должен пройти
	// [operator.ValidAID].
	ArchonAID string

	// DisplayName — display_name в реестре. Если пуст — используется
	// ArchonAID (PM-decision №5).
	DisplayName string

	// TTLBootstrap — TTL JWT-токена первого Архонта. Берётся из
	// `keeper.yml::auth.jwt.ttl_bootstrap` (default 720h).
	TTLBootstrap time.Duration

	// Pool — pgxpool.Pool с применёнными миграциями (003 + 004 уже
	// поставили `operators` и FK).
	Pool *pgxpool.Pool

	// VaultClient — для чтения signing-key. Обязателен (nil → ошибка
	// в validateConfig). Unit-тесты, проверяющие логику без Vault,
	// идут через интеграцию (integration_test.go) — без mock-Vault-а
	// внутри пакета.
	VaultClient *keepervault.Client

	// SigningKeyRef — строка из `keeper.yml::auth.jwt.signing_key_ref`
	// в форме `vault:<path>`. Парсится [parseVaultRef]. Пустая строка
	// или некорректная форма → error.
	SigningKeyRef string

	// IssuerFactory — фабрика JWT-issuer-а от signingKey. Тесты
	// передают mock; keeper/cmd/keeper — реальный jwt.NewIssuer.
	IssuerFactory func(signingKey []byte) (JWTIssuer, error)

	// AuditWriter — куда писать `operator.created`-event.
	AuditWriter audit.Writer

	// CredentialOutput — путь к файлу, в который пишется JWT-токен.
	// Пустая строка → fallback `/tmp/keeper-init-<aid>.token`.
	CredentialOutput string
}

// Result — возврат успешного Init. Используется caller-ом для финального
// stdout-сообщения; токен в Result в логи НЕ выводится (исключение —
// ErrTokenFileWriteFailed recovery, см. поле Token).
type Result struct {
	// CredentialPath — куда фактически записан токен (после fallback).
	CredentialPath string

	// AuditID — ID соответствующей записи в audit_log.
	AuditID string

	// CorrelationID — ULID, привязанный к bootstrap-цепочке (для
	// последующих связанных событий, если будут).
	CorrelationID string

	// Token — заполняется ТОЛЬКО при возврате error-а
	// [ErrTokenFileWriteFailed] (recovery-path: caller печатает токен в
	// stderr с предупреждением о ротации). В happy-path поле пустое —
	// токен живёт в файле, в Result не дублируется, чтобы не утечь в
	// логи случайно.
	Token string
}

// Init выполняет bootstrap первого Архонта.
//
// Последовательность:
//  1. Валидация Config (минимум: ArchonAID, TTLBootstrap, Pool,
//     SigningKeyRef, IssuerFactory, AuditWriter).
//  2. Read signing-key из Vault KV (mount/path из SigningKeyRef).
//  3. Создание JWT-issuer-а.
//  4. BEGIN tx → `pg_advisory_xact_lock(AdvisoryLockID)` →
//     `Count(operators)`; >0 → откат + [ErrAlreadyInitialized].
//  5. Insert operator (created_by_aid=NULL).
//  6. Issue JWT.
//  7. COMMIT.
//  8. Audit-event `operator.created` (после COMMIT — иначе можно
//     записать audit о «фантомном» insert, который откатился).
//  9. Save JWT в credentialOutput файл (mode 0400).
//
// Audit пишется ПОСЛЕ COMMIT-а: ошибка audit-writer-а не должна откатывать
// successful insert (Архонт уже в БД, операторская истина источника). При
// фейле audit Init возвращает error, но БД-state остаётся consistent.
func Init(ctx context.Context, cfg Config) (*Result, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	path, err := keepervault.ParseRef(cfg.SigningKeyRef)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: signing_key_ref: %w", err)
	}
	// path передаётся в logical-форме (`<mount>/<rel>`); Client сам
	// strip-ает префикс. ReadKV терпит и relative.
	kv, err := cfg.VaultClient.ReadKV(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: read vault %q: %w", path, err)
	}
	signingKey, err := extractSigningKey(kv)
	if err != nil {
		return nil, err
	}
	issuer, err := cfg.IssuerFactory(signingKey)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: build jwt issuer: %w", err)
	}

	displayName := cfg.DisplayName
	if displayName == "" {
		displayName = cfg.ArchonAID
	}

	// Транзакция держит advisory lock на всю длительность COMMIT-а.
	// `pg_advisory_xact_lock` освобождается автоматически при COMMIT
	// или ROLLBACK — defer Rollback корректен.
	tx, err := cfg.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: begin tx: %w", err)
	}
	// rollback-вызов после успешного Commit — no-op (pgx возвращает
	// ErrTxClosed), потому глотаем.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, AdvisoryLockID); err != nil {
		return nil, fmt.Errorf("bootstrap: acquire advisory lock: %w", err)
	}

	n, err := operator.Count(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: count operators: %w", err)
	}
	if n > 0 {
		return nil, ErrAlreadyInitialized
	}

	op := &operator.Operator{
		AID:         cfg.ArchonAID,
		DisplayName: displayName,
		AuthMethod:  operator.AuthMethodJWT,
		// CreatedByAID = nil (первый bootstrap-Archon, ADR-013/014).
		// CreatedAt zero → DEFAULT NOW() в БД.
	}
	if err := operator.Insert(ctx, tx, op); err != nil {
		return nil, fmt.Errorf("bootstrap: insert operator: %w", err)
	}

	// Фикс BUG-1 (ADR-028(c)): membership-строка (cluster-admin, <aid>) пишется
	// в rbac_role_operators в ТОЙ ЖЕ advisory-lock-транзакции, что и INSERT
	// operator-а. Роль cluster-admin уже существует из seed-миграции 027 (E1).
	// Без этой строки enforcer (резолвящий membership из БД) не нашёл бы ни одной
	// роли у первого Архонта — JWT-claim `roles` авторитетом membership-а НЕ
	// является. granted_by_aid = NULL — bootstrap-membership без инициатора.
	if err := rbac.GrantOperator(ctx, tx, BootstrapRoleClusterAdmin, cfg.ArchonAID, nil); err != nil {
		return nil, fmt.Errorf("bootstrap: grant cluster-admin membership: %w", err)
	}

	token, err := issuer.Issue(
		cfg.ArchonAID,
		[]string{BootstrapRoleClusterAdmin},
		cfg.TTLBootstrap,
		true, // bootstrapInitial
	)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: issue jwt: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap: commit tx: %w", err)
	}

	// Audit-event пишется после COMMIT. ArchonAID на event — пуст (NULL
	// в БД), per ADR-014(e): первый Архонт сам — субъект, а
	// `archon_aid` — это инициатор; bootstrap инициирован "никем"
	// (keeper_internal).
	correlationID := audit.NewULID()
	ev := &audit.Event{
		AuditID:       audit.NewULID(),
		EventType:     audit.EventOperatorCreated,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: correlationID,
		Payload: map[string]any{
			"bootstrap_initial": true,
			"aid":               cfg.ArchonAID,
			"display_name":      displayName,
			"auth_method":       string(operator.AuthMethodJWT),
		},
	}
	if err := cfg.AuditWriter.Write(ctx, ev); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAuditWriteFailed, err)
	}

	credPath := cfg.CredentialOutput
	if credPath == "" {
		// Defensive guard (belt-and-suspenders): AID встраивается в имя
		// файла bootstrap-<aid>.token. Новый charset (ADR-014 amendment)
		// уже исключает `/`/`\`, но перед попаданием в путь повторно
		// проверяем ValidAID + явный path-traversal-фильтр. Insert/audit
		// уже committed — возвращаем токен в Result для recovery (caller
		// выведет его в stderr).
		if !operator.ValidAID(cfg.ArchonAID) || !safePathComponent(cfg.ArchonAID) {
			return &Result{
				AuditID:       ev.AuditID,
				CorrelationID: correlationID,
				Token:         token,
			}, fmt.Errorf("%w: ArchonAID %q unsafe for credential path", ErrTokenFileWriteFailed, cfg.ArchonAID)
		}
		credPath = defaultCredentialPath(cfg.ArchonAID)
	}
	if err := ensureCredentialDir(credPath); err != nil {
		// Каталог не создан — операторская истина в БД, audit написан,
		// но файл не сохранён. Включаем recovery-path: возвращаем токен
		// в Result + ErrTokenFileWriteFailed.
		return &Result{
			CredentialPath: credPath,
			AuditID:        ev.AuditID,
			CorrelationID:  correlationID,
			Token:          token,
		}, fmt.Errorf("%w: %w", ErrTokenFileWriteFailed, err)
	}
	if err := writeTokenFile(credPath, token); err != nil {
		// См. ErrTokenFileWriteFailed: insert + audit уже committed,
		// файл потерян. Возвращаем токен в Result, чтобы caller вывел
		// его в stderr с warning-ом про ротацию.
		return &Result{
			CredentialPath: credPath,
			AuditID:        ev.AuditID,
			CorrelationID:  correlationID,
			Token:          token,
		}, fmt.Errorf("%w: %w", ErrTokenFileWriteFailed, err)
	}

	return &Result{
		CredentialPath: credPath,
		AuditID:        ev.AuditID,
		CorrelationID:  correlationID,
	}, nil
}

func validateConfig(cfg Config) error {
	if !operator.ValidAID(cfg.ArchonAID) {
		return fmt.Errorf("bootstrap: invalid ArchonAID %q (must match %s)", cfg.ArchonAID, operator.AIDPattern)
	}
	if cfg.TTLBootstrap <= 0 {
		return fmt.Errorf("bootstrap: TTLBootstrap must be positive, got %s", cfg.TTLBootstrap)
	}
	if cfg.Pool == nil {
		return errors.New("bootstrap: Pool is nil")
	}
	if cfg.VaultClient == nil {
		return errors.New("bootstrap: VaultClient is nil")
	}
	if cfg.IssuerFactory == nil {
		return errors.New("bootstrap: IssuerFactory is nil")
	}
	if cfg.AuditWriter == nil {
		return errors.New("bootstrap: AuditWriter is nil")
	}
	if cfg.SigningKeyRef == "" {
		return errors.New("bootstrap: SigningKeyRef is empty (auth.jwt.signing_key_ref)")
	}
	return nil
}

// extractSigningKey достаёт поле `signing_key` из Vault KV payload-а и
// декодирует base64. Поведение:
//
//   - значение типа `string`, валидный base64 → []byte;
//   - значение типа `string`, НЕ base64 → как raw-bytes (fallback, чтобы
//     dev-сценарий `vault kv put ... signing_key=raw-32-bytes` тоже работал);
//   - значение типа `[]byte` → как есть;
//   - отсутствует или пустое → [ErrSigningKeyMissing].
//
// Минимальная длина (>= 32 байт для HS256) валидируется уже jwt.NewIssuer.
func extractSigningKey(kv map[string]any) ([]byte, error) {
	raw, ok := kv[vaultSigningKeyField]
	if !ok {
		return nil, ErrSigningKeyMissing
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil, ErrSigningKeyMissing
		}
		if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
			return decoded, nil
		}
		return []byte(v), nil
	case []byte:
		if len(v) == 0 {
			return nil, ErrSigningKeyMissing
		}
		return v, nil
	default:
		return nil, fmt.Errorf("bootstrap: signing_key has unsupported type %T (want string or []byte)", raw)
	}
}

// writeTokenFile создаёт/перезаписывает path-файл с правами 0400 и пишет
// в него token + один `\n` (для совместимости с инструментами вроде
// `cat | jwt decode`, которые ожидают line-terminated input).
//
// Если файл уже существует, он сначала удаляется — открыть его O_WRONLY
// после прошлого write нельзя (mode 0400 = read-only owner). os.Remove
// игнорирует ErrNotExist; на permission-denied (например, /tmp/-file
// принадлежит другому user-у) — возврат с понятным сообщением.
func writeTokenFile(path, token string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, credentialFileMode)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // ошибка writeTokenFile приходит через Sync/Write ниже

	// `O_CREATE` применяет mode только при создании; на старом umask
	// итоговые права могут оказаться 0400 & ~umask. Явный Chmod держит
	// инвариант 0400 независимо от umask процесса.
	if err := f.Chmod(credentialFileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	return nil
}

// safePathComponent — последний барьер перед встраиванием AID в имя файла:
// запрещает path-separator-ы и `..`. Дублирует гарантии ValidAID (новый
// charset уже без `/`/`\`/`.`-в-начале), но не зависит от него на случай
// будущего расширения формата AID. Возвращает false для пустой строки.
func safePathComponent(s string) bool {
	if s == "" || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '/' || s[i] == '\\' {
			return false
		}
	}
	return true
}

// defaultCredentialPath возвращает путь по умолчанию для JWT-файла.
//
// Приоритет (review M0.5c: уход от predictable world-readable `/tmp`):
//  1. `os.UserCacheDir()` → `<cache>/keeper/bootstrap-<aid>.token`
//     (Linux = `~/.cache/keeper/...`, macOS = `~/Library/Caches/keeper/...`).
//  2. Fallback `/var/lib/keeper/bootstrap-<aid>.token` — для systemd-сервиса
//     без `HOME` (User cache недоступен).
//
// Родительский каталог создаётся [ensureCredentialDir] с `mode 0700`,
// если ещё не существует. AID входит в имя файла, чтобы при повторном
// init с другим AID старый файл не перезаписывался незаметно.
func defaultCredentialPath(aid string) string {
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "keeper", "bootstrap-"+aid+".token")
	}
	return filepath.Join("/var/lib/keeper", "bootstrap-"+aid+".token")
}

// ensureCredentialDir создаёт родительский каталог `path` с `mode 0700`,
// если его ещё нет. Существующий каталог не chmod-ит (могут быть mount /
// custom-перм у оператора). Не-каталог по пути — error.
func ensureCredentialDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("credential dir %q exists but is not a directory", dir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("stat %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}
	return nil
}
