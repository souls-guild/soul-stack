// Package cel — обёртка над google/cel-go для всех YAML-выражений Soul Stack
// (top-level expression-ключи и `${ … }`-интерполяция по [ADR-010]).
//
// Pilot-scope (M2.x scenario-runner, slice .c):
//   - Expression-key form: вся строка = CEL ([EvalExpression]).
//   - `${ … }`-интерполяция в строковых контекстах ([EvalInterpolation]).
//   - Переменные контекста: input, register, incarnation, soulprint.self,
//     essence, vars (task-level `vars:`, заполняется render-ом per-task).
//   - Compile-cache по нормализованному выражению.
//
// Реализованы:
//   - soulprint.hosts и .where(...) — compile-time AST-rewrite (hosts.go).
//   - vault(...) — keeper-side чтение Vault KV в CEL-render-фазе ([vault.go],
//     [ADR-017]); регистрируется только при сборке Engine с KVReader ([WithVault]).
//     Без KVReader vault() остаётся [ErrUnsupported] (guard в functions.go).
//   - Soul-side flow-control sandbox ([NewFlowControl], [ADR-012(d)]): урезанный
//     env для предикатов when:/changed_when:/failed_when:, зависящих от register.*
//     (результатов предыдущих задач, известных только Soul-у во время прогона).
//     Без внешнего доступа: vault()/now() запрещены guard-ами, soulprint.hosts/
//     soulprint.where — изоляцией ([Engine.compile] форсит allowHosts=false).
//
// Сознательно НЕ в pilot (возвращают [ErrUnsupported]):
//   - now(...) — eval-time время (нужно решение о eval-time семантике).
//
// CEL — sandbox by design ([templating.md §7.1]): нет произвольного I/O, сети,
// sleep. Единственная I/O-функция — vault() (контролируемое чтение Vault KV
// через инъектированный KVReader, не произвольный syscall). Прочее —
// стандартная библиотека cel-go (size, contains, timestamp-арифметика — pure).
//
// [ADR-010]: docs/adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов
// [templating.md §7.1]: docs/templating.md
package cel

import (
	"fmt"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/parser"
)

// contextVars — переменные контекста, известные CEL-окружению. Все —
// DynType: типизация по YAML-схемам (input/essence/state_schema) — backlog
// ([templating.md §2.4], open Q типов), до её закрытия узлы получают dyn.
//
// soulprint объявлена как переменная (доступ `soulprint.self.<path>`);
// soulprint.hosts/.where отсекаются на этапе guard (см. unsupported.go).
var contextVars = []string{
	"input",
	"register",
	"incarnation",
	"soulprint",
	"essence",
	"vars",
}

// migrationVars — переменные контекста migration-режима ([NewMigration],
// [ADR-019]). Объявлена ТОЛЬКО `state` (мутируемый по ходу операций корень
// incarnation.state). Прочие имена (`register`/`soulprint`/`essence`/`input`/
// `incarnation`/`vars`) НЕ объявлены — обращение к ним → compile-ошибка
// undeclared reference, то есть migration-CEL sandbox обеспечивается
// необъявленностью, а не текстовым guard-ом ([docs/migrations.md §«Запрещено в
// migration-CEL»]). `vault()`/`now()` дополнительно ловятся существующими
// guard-ами (vaultGuard/unsupportedPatterns/internalIdentGuard в functions.go).
var migrationVars = []string{
	"state",
}

// flowControlVars — переменные Soul-side flow-control-песочницы ([NewFlowControl],
// [ADR-012(d)]). Тот же набор имён, что в обычном scenario/destiny-проходе
// ([contextVars]): register/input/incarnation/soulprint/essence/vars. Объявлен
// отдельной переменной для самодокументируемости: контекст flow-control-предикатов
// (when:/changed_when:/failed_when:) — register.* (Soul строит из результатов
// предыдущих задач) + flow_context-снапшот (Keeper доставляет). `state` НЕ
// объявлена (migration-only). soulprint.hosts/soulprint.where отсекаются
// конструктивно: flow-control форсит allowHosts=false (см. [Engine.compile]) —
// host-аксессор → compile-error изоляции. vault()/now() — существующими guard-ами.
var flowControlVars = []string{
	"input",
	"register",
	"incarnation",
	"soulprint",
	"essence",
	"vars",
}

