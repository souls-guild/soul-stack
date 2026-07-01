// Package reaper — Go-обёртки для Reaper-правил Keeper-а
// (см. docs/keeper/reaper.md, ADR-022(d)). Reaper-loop (cron-driver,
// leader-election через Redis-lease — ADR-006) появится в M0.6;
// в M0.4.1c фиксируется только per-rule SQL-вызов с unit-coverage,
// чтобы loop-driver мог использовать готовый блок.
//
// Пакет — pgx-aware (тянет `pgx/v5`-типы для row-scan), живёт в
// `keeper/internal/`, не в `shared/` — по той же причине, что
// `keeper/internal/auditpg` (изоляция Soul-бинаря от pgx, ADR-011).
package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// queryRower — узкое подмножество интерфейса pgxpool.Pool, нужное для
// SELECT purge_audit_old(...). Сужение позволяет unit-тестировать
// Purger fake-реализацией без поднятия Postgres-а; реальный pool из
// keeper/internal/pg удовлетворяет интерфейсу автоматически.
//
// Query (а не только QueryRow) нужен lease-aware `mark_disconnected`:
// select_disconnect_candidates возвращает SETOF text (много строк).
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// soulLeaseChecker — узкая поверхность Redis-проверки «жив ли EventStream к
// SID», нужная lease-aware `mark_disconnected`. Сужение до одного метода
// изолирует reaper-пакет от полного keeperredis.Client и допускает fake в
// unit-тестах. Реальная реализация — [keeperredis.SoulStreamAlive]-обёртка,
// собранная в cmd/keeper (см. daemon.setupReaper).
type soulLeaseChecker interface {
	SoulStreamAlive(ctx context.Context, sid string) (bool, error)
}

// Purger — Go-обёртка для SQL-функции `purge_audit_old`. Один экземпляр
// на Keeper-процесс; safe for concurrent use — pool сам обеспечивает
// потокобезопасность. Каждый вызов [PurgeAuditOld] — ровно один batch
// (по умолчанию 1000 записей); loop-логика (drain до 0, retry, cron,
// leader-election) — out of scope M0.4.1c, появится в M0.6
// reaper-runner.
type Purger struct {
	pool queryRower

	// lease — опциональная Redis-проверка живого EventStream к SID для
	// lease-aware `mark_disconnected` (ADR-006(a)). nil → правило деградирует
	// в прежний чисто-SQL путь (миграция 014, mark_disconnected): single-
	// instance dev / unit-режим без координации. Production wire-up
	// (cmd/keeper) передаёт обёртку над общим Redis-клиентом.
	lease soulLeaseChecker

	// logger — для warn-а при недоступности Redis в lease-aware ветке
	// (см. MarkDisconnected). nil-safe: при nil лог подавляется.
	logger *slog.Logger
}

// NewPurger оборачивает уже инициализированный pgxpool.Pool. Owner-ship
// пула остаётся у caller-а: Purger не закрывает пул, lifecycle —
// keeper/internal/pg → keeper/cmd/keeper.
//
// Без Redis-проверки: `mark_disconnected` работает в чисто-SQL режиме
// (миграция 014). Для lease-aware режима — [NewPurgerWithLease].
func NewPurger(pool *pgxpool.Pool) *Purger {
	return &Purger{pool: pool}
}

// NewPurgerWithLease — конструктор lease-aware Purger-а: `mark_disconnected`
// сверяется с Redis (живой SID-lease ⇒ Soul НЕ метится disconnected даже при
// stale PG `last_seen_at`). Прочие правила работают как в [NewPurger].
//
// `lease` может быть nil — тогда поведение идентично [NewPurger] (чисто-SQL
// fallback). `logger` опционален (nil → warn-ы подавляются).
func NewPurgerWithLease(pool *pgxpool.Pool, lease soulLeaseChecker, logger *slog.Logger) *Purger {
	return &Purger{pool: pool, lease: lease, logger: logger}
}

// newPurgerFromQueryRower — внутренний конструктор для unit-тестов,
// принимающий узкий интерфейс. Публичный [NewPurger] фиксирует тип
// `*pgxpool.Pool`, чтобы caller-ы не цеплялись за расширение интерфейса
// в будущем.
func newPurgerFromQueryRower(pool queryRower) *Purger {
	return &Purger{pool: pool}
}

