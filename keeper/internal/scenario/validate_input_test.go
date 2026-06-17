package scenario

import (
	"context"
	"errors"
	"fmt"
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
