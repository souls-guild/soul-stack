package topology

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier — узкое подмножество pgxpool.Pool, нужное резолверу (только чтение).
// Симметрично [soul.ExecQueryRower] / [incarnation.ExecQueryRower]: unit-тесты
// ходят через fake без подъёма PG, production — через реальный pool/Conn/Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var (
	_ Querier = (*pgx.Conn)(nil)
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// SoulLeaseChecker — узкая поверхность batch-проверки «жив ли Redis SID-lease»
// (живой EventStream), нужная presence-фазе резолвера (Variant A, ADR-006(a)).
// Сужение до одного метода изолирует topology-пакет от полного keeperredis.Client
// и допускает fake в unit-тестах. Реальная реализация — обёртка над
// [keeperredis.SoulsStreamAlive], собранная в cmd/keeper (см. daemon.setupScenarioDeps).
//
// Возвращает множество SID-ов с живым lease (presence=online). nil-checker
// (unit-тесты / single-instance dev без Redis) → резолвер деградирует на
// SQL-presence (status='connected'), симметрично reaper-у.
type SoulLeaseChecker interface {
	SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error)
}

// Resolver резолвит roster хостов incarnation и их last-reported soulprint.
//
// pool — read-only доступ к Postgres (`souls` + `incarnation`). lease —
// Redis-проверка живого SID-lease (presence-источник, ADR-006(a)); nil →
// SQL-presence fallback. logger — для warn-а об устаревшем soulprint (ADR-018,
// не блокирует прогон) и о fail-safe-деградации presence на Redis-сбое.
type Resolver struct {
	pool   Querier
	lease  SoulLeaseChecker
	logger *slog.Logger
}

// NewResolver конструирует Resolver. pool обязателен; lease опционален (nil →
// SQL-presence fallback, см. [Resolver]); logger допускает nil (warn-ы тогда
// подавляются).
func NewResolver(pool *pgxpool.Pool, lease SoulLeaseChecker, logger *slog.Logger) *Resolver {
	return &Resolver{pool: pool, lease: lease, logger: logger}
}

// rosterSQL — фаза 1 (SQL): кандидаты на таргетинг — souls, у которых
// `incarnation.name` присутствует в `coven[]` (ADR-008: корневая Coven-метка) и
// чей status НЕ terminal/онбординг.
//
// Presence (online/offline) здесь НЕ фильтруется: авторитет «Soul online» —
// живой Redis SID-lease, его проверяет фаза 2 ([Resolver.filterAlive]). Status
// в `souls` несёт только lifecycle-снимок; кандидаты отсекаются лишь по terminal
// (`revoked`/`expired`/`destroyed`) и онбординг (`pending`) — их таргетить нельзя
// независимо от lease. `connected`/`disconnected` (legacy presence-снимок для
// Operator API) в фильтр НЕ входят — presence решает lease.
//
// ORDER BY sid — детерминированный порядок (scenario/orchestration.md §:
// лексикографически по SID; иначе разрушительные операции невоспроизводимы).
const rosterSQL = `
SELECT sid, coven, status,
       soulprint_facts, soulprint_collected_at, soulprint_received_at
FROM souls
WHERE $1 = ANY(coven)
  AND status NOT IN ('pending', 'revoked', 'expired', 'destroyed')
ORDER BY sid ASC
`

// incarnationSpecSQL читает spec одной incarnation для извлечения declared-ролей
// (`spec.hosts[].role`). Cross-incarnation isolation: ровно одна строка по PK.
const incarnationSpecSQL = `
SELECT spec
FROM incarnation
WHERE name = $1
`

