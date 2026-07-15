package validate

// Stage validation for scenarios (ADR-056 §S5): an offline run of the SAME
// Passage stratification the keeper runtime performs before dispatch
// ([config.Stratify]), so the scenario author catches staged errors BEFORE
// apply. Reuses the canonical shared/config (Stratify over the same []Task
// plan + the config validator's reads==refs check): one register-dependency
// graph for both linter and runtime (a duplicate would mean silent-wrong-target).
//
// What this detects OFFLINE (on top of unknown_register_reference, already
// caught by the config validator at parse time):
//   - register cycle (StratifyCycle) — ERROR: no topological order exists,
//     the run would never have started.
//   - passage structure (how many Passages, how many tasks each) — HINT
//     (informational, for the author).
//
// serial: + staged (N>1 Passage) is no longer an error (ADR-056 §S4 amend,
// S-2D1): 2D serial×passage is implemented — each Passage runs its serial
// waves at its own per-Passage width. Such a scenario now passes lint with
// a plain passage_plan HINT.

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// stageDiagnostics runs Passage stratification over an already-parsed
// scenario and returns additional diagnostics (info/errors) that the caller
// appends to the parse diagnostics. scenarioPath is the path to main.yml
// (its directory is used for scenario-local resolution of include targets,
// mirroring the keeper's two-level resolve, but without the service layer —
// unavailable offline).
//
// m==nil (parse failed with errors) → no point stratifying (the graph is
// unreliable) → nil. An include resolve error → HINT "stage graph checked
// only against locally-resolved tasks" + stratification over whatever did
// resolve (we don't fail: the include may have pointed into the service
// layer, unavailable offline).
func stageDiagnostics(scenarioPath string, m *config.ScenarioManifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}

	dir := filepath.Dir(scenarioPath)
	var out []diag.Diagnostic

	// A conditional include with a dynamic `when:` (register./soulprint.) is a
	// static ERROR offline (conditional-include group-drop, ADR-009 amendment).
	// This is a property of the include task itself in m.Tasks, independent of
	// target resolution (so we check it BEFORE ExpandIncludes — otherwise the
	// downgrade-to-HINT below would mask it). Includes expand BEFORE Stratify:
	// register from earlier tasks isn't collected yet, per-host soulprint is
	// unknown → only a static predicate is allowed. Prod rejects this too
	// (ExpandIncludes → include_when_dynamic_unsupported), but soul-lint catches
	// it offline.
	out = append(out, dynamicIncludeWhenDiagnostics(scenarioPath, m.Tasks)...)

	tasks, expandDiags := config.ExpandIncludes(m.Tasks, scenarioLocalIncludeResolver(dir))
	// Offline include resolution is incomplete (no service layer): downgrade
	// expand's error diagnostics to HINT so they don't mask stage validation
	// with a false failure. A genuinely broken include is caught by full
	// validation on the keeper.
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

	// serial + staged (N>1) is no longer an error (ADR-056 §S4 amend, S-2D1): 2D
	// serial×passage is implemented — each Passage runs its own serial waves at
	// its own per-Passage width. Stratification yields a plain passage_plan HINT
	// (below).

	// A within-block register dependency is an ERROR (ADR-056, §"Risks —
	// silent-wrong-target"). A block: child reading the register of a sibling
	// child in the SAME block is impossible at render time: a block is atomic
	// per Passage, peer register is available Soul-side only AFTER probe, while
	// where/when/params are resolved Keeper-side BEFORE dispatch → where would
	// silently select hosts by a stale/foreign register. Stratify doesn't catch
	// this (the within-block edge doesn't cross a top-level task boundary).
	// Caught offline BEFORE apply.
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

	// Cross-passage when-gating is an ERROR (ADR-056:85 amend, FC-5). A task
	// gates `when:`/`changed_when:`/`failed_when:` on a register emitted in an
	// EARLIER Passage. flow-control is Soul-side per-task gating (ADR-012(d)),
	// visible only within its OWN Passage; a cross-passage register is
	// unavailable to it (a different ApplyRequest) → silent `no such key` →
	// the task FAILS. After the narrow-fix, flow-control itself doesn't split
	// the Passage, but a probe may have landed in an earlier Passage for a
	// DIFFERENT reason (another task with `where: register.X`). where: can do
	// this (Keeper re-renders with accumulated register), when: cannot.
	// Fail-closed reject, offline.
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

	// Passage structure — HINT (informational, for the author): how many
	// Passages and how many tasks each.
	out = append(out, diag.Diagnostic{
		Level:   diag.LevelHint,
		Phase:   diag.PhaseSemanticValidate,
		File:    scenarioPath,
		Code:    "passage_plan",
		Message: passagePlanSummary(plan),
	})

	return out
}

// passagePlanSummary is a human-readable description of the stratification:
// the number of Passages and the size of each (N=1 → a single pass,
// BIT-FOR-BIT identical to pre-staged-render behavior).
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

// dynamicIncludeWhenDiagnostics raises include_when_dynamic_unsupported for
// every include task with a non-empty NON-static `when:` (conditional-include
// group-drop, ADR-009 amendment). Target resolution is NOT needed — this is a
// property of the include node itself (the predicate), so offline static
// validation is complete and independent of the service layer. The walk
// recurses through block: (an include child of a block is rejected earlier
// in the pilot as ErrUnexpandedInclude, but the walk stays symmetric in case
// of future support). IsStaticIncludeWhen is the same criterion prod's
// ExpandIncludes uses (input./essence./incarnation./vars. — allowed;
// register./soulprint. — not).
func dynamicIncludeWhenDiagnostics(scenarioPath string, tasks []config.Task) []diag.Diagnostic {
	var out []diag.Diagnostic
	for i := range tasks {
		t := &tasks[i]
		if t.Include != nil && t.When != "" && !config.IsStaticIncludeWhen(t.When) {
			out = append(out, diag.Diagnostic{
				Level:   diag.LevelError,
				Phase:   diag.PhaseSemanticValidate,
				File:    scenarioPath,
				Code:    "include_when_dynamic_unsupported",
				Message: fmt.Sprintf("include %q несёт динамический when %q (ссылка на register./soulprint.) — include раскрывается ДО стратификации, доступен только статический предикат input./essence./incarnation./vars.", t.Include.Include, t.When),
				Hint:    "замените на статический предикат (input./essence./incarnation.) либо перенесите условие на module-задачу подключённого файла через when:",
			})
		}
		if t.Block != nil {
			out = append(out, dynamicIncludeWhenDiagnostics(scenarioPath, t.Block.Block)...)
		}
	}
	return out
}

// scenarioLocalIncludeResolver is a within-scenario [config.IncludeResolver]
// for the offline linter: include targets resolve from main.yml's directory
// (the scenario-local layer of the ADR-009 two-level resolve; the service
// layer is unavailable offline). path.Clean clamps escapes outside the
// scenario directory (`..`/absolute paths).
func scenarioLocalIncludeResolver(dir string) config.IncludeResolver {
	return func(name string) ([]byte, string, error) {
		rel := path.Clean("/" + name)[1:] // strips a leading `..`/absolute path up to scenario-root.
		full := filepath.Join(dir, rel)
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, "", err
		}
		return data, rel, nil
	}
}
