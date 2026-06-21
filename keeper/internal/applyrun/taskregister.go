package applyrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TaskRegister — строка накопителя `apply_task_register` (миграция 022, ключ
// перетянут на plan_index миграцией 079): register-результат одной probe-задачи
// на одном хосте в рамках прогона.
//
// register_name тут нет: handler в момент TaskEvent знает только индексы
// (proto register-имя не несёт, ADR-012(d)). Резолв plan_index → register_name
// делает scenario-runner при чтении (он держит []RenderedTask с полем Register).
type TaskRegister struct {
	ApplyID string
	SID     string

	// PlanIndex — ГЛОБАЛЬНЫЙ сквозной индекс задачи по всему плану прогона (по
	// всем Passage), эхо TaskEvent.plan_index (ADR-056 §S1 fix Variant B). Ключ
	// register-корреляции (PK-компонент, миграция 079): уникален и по всем Passage,
	// и по всем хостам — устраняет коллизию task_idx (probe passage0 / действие
	// passage1 делили локальный idx=0, ON CONFLICT затирал probe-register).
	// scenario-runner мапит его против RenderedTask.Index. N=1 → ==TaskIdx.
	PlanIndex int

	// TaskIdx — ЛОКАЛЬНАЯ позиция задачи в ApplyRequest.tasks[] её Passage
	// (эхо TaskEvent.task_idx). Хранится информационно (триаж); НЕ ключ
	// корреляции — он неуникален между Passage И между хостами одного Passage
	// (разный where:). Резолв register идёт по PlanIndex.
	TaskIdx int

	RegisterData map[string]any

	// Passage — индекс Passage staged-render (ADR-056, миграция 078): компонент
	// FK на apply_runs(apply_id, sid, passage). passage пишется как данные строки
	// и нужен FK-цели. N=1 → 0 (единственный Passage хоста).
	Passage int
}

const upsertTaskRegisterSQL = `
INSERT INTO apply_task_register (apply_id, sid, plan_index, task_idx, register_data, passage)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (apply_id, sid, plan_index)
DO UPDATE SET task_idx = EXCLUDED.task_idx, register_data = EXCLUDED.register_data, passage = EXCLUDED.passage, created_at = NOW()
`

// UpsertTaskRegister пишет (или перезаписывает) register-результат задачи.
// Перезапись — для retry той же задачи на Soul-стороне: побеждает последний
// результат (ON CONFLICT по PK (apply_id, sid, plan_index) — PK сменён в
// миграции 079 с task_idx на plan_index).
//
// Pre-conditions: непустые ApplyID / SID; неотрицательный TaskIdx; непустой
// RegisterData (nil/пустой → no-op: register: без данных нечего копить).
//
// register_data сериализуется в jsonb через encoding/json. FK-violation
// (нет строки apply_runs (apply_id, sid)) → wrapped fmt.Errorf: программная
// ошибка порядка (Insert apply_run обязан предшествовать TaskEvent-у).
func UpsertTaskRegister(ctx context.Context, db ExecQueryRower, tr *TaskRegister) error {
	if tr == nil {
		return fmt.Errorf("applyrun: nil task register")
	}
	if tr.ApplyID == "" {
		return fmt.Errorf("applyrun: empty apply_id")
	}
	if tr.SID == "" {
		return fmt.Errorf("applyrun: empty sid")
	}
	if tr.PlanIndex < 0 {
		return fmt.Errorf("applyrun: negative plan_index %d", tr.PlanIndex)
	}
	if tr.TaskIdx < 0 {
		return fmt.Errorf("applyrun: negative task_idx %d", tr.TaskIdx)
	}
	if len(tr.RegisterData) == 0 {
		return nil
	}

	raw, err := json.Marshal(tr.RegisterData)
	if err != nil {
		return fmt.Errorf("applyrun: marshal register_data: %w", err)
	}

	if _, err := db.Exec(ctx, upsertTaskRegisterSQL, tr.ApplyID, tr.SID, tr.PlanIndex, tr.TaskIdx, raw, tr.Passage); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
			return fmt.Errorf("applyrun: task register FK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return fmt.Errorf("applyrun: upsert task register: %w", err)
	}
	return nil
}

const selectTaskRegistersByApplyIDSQL = `
SELECT sid, plan_index, task_idx, register_data, passage
FROM apply_task_register
WHERE apply_id = $1
ORDER BY sid ASC, plan_index ASC
`

// SelectTaskRegistersByApplyID возвращает все register-строки прогона
// (один apply_id, разные sid/plan_index), отсортированные по (sid, plan_index).
// Используется scenario-runner-ом после барьера: он группирует строки per-host,
// резолвит plan_index → register_name из своих []RenderedTask (по
// RenderedTask.Index = глобальный индекс) и строит RenderInput.Register для
// рендера state_changes.sets. Сортировка по глобальному plan_index сохраняет
// «поздняя в плане задача побеждает» при дублирующемся register-имени.
//
// Возвращает register ВСЕХ Passage прогона (staged-render, ADR-056): финальный
// state_changes.sets-рендер агрегирует register всех passage (после последнего
// барьера). Render следующего Passage в stage-loop читает register предыдущих
// через [SelectTaskRegistersByApplyIDUpToPassage].
//
// Пустой результат — прогон без register: задач (нечего копить); caller
// трактует как пустой register-context.
func SelectTaskRegistersByApplyID(ctx context.Context, db ExecQueryRower, applyID string) ([]TaskRegister, error) {
	rows, err := db.Query(ctx, selectTaskRegistersByApplyIDSQL, applyID)
	if err != nil {
		return nil, fmt.Errorf("applyrun: task registers query: %w", err)
	}
	return scanTaskRegisters(rows, applyID)
}

const selectTaskRegistersUpToPassageSQL = `
SELECT sid, plan_index, task_idx, register_data, passage
FROM apply_task_register
WHERE apply_id = $1 AND passage < $2
ORDER BY sid ASC, plan_index ASC
`

// SelectTaskRegistersByApplyIDUpToPassage возвращает register-строки прогона,
// накопленные в Passage СТРОГО МЕНЬШЕ upToPassage (staged-render, ADR-056 §в.1):
// render Passage N подставляет register всех предыдущих Passage (per-host карта,
// собранная их барьерами). upToPassage=0 (первый Passage) → пусто (register ещё
// не собран — поведение как up-front render).
func SelectTaskRegistersByApplyIDUpToPassage(ctx context.Context, db ExecQueryRower, applyID string, upToPassage int) ([]TaskRegister, error) {
	rows, err := db.Query(ctx, selectTaskRegistersUpToPassageSQL, applyID, upToPassage)
	if err != nil {
		return nil, fmt.Errorf("applyrun: task registers query (up to passage %d): %w", upToPassage, err)
	}
	return scanTaskRegisters(rows, applyID)
}

// scanTaskRegisters читает rows в []TaskRegister (общая часть обоих select-ов).
// Закрывает rows.
func scanTaskRegisters(rows pgx.Rows, applyID string) ([]TaskRegister, error) {
	defer rows.Close()

	var out []TaskRegister
	for rows.Next() {
		var (
			tr  TaskRegister
			raw []byte
		)
		if err := rows.Scan(&tr.SID, &tr.PlanIndex, &tr.TaskIdx, &raw, &tr.Passage); err != nil {
			return nil, fmt.Errorf("applyrun: task registers scan: %w", err)
		}
		if err := json.Unmarshal(raw, &tr.RegisterData); err != nil {
			return nil, fmt.Errorf("applyrun: task registers unmarshal (sid=%s plan_index=%d): %w", tr.SID, tr.PlanIndex, err)
		}
		tr.ApplyID = applyID
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applyrun: task registers iter: %w", err)
	}
	return out, nil
}
