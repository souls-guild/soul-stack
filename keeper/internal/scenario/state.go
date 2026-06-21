package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
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
	// lockApplyingWithEpochSQL переводит incarnation в applying И ОДНИМ UPDATE
	// записывает epoch applying-флага (ADR-027 amend (m-S1)): apply_id / attempt /
	// KID-владелец / момент взятия lock. Атомарность критична: epoch и status
	// меняются в одной строке одной tx — окна «applying без epoch» (которое
	// reconcile_orphan_applying приняло бы за legacy-NULL и НЕ реклеймнуло бы, а
	// при крахе владельца lock завис бы навсегда) не возникает.
	lockApplyingWithEpochSQL = `
UPDATE incarnation
SET status            = 'applying',
    applying_apply_id = $2,
    applying_attempt  = $3,
    applying_by_kid   = $4,
    applying_since    = NOW(),
    updated_at        = NOW()
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

// lockApplyingWithEpoch переводит incarnation в applying И в ТОМ ЖЕ UPDATE/той же
// tx записывает epoch applying-флага (ADR-027 amend (m-S1)): applying_apply_id /
// applying_attempt / applying_by_kid / applying_since. Это превращает голый
// applying-bool в inline-epoch, по которому Reaper-правило reconcile_orphan_applying
// различает «прогон реально идёт» (владелец жив в Conclave) и «владелец мёртв,
// lock осиротел». КРИТИЧНО: один Exec — крах между записью status и записью epoch
// невозможен (один UPDATE атомарен), окна applying-без-epoch нет.
//
// attempt — echo текущего apply_runs.attempt; на момент lockRun строки apply_runs
// ещё нет (dispatch вставляет её позже), поэтому это начальный attempt прогона.
// Колонка пишется для parity apply_runs.attempt (задел под post-MVP epoch-check
// приёма RunResult); standalone-снятие её НЕ читает — смерть доказывает presence,
// FENCING-1 фенсит по apply_id.
func lockApplyingWithEpoch(ctx context.Context, tx pgx.Tx, name, applyID, kid string, attempt int) error {
	tag, err := tx.Exec(ctx, lockApplyingWithEpochSQL, name, applyID, attempt, kid)
	if err != nil {
		return fmt.Errorf("scenario: lock applying with epoch: %w", err)
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

// loadRegisterByHostUpToPassage читает register-данные прогона, накопленные в
// Passage СТРОГО МЕНЬШЕ upToPassage (staged-render, ADR-056 §в.1): render Passage
// N подставляет register всех предыдущих Passage per-host. upToPassage=0 (первый
// Passage) → пустая map (register ещё не собран — как up-front render).
//
// Резолв task_idx → register-name тот же, что в [loadRegisterByHost] (по
// []RenderedTask текущего прогона). Stage-loop run.go вызывает это перед render-ом
// каждого Passage и прокидывает результат в RenderInput.RegisterByHost.
func (r *Runner) loadRegisterByHostUpToPassage(ctx context.Context, applyID string, upToPassage int, tasks []*render.RenderedTask) (map[string]map[string]any, error) {
	rows, err := applyrun.SelectTaskRegistersByApplyIDUpToPassage(ctx, r.deps.DB, applyID, upToPassage)
	if err != nil {
		return nil, fmt.Errorf("scenario: load register-данных прогона (passage < %d): %w", upToPassage, err)
	}
	return buildRegisterByHost(rows, tasks), nil
}

// buildRegisterByHost — чистая свёртка register-строк прогона в per-host map
// (sid → register-name → payload) по mapping plan_index→register-name из tasks.
// Вынесена из loadRegisterByHost для unit-тестирования без PG.
//
// Корреляция по ГЛОБАЛЬНОМУ plan_index (ADR-056 §S1 fix Variant B): nameByIdx
// строится по RenderedTask.Index (глобальный сквозной индекс по всему плану), а
// register-строка несёт TaskRegister.PlanIndex (эхо TaskEvent.plan_index, тот же
// глобальный индекс). Раньше мапилось nameByIdx[t.Index] (глобальный) против
// rows.TaskIdx (ЛОКАЛЬНАЯ позиция в ApplyRequest Passage) — рассинхрон имён на
// passage>0 (latent-баг). Локальный task_idx на корреляцию НЕ годится: он
// неуникален между Passage И между хостами одного Passage (разный where:).
//
// Если несколько задач на одном хосте имеют одно register-имя (программно
// возможно, но валидатор scenario такое не пропускает) — побеждает строка с
// бóльшим plan_index (SelectTaskRegistersByApplyID сортирует по plan_index ASC,
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
		name := nameByIdx[rows[i].PlanIndex]
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
// (sid, plan_index) CHANGED-задач (auditpg.SelectChangedTaskKeys), plans —
// DispatchPlan-ы прогона (TargetSIDs после on:/where:).
//
// Корреляция CHANGED-факта с планом идёт по ГЛОБАЛЬНОМУ RenderedTask.Index
// (= ChangedTaskKey.PlanIndex, ADR-056 §S1 fix Variant B, T3): под staged/
// per-host-where локальный task_idx ≠ глобальному, ключ по нему указывал бы на
// соседнюю задачу (mismatch state_changes-whitelist + audit). SelectChangedTaskKeys
// уже отдаёт глобальный plan_index из payload (fallback task_idx для N=1).
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
		// changedKeys — множество (sid, plan_index); пробегаем target-sids idx-а и
		// берём те, что отметились CHANGED. t.Index — ГЛОБАЛЬНЫЙ RenderedTask.Index,
		// совпадает с ключом PlanIndex (T3); локальный task_idx тут не используется.
		for _, sid := range targetsByIdx[t.Index] {
			if _, ok := changedKeys[auditpg.ChangedTaskKey{SID: sid, PlanIndex: t.Index}]; ok {
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

// mergeStateChanges применяет упорядоченный список отрендеренных операций
// `state_changes` (render.RenderStateOps) поверх stateBefore и возвращает новый
// state (orchestration.md §7, новая list-форма грамматики). ★ Логика идентична
// trial.mergeStateChanges (diff.go): расхождение разведёт Trial с продом — держит
// Mirror-тест (state_test.go ↔ diff_test.go).
//
// deep-copy stateBefore (commit-снапшот не держит ссылку на исходный map) →
// последовательное применение операций к промежуточному state. matchEval —
// CEL-вычислитель match-предиката list-дедупа add (render.Pipeline.EvalStateMatch);
// opEval — CEL-вычислитель modify/remove match+patch с полным scenario-контекстом
// (render.Pipeline.EvalStateOpExpr); schema — state_schema сервиса (тип коллекции
// для материализации отсутствующего поля). Пустой/nil ops → state не меняется.
//
// Семантика операций:
//   - set:    out[field] = value (перезапись поля целиком, last-wins);
//   - add:    материализация коллекции (из существующего значения / schema) →
//     проверка идентичности (map: по Key; list: Match-предикат) → append/insert
//     ЛИБО no-op/replace/error по OnConflict (default skip — идемпотентно);
//   - modify: патч ВСЕХ элементов коллекции, подходящих под Match (all-by-default).
//     map: match видит key/value, патчит запись; list: match видит elem, патчит
//     элемент. patch — merge путь-в-элементе (вложенный точечный путь), не
//     перезапись записи целиком. expect → ассерт кратности до коммита;
//   - remove: удалить ВСЕ подходящие под Match. empty-match → no-op для обоих.
//
// foreach сюда не доходит — раскрыт в render-фазе в N RenderedOp.
func mergeStateChanges(stateBefore map[string]any, ops []render.RenderedOp, schema map[string]any, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) (map[string]any, error) {
	out := deepCopyMap(stateBefore)
	for i := range ops {
		op := ops[i]
		switch op.Verb {
		case config.VerbSet:
			out[op.Field] = op.Value
		case config.VerbAdd:
			if err := applyAddOp(out, op, schema, matchEval, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] add %q: %w", i, op.Field, err)
			}
		case config.VerbModify:
			if err := applyModifyOp(out, op, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] modify %q: %w", i, op.Field, err)
			}
		case config.VerbRemove:
			if err := applyRemoveOp(out, op, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] remove %q: %w", i, op.Field, err)
			}
		default:
			return nil, fmt.Errorf("state_changes[%d]: verb %q не поддержан движком", i, op.Verb)
		}
	}
	return out, nil
}

// applyModifyOp патчит ВСЕ элементы коллекции op.Field, подходящие под op.Match
// (all-by-default). ★ Логика идентична trial.applyModifyOp. map: match видит key/value,
// патч мержится в значение записи; list: match видит elem, патч мержится в
// элемент. Кратность зацепленных сверяется против op.Expect ДО мутации (фейл
// expect → ошибка, state не коммитится). Empty-match → no-op.
func applyModifyOp(out map[string]any, op render.RenderedOp, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	if !present {
		// Поле отсутствует — патчить нечего. empty-match no-op (поле = пустая
		// коллекция семантически); не ошибка.
		return checkExpect(op, 0)
	}
	switch coll := existing.(type) {
	case map[string]any:
		matched := 0
		for k, v := range coll {
			binds := map[string]any{"key": k, "value": v}
			ok, err := evalOpBool(opEval, op.Match, op.Context, binds)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			matched++
			patched, err := applyPatch(v, op.Patch, op.Context, binds, opEval)
			if err != nil {
				return err
			}
			coll[k] = patched
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = coll
		return nil
	case []any:
		matched := 0
		for i := range coll {
			binds := map[string]any{"elem": coll[i]}
			ok, err := evalOpBool(opEval, op.Match, op.Context, binds)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			matched++
			patched, err := applyPatch(coll[i], op.Patch, op.Context, binds, opEval)
			if err != nil {
				return err
			}
			coll[i] = patched
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = coll
		return nil
	}
	return fmt.Errorf("поле %q не является коллекцией (map/list)", op.Field)
}

// applyRemoveOp удаляет ВСЕ элементы коллекции op.Field, подходящие под op.Match.
// ★ Логика идентична trial.applyRemoveOp. Кратность сверяется против op.Expect ДО мутации.
// Empty-match → no-op. Поле отсутствует → no-op (нечего удалять).
func applyRemoveOp(out map[string]any, op render.RenderedOp, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	if !present {
		return checkExpect(op, 0)
	}
	switch coll := existing.(type) {
	case map[string]any:
		matched := 0
		drop := make([]string, 0, len(coll))
		for k, v := range coll {
			ok, err := evalOpBool(opEval, op.Match, op.Context, map[string]any{"key": k, "value": v})
			if err != nil {
				return err
			}
			if ok {
				matched++
				drop = append(drop, k)
			}
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		for _, k := range drop {
			delete(coll, k)
		}
		out[op.Field] = coll
		return nil
	case []any:
		kept := make([]any, 0, len(coll))
		matched := 0
		for i := range coll {
			ok, err := evalOpBool(opEval, op.Match, op.Context, map[string]any{"elem": coll[i]})
			if err != nil {
				return err
			}
			if ok {
				matched++
				continue
			}
			kept = append(kept, coll[i])
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = kept
		return nil
	}
	return fmt.Errorf("поле %q не является коллекцией (map/list)", op.Field)
}

// evalOpBool вычисляет match-предикат modify/remove через opEval (полный
// scenario-контекст + биндинги элемента) и приводит к bool. ★ Логика идентична
// trial.evalOpBool.
func evalOpBool(opEval render.StateOpEvalFunc, match string, ctx, binds map[string]any) (bool, error) {
	if match == "" {
		// Пустой match сюда не приходит (config-валидатор warn-ит, движок мог бы
		// трактовать как «все»). Fail-safe: пустой предикат не зацепляет ничего.
		return false, nil
	}
	res, err := opEval(match, ctx, binds, true)
	if err != nil {
		return false, fmt.Errorf("match-предикат %q: %w", match, err)
	}
	b, ok := res.(bool)
	if !ok {
		return false, fmt.Errorf("match-предикат %q вернул %T, ожидался bool", match, res)
	}
	return b, nil
}

// applyPatch мержит patch-map в запись/элемент коллекции (точечный путь —
// вложенный merge, не перезапись записи целиком). Каждое patch-значение —
// CEL/литерал, вычисляется через opEval (контекст + биндинги элемента). Запись —
// глубоко копируется перед мутацией (исходный элемент state не задевается до
// успеха цепочки). ★ Логика идентична trial.applyPatch.
func applyPatch(elem any, patch, ctx, binds map[string]any, opEval render.StateOpEvalFunc) (any, error) {
	target, ok := deepCopyValue(elem).(map[string]any)
	if !ok {
		// Скалярный элемент list (list of scalars) патчить точечным путём нельзя.
		return nil, fmt.Errorf("patch применим только к объекту-записи (элемент %T не объект)", elem)
	}
	for path, rawVal := range patch {
		val, err := renderPatchValue(rawVal, ctx, binds, opEval)
		if err != nil {
			return nil, fmt.Errorf("patch %q: %w", path, err)
		}
		if err := setNestedPath(target, path, val); err != nil {
			return nil, fmt.Errorf("patch %q: %w", path, err)
		}
	}
	return target, nil
}

// renderPatchValue вычисляет одно patch-значение: строка → CEL/литерал через
// opEval (interpolation, native-тип); прочее (число/bool из YAML-литерала) — как
// есть. ★ Логика идентична trial.renderPatchValue.
func renderPatchValue(raw any, ctx, binds map[string]any, opEval render.StateOpEvalFunc) (any, error) {
	s, ok := raw.(string)
	if !ok {
		return raw, nil
	}
	return opEval(s, ctx, binds, false)
}

// setNestedPath кладёт значение по точечному пути (`config.maxmemory`) в map,
// материализуя ОТСУТСТВУЮЩИЕ промежуточные объекты (ADR-057 §f). ★ Логика
// идентична trial.setNestedPath. Точечный путь — вложенный merge (соседние поля
// записи целы); плоский путь — top-level.
//
// Промежуточный сегмент, который УЖЕ существует и НЕ является map (скаляр/список),
// — ошибка (state_changes_apply_failed, не молчаливый клоббер): спустить вложенный
// путь сквозь не-объект значит потерять прежнее значение узла. Различие с §f:
// отсутствующий промежуточный узел материализуем (ok); существующий не-map узел —
// явный отказ (operator патчит несовместимую форму).
func setNestedPath(m map[string]any, path string, val any) error {
	parts := splitPath(path)
	cur := m
	for i := 0; i < len(parts)-1; i++ {
		seg := parts[i]
		existing, present := cur[seg]
		if !present {
			next := map[string]any{}
			cur[seg] = next
			cur = next
			continue
		}
		next, ok := existing.(map[string]any)
		if !ok {
			return fmt.Errorf("промежуточный узел %q уже существует и не является объектом (%T) — patch вложенного пути %q затёр бы его", seg, existing, path)
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = val
	return nil
}

// splitPath режет точечный путь patch в сегменты. ★ Логика идентична trial.splitPath.
func splitPath(path string) []string {
	return strings.Split(path, ".")
}

// checkExpect сверяет фактическую кратность match с ожиданием op.Expect (ADR-057
// §c). ""/any → без ассерта. one → ровно 1; at_most_one → 0 или 1. Нарушение →
// ошибка (run.go → error_locked, state не коммитится). ★ Логика идентична trial.checkExpect.
func checkExpect(op render.RenderedOp, matched int) error {
	switch op.Expect {
	case "", config.ExpectAny:
		return nil
	case config.ExpectOne:
		if matched != 1 {
			return fmt.Errorf("expect: one — match зацепил %d элементов (ожидался ровно один)", matched)
		}
	case config.ExpectAtMostOne:
		if matched > 1 {
			return fmt.Errorf("expect: at_most_one — match зацепил %d элементов (ожидалось ≤1)", matched)
		}
	}
	return nil
}

// applyAddOp применяет одну add-операцию к промежуточному state out (мутирует
// out на месте — out уже deep-copy исходного state). Коллекция out[field]
// материализуется при отсутствии (тип из schema), затем элемент добавляется
// идемпотентно по политике OnConflict. ★ Логика идентична trial.applyAddOp.
func applyAddOp(out map[string]any, op render.RenderedOp, schema map[string]any, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	kind := collectionKind(existing, present, schema, op.Field)

	switch kind {
	case collKindMap:
		if op.Key == "" {
			return fmt.Errorf("add в map-коллекцию требует key:")
		}
		coll, _ := existing.(map[string]any)
		if coll == nil {
			coll = map[string]any{}
		}
		if _, exists := coll[op.Key]; exists {
			switch op.OnConflict {
			case config.OnConflictError:
				// БЕЗ зарезолвленного op.Key в reason: ключ map мог быть
				// `${ vault(...) }` (зарезолвленный секрет), а reason уезжает в
				// incarnation.status_details.error немаскированным (audit.MaskSecrets
				// ловит `vault:`-ref, не plaintext-значение). Печатаем только имя
				// коллекции-поля (BUG-3, security).
				return fmt.Errorf("add %q: ключ уже существует (on_conflict: error)", op.Field)
			case config.OnConflictReplace:
				coll[op.Key] = op.Value
			default: // skip (default) — идемпотентный no-op
			}
		} else {
			coll[op.Key] = op.Value
		}
		out[op.Field] = coll
		return nil

	case collKindList:
		coll, _ := existing.([]any)
		idx, err := findListMatch(coll, op, matchEval, opEval)
		if err != nil {
			return err
		}
		if idx >= 0 {
			switch op.OnConflict {
			case config.OnConflictError:
				// Без зарезолвленного op.Value/elem в reason (BUG-3, security): value
				// мог быть `${ vault(...) }`. Печатаем только имя коллекции-поля.
				return fmt.Errorf("add %q: элемент с такой идентичностью уже существует (on_conflict: error)", op.Field)
			case config.OnConflictReplace:
				coll[idx] = op.Value
			default: // skip (default) — идемпотентный no-op
			}
		} else {
			coll = append(coll, op.Value)
		}
		out[op.Field] = coll
		return nil
	}
	return fmt.Errorf("поле %q не является коллекцией (map/list) и тип не выводится из schema", op.Field)
}

// findListMatch ищет индекс существующего элемента, идентичного добавляемому
// (op.Value): по Match-предикату (если задан), иначе deep-equal. Возвращает -1,
// если идентичного нет. ★ Логика идентична trial.findListMatch.
//
// Если op.Context != nil (add внутри foreach — match ссылается на `as`-имя,
// например `elem == sid`), match вычисляется context-aware вычислителем opEval
// (биндинги elem/value + foreach-биндинг из Context). Иначе — чистый add-match
// matchEval (только elem/value, ADR-057: идентичность = функция elem+value).
func findListMatch(coll []any, op render.RenderedOp, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) (int, error) {
	for i := range coll {
		if op.Match != "" {
			var ok bool
			var err error
			if op.Context != nil {
				ok, err = evalOpBool(opEval, op.Match, op.Context, map[string]any{"elem": coll[i], "value": op.Value})
			} else {
				ok, err = matchEval(op.Match, coll[i], op.Value)
			}
			if err != nil {
				return -1, fmt.Errorf("match-предикат %q: %w", op.Match, err)
			}
			if ok {
				return i, nil
			}
			continue
		}
		if reflect.DeepEqual(coll[i], op.Value) {
			return i, nil
		}
	}
	return -1, nil
}

// collKind — вид коллекции под полем state (для add-материализации). ★ Логика
// идентична trial.collKind*.
type collKind int

const (
	collKindUnknown collKind = iota
	collKindList
	collKindMap
)

// collectionKind определяет вид коллекции под полем: сначала по уже
// существующему значению state (авторитетно — фактическая форма), при
// отсутствии — из state_schema (`properties.<field>.type`: array→list,
// object→map). Неизвестно → collKindUnknown (applyAddOp вернёт ошибку). ★
// Логика идентична trial.collectionKind.
func collectionKind(existing any, present bool, schema map[string]any, field string) collKind {
	if present {
		switch existing.(type) {
		case []any:
			return collKindList
		case map[string]any:
			return collKindMap
		}
		return collKindUnknown
	}
	switch schemaFieldType(schema, field) {
	case "array":
		return collKindList
	case "object":
		return collKindMap
	}
	return collKindUnknown
}

// schemaFieldType извлекает `state_schema.properties.<field>.type` из плоского
// state_schema-map сервиса (форма service.yml: {type:object, properties:{...}}).
// "" если schema не задекларирована или поле не описано. ★ Логика идентична
// trial.schemaFieldType.
func schemaFieldType(schema map[string]any, field string) string {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return ""
	}
	fieldSchema, ok := props[field].(map[string]any)
	if !ok {
		return ""
	}
	t, _ := fieldSchema["type"].(string)
	return t
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

// deepCopyValue делает глубокую копию произвольного JSON-safe значения (map/
// slice/скаляр) через JSON round-trip. Нужен applyPatch: модифицируемый элемент
// коллекции не должен держать ссылку на исходный state до успеха цепочки. ★
// Логика идентична trial.deepCopyValue. Сбой marshal невозможен (state JSON-safe из JSONB);
// при ошибке возвращаем оригинал (расхождение поймает сверка/тест).
func deepCopyValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
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