// newPurgerWithLeaseFromQueryRower — внутренний конструктор для unit-тестов
// lease-aware ветки: узкий queryRower + fake lease-checker.
func newPurgerWithLeaseFromQueryRower(pool queryRower, lease soulLeaseChecker, logger *slog.Logger) *Purger {
	return &Purger{pool: pool, lease: lease, logger: logger}
}

// defaultBatchSize — фоллбэк batch-size, если caller передал <= 0.
// Совпадает с DEFAULT в SQL-функции (миграция 002); продублирован
// здесь, чтобы caller с batchSize=0 получал предсказуемое значение в
// логах/метриках до вызова PG.
const defaultBatchSize = 1000

// PurgeAuditOld удаляет один batch expired audit_log-записей старше
// `maxAge`. Возвращает количество удалённых записей за этот batch.
// Loop-логика (drain до 0, cron, leader-election, retry) — out of scope
// M0.4.1c, появится в M0.6 reaper-runner.
//
// `maxAge` должен быть positive; ≤0 возвращает error без обращения к PG
// (отрицательная duration → PG-interval вида `-3600 seconds` синтаксически
// валиден, но семантика `NOW() - (-1h) = NOW()+1h` приведёт к удалению
// 0 строк и молчаливому проглатыванию ошибки конфигурации; явный отказ
// — единственный безопасный режим).
//
// `batchSize <= 0` → используется [defaultBatchSize] (1000), без ошибки.
// `maxAge` конвертируется в Postgres-interval-литерал через
// [durationToPGInterval]; caller (reaper-runner) читает значение из
// `keeper.yml → reaper.rules.purge_audit_old.max_age`, alias на
// `audit.retention_days` (проверка совпадения — в shared/config parser,
// M0/M1.thin).
func (p *Purger) PurgeAuditOld(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_audit_old", maxAge, batchSize)
}

// PurgeExpiredPendingTokens удаляет batch неиспользованных bootstrap-токенов,
// у которых истёк `expires_at` (старше на `maxAge` сверх expiry). Имя правила в
// конфиге — `expire_pending_seeds`; PM-решение Reaper.b: семантика —
// DELETE (а не UPDATE-with-status), т.к. таблица `bootstrap_tokens` не имеет
// колонки status, а истёкший pending-токен не может быть использован.
// Аудит создания живёт в `audit_log` под своим retention-ом (ADR-022).
//
// `maxAge` обычно = 0 (удалять сразу после истечения) или небольшой
// grace-period. Передача `0` запрещена через тот же inv, что у
// PurgeAuditOld — иначе caller с дефолтом будет случайно удалять
// активные токены (NOW() - 0 = NOW() — все expired-токены попадают
// под предикат, что собственно и нужно; но `-1h` приведёт к удалению
// токенов, ещё не истёкших).
//
// На практике `expire_pending_seeds` в keeper.yml имеет `max_age: 24h`,
// что соответствует Bootstrap-policy TTL; см. docs/keeper/reaper.md.
func (p *Purger) PurgeExpiredPendingTokens(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "expire_pending_seeds", maxAge, batchSize)
}

// PurgeUsedTokens удаляет batch использованных bootstrap-токенов
// (`used_at IS NOT NULL`) старше `maxAge` от `used_at`. Default
// `maxAge` = 90d (docs/keeper/reaper.md).
func (p *Purger) PurgeUsedTokens(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_used_tokens", maxAge, batchSize)
}

// PurgeSouls удаляет batch записей `souls` в указанных `statuses`
// (например `[disconnected, expired]`) с возрастом старше `maxAge`.
// Возраст считается от `COALESCE(last_seen_at, registered_at)` — для
// никогда не подключавшихся Soul-ов используется `registered_at`.
//
// `statuses` обязательно непустой: без фильтра по статусу `DELETE`
// снёс бы живые `connected`-записи (docs/keeper/reaper.md). Пустой
// или nil — возвращает error без обращения к PG.
//
// Допустимые значения — узкий MVP-enum souls.status (`pending` |
// `connected` | `disconnected` | `revoked` | `expired`); валидация
// значений делается на стороне semantic-фазы keeper.yml парсера, не
// здесь.
//
// CASCADE: ON DELETE bootstrap_tokens/soul_seeds (CASCADE) автоматически
// чистит связанные записи (см. 008/009 миграции).
func (p *Purger) PurgeSouls(ctx context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error) {
	if len(statuses) == 0 {
		return 0, fmt.Errorf("reaper.purge_souls: statuses must be non-empty")
	}
	return p.callStatusesIntervalBatch(ctx, "purge_souls", statuses, maxAge, batchSize)
}