// Engine компилирует и вычисляет CEL-выражения. Потокобезопасен: один
// Engine переиспользуется всеми прогонами; compile-cache защищён RWMutex.
//
// Кеш хранит скомпилированные программы по нормализованному тексту
// выражения. Один Engine на процесс достаточно — env неизменен после
// [New]; естественная инвалидация по git ref происходит уровнем выше
// (ключ кеша scenario включает git-ref, [templating.md §2.5]).
type Engine struct {
	env *cel.Env

	// noMacroParser — парсер БЕЗ зарегистрированных макросов. Используется
	// только в фазе rewrite (rewriteHostsWhere): exists/all/map/filter/
	// exists_one/.where остаются plain CallKind и потому round-trip'абельны
	// parser.Unparse. Финальный env.Compile (макросы включены) раскрывает
	// .filter/.exists нативно в comprehension. См. [hosts.go].
	noMacroParser *parser.Parser

	mu    sync.RWMutex
	cache map[string]cel.Program

	// loopEnvs — дочерние env-ы для `loop:`-выражений, ключ — отсортированный
	// набор loop-имён (`<as>`/`<index_as>`), соединённый через '\x00'. Имена
	// итерации произвольны (заданы автором), поэтому объявляются не в базовом
	// env (он неизменен после New), а в дочернем через env.Extend. Кешируются:
	// уникальных наборов loop-имён в загруженных scenario немного. Защищён тем
	// же mu, что и cache.
	loopEnvs map[string]*cel.Env

	// sink — приёмник DSL-coverage ([ADR-023]). nil в проде (no-op,
	// нулевой оверхед); ненулевой только в раннере Trial. Ставится один
	// раз при сборке Engine (SetCoverageSink), до старта прогона.
	sink CoverageSink

	// kv — Vault KV-reader для CEL-функции vault() ([ADR-017]). nil ⇒ vault()
	// не зарегистрирован (обращение к нему → ErrUnsupported guard-ом). Ставится
	// один раз через [WithVault] при сборке Engine; immutable после New (per-eval
	// ctx прокидывается через Vars.Ctx, см. [Engine.EvalExpression]).
	kv KVReader

	// migration — Engine собран в migration-режиме ([NewMigration], [ADR-019]):
	// объявлена только переменная `state`, активация строится из Vars.State (см.
	// [Vars.activation]). Immutable после конструктора; влияет лишь на набор
	// объявленных имён env и форму активации.
	migration bool

	// flowControl — Engine собран в Soul-side flow-control-режиме ([NewFlowControl],
	// [ADR-012(d)]): sandboxed env для предикатов when:/changed_when:/failed_when:.
	// Immutable после конструктора. Форсит allowHosts=false на компиляции (см.
	// [Engine.compile]): soulprint.hosts/soulprint.where недоступны (cross-host,
	// scenario-only) независимо от Vars.AllowHosts. Активация — обычная (не
	// migration): register/input/… + soulprint.{self,hosts} (hosts всегда пуст —
	// аксессор отсечён на compile).
	flowControl bool
}

// Option настраивает Engine при сборке ([New]). Применяется до создания
// CEL-окружения — опции, влияющие на env (например, регистрация vault()),
// учитываются при NewEnv.
type Option func(*engineConfig)

// engineConfig — собранная из опций конфигурация Engine. Промежуточная: после
// применения опций New строит env и переносит поля в Engine.
type engineConfig struct {
	kv KVReader
}

// WithVault включает CEL-функцию vault() ([templating.md §2.3], [ADR-017]),
// читающую секреты через kv в CEL-render-фазе. nil kv ⇒ опция no-op (vault()
// остаётся незарегистрированным). Передаётся в [New] keeper-side render-ом
// (реальный *vault.Client) либо офлайн-режимами (fixture-backed reader).
func WithVault(kv KVReader) Option {
	return func(c *engineConfig) { c.kv = kv }
}

// New создаёт Engine с CEL-окружением Soul Stack: stdlib cel-go +
// переменные контекста [contextVars]. С [WithVault] дополнительно регистрируется
// функция vault() (см. [vaultEnvOptions]). Ошибка возможна только при
// несовместимой конфигурации cel-go (программная, не пользовательская).
func New(opts ...Option) (*Engine, error) {
	return buildEngine(engineMode{}, contextVars, opts...)
}

