package scenario

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// fakeInputLoader is a mock [InputScenarioLoader]: Load returns an empty
// artifact, ReadFile returns a preset YAML (or an error). No git stack.
type fakeInputLoader struct {
	yaml    string
	loadErr error
	readErr error
}

func (f *fakeInputLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return &artifact.ServiceArtifact{Ref: ref}, nil
}

func (f *fakeInputLoader) ReadFile(_ *artifact.ServiceArtifact, _ string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return []byte(f.yaml), nil
}

// scenarioWithRequiredInput is scenario `create` with one required field
// `name` (type=string, no default) and one optional `replicas` (with
// default).
const scenarioWithRequiredInput = `name: create
description: test scenario
state_changes: {}
input:
  name:
    type: string
    required: true
  replicas:
    type: integer
    default: 1
tasks:
  - name: noop
    module: core.exec.run
    params:
      cmd: echo
    changed_when: "false"
`

func TestValidateInput_RequiredMissing_ErrInputInvalid(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithRequiredInput}
	// input WITHOUT the required field `name` (reproduces the "ba" bug).
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", map[string]any{})
	if err == nil {
		t.Fatal("expected an error for a missing required field, got nil")
	}
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("err does not wrap ErrInputInvalid: %v", err)
	}
}

func TestValidateInput_RequiredMissing_NilInput(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithRequiredInput}
	// nil input (field absent from JSON entirely) — same rejection as `{}`.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", nil)
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("nil input: expected ErrInputInvalid, got %v", err)
	}
}

func TestValidateInput_RequiredProvided_OK(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithRequiredInput}
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"name": "alice"})
	if err != nil {
		t.Fatalf("valid input: %v", err)
	}
}

func TestValidateInput_DefaultPresent_OK(t *testing.T) {
	// `replicas` has a default → absence in provided is fine; `name` is passed.
	loader := &fakeInputLoader{yaml: scenarioWithRequiredInput}
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"name": "alice"})
	if err != nil {
		t.Fatalf("default field without a value should pass: %v", err)
	}
}

func TestValidateInput_TypeMismatch_ErrInputInvalid(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithRequiredInput}
	// `replicas` is declared integer; we pass a string → type mismatch.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"name": "alice", "replicas": "not-int"})
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("type-mismatch: expected ErrInputInvalid, got %v", err)
	}
}

func TestValidateInput_EmptyStringForRequired_ErrInputInvalid(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithRequiredInput}
	// Empty string for a required type=string without allow_empty is
	// treated as "not provided" (docs/input.md §"Empty strings") →
	// required violation.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"name": ""})
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("empty string for required: expected ErrInputInvalid, got %v", err)
	}
}

func TestValidateInput_NoSchema_OK(t *testing.T) {
	// Scenario without an `input:` block (like "ba" if create has no
	// required) — any provided passes; a nil schema doesn't reject.
	const noInput = `name: create
state_changes: {}
tasks:
  - name: noop
    module: core.exec.run
    params:
      cmd: echo
    changed_when: "false"
`
	loader := &fakeInputLoader{yaml: noInput}
	if err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", nil); err != nil {
		t.Fatalf("scenario without input: %v", err)
	}
	if err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"extra": "x"}); err != nil {
		t.Fatalf("scenario without input + unknown key: %v", err)
	}
}

func TestValidateInput_NilLoader_ConfigError(t *testing.T) {
	err := ValidateInput(context.Background(), nil, artifact.ServiceRef{Name: "svc"}, "create", nil)
	if err == nil {
		t.Fatal("nil loader must yield a config error")
	}
	// NOT ErrInputInvalid (this is a config failure, not validation) → handler returns 500.
	if errors.Is(err, ErrInputInvalid) {
		t.Fatalf("nil loader must not map to ErrInputInvalid: %v", err)
	}
}

func TestValidateInput_LoadError_Propagated(t *testing.T) {
	loader := &fakeInputLoader{loadErr: fmt.Errorf("git clone failed")}
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", nil)
	if err == nil {
		t.Fatal("snapshot load error must propagate")
	}
	if errors.Is(err, ErrInputInvalid) {
		t.Fatalf("load-error must not map to ErrInputInvalid (this is a 5xx): %v", err)
	}
}

// scenarioWithValidate is a scenario with a top-level `validate:` section
// (cross-field invariant "port is required if tls is off") + an asymmetric
// assert task.
const scenarioWithValidate = `name: create
input:
  tls:
    type: boolean
    default: false
  port:
    type: integer
    default: 0
validate:
  - that: "input.tls || input.port > 0"
    message: "either enable tls or set a positive port"
tasks:
  - name: noop
    module: core.exec.run
    params:
      cmd: echo
    changed_when: "false"
`

// TestValidateInput_ValidateRuleFalse_ErrValidateFailed: a false rule on the
// request path → ErrValidateFailed (handler → 422 validation_failed) BEFORE
// commit, NOT error_locked. A separate sentinel from ErrInputInvalid.
func TestValidateInput_ValidateRuleFalse_ErrValidateFailed(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithValidate}
	// tls=false (default), port=0 (default) → rule is false.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", map[string]any{})
	if err == nil {
		t.Fatal("expected an error for a violated validate rule, got nil")
	}
	if !errors.Is(err, ErrValidateFailed) {
		t.Fatalf("err does not wrap ErrValidateFailed: %v", err)
	}
	if errors.Is(err, ErrInputInvalid) {
		t.Fatalf("validate failure must not map to ErrInputInvalid (distinct sentinels): %v", err)
	}
}

// TestValidateInput_ValidateRuleTrue_OK: a true rule → passes.
func TestValidateInput_ValidateRuleTrue_OK(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithValidate}
	// port>0 satisfies the invariant.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"port": 6379})
	if err != nil {
		t.Fatalf("valid input (port>0): %v", err)
	}
	// tls=true also satisfies it (cross-field OR).
	err = ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"tls": true})
	if err != nil {
		t.Fatalf("valid input (tls=true): %v", err)
	}
}

// TestValidateInput_MultipleRules_FirstFalseMessage: multiple rules — the
// first false one wins, its message ends up in the error (short-circuit).
func TestValidateInput_MultipleRules_FirstFalseMessage(t *testing.T) {
	const multi = `name: create
input:
  port:
    type: integer
    default: 0
validate:
  - that: "input.port > 0"
    message: "PORT_MUST_BE_POSITIVE"
  - that: "input.port < 65536"
    message: "PORT_TOO_LARGE"
tasks: []
`
	loader := &fakeInputLoader{yaml: multi}
	// port=0 fails the first; the second (port<65536) is true — but the first wins.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", map[string]any{})
	if !errors.Is(err, ErrValidateFailed) {
		t.Fatalf("expected ErrValidateFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "PORT_MUST_BE_POSITIVE") {
		t.Fatalf("error must carry the first rule's message, got: %v", err)
	}
	if strings.Contains(err.Error(), "PORT_TOO_LARGE") {
		t.Fatalf("second rule's message must not appear (short-circuit): %v", err)
	}
}

// TestValidateInput_ValidateAfterSchema: a schema failure (type mismatch)
// hits BEFORE the validate rule — validate `that` assumes correct types.
// port=string → ErrInputInvalid (NOT ErrValidateFailed).
func TestValidateInput_ValidateAfterSchema(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithValidate}
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"port": "not-int"})
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("type-mismatch must yield ErrInputInvalid before validate: %v", err)
	}
	if errors.Is(err, ErrValidateFailed) {
		t.Fatalf("schema failure must not turn into a validate failure: %v", err)
	}
}
