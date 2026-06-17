package applyrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// TaskRegister — строка накопителя `apply_task_register` (миграция 022):
// register-результат одной probe-задачи на одном хосте в рамках прогона.
//
// register_name тут нет: handler в момент TaskEvent знает только task_idx
// (proto register-имя не несёт, ADR-012(d)). Резолв task_idx → register_name
// делает scenario-runner при чтении (он держит []RenderedTask с полем Register).
type TaskRegister struct {
	ApplyID      string
	SID          string
	TaskIdx      int
	RegisterData map[string]any
}

const upsertTaskRegisterSQL = `
INSERT INTO apply_task_register (apply_id, sid, task_idx, register_data)
VALUES ($1, $2, $3, $4)
ON CONFLICT (apply_id, sid, task_idx)
DO UPDATE SET register_data = EXCLUDED.register_data, created_at = NOW()
`

// UpsertTaskRegister пишет (или перезаписывает) register-результат задачи.
// Перезапись — для retry той же задачи на Soul-стороне: побеждает последний
// результат (ON CONFLICT по PK (apply_id, sid, task_idx)).
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

	if _, err := db.Exec(ctx, upsertTaskRegisterSQL, tr.ApplyID, tr.SID, tr.TaskIdx, raw); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
			return fmt.Errorf("applyrun: task register FK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return fmt.Errorf("applyrun: upsert task register: %w", err)
	}
	return nil
}

const selectTaskRegistersByApplyIDSQL = `
SELECT sid, task_idx, register_data
FROM apply_task_register
WHERE apply_id = $1
ORDER BY sid ASC, task_idx ASC
`

// SelectTaskRegistersByApplyID возвращает все register-строки прогона
// (один apply_id, разные sid/task_idx), отсортированные по (sid, task_idx).
// Используется scenario-runner-ом после барьера: он группирует строки per-host,
// резолвит task_idx → register_name из своих []RenderedTask и строит
// RenderInput.Register для рендера state_changes.sets.
//
// Пустой результат — прогон без register: задач (нечего копить); caller
// трактует как пустой register-context.
func SelectTaskRegistersByApplyID(ctx context.Context, db ExecQueryRower, applyID string) ([]TaskRegister, error) {
	rows, err := db.Query(ctx, selectTaskRegistersByApplyIDSQL, applyID)
	if err != nil {
		return nil, fmt.Errorf("applyrun: task registers query: %w", err)
	}
	defer rows.Close()

	var out []TaskRegister
	for rows.Next() {
		var (
			tr  TaskRegister
			raw []byte
		)
		if err := rows.Scan(&tr.SID, &tr.TaskIdx, &raw); err != nil {
			return nil, fmt.Errorf("applyrun: task registers scan: %w", err)
		}
		if err := json.Unmarshal(raw, &tr.RegisterData); err != nil {
			return nil, fmt.Errorf("applyrun: task registers unmarshal (sid=%s task_idx=%d): %w", tr.SID, tr.TaskIdx, err)
		}
		tr.ApplyID = applyID
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applyrun: task registers iter: %w", err)
	}
	return out, nil
}
