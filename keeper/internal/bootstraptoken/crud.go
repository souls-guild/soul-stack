package bootstraptoken

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel-ошибки CRUD-слоя. Handler-сторона маппит:
//
//   - ErrTokenActiveExists  → 409 conflict (на SID уже висит активный токен,
//     по partial unique `bootstrap_tokens_active_by_sid_idx`). Оператор должен
//     отозвать старый или дождаться TTL.
//   - ErrTokenInvalid       → 403 forbidden — токен не существует, истёк
//     или уже сожжён. Возвращается из [Burn]; намеренно не различает
//     причину (защита от user-enum-атак — все три случая отдаются с одной
//     ошибкой).
//   - ErrTokenSoulNotFound  → 404 на Insert при отсутствии SID в souls.
var (
	ErrTokenActiveExists = errors.New("bootstraptoken: active token for SID already exists")
	ErrTokenInvalid      = errors.New("bootstraptoken: token invalid (not found, expired, or already used)")
	ErrTokenSoulNotFound = errors.New("bootstraptoken: target SID not found in souls registry")
	ErrTokenNotFound     = errors.New("bootstraptoken: token_id not found")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество интерфейса pgxpool.Pool, нужное
// CRUD-у. Симметрично [operator.ExecQueryRower] / [soul.ExecQueryRower].
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
)

const insertSQL = `
INSERT INTO bootstrap_tokens (sid, token_hash, expires_at, created_by_aid)
VALUES ($1, $2, $3, $4)
RETURNING token_id, created_at
`

const selectByHashSQL = `
SELECT token_id, sid, token_hash, created_at, expires_at,
       used_at, used_by_kid, created_by_aid
FROM bootstrap_tokens
WHERE token_hash = $1
`

// burnSQL — race-safe UPDATE «сжигания» токена. WHERE-conjunction
// гарантирует, что одновременное двойное предъявление даст один UPDATE
// и один промах. RETURNING token_id — для аудита и проверки rows-affected
// одной round-trip-ой.
//
// Параметры:
//
//	$1 — token_hash от предъявленного plain-токена.
//	$2 — sid из BootstrapRequest (защита от подмены SID при том же хеше).
//	$3 — kid Keeper-инстанса, обрабатывающего запрос.
const burnSQL = `
UPDATE bootstrap_tokens
SET used_at     = NOW(),
    used_by_kid = $3
WHERE token_hash = $1
  AND sid        = $2
  AND used_at    IS NULL
  AND expires_at > NOW()
RETURNING token_id
`

// Insert вписывает новый bootstrap-token (выписка оператором). Возвращает
// созданный [Record] с заполненными TokenID и CreatedAt.
//
// Pre-conditions:
//   - sid — валидный SID в реестре souls (FK проверяется PG);
//   - tokenHash — SHA-256 hex (64 lower-hex);
//   - ttl > 0 (expires_at = NOW() + ttl).
//
// Возврат:
//   - [ErrTokenActiveExists] на UNIQUE по partial-index `_active_by_sid`.
//   - [ErrTokenSoulNotFound] на FK-violation по `bootstrap_tokens_sid_fk`.
//   - wrapped fmt.Errorf на прочие pg-ошибки.
func Insert(ctx context.Context, db ExecQueryRower, sid, tokenHash string, ttl time.Duration, createdByAID *string) (*Record, error) {
	if sid == "" {
		return nil, fmt.Errorf("bootstraptoken: sid is empty")
	}
	if !ValidHashFormat(tokenHash) {
		return nil, errInvalidHash
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("bootstraptoken: ttl must be positive, got %s", ttl)
	}

	expiresAt := time.Now().UTC().Add(ttl)
	var createdByAIDArg any
	if createdByAID != nil {
		createdByAIDArg = *createdByAID
	}

	rec := &Record{
		SID:          sid,
		TokenHash:    tokenHash,
		ExpiresAt:    expiresAt,
		CreatedByAID: createdByAID,
	}
	row := db.QueryRow(ctx, insertSQL, sid, tokenHash, expiresAt, createdByAIDArg)
	if err := row.Scan(&rec.TokenID, &rec.CreatedAt); err != nil {
		return nil, mapInsertError(err)
	}
	return rec, nil
}

