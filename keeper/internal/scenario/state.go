package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
)

const (
	selectIncarnationForUpdateSQL = `
SELECT name, service, service_version, state_schema_version,
       spec, state, status, status_details, created_by_aid,
       created_at, updated_at
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	updateIncarnationStatusSQL = `
UPDATE incarnation
SET status = $2, updated_at = NOW()
WHERE name = $1
`
)

// selectForUpdate читает incarnation под FOR UPDATE (защита от параллельных
// прогонов: пока строка залочена транзакцией lockRun, конкурентный Start
// заблокируется на этом SELECT-е до COMMIT-а).
func selectForUpdate(ctx context.Context, tx pgx.Tx, name string) (*incarnation.Incarnation, error) {
	row := tx.QueryRow(ctx, selectIncarnationForUpdateSQL, name)
	return scanForUpdate(row)
}

// scanForUpdate разбирает строку incarnation (тот же набор колонок, что
// incarnation.SelectByName, но через locking-SELECT внутри транзакции
// runner-а — incarnation.scanIncarnation не экспортирован, дублируем минимум).
func scanForUpdate(row pgx.Row) (*incarnation.Incarnation, error) {
	var (
		inc                incarnation.Incarnation
		statusStr          string
		specBytes          []byte
		stateBytes         []byte
		statusDetailsBytes []byte
		createdByAID       *string
	)
	err := row.Scan(
		&inc.Name, &inc.Service, &inc.ServiceVersion, &inc.StateSchemaVersion,
		&specBytes, &stateBytes, &statusStr, &statusDetailsBytes, &createdByAID,
		&inc.CreatedAt, &inc.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, incarnation.ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("scenario: scan incarnation: %w", err)
	}
	inc.Status = incarnation.Status(statusStr)
	inc.CreatedByAID = createdByAID
	if inc.Spec, err = unmarshalJSONB(specBytes); err != nil {
		return nil, fmt.Errorf("scenario: unmarshal spec: %w", err)
	}
	if inc.State, err = unmarshalJSONB(stateBytes); err != nil {
		return nil, fmt.Errorf("scenario: unmarshal state: %w", err)
	}
	if len(statusDetailsBytes) > 0 {
		if err := json.Unmarshal(statusDetailsBytes, &inc.StatusDetails); err != nil {
			return nil, fmt.Errorf("scenario: unmarshal status_details: %w", err)
		}
	}
	return &inc, nil
}

// updateStatus переводит incarnation в новый статус внутри транзакции (без
// записи state_history — это «промежуточный» переход в applying, не commit
// результата прогона).
func updateStatus(ctx context.Context, tx pgx.Tx, name string, status incarnation.Status) error {
	tag, err := tx.Exec(ctx, updateIncarnationStatusSQL, name, string(status))
	if err != nil {
		return fmt.Errorf("scenario: update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return incarnation.ErrIncarnationNotFound
	}
	return nil
}

// essenceInput собирает [essence.ResolveInput] для представительного хоста:
// OS-family из soulprint, Coven-метки хоста, override из incarnation.spec.essence.
func essenceInput(serviceDir string, inc *incarnation.Incarnation, host *topology.HostFacts) essence.ResolveInput {
	return essence.ResolveInput{
		ServiceDir:      serviceDir,
		OSFamily:        osFamilyOf(host),
		Covens:          host.Coven,
		IncarnationSpec: specEssence(inc),
	}
}

// osFamilyOf извлекает `soulprint.self.os.family` из last-reported фактов хоста.
// Отсутствие фактов / поля → "" (essence пропускает os-слой).
func osFamilyOf(host *topology.HostFacts) string {
	os, ok := host.Soulprint["os"].(map[string]any)
	if !ok {
		return ""
	}
	family, _ := os["family"].(string)
	return family
}

// specEssence возвращает incarnation.spec.essence (override оператора) или nil.
func specEssence(inc *incarnation.Incarnation) map[string]any {
	if inc.Spec == nil {
		return nil
	}
	e, _ := inc.Spec["essence"].(map[string]any)
	return e
}

// loadRegisterByHost читает накопленные register-данные прогона из
// `apply_task_register` (миграция 022) и собирает per-host register-map
// (sid → register-name → payload) для рендера `state_changes.sets` (слайс 2).
//
// Резолв task_idx → register-name делается тут (а не на handler-стороне):
// scenario-runner держит []RenderedTask с полем Register, handler в момент
// TaskEvent имени не знает (proto несёт только task_idx, ADR-012(d)). Этот
// инстанс — тот, что инициировал прогон, поэтому tasks доступны локально, а
// общая Postgres-таблица переживает cross-Keeper-роутинг TaskEvent-ов (ADR-002).
//
// Строки без register-имени (задача без register:, либо register для задачи,
// которой нет в tasks) — пропускаются. Пустой результат → пустая map.
func (r *Runner) loadRegisterByHost(ctx context.Context, applyID string, tasks []*render.RenderedTask) (map[string]map[string]any, error) {
	rows, err := applyrun.SelectTaskRegistersByApplyID(ctx, r.deps.DB, applyID)
	if err != nil {
		return nil, fmt.Errorf("scenario: load register-данных прогона: %w", err)
	}
	return buildRegisterByHost(rows, tasks), nil
}

// buildRegisterByHost — чистая свёртка register-строк прогона в per-host map
// (sid → register-name → payload) по mapping task_idx→register-name из tasks.
// Вынесена из loadRegisterByHost для unit-тестирования без PG.
//
// Если несколько задач на одном хосте имеют одно register-имя (программно
// возможно, но валидатор scenario такое не пропускает) — побеждает строка с
// бóльшим task_idx (SelectTaskRegistersByApplyID сортирует по task_idx ASC,
// поздняя перезаписывает раннюю).
//
// no_log (вариант B): задача с NoLog=true НЕ попадает в nameByIdx, поэтому её
// register-строка не аккумулируется в per-host map и не доходит до state-графа
// (orchestration.md §7). state_changes.sets, ссылающийся на register такой
// задачи, получит no-such-key — чувствительное значение из no_log-задачи не
// оседает в хранимом incarnation.state. Защита источника; маскинг на выходе
// GET — второй слой (defense-in-depth).
func buildRegisterByHost(rows []applyrun.TaskRegister, tasks []*render.RenderedTask) map[string]map[string]any {
	if len(rows) == 0 {
		return map[string]map[string]any{}
	}
	nameByIdx := make(map[int]string, len(tasks))
	for _, t := range tasks {
		if t.NoLog {
			continue
		}
		if t.Register != "" {
			nameByIdx[t.Index] = t.Register
		}
	}

	out := make(map[string]map[string]any)
	for i := range rows {
		name := nameByIdx[rows[i].TaskIdx]
		if name == "" {
			continue
		}
		hostReg := out[rows[i].SID]
		if hostReg == nil {
			hostReg = make(map[string]any)
			out[rows[i].SID] = hostReg
		}
		hostReg[name] = rows[i].RegisterData
	}
	return out
}

// ChangedTask — per-task итог «что изменилось» в одном scenario-run, форма
// записи терминального события `incarnation.run_completed` (T3, ADR-052 §k).
//
// Адрес задачи (Register ∪ ID) — стабильный идентификатор для подписки Tiding-а
// на «изменилась таска X» (T4): Register, если задача его захватывает, иначе ID
// (DSL-ядро `id:`, T1; задача не может иметь оба — config-валидатор T2 запрещает).
// Неадресуемая задача (нет ни register, ни id) попадает в массив с пустым
// адресом — «сколько и где изменилось» остаётся полным (см. buildChangedTasks).
//
// ChangedHosts/TotalHosts — числа УНИКАЛЬНЫХ sid (union по всем idx адреса), не
// суммы по idx: loop разворачивает одну исходную задачу в N RenderedTask со
// сквозными idx, но адрес у всех ОДИН — суммирование раздуло бы знаменатель
// (M хостов × K итераций). Метаданные (Name/Module/Register/ID) — из in-memory
// []RenderedTask, НЕ из journal payload (секрет-гигиена T3).
type ChangedTask struct {
	// Idx — репрезентативный task_idx адреса: минимальный idx среди итераций
	// этого адреса. Для loop-свёрнутого адреса (несколько idx) — наименьший из
	// них; точечная адресация идёт по Register/ID, не по Idx.
	Idx          int
	Name         string
	Register     string
	ID           string
	Module       string
	ChangedHosts int
	TotalHosts   int
}

// taskAddress возвращает адрес задачи (Register ∪ ID) и флаг адресуемости.
// Register приоритетнее ID (захват результата сильнее метки); T1/T2 гарантируют,
// что оба сразу не заданы, поэтому приоритет — лишь защита от программной ошибки.
func taskAddress(t *render.RenderedTask) (addr string, addressable bool) {
	if t.Register != "" {
		return t.Register, true
	}
	if t.ID != "" {
		return t.ID, true
	}
	return "", false
}

// buildChangedTasks — чистая свёртка per-task «что изменилось» по адресу
// (Register ∪ ID). Образец — [buildRegisterByHost] (idx→register-резолв из
// tasks). Без PG/audit-чтения: changedKeys — уже прочитанное множество
// (sid, task_idx) CHANGED-задач (auditpg.SelectChangedTaskKeys), plans —
// DispatchPlan-ы прогона (TargetSIDs после on:/where:).
//
// Группировка:
//   - адресуемая задача (register/id) → ключ = адрес; loop-итерации с одним
//     адресом схлопываются в одну ChangedTask (idx у всех разные, адрес один).
//   - неадресуемая задача (нет register и нет id) → ключ = её Index; каждая
//     остаётся отдельной записью (с чужими неадресуемыми не схлопывается; loop
//     неадресуемой задачи даёт несколько записей — адреса для свёртки нет).
//
// Счётчики — УНИКАЛЬНЫЕ sid (union, не сумма по idx):
//   - TotalHosts = |union TargetSIDs по всем idx адреса| (после on:/where:/
//     run_once: — НЕ весь roster);
//   - ChangedHosts = |union sid из changedKeys по всем idx адреса|.
//
// В результат попадают ТОЛЬКО адреса с ChangedHosts>0 (таска без изменений ни на
// одном хосте отсутствует). Порядок — первое появление адреса (= idx-порядок для
// не-loop задач; для loop-свёрнутого адреса — позиция его минимального idx, по
// keyOrder без сортировки). NoLog-задача в свёртку входит: changed_tasks несёт только counts +
// метаданные (name/register/id/module), payload-значений register/params нет —
// утечки секрета no_log-задачи здесь не происходит.
func buildChangedTasks(
	tasks []*render.RenderedTask,
	plans []render.DispatchPlan,
	changedKeys map[auditpg.ChangedTaskKey]struct{},
) []ChangedTask {
	if len(tasks) == 0 {
		return nil
	}

	// targetsByIdx: idx → TargetSIDs (после on:/where:). DispatchPlan.TaskIndex
	// ссылается на RenderedTask.Index.
	targetsByIdx := make(map[int][]string, len(plans))
	for i := range plans {
		targetsByIdx[plans[i].TaskIndex] = plans[i].TargetSIDs
	}

	// Накопитель агрегата по ключу группировки (адрес либо синтетика неадресуемой).
	type acc struct {
		repIdx      int // репрезентативный (минимальный) idx
		name        string
		register    string
		id          string
		module      string
		totalSIDs   map[string]struct{}
		changedSIDs map[string]struct{}
	}
	// keyOrder сохраняет порядок первого появления ключа (детерминизм до сортировки).
	groups := make(map[string]*acc)
	var keyOrder []string

	for _, t := range tasks {
		addr, addressable := taskAddress(t)
		// Ключ группировки: для адресуемой — "a:"+адрес (схлопывает loop-итерации);
		// для неадресуемой — "i:"+idx (каждая отдельной записью). Префикс разводит
		// пространства имён, чтобы id "5" и idx 5 не столкнулись.
		var key string
		if addressable {
			key = "a:" + addr
		} else {
			key = "i:" + fmt.Sprint(t.Index)
		}

		a := groups[key]
		if a == nil {
			a = &acc{
				repIdx:      t.Index,
				name:        t.Name,
				register:    t.Register,
				id:          t.ID,
				module:      t.Module,
				totalSIDs:   make(map[string]struct{}),
				changedSIDs: make(map[string]struct{}),
			}
			groups[key] = a
			keyOrder = append(keyOrder, key)
		} else if t.Index < a.repIdx {
			a.repIdx = t.Index
		}

		// union TargetSIDs этого idx в total.
		for _, sid := range targetsByIdx[t.Index] {
			a.totalSIDs[sid] = struct{}{}
		}
		// union CHANGED-sid этого idx в changed. Проверяем по адресным TargetSIDs:
		// changedKeys — множество (sid, idx); пробегаем target-sids idx-а и берём
		// те, что отметились CHANGED.
		for _, sid := range targetsByIdx[t.Index] {
			if _, ok := changedKeys[auditpg.ChangedTaskKey{SID: sid, TaskIdx: t.Index}]; ok {
				a.changedSIDs[sid] = struct{}{}
			}
		}
	}

	out := make([]ChangedTask, 0, len(keyOrder))
	for _, key := range keyOrder {
		a := groups[key]
		if len(a.changedSIDs) == 0 {
			continue // таска не изменилась ни на одном хосте — не в массиве
		}
		out = append(out, ChangedTask{
			Idx:          a.repIdx,
			Name:         a.name,
			Register:     a.register,
			ID:           a.id,
			Module:       a.module,
			ChangedHosts: len(a.changedSIDs),
			TotalHosts:   len(a.totalSIDs),
		})
	}
	return out
}

// mergeStateChanges накладывает отрендеренные `state_changes.sets` поверх
// stateBefore и возвращает новый state (orchestration.md §7.1).
//
// renderedSets — поле → уже вычисленное Keeper-side значение (CEL-render +
// last-wins cross-host-свёртка сделаны в render.Pipeline.RenderStateChanges).
// Здесь чистый merge: deep-copy stateBefore (commit-снапшот не должен держать
// ссылку на исходный map) + перезапись объявленных полей (last-wins на уровне
// поля). Пустой/nil renderedSets → state не меняется (stateAfter == copy of
// stateBefore).
//
// appends/modifies (per-host коллекции) — future-расширение полной грамматики;
// здесь не применяются (см. config.StateChanges).
func mergeStateChanges(stateBefore map[string]any, renderedSets map[string]any) map[string]any {
	out := deepCopyMap(stateBefore)
	for field, val := range renderedSets {
		out[field] = val
	}
	return out
}

// deepCopyMap делает глубокую копию map[string]any через JSON round-trip
// (значения — YAML/PG-данные: maps/slices/скаляры, JSON-safe). nil → пустой
// map (incarnation.state не бывает nil в commit-снапшоте).
func deepCopyMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		// state — JSON-safe (читался из JSONB); marshal не падает.
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// unmarshalJSONB парсит JSONB-bytes в map (симметрично incarnation-слою).
// Пустые байты / `null` → nil-map.
func unmarshalJSONB(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
