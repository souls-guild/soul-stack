// Package statemigrate — чистое ядро executor-а миграций state_schema
// ([ADR-019], нормативная спека [docs/migrations.md]). Применяет цепочку
// миграций к incarnation.state как чистую функцию state_v<N> → state_v<M>:
// БЕЗ Postgres, БЕЗ host-side эффектов, БЕЗ хостового контекста (миграция —
// keeper-side операция над одним state-объектом).
//
// Грамматика transform: rename / set / delete / move(=rename) / foreach
// ([dsl.go]). CEL-выражения в set.value / foreach.in / ${ … }-сегментах path
// резолвятся через migration-CEL движок ([eval.go], shared/cel.NewMigration):
// объявлена только переменная `state`, прочий контекст недоступен (sandbox by
// undeclaration), vault()/now() отсекаются guard-ами.
//
// Транзакционная обвязка (PG SELECT FOR UPDATE → state_history snapshot per-step
// → COMMIT) и L1 trial-раннер — НЕ здесь (отдельные задачи поверх этого ядра).
package statemigrate

import (
	"context"
	"fmt"
)

// Result — итог применения цепочки миграций. FinalState — состояние после
// последнего шага (новый map, входной caller-ский не мутируется). Steps —
// snapshot до/после каждого шага (для записи в state_history транзакционным
// слоем поверх ядра).
type Result struct {
	FinalState map[string]any
	Steps      []StepSnapshot
}

// StepSnapshot — снимок одного шага цепочки: версии и state до/после. StateBefore
// и StateAfter — независимые deep-copy (caller волен сериализовать их в
// state_history без риска общих ссылок).
type StepSnapshot struct {
	FromVersion int
	ToVersion   int
	StateBefore map[string]any
	StateAfter  map[string]any
}

// Apply прогоняет цепочку миграций chain поверх state, возвращая итоговый
// state и per-step снимки. Чистая функция: входной state НЕ мутируется
// (deep-copy на входе), Postgres не затрагивается.
//
// Цепочка валидируется на непрерывность версий: ToVersion шага i должен равняться
// FromVersion шага i+1 (разрыв → EvalError ClassChainVersion). Пустая chain →
// FinalState = deep-copy входа, Steps пуст.
//
// ev — Evaluator над migration-CEL ([NewEvaluator]); переиспользуется всеми
// шагами (compile-cache). Ошибка любого шага прерывает цепочку и возвращается
// как есть (транзакционный слой делает ROLLBACK / status: migration_failed).
func Apply(ctx context.Context, state map[string]any, chain Chain, ev Evaluator) (Result, error) {
	_ = ctx // зарезервировано: ядро синхронно; ctx прокидывается для симметрии с PG-слоем

	cur := deepCopyMap(state)
	steps := make([]StepSnapshot, 0, len(chain))

	for i, m := range chain {
		if i > 0 && chain[i-1].ToVersion != m.FromVersion {
			return Result{}, &EvalError{
				Class: ClassChainVersion,
				Msg:   fmt.Sprintf("разрыв цепочки: шаг %d→%d следует за %d→%d", m.FromVersion, m.ToVersion, chain[i-1].FromVersion, chain[i-1].ToVersion),
			}
		}

		before := deepCopyMap(cur)
		if err := applyOps(m.Transform, cur, ev, nil); err != nil {
			return Result{}, fmt.Errorf("миграция %d→%d: %w", m.FromVersion, m.ToVersion, err)
		}
		steps = append(steps, StepSnapshot{
			FromVersion: m.FromVersion,
			ToVersion:   m.ToVersion,
			StateBefore: before,
			StateAfter:  deepCopyMap(cur),
		})
	}

	return Result{FinalState: cur, Steps: steps}, nil
}