// choirVoicesSQL читает Choir-членства всех хостов одной incarnation одним
// запросом (ADR-044, S-T4/S-T6): SID → имена Choir-ов, где он Voice, + role
// Voice-а в каждом Choir-е. Cross-incarnation isolation — фильтр по
// `incarnation_name` (PK включает его, ADR-044 пункт 3). Один round-trip на
// roster (симметрия с loadDeclaredRoles, без N+1); join по
// `incarnation_choir_voices_sid_idx` (060_create_choirs.up.sql). ORDER BY choir_name —
// детерминированный порядок имён внутри `choirs[]` каждого хоста и
// детерминированный выбор role при multi-choir-конфликте (ADR-044 п.2:
// поглощение declared-роли Choir-ом, см. loadChoirMemberships).
const choirVoicesSQL = `
SELECT sid, choir_name, role
FROM incarnation_choir_voices
WHERE incarnation_name = $1
ORDER BY sid ASC, choir_name ASC
`

// LoadIncarnationHosts резолвит хосты прогона scenario для incarnation
// `incarnationName`: online-souls с этой Coven-меткой + last-reported
// soulprint + declared-роль из `incarnation.spec.hosts[].role`.
//
// Двухфазно (ADR-006(a)):
//   - Фаза 1 (SQL, [rosterSQL]): кандидаты по Coven-членству + не-terminal/
//     не-онбординг status. Presence здесь НЕ решается.
//   - Фаза 2 (Redis, [Resolver.filterAlive]): отсев кандидатов без живого
//     SID-lease (presence = online ⇔ lease жив). nil-lease (unit / single-
//     instance dev) → fallback на SQL-presence (status='connected').
//
// Семантика:
//   - Несуществующая incarnation / нет online-хостов → пустой slice, НЕ
//     ошибка (PM-decision #3).
//   - Cross-incarnation isolation: читаются только souls с Coven-меткой
//     `incarnationName` и spec ровно этой incarnation (ADR-008, PM-decision #4).
//   - Устаревший soulprint (`received_at < now - 10m`) → warn в логгер,
//     прогон не блокируется (ADR-018, PM-decision #2).
func (r *Resolver) LoadIncarnationHosts(ctx context.Context, incarnationName string) ([]*HostFacts, error) {
	specRoles, err := r.loadDeclaredRoles(ctx, incarnationName)
	if err != nil {
		return nil, err
	}
	choirs, choirRoles, err := r.loadChoirMemberships(ctx, incarnationName)
	if err != nil {
		return nil, err
	}

	rows, err := r.pool.Query(ctx, rosterSQL, incarnationName)
	if err != nil {
		return nil, fmt.Errorf("topology: roster query: %w", err)
	}
	defer rows.Close()

	var candidates []*HostFacts
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		// Precedence role (ADR-044 п.2): Choir поглощает declared-роль.
		// voice.role (из incarnation_choir_voices) > spec.hosts[].role.
		// spec.hosts[].role остаётся fallback-ом для хостов БЕЗ Voice (и для
		// bootstrap-create, где Choir-членств ещё нет, wire-совместимость).
		if voiceRole, ok := choirRoles[h.SID]; ok {
			h.Role = voiceRole
		} else {
			h.Role = specRoles[h.SID]
		}
		h.Choirs = choirs[h.SID]
		candidates = append(candidates, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("topology: roster iter: %w", err)
	}

	hosts, err := r.filterAlive(ctx, candidates)
	if err != nil {
		return nil, err
	}

	warnStale(ctx, r.logger, hosts, time.Now())
	return hosts, nil
}

