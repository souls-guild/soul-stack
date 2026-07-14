// Package cel wraps google/cel-go for all Soul Stack YAML expressions (top-level
// expression keys and `${ … }` interpolation per [ADR-010]).
//
// Pilot scope (M2.x scenario-runner, slice .c):
//   - Expression-key form: the whole string is CEL ([EvalExpression]).
//   - `${ … }` interpolation in string contexts ([EvalInterpolation]).
//   - Context variables: input, register, incarnation, soulprint.self,
//     essence, vars (task-level `vars:`, filled by render per-task).
//   - Compile-cache keyed by the normalized expression.
//
// Implemented:
//   - soulprint.hosts and .where(...) — compile-time AST rewrite (hosts.go).
//   - vault(...) — keeper-side Vault KV read in the CEL-render phase ([vault.go],
//     [ADR-017]); registered only when the Engine is built with a KVReader
//     ([WithVault]). Without a KVReader vault() stays [ErrUnsupported] (guard in
//     functions.go).
//   - Soul-side flow-control sandbox ([NewFlowControl], [ADR-012(d)]): a trimmed
//     env for when:/changed_when:/failed_when: predicates that depend on register.*
//     (prior-task results known only to the Soul during a run). No external
//     access: vault()/now() are guard-blocked, soulprint.hosts/soulprint.where by
//     isolation ([Engine.compile] forces allowHosts=false).
//
// Deliberately NOT in pilot (return [ErrUnsupported]):
//   - now(...) — eval-time clock (needs a decision on eval-time semantics).
//
// CEL is a sandbox by design ([templating.md §7.1]): no arbitrary I/O, network,
// or sleep. The only I/O function is vault() (a controlled Vault KV read via an
// injected KVReader, not an arbitrary syscall). The rest is the cel-go standard
// library (size, contains, timestamp arithmetic — pure).
//
// [ADR-010]: docs/adr/0010-templating.md
// [templating.md §7.1]: docs/templating.md
package cel

import (
	"fmt"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/parser"
)

// contextVars — context variables known to the CEL env. All DynType: typing from
// YAML schemas (input/essence/state_schema) is backlog ([templating.md §2.4],
// open type Q); until it lands, nodes get dyn.
//
// soulprint is declared as a variable (access `soulprint.self.<path>`);
// soulprint.hosts/.where are rejected at the guard stage (see unsupported.go).
var contextVars = []string{
	"input",
	"register",
	"incarnation",
	"soulprint",
	"essence",
	"vars",
	"compute",
}

// migrationVars — context variables of migration mode ([NewMigration],
// [ADR-019]). ONLY `state` is declared (the incarnation.state root, mutated as
// operations proceed). Other names (`register`/`soulprint`/`essence`/`input`/
// `incarnation`/`vars`) are NOT declared — referencing them is a compile-time
// undeclared-reference error, so the migration-CEL sandbox is enforced by
// non-declaration, not a textual guard ([docs/migrations.md § "Forbidden in
// migration-CEL"]). `vault()`/`now()` are additionally caught by existing guards
// (vaultGuard/unsupportedPatterns/internalIdentGuard in functions.go).
var migrationVars = []string{
	"state",
}

// flowControlVars — variables of the Soul-side flow-control sandbox
// ([NewFlowControl], [ADR-012(d)]). Same name set as the ordinary
// scenario/destiny pass ([contextVars]): register/input/incarnation/soulprint/
// essence/vars. Kept as a separate variable for self-documentation: the context
// of flow-control predicates (when:/changed_when:/failed_when:) is register.*
// (the Soul builds it from prior-task results) plus a flow_context snapshot
// (delivered by the Keeper). `state` is NOT declared (migration-only).
// soulprint.hosts/soulprint.where are rejected by construction: flow-control
// forces allowHosts=false (see [Engine.compile]) — a host accessor becomes an
// isolation compile-error. vault()/now() by the existing guards.
var flowControlVars = []string{
	"input",
	"register",
	"incarnation",
	"soulprint",
	"essence",
	"vars",
}

