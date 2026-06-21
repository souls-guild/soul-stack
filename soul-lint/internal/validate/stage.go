package validate

// Stage-валидация scenario (ADR-056 §S5): офлайн-прогон ТОЙ ЖЕ Passage-
// стратификации, что keeper-рантайм делает перед dispatch ([config.Stratify]),
// чтобы автор сценария ловил staged-ошибки ДО apply. Переиспользует канон
// shared/config (Stratify над тем же []Task-планом + config-валидатор reads==refs):
// один граф register-зависимости для линтера и рантайма (дубль = silent-wrong-target).
//
// Что детектит ОФЛАЙН (поверх unknown_register_reference, который config-валидатор
// уже ловит на парсе):
//   - register-цикл (StratifyCycle) — ОШИБКА: топологического порядка нет, прогон
//     не стартовал бы.
//   - структура passage (сколько Passage, по сколько задач) — HINT (info автору).
//
// serial: + staged (N>1 Passage) больше НЕ ошибка (ADR-056 §S4 amend, S-2D1): 2D
// serial×passage реализован — каждый Passage катит serial-волны по своему per-Passage
// width. Такой сценарий проходит линт с обычным passage_plan HINT.

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// stageDiagnostics прогоняет Passage-стратификацию над уже распарсенным сценарием
// и возвращает дополнительные диагностики (info/ошибки), которые caller дописывает
// к диагностикам парса. scenarioPath — путь к main.yml (директория используется для
// scenario-local резолва include-целей, как двухуровневый резолв keeper-а, но без
// service-слоя — он офлайн недоступен).
//
// m==nil (парс упал error-ами) → нет смысла стратифицировать (граф недостоверен) →
// nil. Ошибка резолва include → HINT «stage-граф проверен только по локально-
// резолвнутым задачам» + стратификация по тому, что раскрылось (не падаем: include
// мог указывать в service-слой, недоступный офлайн).
func stageDiagnostics(scenarioPath string, m *config.ScenarioManifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}

	dir := filepath.Dir(scenarioPath)
	tasks, expandDiags := config.ExpandIncludes(m.Tasks, scenarioLocalIncludeResolver(dir))
	var out []diag.Diagnostic
	// include-резолв офлайн неполон (нет service-слоя): error-диагностики expand-а
	// downgrade-им в HINT, чтобы не маскировать stage-валидацию ложным провалом.
	// Если include реально битый, его поднимет полная валидация на keeper-е.
	for _, d := range expandDiags {
		if d.Level == diag.LevelError {
			out = append(out, diag.Diagnostic{
				Level:   diag.LevelHint,
				Phase:   diag.PhaseSemanticValidate,
				File:    scenarioPath,
				Code:    "stage_include_unresolved",
				Message: fmt.Sprintf("include не резолвится офлайн (%s): %s — stage-граф проверен только по локально-доступным задачам", d.Code, d.Message),
				Hint:    "полная Passage-валидация выполнится на keeper-е, где доступен service-слой include",
			})
		}
	}

	plan, err := config.Stratify(tasks)
	if err != nil {
		var se *config.StratifyError
		code := "register_graph_invalid"
		if errors.As(err, &se) {
			code = se.Code
		}
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    scenarioPath,
			Code:    code,
			Message: err.Error(),
			Hint:    "staged-render не сможет упорядочить Passage по register-зависимости (ADR-056)",
		})
		return out
	}

	// serial + staged (N>1) больше НЕ ошибка (ADR-056 §S4 amend, S-2D1): 2D
	// serial×passage реализован — каждый Passage катит свои serial-волны по своему
	// per-Passage width. Стратификация даёт обычный passage_plan HINT (ниже).

	// Within-block register-зависимость — ОШИБКА (ADR-056, §«Риски — silent-wrong-
	// target»). Потомок block:, читающий register соседнего потомка ТОГО ЖЕ блока,
	// невозможен на render: block атомарен по Passage, peer-register доступен Soul-side
	// только ПОСЛЕ probe, а where/when/params резолвятся Keeper-side ДО dispatch → where
	// отберёт хосты по устаревшему/внешнему register молча. Stratify это не ловит
	// (внутри-блочное ребро не пересекает границу top-level задач). Ловим офлайн ДО apply.
	if info, bad := config.WithinBlockRegisterDependency(tasks); bad {
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    scenarioPath,
			Code:    config.CodeWithinBlockRegisterDependency,
			Message: fmt.Sprintf("задача %q внутри block: читает register %q, эмитнутый соседней %q ТОГО ЖЕ блока — невозможно на render (block атомарен, peer-register доступен только Soul-side ПОСЛЕ probe, а where/when/params резолвятся Keeper-side ДО dispatch)", info.ReaderName, info.RegisterName, info.EmitterName),
			Hint:    "вынесите probe на top-level (probe и потребитель — разные Passage; ADR-056 staged-render тогда упорядочит их штатно)",
		})
		return out
	}

	// Cross-passage when-gating — ОШИБКА (ADR-056:85 amend, FC-5). Задача гейтит
	// `when:`/`changed_when:`/`failed_when:` по register, эмитнутому в БОЛЕЕ РАННЕМ
	// Passage. flow-control = Soul-side per-task gating (ADR-012(d)), видит только
	// register СВОЕГО Passage; cross-passage register ему недоступен (другой
	// ApplyRequest) → `no such key` молча → задача FAILED. После narrow-fix flow-
	// control сам Passage НЕ расщепляет, но probe мог уехать в ранний Passage по
	// ДРУГОЙ причине (иная задача с `where: register.X`). where: это умеет (Keeper
	// пере-рендерит с накопленным register), when: — нет. Fail-closed reject офлайн.
	if info, bad := config.CrossPassageWhenGating(tasks, plan); bad {
		out = append(out, diag.Diagnostic{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    scenarioPath,
			Code:    config.CodeCrossPassageWhenGating,
			Message: fmt.Sprintf("задача %q гейтит %s: по register %q из другого Passage (consumer passage %d, источник passage %d) — Soul-side gating видит только свой Passage, cross-passage register ему недоступен → no such key", info.ConsumerName, info.Kind, info.RegisterName, info.ConsumerPassage, info.SourcePassage),
			Hint:    "when:/changed_when:/failed_when: по register из другого Passage не поддержан (Soul-side gating видит только свой Passage) — используй where: для cross-task register-таргетинга ИЛИ register.self для same-task gating",
		})
		return out
	}

	// Структура passage — HINT (info автору): сколько Passage и по сколько задач.
	out = append(out, diag.Diagnostic{
		Level:   diag.LevelHint,
		Phase:   diag.PhaseSemanticValidate,
		File:    scenarioPath,
		Code:    "passage_plan",
		Message: passagePlanSummary(plan),
	})

	return out
}

