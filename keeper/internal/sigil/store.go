package sigil

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgErrCodeUniqueViolation — SQLSTATE UNIQUE-нарушения: PK или partial unique
// index plugin_sigils_active_idx (active-запись на (namespace, name, ref) уже
// есть). Держим константу локально, как operator/applyrun CRUD.
const pgErrCodeUniqueViolation = "23505"

// pgErrCodeForeignKeyViolation — SQLSTATE FK-нарушения. Для plugin_sigils
// возникает на allowed_by_aid / revoked_by_aid (ссылка на несуществующий AID).
const pgErrCodeForeignKeyViolation = "23503"

// ErrSigilAlreadyActive — Insert при уже существующей активной записи на
// (namespace, name, ref) (partial unique index). Re-allow требует сперва
// Revoke текущей активной записи.
var ErrSigilAlreadyActive = errors.New("sigil: an active record already exists for (namespace, name, ref)")

// ErrSigilNotFound — GetActive / Revoke не нашли активную запись по ключу.
var ErrSigilNotFound = errors.New("sigil: no active record found")

// Sigil — строка реестра plugin_sigils (миграции 028, 030, 038).
//
// ManifestRaw — byte-exact СЫРЫЕ байты manifest.yaml, над которыми поставлена
// подпись (миграция 030). Это КАНОН для verify/broadcast: едет в
// PluginSigil.manifest как есть, S6 re-хеширует именно их через
// NormalizeManifestBytes (S3↔S6-инвариант).
//
// Manifest хранится JSONB-ом ДЛЯ query/audit (искать по side_effects /
// capabilities, показывать в UI). Это ПРОИЗВОДНАЯ проекция, НЕ канон для verify:
// JSONB-роундтрип не сохраняет байты.
// Signature — сырые байты ed25519-подписи (BYTEA, без base64).
//
// CommitSHA — git-commit, из которого резолвлен допущенный бинарь (миграция 038,
// ADR-026(g)). Audit-метка ПРОИСХОЖДЕНИЯ, ВНЕ подписываемого блока: authority
// целостности — SHA256 + подпись Keeper-а, не CommitSHA. Keeper-audit-only: НЕ
// участвует в verify (нет в shared/pluginhost.SigilRecord) и НЕ едет в
// PluginSigil-транспорт broadcast-а. Заполняется из ResolvedSlot.CommitSHA при
// git-verified-allow (слайс S4); пустая строка = legacy operator-asserted
// (Вариант C) или строка до миграции 038 (NULL читается как "").
type Sigil struct {
	ID           int64
	Namespace    string
	Name         string
	Ref          string
	SHA256       string // hex, lowercase, 64 символа
	Signature    []byte // сырые 64 байта ed25519
	ManifestRaw  []byte // byte-exact сырые байты manifest.yaml (КАНОН для verify)
	Manifest     []byte // JSONB-байты (производная проекция для query/audit, не канон)
	CommitSHA    string // git-commit происхождения (audit, вне подписи); "" = legacy/NULL
	AllowedByAID string
	AllowedAt    time.Time
	RevokedAt    *time.Time
	RevokedByAID *string
}

// ExecQueryRower — узкое подмножество pgxpool.Pool, нужное CRUD-у. Сужение
// позволяет unit-тестировать через fake-pool; реальный pool удовлетворяет
// автоматически. Симметрично operator/applyrun CRUD.
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

// insertSigilSQL — commit_sha пишется как NULLIF($9, ”): пустая CommitSHA
// (legacy operator-asserted / заполнение из резолвера придёт в S4) ложится в БД
// NULL-ом, сохраняя семантику «происхождение неизвестно», а не пустой строкой.
const insertSigilSQL = `
INSERT INTO plugin_sigils (namespace, name, ref, sha256, signature, manifest, allowed_by_aid, manifest_raw, commit_sha)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''))
RETURNING id, allowed_at
`

// selectActiveByKeySQL / listActiveSQL — commit_sha читается через
// COALESCE(..., ”) (NULL → "" для legacy/до-038-строк), чтобы скан в string-поле
// не требовал указателя.
const selectActiveByKeySQL = `
SELECT id, namespace, name, ref, sha256, signature, manifest, manifest_raw,
       COALESCE(commit_sha, ''), allowed_by_aid, allowed_at, revoked_at, revoked_by_aid
FROM plugin_sigils
WHERE namespace = $1 AND name = $2 AND ref = $3 AND revoked_at IS NULL
`

const listActiveSQL = `
SELECT id, namespace, name, ref, sha256, signature, manifest, manifest_raw,
       COALESCE(commit_sha, ''), allowed_by_aid, allowed_at, revoked_at, revoked_by_aid
FROM plugin_sigils
WHERE revoked_at IS NULL
ORDER BY allowed_at DESC, id DESC
`

// revokeActiveByKeySQL — мягкая ревокация активной записи по ключу. WHERE
// revoked_at IS NULL — атомарная защита от повторного revoke (rows-affected = 0
// → активной записи нет).
const revokeActiveByKeySQL = `
UPDATE plugin_sigils
SET revoked_at = NOW(), revoked_by_aid = $4
WHERE namespace = $1 AND name = $2 AND ref = $3 AND revoked_at IS NULL
`