// PurgeOldSeeds удаляет batch записей `soul_seeds` в указанных
// `statuses` (default `[superseded, expired, revoked]`) с
// `issued_at` старше `maxAge`. Active-seed-ы исключены через
// statuses-фильтр.
//
// `statuses` обязательно непустой — без фильтра DELETE снёс бы
// активные сертификаты.
func (p *Purger) PurgeOldSeeds(ctx context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error) {
	if len(statuses) == 0 {
		return 0, fmt.Errorf("reaper.purge_old_seeds: statuses must be non-empty")
	}
	return p.callStatusesIntervalBatch(ctx, "purge_old_seeds", statuses, maxAge, batchSize)
}

// PurgeOldCerts удаляет batch строк реестра `warrant` (миграция 092) в
// указанных `statuses` (default `[superseded, expired, failed]`) с `issued_at`
// старше `maxAge`. Retention растущей истории ротаций сервисных сертов (R4,
// cert-rotation Вар1). Active/rotating исключены через statuses-фильтр (живой
// материал / серт в процессе ротации).
//
// `statuses` обязательно непустой — без фильтра DELETE снёс бы активные серты.
// Parity PurgeOldSeeds; SQL-функция `purge_old_certs` (093).
func (p *Purger) PurgeOldCerts(ctx context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error) {
	if len(statuses) == 0 {
		return 0, fmt.Errorf("reaper.purge_old_certs: statuses must be non-empty")
	}
	return p.callStatusesIntervalBatch(ctx, "purge_old_certs", statuses, maxAge, batchSize)
}

// PurgeApplyRuns удаляет batch завершённых apply-прогонов из реестра
// `apply_runs` (миграция 018) с `finished_at` старше `maxAge`. Default
// `maxAge` = 30d (docs/keeper/reaper.md).
//
// Удаляются только finished-записи (`success`/`failed`/`cancelled` с
// `finished_at IS NOT NULL`); `running` SQL-функция не трогает — фильтр
// зашит в `purge_apply_runs` (021), здесь дополнительной проверки не
// требуется. Возраст считается от `finished_at`.
func (p *Purger) PurgeApplyRuns(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_apply_runs", maxAge, batchSize)
}

// PurgeVoyages удаляет batch завершённых Voyage-прогонов из реестра `voyages`
// (миграция 059) с `finished_at` старше `maxAge`. Retention растущей истории
// прогонов (реализация отложенного `purge_voyages`, ADR-046 §79). Default
// `maxAge` = 30d (docs/keeper/reaper.md).
//
// Удаляются только finished-записи (`succeeded`/`failed`/`partial_failed`/
// `cancelled` с `finished_at IS NOT NULL`); `scheduled`/`pending`/`running`
// SQL-функция не трогает — фильтр зашит в `purge_voyages` (075). Возраст
// считается от `finished_at` (parity PurgeApplyRuns).
//
// Каскад: `voyage_targets` уносятся `ON DELETE CASCADE` (059). soft-link-и
// `voyage_targets.apply_id`/`errand_id` (на apply_runs/errands) и
// `tidings.voyage_id` (ephemeral) НЕ являются FK на voyages — purge их не
// удаляет и не оставляет битых ссылок (ephemeral-Tiding-и снимаются раньше
// правилом `purge_orphan_ephemeral_tidings`). Корреляционный инвариант: окно
// по умолчанию выровнено на `purge_apply_runs` (30d), чтобы drill «voyage →
// apply_runs» не терял одну из сторон (см. миграцию 075).
func (p *Purger) PurgeVoyages(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_voyages", maxAge, batchSize)
}