// passagePlanSummary — человекочитаемое описание стратификации: число Passage и
// размер каждого (N=1 → один проход, БИТ-В-БИТ как до staged-render).
func passagePlanSummary(plan config.Passage) string {
	counts := make([]int, plan.Count)
	for _, p := range plan.TaskPassage {
		if p >= 0 && p < plan.Count {
			counts[p]++
		}
	}
	if plan.Count <= 1 {
		return fmt.Sprintf("single-passage прогон (%d задач, без cross-task register-зависимости) — один проход, как до staged-render", len(plan.TaskPassage))
	}
	return fmt.Sprintf("staged-прогон: %d Passage по register-зависимости, задач в каждом %v (потребитель register исполняется строго после probe)", plan.Count, counts)
}

// scenarioLocalIncludeResolver — within-scenario [config.IncludeResolver] для
// офлайн-линта: include-цели резолвятся из директории main.yml (scenario-local
// слой двухуровневого резолва ADR-009; service-слой офлайн недоступен).
// path.Clean клампит выход за пределы директории сценария (`..`/абсолютный).
func scenarioLocalIncludeResolver(dir string) config.IncludeResolver {
	return func(name string) ([]byte, string, error) {
		rel := path.Clean("/" + name)[1:] // отрезает ведущий `..`/абсолют до scenario-root.
		full := filepath.Join(dir, rel)
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, "", err
		}
		return data, rel, nil
	}
}
