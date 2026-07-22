package config

// Narrow CEL evaluator for the input-schema key `required_when` (docs/input.md →
// "Conditional requiredness"). Deliberately does NOT depend on shared/cel: that
// carries the full Engine (vault()/glob()/merge()/coverage/soulprint.hosts) and
// all scenario/destiny context. required_when is input validation BEFORE the
// render phase, a pure function of input.*; it needs a minimal env with the SINGLE
// variable `input`. Any other name (essence/soulprint/register/…) → compile-error
// undeclared reference — the sandbox comes from undeclaration, like migration-CEL
// ([ADR-019]). cel-go is already in the shared module (pulls shared/cel), so a
// package-level config→cel-go dependency adds no new module-dep and creates no
// cycle (cel-go is an external library).

import (
	"fmt"
	"sync"

	"github.com/goccy/go-yaml/ast"
	"github.com/google/cel-go/cel"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// inputEnv — the required_when CEL environment: a single declared variable
// `input` (DynType, like the rest of Soul Stack context — schema-based typing is
// backlog). Immutable after first build; built lazily (env build is not free, and
// most schemas carry no required_when). Program cache is inputPrograms (keyed by
// normalized expression text).
var (
	inputEnvOnce sync.Once
	inputEnv     *cel.Env
	inputEnvErr  error

	inputProgMu sync.RWMutex
	inputProgs  = map[string]cel.Program{}
)

func requiredWhenEnv() (*cel.Env, error) {
	inputEnvOnce.Do(func() {
		inputEnv, inputEnvErr = cel.NewEnv(
			cel.StdLib(),
			cel.Variable("input", cel.DynType),
		)
		if inputEnvErr != nil {
			inputEnvErr = fmt.Errorf("building CEL environment for required_when: %w", inputEnvErr)
		}
	})
	return inputEnv, inputEnvErr
}

// compileRequiredWhen compiles (and caches) a required_when predicate against
// inputEnv. A compile error (syntax / unknown identifier outside `input` /
// incompatible types) is returned to the caller for classification: schema
// validator → input_required_when_invalid, runtime → input error.
func compileRequiredWhen(expr string) (cel.Program, error) {
	inputProgMu.RLock()
	prg, ok := inputProgs[expr]
	inputProgMu.RUnlock()
	if ok {
		return prg, nil
	}

	env, err := requiredWhenEnv()
	if err != nil {
		return nil, err
	}
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	prg, err = env.Program(ast)
	if err != nil {
		return nil, err
	}

	inputProgMu.Lock()
	inputProgs[expr] = prg
	inputProgMu.Unlock()
	return prg, nil
}

// evalRequiredWhen evaluates a required_when predicate over the merged input and
// coerces the result to bool. Used by requireInputValues at the input stage (AFTER
// mergeInputDefaults). merged is nil-safe (empty context). A non-bool predicate
// result is an error (required_when must return a boolean).
func evalRequiredWhen(expr string, merged map[string]any) (bool, error) {
	prg, err := compileRequiredWhen(expr)
	if err != nil {
		return false, fmt.Errorf("required_when %q: %w", expr, err)
	}
	if merged == nil {
		merged = map[string]any{}
	}
	out, _, err := prg.Eval(map[string]any{"input": merged})
	if err != nil {
		return false, fmt.Errorf("required_when %q: %w", expr, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("required_when %q returned %s, expected bool", expr, out.Type().TypeName())
	}
	return b, nil
}

// validateRequiredWhen — schema-time static check: required_when is a non-empty
// CEL string that parses/compiles against inputEnv. Applies to any type (the
// condition is over the rest of input, not the field itself). A parse error or a
// name outside `input` → input_required_when_invalid. kv is the `required_when`
// MappingValueNode (for line/col diagnostics).
func validateRequiredWhen(s *InputSchema, kv *ast.MappingValueNode, path string) []diag.Diagnostic {
	if s == nil {
		return nil
	}
	if s.RequiredWhen == "" {
		// Empty string is a meaningless predicate (no condition). Rejected
		// explicitly, else it's silently never-required (schema author footgun).
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_required_when_invalid",
			Message:  "required_when must be a non-empty CEL predicate over input.*",
			Hint:     `e.g. required_when: "input.redis_type == 'cluster'"`,
			YAMLPath: path + ".required_when",
		})}
	}
	if _, err := compileRequiredWhen(s.RequiredWhen); err != nil {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_required_when_invalid",
			Message:  fmt.Sprintf("required_when does not compile as CEL over input.*: %v", err),
			Hint:     "predicate may reference only input.* (no essence/soulprint/register/vault/now)",
			YAMLPath: path + ".required_when",
		})}
	}
	return nil
}