// PurgePushRuns удаляет batch завершённых push-прогонов из реестра
// `push_runs` (миграция 051) с `finished_at` старше `maxAge`. Retention
// растущей run-history push-стороны (default `maxAge` = 30d,
// docs/keeper/reaper.md). Зеркало PurgeApplyRuns / PurgeVoyages.
//
// Удаляются только finished-записи (`success`/`partial_failed`/`failed`/
// `cancelled` с `finished_at IS NOT NULL`); `pending`/`running` SQL-функция
// не трогает — фильтр зашит в `purge_push_runs` (076). Возраст считается от
// `finished_at`.
//
// НЕ путать с правилом `purge_orphan_push_runs` (push_orphan.go): то
// терминализирует in-flight зомби (pending/running старше TTL → cancelled),
// а это удаляет уже завершённые записи. Каскад отсутствует — per-host
// результаты хранятся inline в `push_runs.summary` (jsonb), дочерних FK на
// `push_runs` нет (051).
func (p *Purger) PurgePushRuns(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_push_runs", maxAge, batchSize)
}

// PurgeIncarnationArchive удаляет batch строк архива снесённых incarnation
// (`incarnation_archive`, миграция 039) с `archived_at` старше `maxAge`.
// Retention compliance-класса — данные историко-аудитные, поэтому окно
// консервативное (default 365d, docs/keeper/reaper.md). Возраст считается от
// `archived_at` (момент записи в архив при destroy); фильтр зашит в
// `purge_incarnation_archive` (077).
//
// Каскад отсутствует — у `incarnation_archive` нет дочерних FK-таблиц (039:
// архив намеренно без ссылочной целостности к live-реестру). DELETE битых
// ссылок не создаёт.
func (p *Purger) PurgeIncarnationArchive(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_incarnation_archive", maxAge, batchSize)
}

// PurgeStateHistoryArchive удаляет batch строк архива журнала state_history
// снесённых incarnation (`state_history_archive`, миграция 039) с `archived_at`
// старше `maxAge`. Retention compliance-класса (default 365d,
// docs/keeper/reaper.md), parity PurgeIncarnationArchive. Возраст считается от
// `archived_at`; фильтр зашит в `purge_state_history_archive` (077).
//
// Каскад отсутствует — дочерних FK на `state_history_archive` нет (039).
func (p *Purger) PurgeStateHistoryArchive(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_state_history_archive", maxAge, batchSize)
}

// PurgeArchivedStateHistory физически удаляет batch soft-deleted-снимков
// (`archived_at IS NOT NULL`) из ЖИВОЙ таблицы `state_history` (миграция 048) с
// `archived_at` старше `maxAge`. Retention compliance-класса (default 365d,
// docs/keeper/reaper.md), parity PurgeIncarnationArchive.
//
// НЕ путать с `archive_state_history` (049 / [Purger.ArchiveStateHistory]): то
// ТОЛЬКО проставляет soft-delete-флаг `archived_at = NOW()` активным снимкам
// сверх N последних, а это правило физически сносит уже soft-deleted строки по
// истечении окна. Активные снимки (`archived_at IS NULL`) НЕ трогаются — фильтр
// зашит в `purge_archived_state_history` (077). Возраст считается от `archived_at`
// (момент soft-delete), не от `at`.
func (p *Purger) PurgeArchivedStateHistory(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_archived_state_history", maxAge, batchSize)
}

// PurgeApplyTaskRegister удаляет batch register-строк из накопителя
// `apply_task_register` (миграция 022) тех прогонов, чей `apply_runs` уже в
// терминальном статусе (`success`/`failed`/`cancelled`) и завершился
// (`finished_at`) старше `gracePeriod`. Default `gracePeriod` = 1h
// (docs/keeper/reaper.md).
//
// Назначение — защитная гигиена транзиентного run-state: register_data —
// plaintext-JSONB probe-результатов (потенциально с секретами), нужный
// scenario-runner-у ровно один раз после cross-host barrier-а для рендера
// state_changes.sets. FK `ON DELETE CASCADE` чистит его каскадом вместе с
// apply_run (правило `purge_apply_runs`, 30d), но это правило снимает register
// раньше — сразу через grace после терминала, сокращая окно plaintext-хранения.
//
// Критерий «терминальный статус + grace» (а НЕ TTL по created_at): register
// АКТИВНОГО (`running`) прогона не удаляется НИКОГДА, независимо от его
// длительности — фильтр по `apply_runs.status`/`finished_at` зашит в
// `purge_apply_task_register` (023). Возраст считается от `finished_at`.
func (p *Purger) PurgeApplyTaskRegister(ctx context.Context, gracePeriod time.Duration, batchSize int) (int64, error) {
	return p.callIntervalBatch(ctx, "purge_apply_task_register", gracePeriod, batchSize)
}