// Engine compiles and evaluates CEL expressions. Thread-safe: one Engine is
// reused by all runs; the compile-cache is guarded by an RWMutex.
//
// The cache stores compiled programs keyed by normalized expression text. One
// Engine per process suffices — env is immutable after [New]; natural
// invalidation by git ref happens a level up (the scenario cache key includes the
// git-ref, [templating.md §2.5]).
type Engine struct {
	env *cel.Env

	// noMacroParser — parser WITHOUT registered macros. Used only in the rewrite
	// phase (rewriteHostsWhere): exists/all/map/filter/exists_one/.where stay plain
	// CallKind and are therefore round-trippable by parser.Unparse. The final
	// env.Compile (macros enabled) expands .filter/.exists natively into a
	// comprehension. See [hosts.go].
	noMacroParser *parser.Parser

	mu    sync.RWMutex
	cache map[string]cel.Program

	// loopEnvs — child envs for `loop:` expressions, keyed by the sorted set of
	// loop names (`<as>`/`<index_as>`) joined by '\x00'. Iteration names are
	// arbitrary (author-defined), so they are declared not in the base env
	// (immutable after New) but in a child via env.Extend. Cached: loaded
	// scenarios have few unique loop-name sets. Guarded by the same mu as cache.
	loopEnvs map[string]*cel.Env

	// sink — DSL-coverage receiver ([ADR-023]). nil in prod (no-op, zero
	// overhead); non-nil only in the Trial runner. Set once at Engine build
	// (SetCoverageSink), before the run starts.
	sink CoverageSink

	// kv — Vault KV reader for the CEL vault() function ([ADR-017]). nil ⇒ vault()
	// is not registered (a call to it → ErrUnsupported by guard). Set once via
	// [WithVault] at Engine build; immutable after New (per-eval ctx is passed
	// through Vars.Ctx, see [Engine.EvalExpression]).
	kv KVReader

	// migration — Engine built in migration mode ([NewMigration], [ADR-019]): only
	// the `state` variable is declared, activation is built from Vars.State (see
	// [Vars.activation]). Immutable after the constructor; affects only the set of
	// declared env names and the activation shape.
	migration bool

	// flowControl — Engine built in Soul-side flow-control mode ([NewFlowControl],
	// [ADR-012(d)]): a sandboxed env for when:/changed_when:/failed_when:
	// predicates. Immutable after the constructor. Forces allowHosts=false at
	// compile (see [Engine.compile]): soulprint.hosts/soulprint.where are
	// unavailable (cross-host, scenario-only) regardless of Vars.AllowHosts.
	// Activation is ordinary (not migration): register/input/… +
	// soulprint.{self,hosts} (hosts is always empty — the accessor is cut at
	// compile).
	flowControl bool
}

// Option configures the Engine at build ([New]). Applied before the CEL env is
// created, so env-affecting options (e.g. registering vault()) are honored at
// NewEnv.
type Option func(*engineConfig)

// engineConfig — Engine configuration assembled from options. Intermediate:
// after applying options, New builds the env and moves the fields into the
// Engine.
type engineConfig struct {
	kv KVReader
}

// WithVault enables the CEL vault() function ([templating.md §2.3], [ADR-017]),
// reading secrets through kv in the CEL-render phase. A nil kv ⇒ the option is a
// no-op (vault() stays unregistered). Passed to [New] by the keeper-side render
// (a real *vault.Client) or by offline modes (a fixture-backed reader).
func WithVault(kv KVReader) Option {
	return func(c *engineConfig) { c.kv = kv }
}

// New builds an Engine with the Soul Stack CEL env: cel-go stdlib + the
// [contextVars] context variables. With [WithVault] the vault() function is also
// registered (see [vaultEnvOptions]). An error is possible only on an
// incompatible cel-go config (programmatic, not user error).
func New(opts ...Option) (*Engine, error) {
	return buildEngine(engineMode{}, contextVars, opts...)
}

// NewFlowControl builds an Engine in Soul-side flow-control mode ([ADR-012(d)]):
// a sandboxed CEL env for when:/changed_when:/failed_when: predicates that depend
// on register.* (prior-task results known only to the Soul during a run).
// [flowControlVars] are declared; vault()/now() are blocked (by guards),
// soulprint.hosts/soulprint.where by isolation (allowHosts is forced false),
// `state` is undeclared. The Soul pulls cel-go but NOT the vault client: external
// access is keeper-only.
//
// [WithVault] must not be used here (no Vault tokens on the host, external access
// keeper-only); if a KVReader is passed anyway the constructor returns an error
// (symmetric to [NewMigration]).
func NewFlowControl(opts ...Option) (*Engine, error) {
	return buildEngine(engineMode{flowControl: true}, flowControlVars, opts...)
}