// NewFlowControl создаёт Engine в Soul-side flow-control-режиме ([ADR-012(d)]):
// sandboxed CEL-песочница для предикатов when:/changed_when:/failed_when:, которые
// зависят от register.* (результатов предыдущих задач, известных только Soul-у во
// время прогона). Объявлены [flowControlVars]; vault()/now() запрещены (guard-ами),
// soulprint.hosts/soulprint.where — изоляцией (allowHosts форсится false), `state`
// — необъявлена. Soul тянет cel-go, но НЕ vault-client: внешний доступ keeper-only.
//
// [WithVault] здесь применять нельзя (Vault-токенов на хосте нет, внешний доступ
// keeper-only); если KVReader всё же передан — конструктор возвращает ошибку
// (симметрично [NewMigration]).
func NewFlowControl(opts ...Option) (*Engine, error) {
	return buildEngine(engineMode{flowControl: true}, flowControlVars, opts...)
}

// NewMigration создаёт Engine в migration-режиме ([ADR-019], [docs/migrations.md]):
// объявлена ТОЛЬКО переменная `state` ([migrationVars]). Sandbox обеспечивается
// необъявленностью прочих контекст-имён (register/soulprint/essence/input/
// incarnation/vars → compile-ошибка undeclared reference); `vault()`/`now()` —
// существующими guard-ами (functions.go). Активация строится из Vars.State.
//
// [WithVault] здесь применять не следует (миграция не должна тянуть секреты);
// если KVReader всё же передан — vault() зарегистрируется, но это
// противоречит спеке и должно отсекаться выше по стеку.
func NewMigration(opts ...Option) (*Engine, error) {
	return buildEngine(engineMode{migration: true}, migrationVars, opts...)
}

// engineMode — взаимоисключающие специальные режимы Engine (zero-value =
// обычный scenario/destiny-режим). Оба запрещают WithVault: песочницы без
// внешнего доступа.
type engineMode struct {
	migration   bool // [NewMigration], [ADR-019]: объявлена только `state`.
	flowControl bool // [NewFlowControl], [ADR-012(d)]: Soul-side flow-control sandbox.
}

// buildEngine — общий конструктор: env над cel.StdLib() с объявленными vars и
// (при KVReader) vault(). mode переключает форму активации (см. [Vars.activation])
// и изоляцию host-аксессора (см. [Engine.compile]).
func buildEngine(mode engineMode, vars []string, opts ...Option) (*Engine, error) {
	var cfg engineConfig
	for _, o := range opts {
		o(&cfg)
	}

	// migration-CEL ([ADR-019]) и flow-control-CEL ([ADR-012(d)]) — песочницы без
	// внешнего доступа: vault() запрещён (миграция — чистая функция от state; на
	// Soul-хосте Vault-токенов нет, внешний доступ keeper-only). WithVault(kv) в
	// этих режимах — мисюз API (latent foot-gun), отсекаем здесь, а не молча
	// регистрируем vault().
	if (mode.migration || mode.flowControl) && cfg.kv != nil {
		switch {
		case mode.migration:
			return nil, fmt.Errorf("сборка migration-Engine: WithVault несовместим с migration-режимом (ADR-019: migration-CEL sandbox запрещает vault())")
		default:
			return nil, fmt.Errorf("сборка flow-control-Engine: WithVault несовместим с flow-control-режимом (ADR-012(d): Soul-CEL sandbox без внешнего доступа, vault() keeper-only)")
		}
	}

	envOpts := make([]cel.EnvOption, 0, len(vars)+5)
	envOpts = append(envOpts, cel.StdLib())
	for _, name := range vars {
		envOpts = append(envOpts, cel.Variable(name, cel.DynType))
	}
	if cfg.kv != nil {
		envOpts = append(envOpts, vaultEnvOptions()...)
	}
	// glob() — pure-функция shell-glob matching ([ADR-040], target.where).
	// В migration-CEL ([ADR-019]) НЕ регистрируется: миграция — sandbox с минимумом
	// surface area (только `state` + stdlib-операции), расширение требует отдельного
	// ADR. Flow-control ([ADR-012(d)]) glob() получает — pure, без внешнего контекста.
	if !mode.migration {
		envOpts = append(envOpts, globEnvOptions()...)
	}

	env, err := cel.NewEnv(envOpts...)
	if err != nil {
		return nil, fmt.Errorf("создание CEL-окружения: %w", err)
	}

	// Парсер без макросов (опции Macros(...) не передаём). Нужен только для
	// rewrite-фазы: см. поле Engine.noMacroParser.
	noMacroParser, err := parser.NewParser()
	if err != nil {
		return nil, fmt.Errorf("создание CEL-парсера без макросов: %w", err)
	}

	return &Engine{
		env:           env,
		noMacroParser: noMacroParser,
		cache:         make(map[string]cel.Program),
		loopEnvs:      make(map[string]*cel.Env),
		kv:            cfg.kv,
		migration:     mode.migration,
		flowControl:   mode.flowControl,
	}, nil
}

