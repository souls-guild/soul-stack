package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// UpdateHostsMode — операция над declared `spec.hosts[]` для
// [UpdateHosts] (PATCH /v1/incarnations/{name}/hosts).
//
//   - ModeReplace — полная замена списка переданным набором.
//   - ModeAppend  — добавить переданные hosts; при совпадении SID обновляется
//     role существующей записи.
//   - ModeRemove  — удалить записи с указанными SID-ами (role в payload
//     игнорируется).
type UpdateHostsMode string

const (
	ModeReplace UpdateHostsMode = "replace"
	ModeAppend  UpdateHostsMode = "append"
	ModeRemove  UpdateHostsMode = "remove"
)

// ValidHostsMode — closed enum проверка mode (handler-сторона мапит 422).
func ValidHostsMode(m UpdateHostsMode) bool {
	switch m {
	case ModeReplace, ModeAppend, ModeRemove:
		return true
	}
	return false
}

// SpecHost — typed-запись declared `spec.hosts[]`. Хранится в jsonb-поле
// `incarnation.spec.hosts`, форма зеркалит парсер в
// [topology.parseDeclaredRoles] (`{sid, role}`). Role — опциональна
// (`null`/пустая строка допустимы — ADR-008: declared-роль может быть null
// для хостов вне declared-spec).
type SpecHost struct {
	SID  string `json:"sid"`
	Role string `json:"role,omitempty"`
}

// ErrIncarnationNotEditable — статус incarnation не допускает правки spec
// (destroying / destroy_failed). Handler-сторона маппит в 409
// incarnation-locked (parity Run/Upgrade gate-status).
var ErrIncarnationNotEditable = errors.New("incarnation: status does not allow spec edits")

// ErrUnknownSouls — переданные SID-ы отсутствуют в реестре `souls`. Handler
// маппит в 422; ошибочные SID-ы возвращаются в .Missing для message.
type ErrUnknownSouls struct {
	Missing []string
}

func (e *ErrUnknownSouls) Error() string {
	return fmt.Sprintf("incarnation: %d unknown SID(s) in souls registry: %v", len(e.Missing), e.Missing)
}

// hostsEditableStatuses — статусы, из которых разрешена правка spec.hosts[].
// destroying / destroy_failed исключены (incarnation сносится, правки spec
// бессмысленны). applying — допустим: спек — declared-вход следующего прогона,
// не текущего; concurrent edit и run сериализуются FOR UPDATE на уровне строки
// (применение spec.hosts на следующем resolve-е).
func hostsEditableStatus(s Status) bool {
	switch s {
	case StatusDestroying, StatusDestroyFailed:
		return false
	}
	return true
}

// UpdateHostsInput — параметры [UpdateHosts]. Hosts — payload (валидируется
// caller-ом на формат SID/role и mode-семантику); ChangedByAID — Архонт-
// инициатор (опционален; nil → audit-нейтрально).
type UpdateHostsInput struct {
	Name         string
	Hosts        []SpecHost
	Mode         UpdateHostsMode
	ChangedByAID *string
}

// UpdateHostsResult — итог [UpdateHosts]: снимки old/new для audit-payload
// + полная обновлённая запись incarnation для response.
type UpdateHostsResult struct {
	OldHosts    []SpecHost
	NewHosts    []SpecHost
	Incarnation *Incarnation
}

