//go:build integration

package trial

import (
	"fmt"
	"os"

	yaml "github.com/goccy/go-yaml"
)

// L2Case — кейс уровня L2 (исполнение на эфемерном Linux-стенде с post-apply
// верификацией, ADR-023). В отличие от L0 (render-only) L2 реально применяет
// отрендеренный план на хосте через `soul apply` (push-oneshot, ADR-004) и
// сверяет результат verify-задачами. Структура read-only после загрузки.
//
// Формат — расширение L0-кейса полями stand:/input:/expect_idempotent:/verify:
// (docs/destiny/testing.md §L2). Декодируется только под build-tag integration:
// дефолтный harness L2-кейс лишь помечает Skipped (см. Run / isL2Case), его
// структуру не парсит.
type L2Case struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description,omitempty"`
	Stand       Stand          `yaml:"stand"`
	Input       map[string]any `yaml:"input,omitempty"`
	// Idempotent — двойной прогон того же ApplyRequest: второе применение обязано
	// не менять state хоста (все register.changed==false). По умолчанию true:
	// идемпотентность — обязательная часть L2-проверки (testing.md §L2 / open Q №5).
	Idempotent *bool    `yaml:"expect_idempotent,omitempty"`
	Verify     []Verify `yaml:"verify,omitempty"`
}

// Stand — описание эфемерного стенда (docs/destiny/testing.md §L2). Reuse
// семантики mode: push (push-oneshot soul apply по ADR-004) — новых stand-значений
// L2-пилот не вводит.
type Stand struct {
	// Driver — драйвер стенда. Пилот поддерживает только docker.
	Driver string `yaml:"driver"`
	// Image — базовый образ хоста-стенда (например ubuntu:24.04).
	Image string `yaml:"image"`
	// Mode — как destiny попадает на стенд. push = push-oneshot soul apply
	// (Keeper рендерит план, доставляет soul + ApplyRequest на хост, исполняет).
	Mode string `yaml:"mode"`
}

// Verify — одна проверка результата (docs/destiny/testing.md §L2). Каждая
// исполняет одну module-задачу на том же стенде тем же `soul apply` (однозадачный
// ApplyRequest) и сверяет поля register-output через Expect. Отдельного DSL
// ассерций нет (testing.md) — проверки выражаются теми же модулями.
type Verify struct {
	Name   string      `yaml:"name"`
	Apply  VerifyApply `yaml:"apply"`
	Expect Expect      `yaml:"expect"`
}

// VerifyApply — module-задача verify-шага. Module — полное имя со state-суффиксом
// (например core.cmd.shell), как в destiny-задаче; Params — её params (без
// CEL-render: verify-шаги задаются литерально на стенде).
type VerifyApply struct {
	Module string         `yaml:"module"`
	Params map[string]any `yaml:"params"`
}

// Expect — ожидания на register-output verify-задачи (docs/destiny/testing.md §L2).
// Набор ключей — минимум зафиксированного (exit_code / stdout / stdout_contains).
// Указатели/опциональность: незаданное поле не сверяется (частичный ассерт).
type Expect struct {
	ExitCode       *int    `yaml:"exit_code,omitempty"`
	Stdout         *string `yaml:"stdout,omitempty"`
	StdoutContains string  `yaml:"stdout_contains,omitempty"`
}

// expectIdempotent возвращает эффективное значение expect_idempotent (default true).
func (c *L2Case) expectIdempotent() bool {
	if c.Idempotent == nil {
		return true
	}
	return *c.Idempotent
}

// LoadL2Case читает и валидирует L2 case.yml (strict-декод: неизвестный ключ —
// ошибка). path — путь к файлу либо к директории кейса (resolveCaseFile).
func LoadL2Case(path string) (*L2Case, string, error) {
	file, err := resolveCaseFile(path)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "", fmt.Errorf("trial: чтение %s: %w", file, err)
	}
	var c L2Case
	if err := yaml.UnmarshalWithOptions(data, &c, yaml.Strict()); err != nil {
		return nil, "", fmt.Errorf("trial: разбор L2 %s: %w", file, err)
	}
	if err := c.validate(); err != nil {
		return nil, "", fmt.Errorf("trial: %s: %w", file, err)
	}
	return &c, file, nil
}

func (c *L2Case) validate() error {
	if c.Name == "" {
		return fmt.Errorf("name: обязателен")
	}
	if c.Stand.Driver != "docker" {
		return fmt.Errorf("stand.driver: пилот L2 поддерживает только docker (получено %q)", c.Stand.Driver)
	}
	if c.Stand.Image == "" {
		return fmt.Errorf("stand.image: обязателен")
	}
	if c.Stand.Mode != "push" {
		return fmt.Errorf("stand.mode: пилот L2 поддерживает только push (получено %q)", c.Stand.Mode)
	}
	if len(c.Verify) == 0 {
		return fmt.Errorf("verify: пуст (L2 сверяет результат apply verify-задачами)")
	}
	for i, v := range c.Verify {
		if v.Apply.Module == "" {
			return fmt.Errorf("verify[%d].apply.module: обязателен", i)
		}
	}
	return nil
}