// reclaimApplyRunsSQL — recovery-скан недо-доставленных Ward (ADR-027 amend, S4).
// Возвращает в `planned` ТОЛЬКО задания, умершие ДО отдачи Soul-у:
// заклеймленные мёртвым Acolyte-ом (`status = 'claimed'` с истёкшим lease
// `claim_expires_at < NOW()`), сбрасывая владельца и lease
// (`claim_by_kid`/`claim_at`/`claim_expires_at` → NULL).
//
// `dispatched` НЕ реклеймится (смена природы правила, GATE-1 передизайн): после
// MarkDispatched задание отдано Soul-у, и прогоном владеет Soul — пере-claim
// dispatched = ВТОРОЙ SendApply = двойной apply. `running` (vestigial) тоже вне
// предиката: Acolyte-флоу его больше не пишет, а пере-claim условного running
// был бы тем же двойным apply. Правило теперь «добить недо-доставленное»
// (claimed, умер до отдачи), а не «реклеймить зомби-running».
//
// `attempt` НЕ сбрасывается: следующий [applyrun.ClaimNext] инкрементит его
// (fencing-epoch растёт), и Keeper-guard на приёме RunResult (S1/S5) отсекает
// stale-результат прежней попытки по `attempt`. Без этого пере-claim протухшего
// claimed мог бы конфликтовать с поздним RunResult — поэтому правило
// `reclaim_apply_runs` включается ТОЛЬКО при раскатанном attempt-fencing
// (см. docs/keeper/reaper.md).
//
// Идиома `WITH … SELECT count(*)`: UPDATE с подзапросом по `apply_runs_claim_scan_idx`
// (миграция 025, partial-индекс на `status IN ('planned','claimed','running')` —
// покрывает `claimed`) + LIMIT $1 батча; внешний SELECT отдаёт affected как
// BIGINT — сохраняет общий `queryRower`-путь Purger-а (`QueryRow`), без отдельной
// SQL-функции и миграции.
//
// Параметр один — `$1 batch` (LIMIT). lease в SQL НЕ передаётся: предикат
// сравнивает `claim_expires_at < NOW()` напрямую (фактический lease зашит в
// claim_expires_at при захвате Ward-а), поэтому interval-аргумента нет — иначе
// PG не вывел бы тип неиспользуемого параметра (42P18).
//
//	$1 batch  — LIMIT возвращаемой за прогон пачки
const reclaimApplyRunsSQL = `
WITH reclaimed AS (
    UPDATE apply_runs
    SET status           = 'planned',
        claim_by_kid     = NULL,
        claim_at         = NULL,
        claim_expires_at = NULL
    WHERE (apply_id, sid) IN (
        SELECT apply_id, sid
        FROM apply_runs
        WHERE status = 'claimed'
          AND claim_expires_at < NOW()
        ORDER BY claim_expires_at ASC
        LIMIT $1
    )
    RETURNING 1
)
SELECT count(*) FROM reclaimed
`

// ReclaimApplyRuns возвращает batch недо-доставленных Ward (умерших ДО отдачи
// Soul-у: `status = 'claimed'` с `claim_expires_at < NOW()`) в `planned` для
// пере-claim, сбрасывая `claim_by_kid`/`claim_at`/`claim_expires_at`.
// `dispatched`/`running` НЕ реклеймятся — после отдачи прогоном владеет Soul,
// пере-claim = двойной apply (ADR-027 amend, S4). `attempt` СОХРАНЯЕТСЯ —
// fencing-epoch инкрементит следующий claim, Keeper-guard на приёме RunResult
// отсекает stale-результат прежней попытки.
// Возвращает число возвращённых заданий за batch.
//
// `lease` здесь — формальный аргумент сигнатуры duration-правила (recovery
// сравнивает `claim_expires_at < NOW()` напрямую, без offset); значение
// валидируется (>0) для единообразия с прочими правилами, но в предикат не входит.
// `batchSize <= 0` → [defaultBatchSize].
//
// Правило `reclaim_apply_runs` по дефолту ВЫКЛЮЧЕНО — включать только при
// раскатанном attempt-fencing (приём RunResult), иначе recovery может конфликтовать
// со stale-результатом.
func (p *Purger) ReclaimApplyRuns(ctx context.Context, lease time.Duration, batchSize int) (int64, error) {
	if lease <= 0 {
		return 0, fmt.Errorf("reaper.reclaim_apply_runs: lease must be > 0, got %v", lease)
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	var count int64
	row := p.pool.QueryRow(ctx, reclaimApplyRunsSQL, batchSize)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.reclaim_apply_runs: %w", err)
	}
	return count, nil
}

