package config

import (
	"errors"
	"fmt"

	"github.com/goccy/go-yaml"
)

// The schema of `<service>/vars/_stack.yaml` — the declarative service-vars
// pipeline of [ADR-0082](docs/adr/0082-service-vars.md).
//
// The schema lives here rather than beside the resolver because two binaries
// have to agree on it: `keeper` executes the stack, and `soul-lint` reports a
// malformed one offline as `stack_step_invalid`, before anything is registered.
// A linter carrying its own second copy of a schema is how a file starts
// passing one and failing the other — the whole point of the offline check is
// that it answers the same question the runtime will.
//
// What is NOT here is the execution: CEL evaluation of `when:`/`foreach:`, the
// layer merge, and path containment stay in `keeper/internal/servicevars`. This
// file is the shape a step may have; that package decides what a step does.

// Merge strategies for one stack step (ADR-0082 §4).
const (
	// StrategyDeep — maps merge recursively, scalars and lists are replaced
	// wholesale. The default, and what every layer did before `_stack.yaml`.
	StrategyDeep = "deep"
	// StrategyReplace — each top-level key of the layer replaces the
	// accumulated value ENTIRELY, without recursing into it.
	//
	// Not a convenience. A service documenting an `install_package` map means
	// "it is the whole map that an override replaces, not individual keys",
	// while a deep merge leaves the base's `gpg_key_url` attached to an
	// overridden `repo_uri` — a mirror URL from one place and its signing key
	// from another. That intent had no way to be expressed.
	StrategyReplace = "replace"
)

// StackFileName / StackFileMisspelt — the pipeline file, and the spelling that
// is refused rather than ignored. `_stack.yml` sitting unread next to a `vars/`
// that resolved lexically is a service whose author believes it is conditional.
const (
	StackFileName     = "_stack.yaml"
	StackFileMisspelt = "_stack.yml"
)

// ErrStackStepInvalid — a malformed `vars/_stack.yaml`. Fail-closed: a stack
// that cannot be read as written must not resolve to a plausible-looking subset
// of itself. soul-lint reports the same class statically as `stack_step_invalid`.
var ErrStackStepInvalid = errors.New("service vars: invalid _stack.yaml step")

// ServiceVarsStack — the parsed `vars/_stack.yaml`.
type ServiceVarsStack struct {
	Stack []ServiceVarsStackStep `yaml:"stack"`
}

// ServiceVarsStackStep — one step of the declarative pipeline.
//
// Exactly one of File / Inline carries the layer. Foreach repeats the step once
// per element of a list, binding the element to the name in As. When gates the
// step (a bare CEL bool, no `${ }`). Optional tolerates a missing File.
type ServiceVarsStackStep struct {
	File     string         `yaml:"file"`
	Inline   map[string]any `yaml:"inline"`
	When     string         `yaml:"when"`
	Optional bool           `yaml:"optional"`
	Foreach  string         `yaml:"foreach"`
	As       string         `yaml:"as"`
	Strategy string         `yaml:"strategy"`
}

// stackContextNames — every name the step env declares (`cel.NewServiceVars`). A
// `foreach:` binding may not take one of them: the shadowed value would still be
// referenced by the same spelling, and which one won would depend on where the
// reader looked. The list must stay in step with that env — the env is
// deliberately two names, so this is two names.
var stackContextNames = map[string]bool{"incarnation": true, "vars": true}

// ParseServiceVarsStack decodes and validates the contents of a `_stack.yaml`.
//
// Decoding is STRICT ([yaml.Strict]), matching the trial case loader: an unknown
// key is an error, not a silent skip. Without it a typo has no symptom at all —
// `whn:` for `when:` drops the gate and applies the layer unconditionally,
// `strateg:` for `strategy:` deep-merges where the author asked for a replace,
// and Validate never sees either, because it only inspects fields that parsed.
// That is the fail-open this error type exists to prevent.
//
// Every returned error wraps [ErrStackStepInvalid], so a caller can answer
// "this stack is malformed" without matching on message text.
func ParseServiceVarsStack(data []byte) (*ServiceVarsStack, error) {
	var doc ServiceVarsStack
	if err := yaml.UnmarshalWithOptions(data, &doc, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", ErrStackStepInvalid, StackFileName, err)
	}
	// A stack file that declares no steps is a typo, not a statement. `steps:`
	// for `stack:` parses cleanly under any decoder that tolerates it, and the
	// result — every var of the service gone, no error — is the most
	// plausible-looking wrong answer available. A service that genuinely has no
	// vars has no `vars/` directory.
	if len(doc.Stack) == 0 {
		return nil, fmt.Errorf("%w: %s declares no steps under `stack:` (a service with no vars needs no vars/ directory)",
			ErrStackStepInvalid, StackFileName)
	}
	for i := range doc.Stack {
		if err := doc.Stack[i].Validate(); err != nil {
			return nil, fmt.Errorf("%s step %d: %w", StackFileName, i+1, err)
		}
	}
	return &doc, nil
}

// Validate checks a step's shape before anything is evaluated, so a typo is an
// error about the typo rather than a layer that quietly did not apply.
func (s *ServiceVarsStackStep) Validate() error {
	switch {
	case s.File == "" && s.Inline == nil:
		return fmt.Errorf("%w: needs one of file: or inline:", ErrStackStepInvalid)
	case s.File != "" && s.Inline != nil:
		return fmt.Errorf("%w: file: and inline: are mutually exclusive", ErrStackStepInvalid)
	case s.Foreach != "" && s.As == "":
		return fmt.Errorf("%w: foreach: requires as:", ErrStackStepInvalid)
	case s.Foreach == "" && s.As != "":
		return fmt.Errorf("%w: as: without foreach:", ErrStackStepInvalid)
	case s.As != "" && stackContextNames[s.As]:
		return fmt.Errorf("%w: as: %q shadows the step context", ErrStackStepInvalid, s.As)
	case s.Optional && s.File == "":
		return fmt.Errorf("%w: optional: applies to file:, an inline layer is always present", ErrStackStepInvalid)
	}
	switch s.Strategy {
	case "", StrategyDeep, StrategyReplace:
	default:
		return fmt.Errorf("%w: strategy: %q is not deep or replace", ErrStackStepInvalid, s.Strategy)
	}
	return nil
}