// NewMigration builds an Engine in migration mode ([ADR-019],
// [docs/migrations.md]): ONLY the `state` variable is declared ([migrationVars]).
// The sandbox is enforced by non-declaration of the other context names
// (register/soulprint/essence/input/incarnation/vars → undeclared-reference
// compile error); `vault()`/`now()` by the existing guards (functions.go).
// Activation is built from Vars.State.
//
// [WithVault] should not be used here (a migration must not pull secrets); if a
// KVReader is passed anyway, vault() is registered, but that contradicts the spec
// and should be rejected higher up the stack.
func NewMigration(opts ...Option) (*Engine, error) {
	return buildEngine(engineMode{migration: true}, migrationVars, opts...)
}

// engineMode — mutually exclusive special Engine modes (zero-value = ordinary
// scenario/destiny mode). Both forbid WithVault: sandboxes with no external
// access.
type engineMode struct {
	migration   bool // [NewMigration], [ADR-019]: only `state` is declared.
	flowControl bool // [NewFlowControl], [ADR-012(d)]: Soul-side flow-control sandbox.
}

// buildEngine — shared constructor: an env over cel.StdLib() with the declared
// vars and (if a KVReader) vault(). mode switches the activation shape (see
// [Vars.activation]) and host-accessor isolation (see [Engine.compile]).
func buildEngine(mode engineMode, vars []string, opts ...Option) (*Engine, error) {
	var cfg engineConfig
	for _, o := range opts {
		o(&cfg)
	}

	// migration-CEL ([ADR-019]) and flow-control-CEL ([ADR-012(d)]) are sandboxes
	// with no external access: vault() is forbidden (a migration is a pure function
	// of state; a Soul host has no Vault tokens, external access is keeper-only).
	// WithVault(kv) in these modes is API misuse (a latent foot-gun); we reject it
	// here instead of silently registering vault().
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
	// glob() — pure shell-glob matching ([ADR-040], target.where). merge() — pure
	// SHALLOW last-wins map merge ([ADR-010 Amendment 2026-06-22], translating a
	// simple input → a detailed config). default() — pure "value-or-default" macro
	// ([ADR-010 Amendment 2026-06-23], parity with Ansible `| default()`;
	// compile-time rewrite to has(x)?x:y). All pure (no external context),
	// registered in the Keeper-full and flow-control env but NOT in migration-CEL
	// ([ADR-019]): a migration is a minimal-surface sandbox (`state` + stdlib ops
	// only); extending it needs a separate ADR.
	if !mode.migration {
		envOpts = append(envOpts, globEnvOptions()...)
		envOpts = append(envOpts, mergeEnvOptions()...)
		envOpts = append(envOpts, defaultEnvOptions()...)
	}

	env, err := cel.NewEnv(envOpts...)
	if err != nil {
		return nil, fmt.Errorf("создание CEL-окружения: %w", err)
	}

	// Parser without macros (no Macros(...) options). Needed only for the rewrite
	// phase: see the Engine.noMacroParser field.
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

// loopEnv returns a child env that, on top of the base contextVars, declares the
// loop-variable names (as DynType, like the base ones). names must be sorted (see
// Vars.loopNames) — it is the cache key. Empty names → the base env (no loop
// variables).
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

// compile returns the compiled program for expression expr, compiled against env.
// The text must already be normalized (see normalize). On a syntax/type error —
// [ErrCompile].
//
// allowHosts permits soulprint.hosts/soulprint.where(...) (true in the scenario
// pass, false in the destiny pass — isolation, orchestration.md §4.1).
// soulprint.hosts.where(...)/soulprint.where(...) is rewritten by an AST pass
// (rewriteHostsWhere) into a native filter-comprehension BEFORE compile — the
// rewritten result is cached.
//
// The cache key includes the env discriminator (loopKey) and allowHosts: a
// program compiled against a child loop-env is incompatible with the base
// (different declared-variable set); allowHosts changes the outcome for the same
// text (rewrite vs isolation error). loopKey == "" — the base env.
func (e *Engine) compile(env *cel.Env, loopKey, expr string, allowHosts bool) (cel.Program, error) {
	// flow-control mode ([NewFlowControl]) forces host-accessor isolation:
	// soulprint.hosts/soulprint.where are unavailable regardless of Vars.AllowHosts
	// (cross-host, scenario-only — the Soul has none). Guards against a caller that
	// accidentally set Vars.AllowHosts=true.
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

// Reset clears the compile-cache and the loop-env cache. Intended for tests; in
// prod the caches grow boundedly (the number of unique expressions and loop-name
// sets in loaded scenarios).
func (e *Engine) Reset() {
	e.mu.Lock()
	clear(e.cache)
	clear(e.loopEnvs)
	e.mu.Unlock()
}