// ArchiveStateHistory soft-deletes (`archived_at = NOW()`) активные снимки
// `state_history` сверх `keepLastN` последних на incarnation (по `at DESC`),
// опционально исключая снимки шагов state_schema-миграции
// (`scenario = 'migration'`, см. ADR-019). Реализация — SQL-функция
// `archive_state_history(integer, boolean, integer)` из миграции 049.
// Возвращает число помеченных снимков за этот батч.
//
// `keepLastN <= 0` отвергается без обращения к PG: нулевой keep означал бы
// «архивировать всё», что почти наверняка ошибка конфигурации (`enabled:
// true` без сознательного выбора политики). Caller (Reaper-runner) подставляет
// дефолт 50 при пустом cfg-значении.
//
// `batchSize <= 0` → [defaultBatchSize]: соответствует общему контракту
// Purger-правил.
//
// `keepVersionBump = true` — version-bump-snapshots (scenario='migration')
// никогда не архивируются; restorable anchor для миграций ADR-019 (recovery
// схемы при rollback). false — правило архивирует их наравне с обычными.
func (p *Purger) ArchiveStateHistory(ctx context.Context, keepLastN int, keepVersionBump bool, batchSize int) (int64, error) {
	if keepLastN <= 0 {
		return 0, fmt.Errorf("reaper.archive_state_history: keep_last_n must be > 0, got %d", keepLastN)
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	var count int64
	row := p.pool.QueryRow(ctx, "SELECT archive_state_history($1, $2, $3)", keepLastN, keepVersionBump, batchSize)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.archive_state_history: %w", err)
	}
	return count, nil
}

// MarkDisconnected согласует снимок `souls.status` с фактом Redis SID-lease в
// ОБЕ стороны (ленивый reconcile, ADR-006(a)). Action `set_status` в reaper.md.
// Возвращает суммарное число обновлённых строк (disconnect + reconnect).
//
// `souls.status` — «последнее известное» для Operator API, НЕ источник presence
// (online/offline решает lease). Reconcile приводит снимок к факту фоном:
//
//   - connected → disconnected: `last_seen_at` старше `staleAfter` И нет живого
//     SID-lease (реально протух);
//   - disconnected → connected: жив SID-lease (Soul online; реконнект уже-
//     онбордированного Soul-а Bootstrap-RPC не трогает, eventstream presence в
//     PG на hot-path не пишет — снимок чинит только этот reconcile).
//
// Без обратного направления снимок латчился в `disconnected` навсегда после
// первого «обрыв+sweep» (Operator API отдавал status=disconnected при свежем
// last_seen_at и живом lease).
//
// `staleAfter` обычно = 90s (docs/keeper/reaper.md), что соответствует
// нескольким heartbeat-интервалам. Слишком короткое значение чревато
// flapping-ом (connected ↔ disconnected) при сетевых джиттерах;
// валидация значения — на стороне operator-а через semantic-фазу,
// здесь только формальный sanity (>0).
//
// Lease-aware (Purger собран через [NewPurgerWithLease]): правило двухфазное в
// каждом направлении — (1) выбрать PG-кандидатов
// (select_disconnect_candidates / select_reconnect_candidates), (2) сверить с
// Redis SID-lease, (3) применить (mark_disconnected_sids / mark_connected_sids).
// Это закрывает и ложный disconnect idle-Soul-а (PG `last_seen_at` stale, но
// стрим жив), и латч `disconnected` после реконнекта.
//
// Без lease-checker-а (Purger из [NewPurger], single-instance dev / unit без
// Redis) — fallback на прежнее ОДНОСТОРОННЕЕ чисто-SQL правило mark_disconnected
// (миграция 014): там stale `last_seen_at` ⇔ нет стрима по построению (один
// инстанс), и латча нет — реконнект сразу делает `last_seen_at` свежим.
func (p *Purger) MarkDisconnected(ctx context.Context, staleAfter time.Duration, batchSize int) (int64, error) {
	if p.lease == nil {
		return p.callIntervalBatch(ctx, "mark_disconnected", staleAfter, batchSize)
	}
	return p.reconcileLeaseAware(ctx, staleAfter, batchSize)
}

