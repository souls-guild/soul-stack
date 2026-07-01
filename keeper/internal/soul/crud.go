package soul

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel-ошибки CRUD-слоя. Handler-сторона маппит:
//   - ErrSoulAlreadyExists  → 409 soul-already-exists.
//   - ErrSoulNotFound       → 404 not-found.
//   - ErrSoulCreatorNotFound → 422 validation-failed (AID создателя отсутствует
//     в реестре operators). Симметрично bootstraptoken.ErrTokenSoulNotFound.
//   - ErrSoulprintNotReceived → 410 gone (`GET /v1/souls/{sid}/soulprint`):
//     запись Soul-а есть, но SoulprintReport ещё ни разу не приходил — пустая
//     запись `soulprint_facts IS NULL`. Различение от 404: сам Soul существует.
var (
	ErrSoulAlreadyExists    = errors.New("soul: SID already exists")
	ErrSoulNotFound         = errors.New("soul: SID not found")
	ErrSoulCreatorNotFound  = errors.New("soul: created_by AID not found in operators registry")
	ErrSoulprintNotReceived = errors.New("soul: soulprint not yet received")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество интерфейса pgxpool.Pool, нужное
// CRUD-у. Симметрично [operator.ExecQueryRower] / [incarnation.ExecQueryRower]:
// unit-тесты ходят через fake без подъёма PG, production — через реальный
// pool / Conn / Tx.
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
INSERT INTO souls (
    sid, transport, status, coven, traits,
    registered_at, last_seen_at, last_seen_by_kid,
    created_by_aid, requested_at, note
) VALUES ($1, $2, $3, $4, COALESCE($5, '{}'::jsonb),
    COALESCE($6, NOW()), $7, $8,
    $9, COALESCE($10, NOW()), $11)
RETURNING registered_at, requested_at
`

const selectBySIDSQL = `
SELECT sid, transport, status, coven, traits,
       registered_at, last_seen_at, last_seen_by_kid,
       created_by_aid, requested_at, note
FROM souls
WHERE sid = $1
`

const deleteBySIDSQL = `
DELETE FROM souls
WHERE sid = $1
`

// updateStatusSQL — атомарный UPDATE статуса с фиксацией last_seen_by_kid
// (для аудита «какой Keeper последним держал стрим»). last_seen_at пишется
// в Redis, в PG — flush; здесь не трогаем.
const updateStatusSQL = `
UPDATE souls
SET status = $2,
    last_seen_by_kid = COALESCE($3, last_seen_by_kid)
WHERE sid = $1
`

// updateCovenSQL — UPDATE набора стабильных Coven-меток. Используется
// keeper-side core-модулем `core.soul.registered` (docs/keeper/modules.md).
// Возвращает финальный набор coven одной round-trip-ой; RETURNING избавляет
// от лишнего SelectBySID для построения output-а модуля.
const updateCovenSQL = `
UPDATE souls
SET coven = $2
WHERE sid = $1
RETURNING coven
`

// updateLastSeenSQL — точечный UPDATE `last_seen_at`/`last_seen_by_kid`
// (ADR-006(a) flush из Redis-кэша; авторитетное значение — в Redis,
// PG — снимок). Вызывается из throttled-flush в touchSeen на каждое
// app-сообщение EventStream, но не чаще stale_after/3 (см. fix 89b4f0a):
// частые heartbeat-ы держат Redis, в PG прилетает прорежённый снимок.
//
// status НЕ трогаем — UpdateStatus оставлен под bootstrap/Reaper.
const updateLastSeenSQL = `
UPDATE souls
SET last_seen_at     = $2,
    last_seen_by_kid = $3
WHERE sid = $1
`

// updateSoulprintSQL — UPDATE typed-soulprint полей (миграция 015).
// `facts` JSON-сериализован вызывающей стороной (proto → Struct → JSON).
// `received_at` — Keeper-side timestamp, отдельный от `collected_at`
// (последний приходит от Soul-а в `SoulprintReport.collected_at`).
const updateSoulprintSQL = `
UPDATE souls
SET soulprint_facts        = $2,
    soulprint_collected_at = $3,
    soulprint_received_at  = $4
WHERE sid = $1
`

// Insert вставляет нового Soul-а в реестр. Используется Operator API при
// выписке bootstrap-токена (создаёт строку в статусе `pending`).
//
// Pre-conditions:
//   - s.SID соответствует [SIDPattern];
//   - s.Transport / s.Status — допустимые enum-значения.
//
// Возврат:
//   - [ErrSoulAlreadyExists] на UNIQUE по PK.
//   - [ErrSoulCreatorNotFound] на FK-violation по `souls_created_by_aid_fk`
//     (`created_by_aid` указывает на несуществующего operator-а).
//   - wrapped fmt.Errorf на прочие FK-violation и CHECK-violation
//     (status / transport / sid-format).
//
// `requested_at` проставляется на стороне PG (`COALESCE($9, NOW())`), если
// caller не задал s.RequestedAt — нормативная семантика pending-записи
// (docs/soul/onboarding.md). После Insert s.RequestedAt содержит фактическое
// значение (`RETURNING requested_at`).
func Insert(ctx context.Context, db ExecQueryRower, s *Soul) error {
	if s == nil {
		return fmt.Errorf("soul: nil soul")
	}
	if !ValidSID(s.SID) {
		return fmt.Errorf("soul: invalid SID %q (must match %s)", s.SID, SIDPattern)
	}
	if s.Transport == "" {
		s.Transport = TransportAgent
	}
	if !validTransport(s.Transport) {
		return fmt.Errorf("soul: invalid transport %q", s.Transport)
	}
	if s.Status == "" {
		s.Status = StatusPending
	}
	if !validStatus(s.Status) {
		return fmt.Errorf("soul: invalid status %q", s.Status)
	}

	coven := s.Coven
	if coven == nil {
		coven = []string{}
	}

	// traits — jsonb (миграция 087, ADR-060): сериализуем map в []byte
	// (паттерн incarnation marshalJSONB; pgx-codec-auto для jsonb здесь
	// сознательно не используется, единообразно с прочими jsonb-колонками).
	// nil/пустой → arg=nil, COALESCE($5,'{}') в SQL даёт пустой объект.
	var traitsArg any
	if len(s.Traits) > 0 {
		b, err := json.Marshal(s.Traits)
		if err != nil {
			return fmt.Errorf("soul: marshal traits: %w", err)
		}
		traitsArg = b
	}

	var registeredAtArg any
	if !s.RegisteredAt.IsZero() {
		registeredAtArg = s.RegisteredAt.UTC()
	}
	var lastSeenAtArg any
	if s.LastSeenAt != nil {
		lastSeenAtArg = s.LastSeenAt.UTC()
	}
	var lastSeenByKIDArg any
	if s.LastSeenByKID != nil {
		lastSeenByKIDArg = *s.LastSeenByKID
	}
	var createdByAIDArg any
	if s.CreatedByAID != nil {
		createdByAIDArg = *s.CreatedByAID
	}
	var requestedAtArg any
	if s.RequestedAt != nil {
		requestedAtArg = s.RequestedAt.UTC()
	}
	var noteArg any
	if s.Note != "" {
		noteArg = s.Note
	}

	row := db.QueryRow(ctx, insertSQL,
		s.SID,
		string(s.Transport),
		string(s.Status),
		coven,
		traitsArg,
		registeredAtArg,
		lastSeenAtArg,
		lastSeenByKIDArg,
		createdByAIDArg,
		requestedAtArg,
		noteArg,
	)
	if err := row.Scan(&s.RegisteredAt, &s.RequestedAt); err != nil {
		return mapInsertError(err)
	}
	return nil
}

// DeleteBySID удаляет запись souls по SID. FK bootstrap_tokens.sid и
// soul_seeds.sid объявлены ON DELETE CASCADE (миграции 008/009) — связанные
// токены и seed-записи уходят вместе с Soul-ом. Возвращает [ErrSoulNotFound],
// если строки с таким SID нет (идемпотентно для caller-а, который откатывает
// только что вставленную запись).
//
// Точечный откат по SID; batch-GC просроченных Soul-ов — отдельная Reaper-
// функция purge_souls (миграция 012), она статус-фильтрованная.
func DeleteBySID(ctx context.Context, db ExecQueryRower, sid string) error {
	if !ValidSID(sid) {
		return fmt.Errorf("soul: invalid SID %q", sid)
	}
	tag, err := db.Exec(ctx, deleteBySIDSQL, sid)
	if err != nil {
		return fmt.Errorf("soul: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSoulNotFound
	}
	return nil
}

func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w",
				ErrSoulAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			if pgErr.ConstraintName == "souls_created_by_aid_fk" {
				return fmt.Errorf("%w (constraint %s): %w",
					ErrSoulCreatorNotFound, pgErr.ConstraintName, err)
			}
			return fmt.Errorf("soul: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("soul: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("soul: insert: %w", err)
}

// SelectBySID читает Soul по PK. [ErrSoulNotFound] при pgx.ErrNoRows.
func SelectBySID(ctx context.Context, db ExecQueryRower, sid string) (*Soul, error) {
	row := db.QueryRow(ctx, selectBySIDSQL, sid)
	return scanSoul(row)
}

// SoulprintRecord — последний полученный SoulprintReport одного хоста
// (`GET /v1/souls/{sid}/soulprint`). FactsJSON — сырой JSONB, ровно тот, что
// собирает eventstream через `protojson.Marshal(SoulprintFacts)` с
// `UseProtoNames` (snake_case ключи `pkg_mgr`/`init_system` и т.д., ADR-018).
// Парсинг — на стороне consumer-а (handler отдаёт как `map[string]any` для
// UI-симметрии с другими jsonb-полями вроде `incarnation.state`).
//
// CollectedAt — Soul-side timestamp сбора (из proto `SoulprintReport.collected_at`),
// ReceivedAt — Keeper-side momentum приёма стрима (см. [UpdateSoulprint]).
// Различие — диагностика skew, при > 10 минут eventstream пишет warn в OTel
// (docs/soul/soulprint.md → §`received_at`/`collected_at`).
type SoulprintRecord struct {
	SID         string
	FactsJSON   []byte
	CollectedAt time.Time
	ReceivedAt  time.Time
}

const selectSoulprintSQL = `
SELECT sid, soulprint_facts, soulprint_collected_at, soulprint_received_at
FROM souls
WHERE sid = $1
`

// SelectSoulprint читает последний typed-SoulprintReport одного Soul-а.
//
// Возврат:
//   - [ErrSoulNotFound] — записи в реестре `souls` нет.
//   - [ErrSoulprintNotReceived] — запись есть, но SoulprintReport ни разу не
//     приходил (`soulprint_facts IS NULL`). Маппится handler-ом в HTTP 410.
//
// FactsJSON отдаётся как есть, без unmarshal-а: storage-инвариант — JSONB
// уже в форме proto-snake_case ключей; handler пробрасывает в JSON-ответ
// через `json.RawMessage`/decoded `map[string]any`, не дублируя proto-схему
// на Go-сторону Keeper-а.
func SelectSoulprint(ctx context.Context, db ExecQueryRower, sid string) (*SoulprintRecord, error) {
	if !ValidSID(sid) {
		return nil, fmt.Errorf("soul: invalid SID %q", sid)
	}
	var (
		rec         SoulprintRecord
		factsJSON   []byte
		collectedAt *time.Time
		receivedAt  *time.Time
	)
	err := db.QueryRow(ctx, selectSoulprintSQL, sid).Scan(&rec.SID, &factsJSON, &collectedAt, &receivedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSoulNotFound
		}
		return nil, fmt.Errorf("soul: soulprint select: %w", err)
	}
	if len(factsJSON) == 0 {
		// Запись Soul-а есть, но фактов нет: typed-SoulprintReport ещё не
		// присылался (Soul только что прошёл онбординг либо transport: ssh).
		return nil, ErrSoulprintNotReceived
	}
	rec.FactsJSON = factsJSON
	if collectedAt != nil {
		rec.CollectedAt = collectedAt.UTC()
	}
	if receivedAt != nil {
		rec.ReceivedAt = receivedAt.UTC()
	}
	return &rec, nil
}

func scanSoul(row pgx.Row) (*Soul, error) {
	var (
		s             Soul
		transportStr  string
		statusStr     string
		traitsJSON    []byte
		lastSeenAt    *time.Time
		lastSeenByKID *string
		createdByAID  *string
		requestedAt   *time.Time
		note          *string
	)
	err := row.Scan(
		&s.SID,
		&transportStr,
		&statusStr,
		&s.Coven,
		&traitsJSON,
		&s.RegisteredAt,
		&lastSeenAt,
		&lastSeenByKID,
		&createdByAID,
		&requestedAt,
		&note,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSoulNotFound
		}
		return nil, fmt.Errorf("soul: scan: %w", err)
	}
	s.Transport = Transport(transportStr)
	s.Status = Status(statusStr)
	// traits jsonb (ADR-060): '{}' (NOT NULL DEFAULT) → пустой map, не nil.
	if len(traitsJSON) > 0 {
		if err := json.Unmarshal(traitsJSON, &s.Traits); err != nil {
			return nil, fmt.Errorf("soul: unmarshal traits for %q: %w", s.SID, err)
		}
	}
	s.LastSeenAt = lastSeenAt
	s.LastSeenByKID = lastSeenByKID
	s.CreatedByAID = createdByAID
	s.RequestedAt = requestedAt
	if note != nil {
		s.Note = *note
	}
	return &s, nil
}

// UpdateStatus переводит Soul в новый status и обновляет last_seen_by_kid.
// kid — указатель: nil сохраняет старое значение (PG `COALESCE`),
// non-nil перетирает (типичный путь после `Bootstrap`-RPC или
// EventStream-handshake).
//
// Возвращает [ErrSoulNotFound], если SID не существует или UPDATE не
// затронул строк.
func UpdateStatus(ctx context.Context, db ExecQueryRower, sid string, newStatus Status, kid *string) error {
	if !ValidSID(sid) {
		return fmt.Errorf("soul: invalid SID %q", sid)
	}
	if !validStatus(newStatus) {
		return fmt.Errorf("soul: invalid status %q", newStatus)
	}
	var kidArg any
	if kid != nil {
		kidArg = *kid
	}
	tag, err := db.Exec(ctx, updateStatusSQL, sid, string(newStatus), kidArg)
	if err != nil {
		return fmt.Errorf("soul: update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSoulNotFound
	}
	return nil
}

// UpdateCoven — атомарный UPDATE набора стабильных Coven-меток. Используется
// keeper-side core-модулем `core.soul.registered` (docs/keeper/modules.md).
//
// Caller сам считает финальный набор по `mode` (`append`/`replace`/`remove`):
// эта функция выполняет только сам UPDATE. Возвращает фактически сохранённый
// набор (PG `RETURNING coven`) — гарантирует, что output модуля построен
// на actual, а не на пере-вычисленном клиентом значении.
//
// Возвращает [ErrSoulNotFound], если SID не существует.
func UpdateCoven(ctx context.Context, db ExecQueryRower, sid string, coven []string) ([]string, error) {
	if !ValidSID(sid) {
		return nil, fmt.Errorf("soul: invalid SID %q", sid)
	}
	if coven == nil {
		coven = []string{}
	}
	var saved []string
	err := db.QueryRow(ctx, updateCovenSQL, sid, coven).Scan(&saved)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSoulNotFound
		}
		return nil, fmt.Errorf("soul: update coven: %w", err)
	}
	return saved, nil
}

// UpdateLastSeen — flush last_seen_at/last_seen_by_kid в PG (ADR-006(a)).
// real-time значение живёт в Redis-heartbeat-кэше; в PG — snapshot,
// нужный Reaper-у (`mark_disconnected`) и Operator API (`GET /v1/souls`).
//
// Возвращает [ErrSoulNotFound], если SID не существует.
func UpdateLastSeen(ctx context.Context, db ExecQueryRower, sid, kid string, at time.Time) error {
	if !ValidSID(sid) {
		return fmt.Errorf("soul: invalid SID %q", sid)
	}
	if kid == "" {
		return fmt.Errorf("soul: empty kid")
	}
	tag, err := db.Exec(ctx, updateLastSeenSQL, sid, at.UTC(), kid)
	if err != nil {
		return fmt.Errorf("soul: update last_seen: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSoulNotFound
	}
	return nil
}

// UpdateSoulprint фиксирует typed-facts в PG (миграция 015 → колонки
// `soulprint_facts`/`soulprint_collected_at`/`soulprint_received_at`).
//
// `factsJSON` — заранее marshal-ленные байты proto `SoulprintFacts`,
// caller вызывает [protojson.Marshal] (форвард-compat — proto-default
// сериализация). nil / пустой slice разрешён: при первом подключении
// Soul-а ещё нет SoulprintReport-а, и мы хотим уметь обнулять колонку
// (тесты, ручной reset).
//
// Возвращает [ErrSoulNotFound], если SID не существует.
func UpdateSoulprint(ctx context.Context, db ExecQueryRower, sid string, factsJSON []byte, collectedAt, receivedAt time.Time) error {
	if !ValidSID(sid) {
		return fmt.Errorf("soul: invalid SID %q", sid)
	}
	var factsArg any
	if len(factsJSON) > 0 {
		factsArg = factsJSON
	}
	var collectedArg any
	if !collectedAt.IsZero() {
		collectedArg = collectedAt.UTC()
	}
	var receivedArg any
	if !receivedAt.IsZero() {
		receivedArg = receivedAt.UTC()
	}
	tag, err := db.Exec(ctx, updateSoulprintSQL, sid, factsArg, collectedArg, receivedArg)
	if err != nil {
		return fmt.Errorf("soul: update soulprint: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSoulNotFound
	}
	return nil
}

// BulkPool — поверхность над pgxpool.Pool, нужная [BulkAssignCoven]:
// ExecQueryRower (count + per-chunk UPDATE/Scan) + BeginTx (коммит на чанк).
// `*pgxpool.Pool` удовлетворяет автоматически; unit-тесты — fake.
type BulkPool interface {
	ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// BulkSelector — подмножество словаря таргетинга `soul.*` для bulk-операций
// (`POST /v1/souls/coven`). НЕ topology.Resolver: чистый PG-предикат по
// холодным колонкам `souls`, без presence/soulprint.
//
//   - All         — без host-фильтра (весь реестр; пересекается со scope).
//   - SIDs        — точечный список хостов (`sid = ANY($n)`).
//   - Coven       — хосты, у которых УЖЕ есть эта метка (`$n = ANY(coven)`).
//   - Incarnation — хосты этой incarnation: матчатся по совпадению её имени
//     с одной из coven-меток хоста (`incarnation.name` — корневая Coven-метка
//     по ADR-008), `$n = ANY(coven)`. Семантически отличается от Coven (имя
//     incarnation vs произвольная stable-метка), хотя SQL-предикат тот же.
//   - Status      — фильтр по статусу.
//
// Пустые SIDs/Coven/Incarnation/Status — «не фильтровать». All=false при
// пустых остальных критериях даёт пустой host-набор (caller обязан задать
// хотя бы один критерий — иначе bulk без таргета).
//
// Комбинации критериев соединяются AND-ом (ужесточение). Например,
// {Incarnation: "redis", Status: connected} матчит только connected-хосты
// incarnation `redis`.
type BulkSelector struct {
	All         bool
	SIDs        []string
	Coven       string
	Incarnation string
	Status      Status
}

// BulkScope — coven-scope оператора для `soul.coven-assign` (из
// rbac.Enforcer.CovenScope). Unrestricted=true (bare/`*`) снимает оба
// ограничения scope-intersection-а.
type BulkScope struct {
	Covens       []string
	Unrestricted bool
}

// BulkStatus — терминальный статус bulk-операции.
type BulkStatus string

const (
	// BulkCompleted — все чанки закоммичены (или dry_run).
	BulkCompleted BulkStatus = "completed"
	// BulkPartial — чанк K упал; 1..K-1 закоммичены и не откатываются
	// (идемпотентно до-повторяется оператором).
	BulkPartial BulkStatus = "partial"
)

// Report — итог bulk coven-assign.
//
//   - Matched         — сколько хостов попало под selector ∩ scope.
//   - Changed         — сколько строк фактически изменено (сумма RowsAffected
//     по чанкам; идемпотентный отсев не считается).
//   - ChunksCommitted — число успешно закоммиченных чанков.
//   - Status          — completed | partial.
//   - Err             — причина partial-фейла (nil для completed).
type Report struct {
	Matched         int
	Changed         int
	ChunksCommitted int
	Status          BulkStatus
	Err             error
}

// bulkChunkSize — размер чанка keyset-итерации (ТЗ: 1–2k SID, коммит на чанк).
// Меньше — больше round-trip-ов; больше — дольше держится row-lock на `souls`,
// блокируя горячий heartbeat-flush UpdateLastSeen. 2000 — верх рекомендованного
// окна.
const bulkChunkSize = 2000

// ErrBulkEmptySelector — selector без единого критерия (All=false и пусто всё
// остальное): bulk без таргета — почти всегда ошибка вызова, отвергаем.
var ErrBulkEmptySelector = errors.New("soul: bulk selector matches no hosts (set all/sids/coven/status)")

// ErrBulkLabelOutOfScope — назначаемая (append) метка вне coven-scope
// оператора. Privilege-escalation-гейт (b): оператор не может навесить метку,
// которой не владеет в своём scope.
var ErrBulkLabelOutOfScope = errors.New("soul: label is outside operator coven-scope")

// CountBulkMatched считает хосты под selector ∩ scope без мутации (dry_run и
// предрасчёт Matched). Возвращает [ErrBulkEmptySelector] на пустой selector.
func CountBulkMatched(ctx context.Context, db ExecQueryRower, sel BulkSelector, scope BulkScope) (int, error) {
	where, args, err := buildBulkWhere(sel, scope)
	if err != nil {
		return 0, err
	}
	var n int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM souls"+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("soul: bulk count: %w", err)
	}
	return n, nil
}

// BulkAssignCoven массово добавляет (append) или снимает (remove) ОДНУ
// Coven-метку с хостов под selector ∩ scope (`POST /v1/souls/coven`).
//
// Инварианты:
//   - Coven — ХОЛОДНАЯ PG-метка: чистый UPDATE souls, никаких записей в Redis.
//   - Keyset-итерация по PK (`sid > cursor ORDER BY sid LIMIT chunk`, НЕ
//     OFFSET) + коммит на чанк: одна гигантская транзакция держала бы row-lock
//     на `souls` десятки секунд, блокируя heartbeat-flush UpdateLastSeen.
//   - Идемпотентный отсев в WHERE: append не трогает хост, где метка уже есть;
//     remove — где метки нет. Нетронутая строка lock не берёт.
//   - scope-intersection: целевые хосты ⊆ scope (предикат coven && ARRAY[scope]);
//     для append назначаемая метка ∈ scope (иначе [ErrBulkLabelOutOfScope]).
//     Unrestricted-scope (bare/`*`) снимает оба ограничения.
//   - При фейле чанка K: 1..K-1 закоммичены, Status=partial — НЕ откатываем
//     (идемпотентно до-повторяется оператором).
//
// label обязан быть валиден ([ValidCoven]) и mode ∈ {append, remove} — caller
// (handler) проверяет до вызова; здесь — defensive re-check. Для mode=replace
// используется [BulkReplaceCoven] с набором меток.
func BulkAssignCoven(ctx context.Context, db BulkPool, sel BulkSelector, scope BulkScope, label string, mode CovenMode) (Report, error) {
	if !ValidCoven(label) {
		return Report{}, fmt.Errorf("soul: invalid coven label %q (must match %s)", label, CovenPattern)
	}
	if mode != CovenAppend && mode != CovenRemove {
		return Report{}, fmt.Errorf("soul: bulk mode %q unsupported (want append/remove; use BulkReplaceCoven for replace)", mode)
	}
	// Гейт (b): append-метку вне scope назначать нельзя.
	if mode == CovenAppend && !scope.Unrestricted && !covenInScope(label, scope.Covens) {
		return Report{}, fmt.Errorf("%w: %q", ErrBulkLabelOutOfScope, label)
	}

	matched, err := CountBulkMatched(ctx, db, sel, scope)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Matched: matched, Status: BulkCompleted}
	if matched == 0 {
		return rep, nil
	}

	cursor := ""
	for {
		tag, lastSID, n, cerr := bulkUpdateChunk(ctx, db, sel, scope, label, mode, cursor)
		if cerr != nil {
			rep.Status = BulkPartial
			rep.Err = cerr
			return rep, cerr
		}
		rep.Changed += int(tag)
		rep.ChunksCommitted++
		if n < bulkChunkSize {
			break
		}
		cursor = lastSID
	}
	return rep, nil
}

// BulkReplaceCoven массово ЗАМЕНЯЕТ набор Coven-меток хоста на `labels`
// ровно (выкидывая существующие) для хостов под selector ∩ scope.
//
// Семантические отличия от [BulkAssignCoven]:
//   - mode подразумевается replace; SET `coven = $labels` целиком, не
//     array_append/array_remove над одной меткой.
//   - Гейт (b) проверяет КАЖДУЮ метку набора (любая вне scope →
//     [ErrBulkLabelOutOfScope]). Иначе scope-bypass: оператор с scope `dev`
//     передал бы [dev, prod] и навесил `prod` чужим хостам внутри `dev`.
//   - Идемпотентный отсев: `coven IS DISTINCT FROM $labels` (PG-предикат,
//     корректный для NULL и для массивов; учитывает порядок элементов — поэтому
//     caller обязан передать набор в КАНОНИЧНОЙ форме через [covenUniqueSorted],
//     иначе одинаковый по множеству, но разный по порядку набор будет писаться
//     повторно).
//   - Пустой набор (`labels = []`) допустим — это «снять все метки». Гейт (b)
//     для пустого набора вырождается в no-op (нет ни одной out-of-scope метки),
//     но гейт (a) WHERE-предикат scope-intersection обязательно прогоняется:
//     coven-scoped оператор не вычистит метки с чужих хостов.
//
// Чанкинг, partial-семантика и общий каркас итерации — те же, что у
// [BulkAssignCoven].
func BulkReplaceCoven(ctx context.Context, db BulkPool, sel BulkSelector, scope BulkScope, labels []string) (Report, error) {
	canonical := covenUniqueSorted(labels)
	for _, l := range canonical {
		if !ValidCoven(l) {
			return Report{}, fmt.Errorf("soul: invalid coven label %q (must match %s)", l, CovenPattern)
		}
	}
	// Гейт (b): КАЖДАЯ метка набора обязана быть в scope. Симметрично
	// append-у, но проверка циклом — расширение «privilege-escalation-гейта»
	// на replace-набор.
	if !scope.Unrestricted {
		for _, l := range canonical {
			if !covenInScope(l, scope.Covens) {
				return Report{}, fmt.Errorf("%w: %q", ErrBulkLabelOutOfScope, l)
			}
		}
	}

	matched, err := CountBulkMatched(ctx, db, sel, scope)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Matched: matched, Status: BulkCompleted}
	if matched == 0 {
		return rep, nil
	}

	cursor := ""
	for {
		tag, lastSID, n, cerr := bulkReplaceChunk(ctx, db, sel, scope, canonical, cursor)
		if cerr != nil {
			rep.Status = BulkPartial
			rep.Err = cerr
			return rep, cerr
		}
		rep.Changed += int(tag)
		rep.ChunksCommitted++
		if n < bulkChunkSize {
			break
		}
		cursor = lastSID
	}
	return rep, nil
}

// bulkUpdateChunk выполняет один чанк в собственной транзакции: keyset-окно
// `sid > cursor ORDER BY sid LIMIT chunk` под selector ∩ scope + идемпотентный
// отсев. Возвращает (changedRows, lastSID, scannedRows, err):
//
//   - changedRows — RETURNING-строки (фактически изменённые в этом чанке);
//   - lastSID     — макс. sid в окне (следующий cursor); пуст, если окно пусто;
//   - scannedRows — число строк в keyset-окне ДО идемпотентного отсева
//     (для условия выхода: scannedRows < chunk → последний чанк).
//
// Идемпотентный отсев и keyset-окно совмещены: RETURNING даёт changedRows,
// но условие выхода — по размеру keyset-окна, а не по changedRows (иначе чанк,
// где все метки уже на месте, оборвал бы итерацию преждевременно). Поэтому
// окно отбирается отдельным keyset-предикатом, а UPDATE применяется к его
// подмножеству.
func bulkUpdateChunk(ctx context.Context, db BulkPool, sel BulkSelector, scope BulkScope, label string, mode CovenMode, cursor string) (changed int64, lastSID string, scanned int, err error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	where, args := buildBulkWhereWithCursor(sel, scope, cursor)
	// $label — последний позиционный аргумент (для array_append/remove и
	// идемпотентного предиката).
	labelPos := len(args) + 1
	args = append(args, label)

	var setExpr, idemPred string
	switch mode {
	case CovenAppend:
		setExpr = fmt.Sprintf("array_append(coven, $%d)", labelPos)
		idemPred = fmt.Sprintf("NOT ($%d = ANY(coven))", labelPos)
	case CovenRemove:
		setExpr = fmt.Sprintf("array_remove(coven, $%d)", labelPos)
		idemPred = fmt.Sprintf("$%d = ANY(coven)", labelPos)
	}

	// CTE: keyset-окно (window) фиксирует chunk хостов по PK; scanned — его
	// размер (для условия выхода); upd мутирует только подмножество с
	// идемпотентным отсевом и возвращает изменённые sid.
	sql := fmt.Sprintf(`
WITH chunk AS (
    SELECT sid FROM souls%s
    ORDER BY sid LIMIT %d
),
upd AS (
    UPDATE souls
    SET coven = %s
    WHERE sid IN (SELECT sid FROM chunk) AND %s
    RETURNING sid
)
SELECT
    (SELECT COUNT(*) FROM chunk),
    (SELECT COUNT(*) FROM upd),
    (SELECT MAX(sid) FROM chunk)
`, where, bulkChunkSize, setExpr, idemPred)

	var (
		scannedN int
		changedN int64
		maxSID   *string
	)
	if err := tx.QueryRow(ctx, sql, args...).Scan(&scannedN, &changedN, &maxSID); err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk chunk update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk chunk commit: %w", err)
	}
	committed = true

	last := ""
	if maxSID != nil {
		last = *maxSID
	}
	return changedN, last, scannedN, nil
}

// bulkReplaceChunk выполняет один чанк replace-режима: тот же CTE-каркас, что
// [bulkUpdateChunk], но UPDATE заменяет весь набор `coven = $labels` с
// идемпотентным отсевом `coven IS DISTINCT FROM $labels` (PG-форма «не равно»,
// безопасная для NULL и для массивов).
func bulkReplaceChunk(ctx context.Context, db BulkPool, sel BulkSelector, scope BulkScope, labels []string, cursor string) (changed int64, lastSID string, scanned int, err error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	where, args := buildBulkWhereWithCursor(sel, scope, cursor)
	labelsPos := len(args) + 1
	// pgx маппит nil-slice в NULL; для replace на пустой набор хотим пустой
	// массив (`coven = ARRAY[]::text[]`), иначе нарушится NOT NULL-ожидание
	// колонки. Симметрично [UpdateCoven] выше.
	canonical := labels
	if canonical == nil {
		canonical = []string{}
	}
	args = append(args, canonical)

	sql := fmt.Sprintf(`
WITH chunk AS (
    SELECT sid FROM souls%s
    ORDER BY sid LIMIT %d
),
upd AS (
    UPDATE souls
    SET coven = $%d
    WHERE sid IN (SELECT sid FROM chunk) AND coven IS DISTINCT FROM $%d
    RETURNING sid
)
SELECT
    (SELECT COUNT(*) FROM chunk),
    (SELECT COUNT(*) FROM upd),
    (SELECT MAX(sid) FROM chunk)
`, where, bulkChunkSize, labelsPos, labelsPos)

	var (
		scannedN int
		changedN int64
		maxSID   *string
	)
	if err := tx.QueryRow(ctx, sql, args...).Scan(&scannedN, &changedN, &maxSID); err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk replace chunk update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk replace chunk commit: %w", err)
	}
	committed = true

	last := ""
	if maxSID != nil {
		last = *maxSID
	}
	return changedN, last, scannedN, nil
}

// buildBulkWhere строит WHERE для selector ∩ scope (без keyset-cursor) —
// используется count-ом (Matched/dry_run). Возвращает [ErrBulkEmptySelector],
// если selector не задаёт ни одного критерия (All=false и пусто остальное).
func buildBulkWhere(sel BulkSelector, scope BulkScope) (string, []any, error) {
	clauses, args := bulkSelectorClauses(sel)
	if len(clauses) == 0 && !sel.All {
		return "", nil, ErrBulkEmptySelector
	}
	clauses, args = appendScopeClause(clauses, args, scope)
	return joinWhere(clauses), args, nil
}

// buildBulkWhereWithCursor — то же, плюс keyset-предикат `sid > $cursor`
// (пустой cursor = первый чанк, без предиката). Не возвращает ошибку:
// selector уже провалидирован buildBulkWhere в CountBulkMatched до итерации.
func buildBulkWhereWithCursor(sel BulkSelector, scope BulkScope, cursor string) (string, []any) {
	clauses, args := bulkSelectorClauses(sel)
	clauses, args = appendScopeClause(clauses, args, scope)
	if cursor != "" {
		args = append(args, cursor)
		clauses = append(clauses, fmt.Sprintf("sid > $%d", len(args)))
	}
	return joinWhere(clauses), args
}

// bulkSelectorClauses переводит BulkSelector в SQL-clauses + args. All сам по
// себе clause не добавляет (он означает «без host-фильтра»).
//
// Coven и Incarnation формируют одинаковый SQL-предикат `$n = ANY(coven)`:
// имя incarnation — корневая Coven-метка по ADR-008. Семантически они различны
// (Coven — произвольная stable-метка, Incarnation — имя из реестра
// `incarnation`), потому разнесены на уровне API/audit; на SQL-уровне сливаются.
func bulkSelectorClauses(sel BulkSelector) ([]string, []any) {
	var (
		clauses []string
		args    []any
	)
	if len(sel.SIDs) > 0 {
		args = append(args, sel.SIDs)
		clauses = append(clauses, fmt.Sprintf("sid = ANY($%d)", len(args)))
	}
	if sel.Coven != "" {
		args = append(args, sel.Coven)
		clauses = append(clauses, fmt.Sprintf("$%d = ANY(coven)", len(args)))
	}
	if sel.Incarnation != "" {
		args = append(args, sel.Incarnation)
		clauses = append(clauses, fmt.Sprintf("$%d = ANY(coven)", len(args)))
	}
	if sel.Status != "" {
		args = append(args, string(sel.Status))
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	return clauses, args
}

// appendScopeClause добавляет scope-предикат (a): целевые хосты ⊆ scope
// оператора (`coven && ARRAY[scope...]`). Unrestricted — без ограничения.
// Пустой Covens при non-unrestricted даёт `coven && ARRAY[]::text[]` —
// заведомо false (оператор не вправе трогать ни один coven) — это корректно.
func appendScopeClause(clauses []string, args []any, scope BulkScope) ([]string, []any) {
	if scope.Unrestricted {
		return clauses, args
	}
	covens := scope.Covens
	if covens == nil {
		covens = []string{} // NULL && coven = NULL; пустой массив = детерминированный false.
	}
	args = append(args, covens)
	clauses = append(clauses, fmt.Sprintf("coven && $%d", len(args)))
	return clauses, args
}

func joinWhere(clauses []string) string {
	if len(clauses) == 0 {
		return ""
	}
	where := " WHERE " + clauses[0]
	for _, c := range clauses[1:] {
		where += " AND " + c
	}
	return where
}

func covenInScope(label string, scope []string) bool {
	for _, c := range scope {
		if c == label {
			return true
		}
	}
	return false
}

// ListFilter — фильтры для [SelectAll]. Пустые поля означают «не фильтровать».
type ListFilter struct {
	Status    Status
	Transport Transport
	Coven     string // ANY одной метки; пусто = без фильтра.
}

// ListScope — RBAC scope-граница видимости (`GET /v1/souls`, ADR-047 S3b),
// ОТДЕЛЬНАЯ от пользовательских [ListFilter]: filter — что оператор попросил
// показать (query-params), scope — что ему вообще ПОЛОЖЕНО видеть (из JWT,
// резолвится keeper/internal/soulpurview). Оба пересекаются AND-ом в WHERE
// (фильтр сужает внутри scope, не наоборот).
//
// Семантика — fail-closed (ADR-047): пустой Covens при !Unrestricted даёт
// `coven && ARRAY[]::text[]` = заведомо false (ни одного хоста), а НЕ весь флот.
// Симметрично [BulkScope] / [appendScopeClause]. Unrestricted=true снимает
// scope-фильтр (весь список).
type ListScope struct {
	Covens       []string
	Unrestricted bool
}

// ScopeEvalRow — полная строка карточки `souls` для Go-side scope-eval-а
// (ADR-047 S3b-2a keyset-режим). Несёт ВСЕ колонки [soulListItem] (как
// [SelectAll]) — keyset-карточка обязана быть формо-идентична offset-карточке,
// иначе presence-overlay не флипнёт status (он опускается на пустом снимке) и
// `GET /v1/souls` отдавал бы разную форму карточки по Purview оператора.
// union-фильтр `covenMatch OR regexMatch` использует SID + Coven; RegisteredAt
// — для composite-курсора. soulprint_facts сюда НЕ тянется — soulprint-измерение
// в этом срезе не вычисляется (S3b-2b).
type ScopeEvalRow struct {
	SID           string
	Transport     Transport
	Status        Status
	Coven         []string
	Traits        map[string]any
	RegisteredAt  time.Time
	LastSeenAt    *time.Time
	LastSeenByKID *string
	CreatedByAID  *string
	RequestedAt   *time.Time
	Note          string
}

// KeysetCursorBound — composite-граница keyset-окна `(registered_at, sid)` для
// [ListForScopeEval]. nil = первая страница (без нижней границы). Голый sid дал
// бы дыры на равных registered_at, поэтому граница composite.
type KeysetCursorBound struct {
	RegisteredAt time.Time
	SID          string
}

// scopeEvalSelectSQL — проекция keyset-окна (полная карточка, как [SelectAll]).
// WHERE/ORDER/LIMIT достраиваются [buildScopeEvalSQL] динамически: user-filter
// (status/transport/coven) комбинируется с keyset-предикатом через AND.
const scopeEvalSelectSQL = `SELECT sid, transport, status, coven, traits,
       registered_at, last_seen_at, last_seen_by_kid,
       created_by_aid, requested_at, note
FROM souls`

// scopeEvalMaxPageSize — верхняя граница ВНУТРЕННЕГО page-size keyset-eval-а
// (НЕ клиентский limit). Защита от чтения всего флота одной выборкой: handler
// читает внутренние страницы окном ~2000 и применяет Go-OR-постфильтр поверх,
// набирая клиентский limit (симметрично [bulkChunkSize]).
const scopeEvalMaxPageSize = 2000

// buildScopeEvalSQL собирает keyset-страницу: user-filter (status/transport/
// coven) как SQL WHERE, скомбинированный с keyset-предикатом `(registered_at,
// sid)`. Семантика видимости = (user-filter в SQL) AND (scope-union в Go-eval
// поверх) — фильтр сужает ВНУТРИ scope (ADR-047 S3b-2a, AND), а scope-union
// (covenMatch OR regexMatch) считается в Go, поэтому coven-scope-pushdown сюда
// НЕ добавляется (иначе сузил бы видимость ниже Purview).
//
// User-filter применяется в SQL ДО Go-eval, что сужает page-scan: меньше
// внутренних страниц проходит через keyset-добор (бонус к перфу). Курсор-
// инвариант сохраняется — `(registered_at, sid)`-окно идёт поверх отфильтрованного
// набора, ORDER BY совпадает с [SelectAll] (registered_at DESC, sid ASC).
//
// Порядок аргументов: сначала filter-clauses ($1..), затем keyset-границы
// (curAt, curSid) при наличии cursor, затем pageSize — последним.
func buildScopeEvalSQL(filter ListFilter, cursor *KeysetCursorBound, pageSize int) (string, []any) {
	clauses, args := listFilterClauses(filter)
	if cursor != nil {
		curAtPos := len(args) + 1
		curSidPos := len(args) + 2
		args = append(args, cursor.RegisteredAt.UTC(), cursor.SID)
		clauses = append(clauses, fmt.Sprintf(
			"(registered_at < $%d OR (registered_at = $%d AND sid > $%d))",
			curAtPos, curAtPos, curSidPos))
	}
	args = append(args, pageSize)
	sql := scopeEvalSelectSQL + joinWhere(clauses) +
		fmt.Sprintf(" ORDER BY registered_at DESC, sid ASC LIMIT $%d", len(args))
	return sql, args
}

// ListForScopeEval читает ОДНУ внутреннюю keyset-страницу `souls` (полная
// карточка, как [SelectAll]) для Go-side scope-eval-а (ADR-047 S3b-2a).
//
// scope-фильтр (`covenMatch OR regexMatch`) применяется в Go ПОВЕРХ возвращённых
// строк (наличие regex отключает coven-SQL-pushdown — иначе AND сузил бы
// видимость ниже Purview). А вот пользовательский `filter` (status/transport/
// coven из query-params) ПРИМЕНЯЕТСЯ ЗДЕСЬ как SQL WHERE: он сужает ВНУТРИ scope
// (AND), не расширяет — поэтому корректно pushdown-ить его в БД (он не зависит
// от scope-union и заодно режет page-scan). Итоговая видимость хоста ⟺
// (filter в SQL) AND (scope-union в Go).
//
// Проекция полная (а не sid/coven/registered_at), чтобы keyset-карточка несла
// status/transport/last_seen — иначе presence-overlay не флипнул бы status, и
// карточка отличалась бы от offset-режима. Стоимость лишних колонок ничтожна
// (страница ≤ 2000 строк).
//
// cursor=nil → первая страница; иначе строго ПОСЛЕ composite-границы
// `(registered_at, sid)`. pageSize клампится в [1, scopeEvalMaxPageSize].
func ListForScopeEval(ctx context.Context, db ExecQueryRower, filter ListFilter, cursor *KeysetCursorBound, pageSize int) ([]ScopeEvalRow, error) {
	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > scopeEvalMaxPageSize {
		pageSize = scopeEvalMaxPageSize
	}

	sql, args := buildScopeEvalSQL(filter, cursor, pageSize)
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("soul: scope-eval query: %w", err)
	}
	defer rows.Close()

	out := make([]ScopeEvalRow, 0, pageSize)
	for rows.Next() {
		var (
			r            ScopeEvalRow
			transportStr string
			statusStr    string
			traitsJSON   []byte
			note         *string
		)
		if err := rows.Scan(
			&r.SID, &transportStr, &statusStr, &r.Coven, &traitsJSON,
			&r.RegisteredAt, &r.LastSeenAt, &r.LastSeenByKID,
			&r.CreatedByAID, &r.RequestedAt, &note,
		); err != nil {
			return nil, fmt.Errorf("soul: scope-eval scan: %w", err)
		}
		r.Transport = Transport(transportStr)
		r.Status = Status(statusStr)
		// traits jsonb (ADR-060): '{}' (NOT NULL DEFAULT) → пустой map, не nil
		// (симметрично scanSoul).
		if len(traitsJSON) > 0 {
			if err := json.Unmarshal(traitsJSON, &r.Traits); err != nil {
				return nil, fmt.Errorf("soul: scope-eval unmarshal traits for %q: %w", r.SID, err)
			}
		}
		if note != nil {
			r.Note = *note
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("soul: scope-eval iter: %w", err)
	}
	return out, nil
}

// SelectAll возвращает страницу Soul-ов с применённым пользовательским фильтром
// (filter) ∩ RBAC scope-границей (scope) и общее количество.
//
// scope-предикат идёт в WHERE ОБОИХ запросов (COUNT и SELECT) — total
// когерентен выдаче (не считает хосты вне scope). Coven-pushdown полон для
// offset-пагинации: дрейфа total нет (в отличие от Go-постфильтра, который
// потребует keyset — S3b-2).
//
// Сортировка — `registered_at DESC, sid ASC` (поздние выше; tie-break по
// SID, иначе пагинация неустойчива при одинаковом таймстемпе — симметрично
// incarnation.SelectAll).
func SelectAll(ctx context.Context, db ExecQueryRower, filter ListFilter, scope ListScope, offset, limit int) ([]*Soul, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("soul: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("soul: limit must be >= 1, got %d", limit)
	}

	whereSQL, args := buildListWhere(filter, scope)

	countSQL := "SELECT COUNT(*) FROM souls" + whereSQL
	var total int
	if err := db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("soul: count: %w", err)
	}

	listSQL := `SELECT sid, transport, status, coven, traits,
       registered_at, last_seen_at, last_seen_by_kid,
       created_by_aid, requested_at, note
FROM souls` + whereSQL +
		fmt.Sprintf(" ORDER BY registered_at DESC, sid ASC OFFSET $%d LIMIT $%d", len(args)+1, len(args)+2)
	listArgs := append(append([]any{}, args...), offset, limit)

	rows, err := db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("soul: list query: %w", err)
	}
	defer rows.Close()

	var out []*Soul
	for rows.Next() {
		s, err := scanSoul(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("soul: list iter: %w", err)
	}
	return out, total, nil
}

// buildListWhere строит WHERE для пользовательского фильтра ∩ RBAC scope.
// scope-предикат (`coven && ARRAY[scope]`) переиспользует [appendScopeClause] —
// единый источник scope-intersection-семантики с bulk coven-assign (одна форма
// fail-closed для всего souls-слоя). joinWhere — общий с bulk-веткой.
func buildListWhere(f ListFilter, scope ListScope) (string, []any) {
	clauses, args := listFilterClauses(f)
	clauses, args = appendScopeClause(clauses, args, BulkScope(scope))
	return joinWhere(clauses), args
}

// listFilterClauses переводит пользовательский [ListFilter] (status/transport/
// coven) в SQL-clauses + позиционные args. Пустые поля = «не фильтровать».
// Единый источник filter-семантики: offset-путь ([buildListWhere]) и keyset-путь
// ([buildScopeEvalSQL]) применяют один и тот же фильтр одинаково — иначе keyset-
// режим молча игнорировал бы фильтр (ADR-047 S3b-2a fix).
func listFilterClauses(f ListFilter) ([]string, []any) {
	var (
		clauses []string
		args    []any
	)
	if f.Status != "" {
		args = append(args, string(f.Status))
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	if f.Transport != "" {
		args = append(args, string(f.Transport))
		clauses = append(clauses, fmt.Sprintf("transport = $%d", len(args)))
	}
	if f.Coven != "" {
		args = append(args, f.Coven)
		clauses = append(clauses, fmt.Sprintf("$%d = ANY(coven)", len(args)))
	}
	return clauses, args
}

// Stats — агрегированная сводка реестра `souls` в границах Purview-scope
// оператора (`GET /v1/souls/stats`, Souls Overview UI). Считается ОДНИМ
// round-trip-ом ([SelectStats]) поверх того же scope-предиката, что и
// [SelectAll]: агрегат не включает хосты вне видимости оператора (иначе цифры
// на дашборде расходились бы со scoped-списком).
//
//   - ByStatus / ByTransport / ByCoven — плотные карты «значение оси → число
//     хостов». Пустые оси (нет ни одного хоста) → пустые (не nil) карты.
//     Transport в модели — agent/ssh (НЕ pull/push): UI сам маппит на pull/push-
//     лейблы, storage-слой отдаёт доменные значения как есть.
//   - Total — всего видимых хостов (= сумма ByStatus).
//   - StaleCount — сколько видимых хостов «протухли» по `last_seen_at`
//     (< now()-staleThreshold). Порог — тот же `mark_disconnected.stale_after`
//     Reaper-а (reaper.ResolveMarkDisconnectedStale), чтобы цифра совпадала с
//     фактом перевода в disconnected. Хост без `last_seen_at` (только-что
//     pending, ни разу не подключался) в StaleCount НЕ попадает (NULL < X = NULL).
type Stats struct {
	ByStatus    map[Status]int
	ByTransport map[Transport]int
	ByCoven     map[string]int
	Total       int
	StaleCount  int
}

// statsAxisSQL — единый CTE-запрос агрегата: `scoped` фиксирует набор видимых
// хостов ОДИН раз (scope-WHERE подставляется в %s), далее оси считаются
// UNION ALL-ом с дискриминатором `axis`. Строка axis='stale' несёт единственный
// bucket=COUNT протухших (last_seen_at < now()-$stale); прочие оси группируются.
// Один плейсхолдер stale-порога — последний позиционный аргумент ($%d).
//
// unnest(coven) для by_coven: хост с N метками даёт N строк оси coven (сумма
// by_coven >= total — это ожидаемо, метки пересекаются). Хост без меток в
// by_coven не участвует (unnest пустого массива = 0 строк).
const statsAxisSQLTemplate = `
WITH scoped AS (
    SELECT status, transport, coven, last_seen_at
    FROM souls%s
)
SELECT 'status'    AS axis, status                     AS bucket, COUNT(*) AS n FROM scoped GROUP BY status
UNION ALL
SELECT 'transport' AS axis, transport                  AS bucket, COUNT(*) AS n FROM scoped GROUP BY transport
UNION ALL
SELECT 'coven'     AS axis, c                          AS bucket, COUNT(*) AS n FROM scoped, unnest(coven) AS c GROUP BY c
UNION ALL
SELECT 'stale'     AS axis, ''                         AS bucket, COUNT(*) AS n FROM scoped WHERE last_seen_at < now() - $%d::interval
`

// SelectStats считает агрегированную сводку реестра `souls` в границах RBAC
// scope-а оператора (`GET /v1/souls/stats`). Один round-trip: все оси
// (status/transport/coven) + stale-count объединены UNION ALL-ом поверх общего
// scope-CTE.
//
// scope-предикат — тот же [appendScopeClause], что у [SelectAll]/bulk: единый
// источник scope-intersection-семантики fail-closed (пустой scope без
// Unrestricted → `coven && ARRAY[]::text[]` = ноль хостов, а НЕ весь флот).
//
// staleThreshold — cutoff для StaleCount (хост «протух», если
// `last_seen_at < now()-staleThreshold`); передаётся из
// reaper.ResolveMarkDisconnectedStale, чтобы цифра совпала с disconnect-порогом.
// <= 0 отвергается (иначе cutoff в будущем/сейчас — бессмысленный агрегат;
// caller обязан передать реальный порог).
func SelectStats(ctx context.Context, db ExecQueryRower, scope ListScope, staleThreshold time.Duration) (Stats, error) {
	if staleThreshold <= 0 {
		return Stats{}, fmt.Errorf("soul: stale threshold must be > 0, got %v", staleThreshold)
	}
	stats := Stats{
		ByStatus:    map[Status]int{},
		ByTransport: map[Transport]int{},
		ByCoven:     map[string]int{},
	}

	clauses, args := appendScopeClause(nil, nil, BulkScope(scope))
	whereSQL := joinWhere(clauses)
	// stale-порог — последний позиционный аргумент; PG-interval из Go-duration
	// через строку "<seconds> seconds" (устойчиво к суб-секундным порогам).
	args = append(args, fmt.Sprintf("%d seconds", int64(staleThreshold.Seconds())))
	sql := fmt.Sprintf(statsAxisSQLTemplate, whereSQL, len(args))

	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return Stats{}, fmt.Errorf("soul: stats query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			axis   string
			bucket string
			n      int
		)
		if err := rows.Scan(&axis, &bucket, &n); err != nil {
			return Stats{}, fmt.Errorf("soul: stats scan: %w", err)
		}
		switch axis {
		case "status":
			stats.ByStatus[Status(bucket)] = n
			stats.Total += n
		case "transport":
			stats.ByTransport[Transport(bucket)] = n
		case "coven":
			stats.ByCoven[bucket] = n
		case "stale":
			stats.StaleCount = n
		}
	}
	if err := rows.Err(); err != nil {
		return Stats{}, fmt.Errorf("soul: stats iter: %w", err)
	}
	return stats, nil
}
