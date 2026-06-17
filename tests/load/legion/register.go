package legion

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// minSidPrefixLen — нижняя граница длины --sid-prefix. Короткий/пустой prefix в
// LIKE-выражении Cleanup снёс бы чужие (реальные) souls. 3 символа — минимальный
// разумный дискриминатор тестового легиона от прод-флота.
const minSidPrefixLen = 3

// validateSidPrefix — единый guard валидности legion-префикса (общий для INSERT-
// и Cleanup-фаз). Защищает реальный реестр souls: с пустым/коротким prefix-ом
// Cleanup `DELETE ... LIKE 'prefix%'` удалил бы весь флот. Префикс обязан быть не
// короче minSidPrefixLen и не содержать пробельных символов (опечатка в флаге).
func validateSidPrefix(sidPrefix string) error {
	if len(sidPrefix) < minSidPrefixLen {
		return fmt.Errorf("legion: отказ: префикс %q снёс бы чужие souls (минимум %d символа)", sidPrefix, minSidPrefixLen)
	}
	if strings.ContainsAny(sidPrefix, " \t\r\n") {
		return fmt.Errorf("legion: отказ: префикс %q содержит пробельные символы", sidPrefix)
	}
	return nil
}

// escapeLikePrefix экранирует LIKE-метасимволы (% _ и сам escape-символ \) в
// prefix-е, чтобы он матчил буквально под `LIKE ... ESCAPE '\'`. Без этого
// prefix вида "legion_" трактовался бы как «legion + любой символ».
func escapeLikePrefix(sidPrefix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(sidPrefix)
}

// Registrar — setup-фаза: предрегистрация N fake-Soul-идентичностей в БД
// Keeper-кластера (souls + soul_seeds). Keeper авторизует EventStream-стрим по
// fingerprint-у в soul_seeds (status='active'); без предрегистрации стрим
// отвергается «unknown soul seed» (keeper/internal/grpc/auth.go). Это самый
// дешёвый валидный путь онбординга N душ: прямой SQL вместо настоящего
// Bootstrap-CSR (как tests/e2e/harness/cert.go::RegisterSoulPreAuth).
type Registrar struct {
	pool *pgxpool.Pool
}

// NewRegistrar открывает pgx-pool на DSN Keeper-кластера. Caller обязан вызвать
// Close.
func NewRegistrar(ctx context.Context, dsn string) (*Registrar, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("legion: parse dsn: %w", err)
	}
	// Пул под batch-INSERT setup-фазы: не на горячем пути стрима, скромный.
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("legion: pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("legion: pg ping: %w", err)
	}
	return &Registrar{pool: pool}, nil
}

// Close освобождает pool.
func (r *Registrar) Close() {
	if r.pool != nil {
		r.pool.Close()
	}
}