// reconcileLeaseAware — двунаправленный lease-aware reconcile (см.
// [MarkDisconnected]). Оба направления выполняются за один прогон; возвращает
// сумму обновлённых строк. Ошибка любой PG-фазы прерывает прогон (возврат err) —
// следующий тик повторит; ошибка Redis-проверки конкретного SID fail-safe
// пропускает его (см. filterByLease).
func (p *Purger) reconcileLeaseAware(ctx context.Context, staleAfter time.Duration, batchSize int) (int64, error) {
	if staleAfter <= 0 {
		return 0, fmt.Errorf("reaper.mark_disconnected: duration must be > 0, got %v", staleAfter)
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}

	disconnected, err := p.reconcileDisconnect(ctx, staleAfter, batchSize)
	if err != nil {
		return 0, err
	}
	reconnected, err := p.reconcileReconnect(ctx, batchSize)
	if err != nil {
		return 0, err
	}
	return disconnected + reconnected, nil
}

// reconcileDisconnect — направление connected → disconnected: кандидаты по stale
// `last_seen_at`, оставляем тех, у кого lease МЁРТВ (реально протухли), метим.
func (p *Purger) reconcileDisconnect(ctx context.Context, staleAfter time.Duration, batchSize int) (int64, error) {
	candidates, err := p.selectDisconnectCandidates(ctx, staleAfter, batchSize)
	if err != nil {
		return 0, err
	}
	stale := p.filterByLease(ctx, candidates, false)
	if len(stale) == 0 {
		return 0, nil
	}
	return p.markDisconnectedSIDs(ctx, stale)
}

// reconcileReconnect — направление disconnected → connected: кандидаты —
// disconnected-souls (любой last_seen), оставляем тех, у кого lease ЖИВ (Soul
// online), метим обратно connected. Закрывает латч снимка.
func (p *Purger) reconcileReconnect(ctx context.Context, batchSize int) (int64, error) {
	candidates, err := p.selectReconnectCandidates(ctx, batchSize)
	if err != nil {
		return 0, err
	}
	online := p.filterByLease(ctx, candidates, true)
	if len(online) == 0 {
		return 0, nil
	}
	return p.markConnectedSIDs(ctx, online)
}

// filterByLease сверяет каждого кандидата с Redis SID-lease и возвращает SID-ы,
// чей lease совпал с `wantAlive` (true → online-кандидаты на reconnect, false →
// мёртвые на disconnect). Ошибка Redis-проверки конкретного SID — fail-safe:
// SID пропускается (не метится ни в одну сторону; следующий прогон повторит),
// warn в лог. Живой стрим важнее своевременности снимка в обе стороны.
func (p *Purger) filterByLease(ctx context.Context, candidates []string, wantAlive bool) []string {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]string, 0, len(candidates))
	for _, sid := range candidates {
		alive, checkErr := p.lease.SoulStreamAlive(ctx, sid)
		if checkErr != nil {
			if p.logger != nil {
				p.logger.Warn("reaper.mark_disconnected: lease check failed, skipping (fail-safe)",
					slog.String("sid", sid),
					slog.Bool("want_alive", wantAlive),
					slog.Any("error", checkErr),
				)
			}
			continue
		}
		if alive == wantAlive {
			out = append(out, sid)
		}
	}
	return out
}

// selectDisconnectCandidates — фаза 1: SID-ы connected-souls со stale
// `last_seen_at` (SQL-функция select_disconnect_candidates, миграция 043).
func (p *Purger) selectDisconnectCandidates(ctx context.Context, staleAfter time.Duration, batchSize int) ([]string, error) {
	pgInterval := durationToPGInterval(staleAfter)
	rows, err := p.pool.Query(ctx, "SELECT select_disconnect_candidates($1::interval, $2)", pgInterval, batchSize)
	if err != nil {
		return nil, fmt.Errorf("reaper.mark_disconnected: select candidates: %w", err)
	}
	defer rows.Close()

	var sids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("reaper.mark_disconnected: scan candidate: %w", err)
		}
		sids = append(sids, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reaper.mark_disconnected: iter candidates: %w", err)
	}
	return sids, nil
}