// filterAlive — фаза 2: presence-фильтр кандидатов по живому Redis SID-lease
// (ADR-006(a), Variant A). Online ⇔ lease-ключ `soul:<sid>:lock` существует.
//
// lease==nil (unit-тесты / single-instance dev без Redis) → fallback на
// SQL-presence: оставляем только status='connected' кандидатов (legacy-снимок
// в PG в single-instance режиме когерентен с фактом стрима по построению).
// Симметрично reaper-у (`mark_disconnected`, lease==nil → чисто-SQL).
//
// Ошибка Redis-проверки → fail-safe: чтобы сетевой сбой Redis не «погасил»
// весь incarnation (no_hosts → error_locked), деградируем на тот же
// SQL-presence fallback (status='connected') с warn-ом, а не возвращаем ошибку
// прогону. Прогон таргетит последний известный снимок до восстановления Redis.
func (r *Resolver) filterAlive(ctx context.Context, candidates []*HostFacts) ([]*HostFacts, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	if r.lease == nil {
		return filterConnectedSnapshot(candidates), nil
	}

	sids := make([]string, len(candidates))
	for i, h := range candidates {
		sids[i] = h.SID
	}
	alive, err := r.lease.SoulsStreamAlive(ctx, sids)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("topology: lease presence check failed — fallback to SQL snapshot (fail-safe)",
				slog.Any("error", err))
		}
		return filterConnectedSnapshot(candidates), nil
	}

	out := make([]*HostFacts, 0, len(candidates))
	for _, h := range candidates {
		if _, ok := alive[h.SID]; ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// filterConnectedSnapshot — SQL-presence fallback: оставляет кандидатов с
// legacy-снимком status='connected' в PG. Используется при lease==nil либо на
// Redis-сбое (fail-safe). Status кандидата читается из scan-а ([HostFacts.Status]).
func filterConnectedSnapshot(candidates []*HostFacts) []*HostFacts {
	out := make([]*HostFacts, 0, len(candidates))
	for _, h := range candidates {
		if h.Status == "connected" {
			out = append(out, h)
		}
	}
	return out
}

// loadDeclaredRoles читает `incarnation.spec.hosts[].role` и строит map
// SID → declared-роль. Несуществующая incarnation → пустой map (роли всех
// хостов будут "" — допустимо, ADR-008: declared-роль может быть null для
// хостов вне declared-spec).
func (r *Resolver) loadDeclaredRoles(ctx context.Context, incarnationName string) (map[string]string, error) {
	var specJSON []byte
	err := r.pool.QueryRow(ctx, incarnationSpecSQL, incarnationName).Scan(&specJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("topology: incarnation spec query: %w", err)
	}
	return parseDeclaredRoles(specJSON), nil
}

// loadChoirMemberships читает `incarnation_choir_voices` и строит две map-ы:
//   - choirs: SID → имена Choir-ов, где этот SID — Voice (ADR-044, S-T4);
//   - roles:  SID → role Voice-а (ADR-044, S-T6/п.2: Choir поглощает declared-
//     роль, role хоста теперь приходит из Voice, а не из spec.hosts[].role).
//
// Один запрос на весь roster (симметрия с loadDeclaredRoles, без N+1); каждый
// SID может присутствовать в нескольких Choir-ах → slice имён. Хосты без
// Voice-ов в обеих map-ах отсутствуют (Choirs останется nil, role — fallback
// на spec в LoadIncarnationHosts).
//
// Multi-choir-конфликт role (зафиксировано ADR-044 amendment): HostFacts.Role —
// скаляр, но SID может быть Voice-ом в нескольких Choir-ах одной инкарнации с
// разными непустыми role. Детерминированное правило — берём role из ПЕРВОГО по
// сортировке choir_name Choir-а С НЕПУСТОЙ role (SQL уже ORDER BY ... choir_name
// ASC, Choir-ы с пустой/NULL role пропускаются, поэтому первый встреченный
// непустой role и есть искомый) + WARN-лог о конфликте. Если role пусты во всех
// Choir-ах — SID в map roles не попадает → fallback на spec.
//
// Cross-incarnation isolation — фильтр choirVoicesSQL по `incarnation_name`.
// Порядок имён внутри slice choirs детерминирован (ORDER BY choir_name в SQL).
func (r *Resolver) loadChoirMemberships(ctx context.Context, incarnationName string) (choirs map[string][]string, roles map[string]string, err error) {
	rows, err := r.pool.Query(ctx, choirVoicesSQL, incarnationName)
	if err != nil {
		return nil, nil, fmt.Errorf("topology: choir voices query: %w", err)
	}
	defer rows.Close()

	choirs = map[string][]string{}
	roles = map[string]string{}
	// roleChoir[sid] — имя Choir-а, из которого взята role (для WARN-а о конфликте).
	roleChoir := map[string]string{}
	for rows.Next() {
		// role nullable (060_create_choirs.up.sql — TEXT без NOT NULL): AddVoice пишет SQL
		// NULL при опущенной роли (ADR-044 п.2/п.4 — role опциональна). Сканим в
		// *string (паттерн crud.go scanVoice / scanHost для nullable), иначе pgx
		// падает «cannot scan NULL into *string» и валит весь roster. nil/пустой
		// role → нет роли → fallback на spec.hosts[].role в LoadIncarnationHosts.
		var sid, choirName string
		var role *string
		if err := rows.Scan(&sid, &choirName, &role); err != nil {
			return nil, nil, fmt.Errorf("topology: scan choir voice: %w", err)
		}
		choirs[sid] = append(choirs[sid], choirName)
		if role == nil || *role == "" {
			continue
		}
		if existing, ok := roles[sid]; !ok {
			roles[sid] = *role
			roleChoir[sid] = choirName
		} else if existing != *role && r.logger != nil {
			r.logger.Warn("topology: multi-choir role conflict — берём первый Choir по сорт. имени",
				slog.String("sid", sid),
				slog.String("resolved_choir", roleChoir[sid]),
				slog.String("resolved_role", existing),
				slog.String("conflicting_choir", choirName),
				slog.String("conflicting_role", *role))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("topology: choir voices iter: %w", err)
	}
	return choirs, roles, nil
}

// parseDeclaredRoles извлекает SID → role из freeform-spec incarnation.
// Ожидаемая форма: `spec.hosts` — список объектов с `sid` и `role`
// (scenario/orchestration.md §4.1). spec freeform (jsonb): любое отклонение
// формы — пропуск элемента, НЕ ошибка (резолвер read-only, не валидатор spec;
// валидация формы spec — на слое создания incarnation).
func parseDeclaredRoles(specJSON []byte) map[string]string {
	roles := map[string]string{}
	if len(specJSON) == 0 {
		return roles
	}

	var spec struct {
		Hosts []struct {
			SID  string `json:"sid"`
			Role string `json:"role"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return roles
	}
	for _, h := range spec.Hosts {
		if h.SID != "" && h.Role != "" {
			roles[h.SID] = h.Role
		}
	}
	return roles
}

// scanHost разбирает одну строку roster-а. soulprint_facts (JSONB) → map;
// NULL-колонка (Soul ещё не присылал SoulprintReport) → nil-map.
func scanHost(row pgx.Row) (*HostFacts, error) {
	var (
		h           HostFacts
		factsJSON   []byte
		collectedAt *time.Time
		receivedAt  *time.Time
	)
	if err := row.Scan(&h.SID, &h.Coven, &h.Status, &factsJSON, &collectedAt, &receivedAt); err != nil {
		return nil, fmt.Errorf("topology: scan host: %w", err)
	}

	if len(factsJSON) > 0 {
		if err := json.Unmarshal(factsJSON, &h.Soulprint); err != nil {
			return nil, fmt.Errorf("topology: unmarshal soulprint for %q: %w", h.SID, err)
		}
	}
	if collectedAt != nil {
		h.CollectedAt = collectedAt.UTC()
	}
	if receivedAt != nil {
		h.ReceivedAt = receivedAt.UTC()
	}
	return &h, nil
}

// inventorySQL — read-only выборка souls по списку SID-ов для push-прогона
// (Variant C, [keeper/internal/pushorch]). По форме поля совпадает с [rosterSQL]
// (один scanHost обрабатывает оба пути): SID, coven, status, soulprint-факты
// + timestamps.
//
// Отличие от rosterSQL — фильтр НЕ по Coven-членству, а по точному списку SID;
// incarnation-spec-фазы здесь нет (push-прогон не привязан к incarnation,
// declared-роли неприменимы — Role="" для всех). Status-фильтр такой же:
// исключаем terminal (`revoked`/`expired`/`destroyed`) и онбординг (`pending`) —
// SshDispatcher не имеет смысла на «не-готовых» хостах независимо от lease.
//
// ORDER BY sid — детерминизм per-host dispatch-а.
const inventorySQL = `
SELECT sid, coven, status,
       soulprint_facts, soulprint_collected_at, soulprint_received_at
FROM souls
WHERE sid = ANY($1)
  AND status NOT IN ('pending', 'revoked', 'expired', 'destroyed')
ORDER BY sid ASC
`

// LoadByInventory резолвит хосты push-прогона по точному списку SID-ов
// (`POST /v1/push/apply::inventory`, Variant C). Симметрично
// [Resolver.LoadIncarnationHosts], но:
//
//   - входной фильтр — список SID, а не Coven-метка;
//   - declared-роли НЕТ (Role="" для всех — push-хосты не привязаны к
//     incarnation.spec);
//   - вторая фаза (filterAlive) применяется так же: lease-presence для
//     fail-safe-фильтра «живых» хостов; lease==nil → SQL-snapshot fallback.
//
// Семантика:
//   - не-найденный SID / hard-terminal status / онбординг → молча отсутствует
//     в результате (caller получает len(out) < len(sids));
//   - пустой sids → пустой результат, не ошибка;
//   - устаревший soulprint (`received_at < now - 10m`) → warn в логгер,
//     прогон не блокируется (паритет с LoadIncarnationHosts).
//
// FK на operators / cross-incarnation isolation НЕ применимы: push-инвентарь —
// плоский список, без incarnation-границы.
func (r *Resolver) LoadByInventory(ctx context.Context, sids []string) ([]*HostFacts, error) {
	if len(sids) == 0 {
		return nil, nil
	}

	rows, err := r.pool.Query(ctx, inventorySQL, sids)
	if err != nil {
		return nil, fmt.Errorf("topology: inventory query: %w", err)
	}
	defer rows.Close()

	var candidates []*HostFacts
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		// Role="" — push не имеет declared-роли (см. doc).
		candidates = append(candidates, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("topology: inventory iter: %w", err)
	}

	hosts, err := r.filterAlive(ctx, candidates)
	if err != nil {
		return nil, err
	}

	warnStale(ctx, r.logger, hosts, time.Now())
	return hosts, nil
}

// FilterByCovens оставляет хосты, у которых присутствуют ВСЕ requiredCovens —
// AND-пересечение по меткам (scenario/orchestration.md §3; [ADR-040] amendment
// 2026-05-27 «Multi-label семантика внутри одного списка»). Хост попадает в
// результат, только если каждая метка из requiredCovens есть в `h.Coven`.
// Пустой requiredCovens → исходный slice без изменений (нет фильтра = весь
// incarnation, ADR-009).
//
// Security-инвариант: AND-семантика fail-closed — перечисление меток не
// расширяет scope. Для OR-кейса оператор использует `target.where: CEL` с явным
// predicate.
//
// Чистая функция над уже загруженным roster-ом — никаких round-trip-ов в PG.
func (r *Resolver) FilterByCovens(hosts []*HostFacts, requiredCovens []string) []*HostFacts {
	if len(requiredCovens) == 0 {
		return hosts
	}

	out := make([]*HostFacts, 0, len(hosts))
	for _, h := range hosts {
		if hostHasAllCovens(h, requiredCovens) {
			out = append(out, h)
		}
	}
	return out
}

// hostHasAllCovens — AND-предикат: все метки required присутствуют в h.Coven.
// Линейное сканирование оптимально на типичных размерах (Coven у хоста единицы
// меток, required — единицы-десятки): map-аллокация дороже двойного цикла.
func hostHasAllCovens(h *HostFacts, required []string) bool {
	for _, want := range required {
		found := false
		for _, c := range h.Coven {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