// Register вставляет souls + soul_seeds для всех ids одной транзакцией через
// pgx.Batch (минимум round-trip-ов на setup N душ). Идемпотентно: ON CONFLICT
// переиспользует строку (повторный прогон с тем же legion-prefix-ом). Колонки
// зафиксированы по живой схеме (миграции 007 souls / 009+ soul_seeds): status
// 'connected', transport 'agent', seed 'active' с TTL 365 дней.
//
// status='connected' (не 'pending') сознательно: на подъёме EventStream Keeper
// НЕ пишет souls.status в PG (presence деривируется из Redis SID-lease, ADR-006(a)
// amend) — поле трогает только онбординг-CSR (bootstrap.go). Предрегистрация под
// 'pending' осталась бы 'pending' навсегда и вводила бы в заблуждение (флот «не
// подключён» в PG, хотя стримы живы). Авторитет presence — Redis-lease и метрика
// keeper_grpc_streams_active, не это поле.
//
// sidPrefix валидируется validateSidPrefix-ом до записи: тем же guard-ом, что
// защищает Cleanup, чтобы не плодить легион под опасным prefix-ом.
//
// covens — стабильные coven-метки легиона (souls.coven, text[], ADR-008). Пишутся
// на каждый SID, чтобы Voyage мог таргетить флот по coven (`coven @> $::text[]` в
// VoyageCommandPGResolver). Пустой срез → coven остаётся ARRAY[]::TEXT[] (default
// схемы), легион не таргетируем по coven. Все метки уже провалидированы caller-ом.
func (r *Registrar) Register(ctx context.Context, sidPrefix string, covens []string, ids []Identity) error {
	if err := validateSidPrefix(sidPrefix); err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("legion: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// nil-срез пишем как пустой text[] (default схемы); pgx сериализует []string в
	// text[] напрямую (тот же тип, что читает VoyageCommandPGResolver).
	covenArg := covens
	if covenArg == nil {
		covenArg = []string{}
	}

	batch := &pgx.Batch{}
	for _, id := range ids {
		// souls ПЕРВЫМ: soul_seeds.sid — FK на souls(sid). ON CONFLICT обновляет и
		// coven: повторный прогон с другим --coven перепишет метки легиона.
		batch.Queue(`
			INSERT INTO souls (sid, status, transport, coven, registered_at, last_seen_at)
			VALUES ($1, 'connected', 'agent', $2::text[], NOW(), NOW())
			ON CONFLICT (sid) DO UPDATE SET status = 'connected', coven = $2::text[], last_seen_at = NOW()
		`, id.SID, covenArg)
	}
	for _, id := range ids {
		batch.Queue(`
			INSERT INTO soul_seeds (sid, fingerprint, serial_number, status, issued_at, expires_at)
			VALUES ($1, $2, $3, 'active', NOW(), NOW() + INTERVAL '365 days')
			ON CONFLICT (fingerprint) DO NOTHING
		`, id.SID, id.Fingerprint, id.Serial)
	}

	br := tx.SendBatch(ctx, batch)
	for range batch.Len() {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("legion: batch exec: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("legion: batch close: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("legion: commit: %w", err)
	}
	return nil
}

// Cleanup удаляет легион из реестра по SID-prefix-у (souls cascade-удалит
// soul_seeds через FK ON DELETE CASCADE). Возвращает число удалённых souls-строк.
// Изолирует тестовые SID от реального флота: чистит только legion-* по prefix-у.
//
// БЕЗОПАСНОСТЬ: prefix валидируется validateSidPrefix-ом (пустой/короткий →
// отказ, DELETE не выполняется); LIKE-метасимволы (% _ \) экранируются и
// сопоставляются буквально через ESCAPE '\'. Без этого пустой prefix снёс бы
// ВЕСЬ реестр souls (включая реальный флот), а prefix с '_'/'%' захватил бы
// чужие SID.
func (r *Registrar) Cleanup(ctx context.Context, sidPrefix string) (int64, error) {
	if err := validateSidPrefix(sidPrefix); err != nil {
		return 0, err
	}
	pattern := escapeLikePrefix(sidPrefix) + "%"
	tag, err := r.pool.Exec(ctx, `DELETE FROM souls WHERE sid LIKE $1 ESCAPE '\'`, pattern)
	if err != nil {
		return 0, fmt.Errorf("legion: cleanup: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteVoyage сносит один Voyage из PG по точному voyage_id (ULID). voyage_targets
// каскадятся (FK ON DELETE CASCADE, миграция 059); errands — soft-link без FK,
// короткоживущие (ttl_at), их подберёт purge_old_errands. Точный PK-match (не
// prefix) — снос затрагивает только наш нагрузочный Voyage, чужие прогоны не
// задеты. API-удаления терминального Voyage нет (DELETE /v1/voyages/{id} — только
// cancel pending/scheduled), поэтому cleanup идёт прямым SQL, как и регистрация.
func (r *Registrar) DeleteVoyage(ctx context.Context, voyageID string) error {
	if voyageID == "" {
		return fmt.Errorf("legion: пустой voyage_id для DeleteVoyage")
	}
	if _, err := r.pool.Exec(ctx, `DELETE FROM voyages WHERE voyage_id = $1`, voyageID); err != nil {
		return fmt.Errorf("legion: delete voyage %s: %w", voyageID, err)
	}
	return nil
}
