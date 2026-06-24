package render

import (
	"errors"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// ErrVarUnknownRef — var-значение ссылается через `${ vars.<X> }` на имя <X>,
// которого нет в ТОМ ЖЕ слое (file-level или task-level). EAGER-маркер: ошибка
// поднимается на этапе построения графа зависимостей слоя, даже если сам
// ссылающийся var нигде потом не используется. Раннее падение лучше тихого
// no-such-key в неиспользуемой ветке: битая перекрёстная ссылка — это опечатка
// автора, а не валидный «отложенный» var. Текст несёт var_unknown_ref как
// стабильный маркер (trial expect_render_error / grep по логам).
var ErrVarUnknownRef = errors.New("render: var_unknown_ref")

// ErrVarCycle — циклическая зависимость var→var внутри одного слоя
// (a → b → … → a). Текст ошибки несёт ТРАССУ цикла (`a → b → c → a`) для
// диагностики автору. Цикл неразрешим топосортом (Kahn): после удаления всех
// узлов с нулевой входящей степенью в acc остаются только узлы цикла.
var ErrVarCycle = errors.New("render: var_cycle")

// resolveVarLayer резолвит ОДИН слой переменных `vars.*` (file-level vars.yml ЛИБО
// task-level `vars:`) с поддержкой ссылок var→var ВНУТРИ слоя (eager-topological).
// Зеркало resolveCompute (compute.go): резолв в топопорядке зависимостей с
// накоплением результата в base.Vars, чтобы var, объявленный позже, видел ранее
// вычисленный var того же слоя.
//
// Граф зависимостей строится через engine.VarRefs на каждом строковом значении
// (AST-обход, не regex): `${ vars.X }` → ребро текущий-var → X. Ссылка на имя,
// отсутствующее в raw → [ErrVarUnknownRef] (eager, даже для неиспользуемого var).
// Цикл → [ErrVarCycle] с трассой. non-string значения — литералы насквозь (CEL
// трогает только строки, симметрично renderValue/resolveCompute); ребёр не дают.
//
// ИЗОЛЯЦИЯ (КРИТИЧНО): var→var разрешён ТОЛЬКО внутри своего слоя. base несёт
// контекст резолва (input/soulprint.self/incarnation для file-vars; см. caller-ов),
// а base.Vars на старте слоя ОБЯЗАН быть пуст — иначе ссылка `vars.<X>` на чужой
// слой (file-var из task-слоя и наоборот) резолвилась бы вместо ошибки изоляции.
// Активация CEL получает только ключ `vars` (накопитель этого слоя); restricted-env
// (register/soulprint.hosts) НЕ ослабляется — он определяется самим base, который
// caller строит изолированным.
func resolveVarLayer(engine *cel.Engine, raw map[string]any, base cel.Vars) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	// deps[name] — имена того же слоя, на которые ссылается значение name (рёбра
	// name → dep). Несуществующая ссылка → eager ErrVarUnknownRef.
	deps := make(map[string][]string, len(raw))
	for name, val := range raw {
		s, ok := val.(string)
		if !ok {
			continue // литерал — рёбер не даёт
		}
		refs, err := engine.VarRefs(s)
		if err != nil {
			return nil, fmt.Errorf("render: vars.%s: %w", name, err)
		}
		for _, ref := range refs {
			if _, exists := raw[ref]; !exists {
				return nil, fmt.Errorf("%w: vars.%s ссылается на vars.%s, которого нет в слое", ErrVarUnknownRef, name, ref)
			}
		}
		deps[name] = refs
	}

	order, cycle := topoSort(raw, deps)
	if cycle != nil {
		return nil, fmt.Errorf("%w: %s", ErrVarCycle, strings.Join(cycle, " → "))
	}

	acc := make(map[string]any, len(raw))
	base.Vars = acc // накопитель слоя; var в топопорядке видит ранее вычисленные
	for _, name := range order {
		val := raw[name]
		s, ok := val.(string)
		if !ok {
			acc[name] = val // литерал — насквозь
			continue
		}
		r, err := engine.EvalInterpolation(s, base)
		if err != nil {
			return nil, fmt.Errorf("render: vars.%s: %w", name, err)
		}
		acc[name] = r
	}
	return acc, nil
}