func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			// partial unique index `_active_by_sid` или `_token_hash`.
			// Differentiate by constraint name для UX-handler-а: hash-collision
			// (де-факто невозможно) vs «уже выписан активный для этого SID».
			if pgErr.ConstraintName == "bootstrap_tokens_token_hash_idx" {
				return fmt.Errorf("bootstraptoken: token_hash collision (constraint %s): %w",
					pgErr.ConstraintName, err)
			}
			return fmt.Errorf("%w (constraint %s): %w",
				ErrTokenActiveExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			if pgErr.ConstraintName == "bootstrap_tokens_sid_fk" {
				return fmt.Errorf("%w (constraint %s): %w",
					ErrTokenSoulNotFound, pgErr.ConstraintName, err)
			}
			return fmt.Errorf("bootstraptoken: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("bootstraptoken: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("bootstraptoken: insert: %w", err)
}

// SelectByHash читает Record по token_hash. [ErrTokenNotFound] при
// pgx.ErrNoRows.
//
// Используется gRPC Bootstrap-handler-ом до Burn-а — чтобы достать SID
// и payload для audit-event-а, **если** caller предъявил валидный SID.
// Сам Burn по-прежнему атомарный (через WHERE-clause).
func SelectByHash(ctx context.Context, db ExecQueryRower, tokenHash string) (*Record, error) {
	if !ValidHashFormat(tokenHash) {
		return nil, errInvalidHash
	}
	row := db.QueryRow(ctx, selectByHashSQL, tokenHash)
	return scanRecord(row)
}

func scanRecord(row pgx.Row) (*Record, error) {
	var (
		rec          Record
		usedAt       *time.Time
		usedByKID    *string
		createdByAID *string
	)
	err := row.Scan(
		&rec.TokenID,
		&rec.SID,
		&rec.TokenHash,
		&rec.CreatedAt,
		&rec.ExpiresAt,
		&usedAt,
		&usedByKID,
		&createdByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTokenNotFound
		}
		return nil, fmt.Errorf("bootstraptoken: scan: %w", err)
	}
	rec.UsedAt = usedAt
	rec.UsedByKID = usedByKID
	rec.CreatedByAID = createdByAID
	return &rec, nil
}

// Burn — race-safe сжигание токена в момент предъявления Soul-ом в
// `Bootstrap`-RPC. ОБЯЗАТЕЛЬНО выполнять внутри транзакции вместе с
// `soul.UpdateStatus` (pending → connected) и `soulseed.Insert` — caller
// (gRPC handler) держит этот инвариант.
//
// Параметры:
//   - tokenHash — SHA-256 hex от plain-токена, предъявленного клиентом.
//   - claimedSID — SID из BootstrapRequest.sid (защита от подмены SID
//     при том же хеше — атакующий не сможет «угнать» токен под чужой SID).
//   - usedByKID — KID Keeper-инстанса, обрабатывающего запрос.
//
// Возврат:
//   - tokenID — UUID сожжённой записи (для audit-payload-а).
//   - [ErrTokenInvalid] — токен не существует, истёк, уже использован, или
//     SID не совпадает. Намеренно не различает причину (anti-enum).
//   - wrapped fmt.Errorf на прочие pg-ошибки.
func Burn(ctx context.Context, db ExecQueryRower, tokenHash, claimedSID, usedByKID string) (string, error) {
	if !ValidHashFormat(tokenHash) {
		return "", errInvalidHash
	}
	if claimedSID == "" {
		return "", fmt.Errorf("bootstraptoken: claimedSID is empty")
	}
	if usedByKID == "" {
		return "", fmt.Errorf("bootstraptoken: usedByKID is empty")
	}

	var tokenID string
	err := db.QueryRow(ctx, burnSQL, tokenHash, claimedSID, usedByKID).Scan(&tokenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrTokenInvalid
		}
		return "", fmt.Errorf("bootstraptoken: burn: %w", err)
	}
	return tokenID, nil
}

// SystemKIDCloudDestroy — спец-значение `used_by_kid` для записей,
// «сожжённых» cascade-обработчиком `core.cloud.provisioned destroyed`
// (ADR-017): хост удалён вместе с VM, реального предъявления токена
// не было, оператор-Архонт здесь тоже не при чём, поэтому пишем системный
// маркер. Формат отличается от валидного `kid` (kebab-case `keeper-XXX`):
// `system-cloud-destroy` явно не может коллидировать с реальным KID-ом,
// audit-handler фильтрует по этому префиксу.
const SystemKIDCloudDestroy = "system-cloud-destroy"