// Insert вставляет новую запись допуска (allow). ManifestRaw — byte-exact
// сырые байты manifest.yaml (КАНОН для verify), manifest — JSONB-проекция,
// signature — сырые байты ed25519-подписи. CommitSHA — audit-метка
// происхождения (вне подписи); пустая допустима (NULL = legacy/неизвестно,
// заполнение из резолвера — S4). id и allowed_at заполняются из БД (RETURNING).
//
// Re-allow после Revoke — это чистый Insert новой записи: partial unique
// index plugin_sigils_active_idx считает только active-строки, отозванные не
// мешают. При уже существующей активной записи → [ErrSigilAlreadyActive].
func Insert(ctx context.Context, db ExecQueryRower, s *Sigil) error {
	if s == nil {
		return fmt.Errorf("sigil: nil sigil")
	}
	if !reSHA256Hex.MatchString(s.SHA256) {
		return fmt.Errorf("sigil: sha256 %q must be 64 lower-hex chars", s.SHA256)
	}
	if len(s.Signature) == 0 {
		return fmt.Errorf("sigil: signature is empty")
	}
	if s.AllowedByAID == "" {
		return fmt.Errorf("sigil: allowed_by_aid is empty")
	}
	// Пустой ManifestRaw — баг вызова (корень доверия): подпись ставится над
	// ИМЕННО этими байтами, fallback тут невозможен (Normalize("{}") !=
	// Normalize(""), в отличие от JSONB-колонки manifest, которая допускает
	// "{}"-заглушку).
	if len(s.ManifestRaw) == 0 {
		return fmt.Errorf("sigil: manifest_raw is empty (signed bytes must be persisted byte-exact)")
	}
	manifest := s.Manifest
	if len(manifest) == 0 {
		manifest = []byte("{}")
	}

	err := db.QueryRow(ctx, insertSigilSQL,
		s.Namespace, s.Name, s.Ref, s.SHA256, s.Signature, manifest, s.AllowedByAID, s.ManifestRaw, s.CommitSHA,
	).Scan(&s.ID, &s.AllowedAt)
	if err != nil {
		return mapInsertError(err)
	}
	return nil
}

// mapInsertError маппит pgx-ошибки Insert в sentinel-ы пакета.
func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrSigilAlreadyActive, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("sigil: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("sigil: insert: %w", err)
}

// GetActive читает активную (не отозванную) запись по ключу (namespace, name,
// ref). Lookup-путь будущего S6-verify. Возвращает [ErrSigilNotFound], если
// активной записи нет.
func GetActive(ctx context.Context, db ExecQueryRower, namespace, name, ref string) (*Sigil, error) {
	row := db.QueryRow(ctx, selectActiveByKeySQL, namespace, name, ref)
	s, err := scanSigil(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSigilNotFound
		}
		return nil, err
	}
	return s, nil
}

// ListActive возвращает все активные записи, новые первыми. Лента allow-list-а
// для UI / audit-триажа.
func ListActive(ctx context.Context, db ExecQueryRower) ([]*Sigil, error) {
	rows, err := db.Query(ctx, listActiveSQL)
	if err != nil {
		return nil, fmt.Errorf("sigil: list active: %w", err)
	}
	defer rows.Close()

	var out []*Sigil
	for rows.Next() {
		s, err := scanSigil(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sigil: list active rows: %w", err)
	}
	return out, nil
}

// Revoke мягко отзывает активный допуск по ключу: ставит revoked_at = NOW() и
// revoked_by_aid. Запись остаётся в реестре для аудита.
//
// Семантика:
//   - активной записи по ключу нет → [ErrSigilNotFound];
//   - revokedByAID пуст → ошибка (audit-инвариант: кто отозвал, обязателен).
func Revoke(ctx context.Context, db ExecQueryRower, namespace, name, ref, revokedByAID string) error {
	if revokedByAID == "" {
		return fmt.Errorf("sigil: revoked_by_aid is empty")
	}
	tag, err := db.Exec(ctx, revokeActiveByKeySQL, namespace, name, ref, revokedByAID)
	if err != nil {
		return fmt.Errorf("sigil: revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSigilNotFound
	}
	return nil
}

// scanSigil — общий Scan одной строки plugin_sigils. Вынесен, чтобы GetActive
// и ListActive читали колонки одинаково.
func scanSigil(row pgx.Row) (*Sigil, error) {
	var s Sigil
	err := row.Scan(
		&s.ID,
		&s.Namespace,
		&s.Name,
		&s.Ref,
		&s.SHA256,
		&s.Signature,
		&s.Manifest,
		&s.ManifestRaw,
		&s.CommitSHA,
		&s.AllowedByAID,
		&s.AllowedAt,
		&s.RevokedAt,
		&s.RevokedByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("sigil: scan: %w", err)
	}
	return &s, nil
}