// UpdateHosts атомарно правит declared `spec.hosts[]` incarnation
// (ADR-008, UI Hosts editing). Тот же транзакционный паттерн, что
// [Unlock] / [Destroy]: одна tx SELECT … FOR UPDATE → guard статуса →
// валидация SID-ов через souls → merge by mode → UPDATE spec/updated_at →
// commit.
//
// Validation:
//   - SID существует в `souls` (валидируется единым batch-SELECT, не
//     per-host round-trip): неизвестные SID-ы → [ErrUnknownSouls] (422).
//     Для mode=remove проверка тоже выполняется (защищает от тихого no-op
//     на опечатке SID). При пустом hosts (legitimate для replace=пустой
//     список) проверка skip-ается.
//   - Mode — closed enum (caller-сторона должна вызвать [ValidHostsMode]).
//
// Merge:
//   - replace — `spec.hosts` ← payload (включая пустой массив = очистить);
//   - append  — payload merge-ится в existing; матчинг по SID, новая role
//     перекрывает старую (insert-or-update);
//   - remove  — payload-SID-ы вычитаются из existing (role в payload не
//     учитывается).
//
// Возврат:
//   - [ErrIncarnationNotFound]   — name не существует (404).
//   - [ErrIncarnationNotEditable] — статус destroying / destroy_failed (409).
//   - [ErrUnknownSouls]          — переданные SID-ы не в `souls` (422).
//
// Audit (`incarnation.hosts_updated`) пишется handler-ом (нужен AID + source);
// сюда передаётся ChangedByAID для будущего расширения (state_history-row
// для hosts-edits не пишется — это spec-правка, не state-переход, ADR-009).
func UpdateHosts(ctx context.Context, pool TxBeginner, in UpdateHostsInput) (*UpdateHostsResult, error) {
	if !ValidName(in.Name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", in.Name)
	}
	if !ValidHostsMode(in.Mode) {
		return nil, fmt.Errorf("incarnation: invalid hosts mode %q", in.Mode)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin update-hosts tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// SELECT FOR UPDATE — сериализуем с конкурентным Unlock / Upgrade / Destroy /
	// scenario-runner (все они лочат ту же строку).
	const selectForUpdateSQL = `
SELECT name, service, service_version, state_schema_version,
       spec, state, status, status_details, created_by_aid,
       created_at, updated_at, covens, traits,
       last_drift_check_at, last_drift_summary, created_scenario,
       applying_apply_id
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	inc, err := scanIncarnation(tx.QueryRow(ctx, selectForUpdateSQL, in.Name))
	if err != nil {
		return nil, err
	}

	if !hostsEditableStatus(inc.Status) {
		return nil, ErrIncarnationNotEditable
	}

	// Snapshot существующих spec.hosts[] для merge + audit-payload.
	oldHosts := readSpecHosts(inc.Spec)

	// Validate SID-ы payload-а в souls (replace+append+remove — все требуют, что
	// SID существует, чтобы не было silent no-op на опечатке).
	if len(in.Hosts) > 0 {
		if err := validateSoulsExist(ctx, tx, in.Hosts); err != nil {
			return nil, err
		}
	}

	newHosts := mergeHosts(oldHosts, in.Hosts, in.Mode)

	// Merge только поля `hosts`: остальные ключи spec сохраняются. nil-spec →
	// инициализируем (NOT NULL DEFAULT '{}' гарантирует non-null в БД, но Spec в
	// scanIncarnation может быть nil-map при unmarshal-е `{}`).
	specOut := inc.Spec
	if specOut == nil {
		specOut = map[string]any{}
	}
	if len(newHosts) == 0 {
		// Пустой массив сохраняем явно (replace со списком []) — иначе resolver
		// не отличит «оператор очистил список» от «hosts вообще не было».
		specOut["hosts"] = []any{}
	} else {
		out := make([]any, 0, len(newHosts))
		for _, h := range newHosts {
			obj := map[string]any{"sid": h.SID}
			if h.Role != "" {
				obj["role"] = h.Role
			}
			out = append(out, obj)
		}
		specOut["hosts"] = out
	}

	specBytes, err := json.Marshal(specOut)
	if err != nil {
		return nil, fmt.Errorf("incarnation: marshal updated spec: %w", err)
	}

	const updateSQL = `
UPDATE incarnation
SET spec       = $2,
    updated_at = NOW()
WHERE name = $1
RETURNING updated_at
`
	if err := tx.QueryRow(ctx, updateSQL, in.Name, specBytes).Scan(&inc.UpdatedAt); err != nil {
		return nil, fmt.Errorf("incarnation: update hosts: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit update-hosts tx: %w", err)
	}

	inc.Spec = specOut
	return &UpdateHostsResult{
		OldHosts:    oldHosts,
		NewHosts:    newHosts,
		Incarnation: inc,
	}, nil
}

// readSpecHosts извлекает SpecHost-список из freeform jsonb-spec. Симметрично
// [topology.parseDeclaredRoles] (тот возвращает map SID→role, тут — упорядоченный
// список). Любое отклонение формы — пропуск элемента, НЕ ошибка (spec freeform).
func readSpecHosts(spec map[string]any) []SpecHost {
	if spec == nil {
		return nil
	}
	raw, ok := spec["hosts"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]SpecHost, 0, len(arr))
	for _, el := range arr {
		obj, ok := el.(map[string]any)
		if !ok {
			continue
		}
		sid, _ := obj["sid"].(string)
		if sid == "" {
			continue
		}
		role, _ := obj["role"].(string)
		out = append(out, SpecHost{SID: sid, Role: role})
	}
	return out
}

// mergeHosts применяет mode к существующему списку. Поддерживает порядок
// existing → новые добавляются в конец (append-семантика стабильна для UI);
// remove сохраняет порядок оставшихся; replace полностью переписывает.
func mergeHosts(existing, payload []SpecHost, mode UpdateHostsMode) []SpecHost {
	switch mode {
	case ModeReplace:
		// Копия payload, чтобы caller не зашарил slice с tx.
		out := make([]SpecHost, len(payload))
		copy(out, payload)
		return out

	case ModeAppend:
		// Index existing по SID для O(1) lookup; updates перекрывают role,
		// новые SID-ы добавляются в конец сохранением порядка payload.
		idx := make(map[string]int, len(existing))
		for i, h := range existing {
			idx[h.SID] = i
		}
		out := make([]SpecHost, len(existing))
		copy(out, existing)
		for _, h := range payload {
			if i, ok := idx[h.SID]; ok {
				out[i].Role = h.Role
				continue
			}
			idx[h.SID] = len(out)
			out = append(out, h)
		}
		return out

	case ModeRemove:
		drop := make(map[string]struct{}, len(payload))
		for _, h := range payload {
			drop[h.SID] = struct{}{}
		}
		out := make([]SpecHost, 0, len(existing))
		for _, h := range existing {
			if _, rm := drop[h.SID]; rm {
				continue
			}
			out = append(out, h)
		}
		return out
	}
	// ValidHostsMode уже отсёк unknown; unreachable.
	return existing
}

// validateSoulsExist проверяет, что все SID-ы есть в реестре `souls`.
// Один batch-SELECT (`= ANY($1)`), не per-host round-trip. Пустой in → no-op.
// Дубликаты в payload не страшны (PG-IN устойчив); порядок Missing совпадает
// с порядком первого вхождения в payload (стабильно для тестов).
func validateSoulsExist(ctx context.Context, db ExecQueryRower, payload []SpecHost) error {
	if len(payload) == 0 {
		return nil
	}
	// Dedup + сохранение порядка первого вхождения для Missing.
	seen := make(map[string]struct{}, len(payload))
	sids := make([]string, 0, len(payload))
	for _, h := range payload {
		if _, ok := seen[h.SID]; ok {
			continue
		}
		seen[h.SID] = struct{}{}
		sids = append(sids, h.SID)
	}

	const sql = `SELECT sid FROM souls WHERE sid = ANY($1)`
	rows, err := db.Query(ctx, sql, sids)
	if err != nil {
		return fmt.Errorf("incarnation: souls existence query: %w", err)
	}
	defer rows.Close()

	found := make(map[string]struct{}, len(sids))
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return fmt.Errorf("incarnation: souls existence scan: %w", err)
		}
		found[sid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("incarnation: souls existence iter: %w", err)
	}

	var missing []string
	for _, sid := range sids {
		if _, ok := found[sid]; !ok {
			missing = append(missing, sid)
		}
	}
	if len(missing) > 0 {
		return &ErrUnknownSouls{Missing: missing}
	}
	return nil
}