// burnAllForSIDSQL — cascade-сжигание всех ещё-не-использованных токенов
// данного SID. В отличие от [Burn], НЕ проверяет expires_at > NOW():
// в момент cloud-destroy любой ещё активный токен теряет смысл (хост
// не существует), даже если он истёк секунду назад — Reaper всё равно
// удалит запись по purge_used_tokens. Здесь нам важно зафиксировать
// факт «погашен в момент cloud-destroy», чтобы анти-replay-инвариант
// держался без race с TTL-границей.
const burnAllForSIDSQL = `
UPDATE bootstrap_tokens
SET used_at     = NOW(),
    used_by_kid = $2
WHERE sid     = $1
  AND used_at IS NULL
`

// BurnAllForSID — cascade-сжигание всех ещё-не-использованных bootstrap-токенов
// данного SID. Используется keeper-side core-модулем `core.cloud.provisioned
// destroyed` (ADR-017 cascade) внутри общей PG-транзакции вместе с
// `soul.UpdateStatus(destroyed)` и `soulseed`-cascade-update-ом.
//
// `usedByKID` — KID или спец-маркер (см. [SystemKIDCloudDestroy]).
//
// Возвращает количество затронутых строк (0 = у SID не было активных токенов,
// нормальный случай для долго работавшего Soul-а).
func BurnAllForSID(ctx context.Context, db ExecQueryRower, sid, usedByKID string) (int64, error) {
	if sid == "" {
		return 0, fmt.Errorf("bootstraptoken: sid is empty")
	}
	if usedByKID == "" {
		return 0, fmt.Errorf("bootstraptoken: usedByKID is empty")
	}
	tag, err := db.Exec(ctx, burnAllForSIDSQL, sid, usedByKID)
	if err != nil {
		return 0, fmt.Errorf("bootstraptoken: burn all for sid: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SystemKIDForceReissue — спец-значение `used_by_kid` для токена,
// инвалидированного оператором при `issue-token?force=true` (новый токен
// выписывается взамен ещё-активного). Реального предъявления токена Soul-ом
// не было — фиксируем системный маркер, чтобы audit отличал force-reissue
// от настоящего Burn-а в Bootstrap-RPC. Формат отличается от валидного KID
// (`keeper-XXX`) — коллизии исключены.
const SystemKIDForceReissue = "system-force-reissue"

// expireActiveBySIDSQL — инвалидация ещё-активного токена SID при
// force-reissue. Ставит `used_at = NOW()`, что одновременно (1) делает
// токен непригодным для Burn-а (WHERE `used_at IS NULL` больше не пройдёт)
// и (2) освобождает partial-unique-slot `bootstrap_tokens_active_by_sid_idx`
// (`WHERE used_at IS NULL`) для последующего Insert нового токена.
//
// Только set `expires_at = NOW()` НЕ годится: запись осталась бы с
// `used_at IS NULL` и продолжала бы держать unique-slot.
const expireActiveBySIDSQL = `
UPDATE bootstrap_tokens
SET used_at     = NOW(),
    used_by_kid = $2
WHERE sid     = $1
  AND used_at IS NULL
RETURNING token_id
`

// ExpireActiveBySID инвалидирует активный (ещё-неиспользованный) токен SID
// и возвращает его token_id. Используется Operator API при
// `issue-token?force=true` внутри той же транзакции, что и Insert нового
// токена (caller держит инвариант атомарности).
//
// `usedByKID` — спец-маркер (см. [SystemKIDForceReissue]).
//
// Возвращает (tokenID, true, nil), если активный токен был и инвалидирован;
// ("", false, nil), если активного токена не было (force на чистом SID —
// нормально, просто Insert); error на pg-сбое.
func ExpireActiveBySID(ctx context.Context, db ExecQueryRower, sid, usedByKID string) (string, bool, error) {
	if sid == "" {
		return "", false, fmt.Errorf("bootstraptoken: sid is empty")
	}
	if usedByKID == "" {
		return "", false, fmt.Errorf("bootstraptoken: usedByKID is empty")
	}
	var tokenID string
	err := db.QueryRow(ctx, expireActiveBySIDSQL, sid, usedByKID).Scan(&tokenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("bootstraptoken: expire active for sid: %w", err)
	}
	return tokenID, true, nil
}

// DeleteByTokenID удаляет запись по PK. Используется Reaper-ом по правилу
// `purge_used_tokens` (см. ADR-022 / docs/keeper/reaper.md).
//
// Возвращает [ErrTokenNotFound], если записи с таким token_id нет.
func DeleteByTokenID(ctx context.Context, db ExecQueryRower, tokenID string) error {
	tag, err := db.Exec(ctx, `DELETE FROM bootstrap_tokens WHERE token_id = $1`, tokenID)
	if err != nil {
		return fmt.Errorf("bootstraptoken: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTokenNotFound
	}
	return nil
}
