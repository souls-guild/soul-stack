package scenario

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// fakeInputLoader — мок [InputScenarioLoader]: Load возвращает пустой артефакт,
// ReadFile отдаёт заранее заданный YAML (или ошибку). Без git-стека.
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

// scenarioWithRequiredInput — scenario `create` с одним required-полем `name`
// (type=string, без default) и одним опциональным `replicas` (с default).
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
	// input БЕЗ required-поля `name` (воспроизведение бага "ba").
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", map[string]any{})
	if err == nil {
		t.Fatal("ожидалась ошибка для отсутствующего required-поля, got nil")
	}
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("err не оборачивает ErrInputInvalid: %v", err)
	}
}

func TestValidateInput_RequiredMissing_NilInput(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithRequiredInput}
	// nil input (поле отсутствует в JSON вовсе) — тот же отказ, что и `{}`.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", nil)
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("nil input: ожидался ErrInputInvalid, got %v", err)
	}
}

func TestValidateInput_RequiredProvided_OK(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithRequiredInput}
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"name": "alice"})
	if err != nil {
		t.Fatalf("валидный input: %v", err)
	}
}

func TestValidateInput_DefaultPresent_OK(t *testing.T) {
	// `replicas` имеет default → отсутствие в provided допустимо; `name` передан.
	loader := &fakeInputLoader{yaml: scenarioWithRequiredInput}
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"name": "alice"})
	if err != nil {
		t.Fatalf("default-поле без передачи должно проходить: %v", err)
	}
}

func TestValidateInput_TypeMismatch_ErrInputInvalid(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithRequiredInput}
	// `replicas` объявлен integer; передаём строку → type-mismatch.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"name": "alice", "replicas": "not-int"})
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("type-mismatch: ожидался ErrInputInvalid, got %v", err)
	}
}

func TestValidateInput_EmptyStringForRequired_ErrInputInvalid(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithRequiredInput}
	// Пустая строка для required type=string без allow_empty трактуется как
	// «не передано» (docs/input.md §«Пустые строки») → required-нарушение.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"name": ""})
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("пустая строка для required: ожидался ErrInputInvalid, got %v", err)
	}
}

func TestValidateInput_NoSchema_OK(t *testing.T) {
	// Scenario без `input:` блока (как у "ba", если create не имеет required) —
	// любой provided проходит; nil-схема не отвергает.
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
		t.Fatalf("scenario без input: %v", err)
	}
	if err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"extra": "x"}); err != nil {
		t.Fatalf("scenario без input + unknown key: %v", err)
	}
}

func TestValidateInput_NilLoader_ConfigError(t *testing.T) {
	err := ValidateInput(context.Background(), nil, artifact.ServiceRef{Name: "svc"}, "create", nil)
	if err == nil {
		t.Fatal("nil loader должен давать config-ошибку")
	}
	// НЕ ErrInputInvalid (это сбой конфигурации, не валидации) → handler даёт 500.
	if errors.Is(err, ErrInputInvalid) {
		t.Fatalf("nil loader не должен маппиться в ErrInputInvalid: %v", err)
	}
}

func TestValidateInput_LoadError_Propagated(t *testing.T) {
	loader := &fakeInputLoader{loadErr: fmt.Errorf("git clone failed")}
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", nil)
	if err == nil {
		t.Fatal("ошибка загрузки снапшота должна пробрасываться")
	}
	if errors.Is(err, ErrInputInvalid) {
		t.Fatalf("load-error не должен маппиться в ErrInputInvalid (это 5xx): %v", err)
	}
}

// scenarioWithValidate — scenario с top-level `validate:`-секцией (кросс-полевой
// инвариант «port обязателен, если tls выключен») + ассимметричный assert-таск.
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

// TestValidateInput_ValidateRuleFalse_ErrValidateFailed — правило-false на
// request-пути → ErrValidateFailed (handler → 422 validation_failed) ДО коммита,
// НЕ error_locked. Отдельный sentinel от ErrInputInvalid.
func TestValidateInput_ValidateRuleFalse_ErrValidateFailed(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithValidate}
	// tls=false (default), port=0 (default) → правило false.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", map[string]any{})
	if err == nil {
		t.Fatal("ожидалась ошибка для нарушенного validate-правила, got nil")
	}
	if !errors.Is(err, ErrValidateFailed) {
		t.Fatalf("err не оборачивает ErrValidateFailed: %v", err)
	}
	if errors.Is(err, ErrInputInvalid) {
		t.Fatalf("validate-провал не должен маппиться в ErrInputInvalid (различимые sentinel): %v", err)
	}
}

// TestValidateInput_ValidateRuleTrue_OK — правило-true → проходит.
func TestValidateInput_ValidateRuleTrue_OK(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithValidate}
	// port>0 покрывает инвариант.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"port": 6379})
	if err != nil {
		t.Fatalf("валидный input (port>0): %v", err)
	}
	// tls=true тоже покрывает (кросс-полевой OR).
	err = ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"tls": true})
	if err != nil {
		t.Fatalf("валидный input (tls=true): %v", err)
	}
}

// TestValidateInput_MultipleRules_FirstFalseMessage — несколько правил: первый
// false выигрывает, его message попадает в ошибку (короткое замыкание).
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
	// port=0 валит первое; второе (port<65536) истинно — но первый выигрывает.
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", map[string]any{})
	if !errors.Is(err, ErrValidateFailed) {
		t.Fatalf("ожидался ErrValidateFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "PORT_MUST_BE_POSITIVE") {
		t.Fatalf("ошибка должна нести message первого правила, got: %v", err)
	}
	if strings.Contains(err.Error(), "PORT_TOO_LARGE") {
		t.Fatalf("message второго правила не должен фигурировать (короткое замыкание): %v", err)
	}
}

// TestValidateInput_ValidateAfterSchema — schema-провал (type-mismatch) бьёт
// РАНЬШЕ validate-правила: validate `that` рассчитан на корректные типы. port=
// строка → ErrInputInvalid (НЕ ErrValidateFailed).
func TestValidateInput_ValidateAfterSchema(t *testing.T) {
	loader := &fakeInputLoader{yaml: scenarioWithValidate}
	err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create",
		map[string]any{"port": "not-int"})
	if !errors.Is(err, ErrInputInvalid) {
		t.Fatalf("type-mismatch должен дать ErrInputInvalid до validate: %v", err)
	}
	if errors.Is(err, ErrValidateFailed) {
		t.Fatalf("schema-провал не должен превращаться в validate-провал: %v", err)
	}
}