// topoSort упорядочивает имена слоя так, что каждое имя идёт ПОСЛЕ имён, на которые
// оно ссылается (deps[name]) — порядок резолва, при котором ссылающийся var видит
// уже вычисленные зависимости. Алгоритм — Kahn по входящей степени; узлы с равной
// степенью берутся в лексикографическом порядке для детерминизма (порядок ключей в
// YAML безразличен — кейс #7).
//
// Возврат cycle != nil, если граф не ацикличен: cycle — трасса одного цикла
// (`a → b → c → a`, замкнутая повтором стартового узла) для ErrVarCycle. Самоссылка
// (a→a) — частный случай цикла, трасса `a → a`.
func topoSort(raw map[string]any, deps map[string][]string) (order []string, cycle []string) {
	// remaining[name] — число его ИСХОДЯЩИХ неразрешённых зависимостей: узел готов к
	// резолву, когда все его deps уже в order (Kahn по исходящей степени).
	remaining := make(map[string]int, len(raw))
	// dependents[dep] — кто ссылается на dep (обратные рёбра), чтобы при готовности
	// dep уменьшать счётчик зависящих.
	dependents := make(map[string][]string, len(raw))
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
		remaining[name] = len(deps[name])
		for _, d := range deps[name] {
			dependents[d] = append(dependents[d], name)
		}
	}

	// Очередь готовых узлов (remaining==0), извлекаем лексикографически наименьший —
	// детерминизм при равной готовности. Малые слои (единицы-десятки vars) → линейный
	// поиск минимума дешевле кучи и проще.
	resolved := make(map[string]bool, len(raw))
	for len(order) < len(raw) {
		next := ""
		for _, name := range names {
			if resolved[name] || remaining[name] != 0 {
				continue
			}
			if next == "" || name < next {
				next = name
			}
		}
		if next == "" {
			// Готовых узлов нет, но не все разрешены → остаток образует цикл.
			return nil, traceCycle(names, deps, resolved)
		}
		resolved[next] = true
		order = append(order, next)
		for _, dep := range dependents[next] {
			remaining[dep]--
		}
	}
	return order, nil
}

// traceCycle строит человекочитаемую трассу одного цикла среди ещё не разрешённых
// узлов (resolved[x]==false). Идёт по рёбрам deps от первого неразрешённого узла,
// пока не встретит уже посещённый в этом обходе — отрезок от его первого появления
// до повтора и есть цикл (замкнутый повтором стартового элемента).
func traceCycle(names []string, deps map[string][]string, resolved map[string]bool) []string {
	// Стартуем с лексикографически наименьшего неразрешённого узла — детерминизм
	// трассы (кейс #7: порядок YAML не должен влиять на текст ошибки).
	start := ""
	for _, n := range names {
		if !resolved[n] {
			if start == "" || n < start {
				start = n
			}
		}
	}
	pos := make(map[string]int)
	var path []string
	cur := start
	for {
		if i, seen := pos[cur]; seen {
			return append(path[i:], cur) // замыкаем повтором
		}
		pos[cur] = len(path)
		path = append(path, cur)
		// Следующий узел цикла — первая неразрешённая зависимость (детерминированно
		// наименьшая среди deps, ведущих обратно в цикл).
		nextHop := ""
		for _, d := range deps[cur] {
			if resolved[d] {
				continue
			}
			if nextHop == "" || d < nextHop {
				nextHop = d
			}
		}
		if nextHop == "" {
			return path // защита: deps закончились (не должно случиться для цикла)
		}
		cur = nextHop
	}
}