// loopEnv возвращает дочерний env, в котором поверх базовых contextVars
// объявлены имена loop-переменных names (как DynType, симметрично базовым).
// names должны быть отсортированы (см. Vars.loopNames) — это ключ кеша.
// Пустой names → базовый env (без loop-переменных).
func (e *Engine) loopEnv(names []string) (*cel.Env, error) {
	if len(names) == 0 {
		return e.env, nil
	}
	key := strings.Join(names, "\x00")

	e.mu.RLock()
	env, ok := e.loopEnvs[key]
	e.mu.RUnlock()
	if ok {
		return env, nil
	}

	opts := make([]cel.EnvOption, 0, len(names))
	for _, name := range names {
		opts = append(opts, cel.Variable(name, cel.DynType))
	}
	env, err := e.env.Extend(opts...)
	if err != nil {
		return nil, fmt.Errorf("расширение CEL-окружения loop-переменными %v: %w", names, err)
	}

	e.mu.Lock()
	e.loopEnvs[key] = env
	e.mu.Unlock()
	return env, nil
}

// compile возвращает скомпилированную программу для выражения expr,
// скомпилированную против env. Текст уже должен быть нормализован (см.
// normalize). При синтаксической/типовой ошибке — [ErrCompile].
//
// allowHosts разрешает soulprint.hosts/soulprint.where(...) (true в scenario-
// проходе, false в destiny-проходе — изоляция, orchestration.md §4.1).
// soulprint.hosts.where(...)/soulprint.where(...) переписывается AST-проходом
// (rewriteHostsWhere) в нативный filter-comprehension ДО compile — кешируется
// уже переписанный результат.
//
// Ключ кеша включает дискриминатор env (loopKey) и allowHosts: программа,
// скомпилированная против дочернего loop-env, несовместима с базовым (разный
// набор объявленных переменных); allowHosts меняет исход для одного и того же
// текста (rewrite vs ошибка изоляции). loopKey == "" — базовый env.
func (e *Engine) compile(env *cel.Env, loopKey, expr string, allowHosts bool) (cel.Program, error) {
	// flow-control-режим ([NewFlowControl]) форсит изоляцию host-аксессора:
	// soulprint.hosts/soulprint.where недоступны независимо от Vars.AllowHosts
	// (cross-host, scenario-only — Soul их не имеет). Защита от caller-а, случайно
	// выставившего Vars.AllowHosts=true.
	if e.flowControl {
		allowHosts = false
	}

	cacheKey := expr
	if loopKey != "" {
		cacheKey = loopKey + "\x01" + expr
	}
	if !allowHosts {
		cacheKey = "\x02" + cacheKey
	}

	e.mu.RLock()
	prg, ok := e.cache[cacheKey]
	e.mu.RUnlock()
	if ok {
		return prg, nil
	}

	if err := guardUnsupported(expr, e.kv != nil); err != nil {
		return nil, err
	}

	compiled, err := e.rewriteHostsWhere(expr, allowHosts)
	if err != nil {
		return nil, err
	}

	ast, issues := env.Compile(compiled)
	if issues != nil && issues.Err() != nil {
		return nil, &ErrCompile{Expr: expr, Err: issues.Err()}
	}

	prg, err = env.Program(ast)
	if err != nil {
		return nil, &ErrCompile{Expr: expr, Err: err}
	}

	e.mu.Lock()
	e.cache[cacheKey] = prg
	e.mu.Unlock()

	return prg, nil
}

// Reset очищает compile-cache и кеш loop-env. Предназначен для тестов; в
// проде кеши растут ограниченно (число уникальных выражений и наборов
// loop-имён в загруженных scenario).
func (e *Engine) Reset() {
	e.mu.Lock()
	clear(e.cache)
	clear(e.loopEnvs)
	e.mu.Unlock()
}