// markDisconnectedSIDs — фаза 3: пометить отфильтрованные SID-ы (SQL-функция
// mark_disconnected_sids, миграция 043). Возвращает число обновлённых строк.
func (p *Purger) markDisconnectedSIDs(ctx context.Context, sids []string) (int64, error) {
	var count int64
	row := p.pool.QueryRow(ctx, "SELECT mark_disconnected_sids($1::text[])", sids)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.mark_disconnected: mark sids: %w", err)
	}
	return count, nil
}

// selectReconnectCandidates — фаза 1 обратного направления: SID-ы disconnected-
// souls любого `last_seen_at` (SQL-функция select_reconnect_candidates, миграция
// 043). Без duration-предиката — онлайновость решает lease, не свежесть снимка.
func (p *Purger) selectReconnectCandidates(ctx context.Context, batchSize int) ([]string, error) {
	rows, err := p.pool.Query(ctx, "SELECT select_reconnect_candidates($1)", batchSize)
	if err != nil {
		return nil, fmt.Errorf("reaper.mark_disconnected: select reconnect candidates: %w", err)
	}
	defer rows.Close()

	var sids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("reaper.mark_disconnected: scan reconnect candidate: %w", err)
		}
		sids = append(sids, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reaper.mark_disconnected: iter reconnect candidates: %w", err)
	}
	return sids, nil
}

// markConnectedSIDs — фаза 3 обратного направления: вернуть disconnected → connected
// для online-SID-ов (SQL-функция mark_connected_sids, миграция 043). Возвращает
// число обновлённых строк.
func (p *Purger) markConnectedSIDs(ctx context.Context, sids []string) (int64, error) {
	var count int64
	row := p.pool.QueryRow(ctx, "SELECT mark_connected_sids($1::text[])", sids)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.mark_disconnected: mark connected sids: %w", err)
	}
	return count, nil
}

// callIntervalBatch — общий вызов SQL-функции с сигнатурой
// `(interval, integer) → BIGINT`. Используется для всех правил,
// у которых нет statuses[]-параметра.
//
// Семантика валидации (`maxAge <= 0` → error без PG, `batchSize <= 0`
// → defaultBatchSize) совпадает с docstring-ом PurgeAuditOld и
// зафиксирована тестами per-method.
func (p *Purger) callIntervalBatch(ctx context.Context, fnName string, duration time.Duration, batchSize int) (int64, error) {
	if duration <= 0 {
		return 0, fmt.Errorf("reaper.%s: duration must be > 0, got %v", fnName, duration)
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	var count int64
	pgInterval := durationToPGInterval(duration)
	sql := fmt.Sprintf("SELECT %s($1::interval, $2)", fnName)
	row := p.pool.QueryRow(ctx, sql, pgInterval, batchSize)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.%s: %w", fnName, err)
	}
	return count, nil
}

// callStatusesIntervalBatch — общий вызов SQL-функции с сигнатурой
// `(text[], interval, integer) → BIGINT`. Caller гарантирует, что
// `statuses` непустой (см. PurgeSouls / PurgeOldSeeds).
func (p *Purger) callStatusesIntervalBatch(ctx context.Context, fnName string, statuses []string, duration time.Duration, batchSize int) (int64, error) {
	if duration <= 0 {
		return 0, fmt.Errorf("reaper.%s: duration must be > 0, got %v", fnName, duration)
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	var count int64
	pgInterval := durationToPGInterval(duration)
	sql := fmt.Sprintf("SELECT %s($1::text[], $2::interval, $3)", fnName)
	row := p.pool.QueryRow(ctx, sql, statuses, pgInterval, batchSize)
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("reaper.%s: %w", fnName, err)
	}
	return count, nil
}

// durationToPGInterval конвертирует time.Duration в Postgres
// interval-литерал в секундах. Секунды выбраны как универсальный
// формат: устраняют day-precision-аномалии Postgres-а
// (`'1 day'::interval` ≠ `'24 hours'::interval` при переходах летнего
// времени, см. PG docs «9.9.4 Interval Input»), и любая duration
// (включая sub-second в тестах) представима без потери.
func durationToPGInterval(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}
