package trial

import (
	"fmt"
	"os"
	"path/filepath"

	yaml "github.com/goccy/go-yaml"
)

// caseFileName — каноническое имя файла испытания внутри tests/<case>/.
const caseFileName = "case.yml"

// l2Markers — ключи верхнего уровня, маркирующие кейс как уровень L2
// (исполнение на стенде с post-apply-верификацией, ADR-023 post-MVP). MVP-harness
// уровня L0 (render-only) их не исполняет и не должен на них падать strict-декодом
// — такой кейс распознаётся мягким пред-парсом и пропускается при рекурсивном
// прогоне дерева.
var l2Markers = []string{"stand", "verify"}

// l1Markers — ключи верхнего уровня, маркирующие кейс как уровень L1 (тест
// миграции state_schema, ADR-019/docs/migrations.md §Тесты). Форма L1-кейса
// принципиально отличается от L0 (нет fixtures/assert.rendered_tasks), поэтому
// он распознаётся мягким пред-парсом ДО strict L0-декода и уходит в отдельный
// раннер RunMigrationCase. Обе ключевые секции обязаны присутствовать.
var l1Markers = []string{"state_before", "state_after"}

// LoadCase читает и валидирует один `case.yml`. path принимается двумя
// формами: путь к самому файлу или путь к директории кейса
// (`.../tests/<case>/`), внутри которой ищется case.yml.
//
// Декод strict ([yaml.Strict]): неизвестный ключ — ошибка, а не silent-skip.
// Это отсекает кейсы, опирающиеся на нереализованные секции пилота
// (assert.dispatch / assert.state_after), явной ошибкой.
func LoadCase(path string) (*Case, string, error) {
	file, err := resolveCaseFile(path)
	if err != nil {
		return nil, "", err
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "", fmt.Errorf("trial: чтение %s: %w", file, err)
	}

	var c Case
	if err := yaml.UnmarshalWithOptions(data, &c, yaml.Strict()); err != nil {
		return nil, "", fmt.Errorf("trial: разбор %s: %w", file, err)
	}
	if err := c.validate(); err != nil {
		return nil, "", fmt.Errorf("trial: %s: %w", file, err)
	}
	return &c, file, nil
}

// isL2Case — мягкий пред-парс case.yml: распознаёт уровень L2 по наличию
// верхнеуровневого маркера stand:/verify: ДО strict L0-декода. Парсит только
// ключи верхнего уровня в свободную карту (lax-декод), не валидируя их форму:
// L2-секции (stand/verify/expect/…) MVP-harness не исполняет, поэтому их строгая
// структура здесь не важна — важен лишь сам факт принадлежности к L2.
//
// L0-кейс маркеров не несёт → false → дальше идёт обычный strict-декод, где
// unknown-field остаётся ошибкой (strict-decode для L0 не ослаблен).
func isL2Case(file string) (bool, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return false, fmt.Errorf("trial: чтение %s: %w", file, err)
	}
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return false, fmt.Errorf("trial: пред-парс %s: %w", file, err)
	}
	for _, marker := range l2Markers {
		if _, ok := top[marker]; ok {
			return true, nil
		}
	}
	return false, nil
}

// isL1Case — мягкий пред-парс case-файла: распознаёт уровень L1 по наличию
// верхнеуровневых маркеров state_before:/state_after: ДО strict L0-декода
// (симметрично isL2Case). Парсит только ключи верхнего уровня в свободную карту,
// форму секций не валидирует — это делает раннер RunMigrationCase.
//
// L1-кейс требует ОБЕ секции: одиночный state_before без state_after (или
// наоборот) — не L1, идёт дальше и будет отвергнут strict L0-декодом как явная
// ошибка формы, а не молча-пропущен. L0-кейс маркеров не несёт → false.
func isL1Case(file string) (bool, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return false, fmt.Errorf("trial: чтение %s: %w", file, err)
	}
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return false, fmt.Errorf("trial: пред-парс %s: %w", file, err)
	}
	for _, marker := range l1Markers {
		if _, ok := top[marker]; !ok {
			return false, nil
		}
	}
	return true, nil
}

// resolveCaseFile сводит вход (файл или директория) к пути case.yml.
func resolveCaseFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("trial: %w", err)
	}
	if info.IsDir() {
		return filepath.Join(path, caseFileName), nil
	}
	return path, nil
}

// validate — структурная проверка кейса после декода. Глубже схему не
// валидируем: render-пайплайн сам отвергнет некорректный scenario, а
// fixtures — свободные YAML-данные.
func (c *Case) validate() error {
	if c.Name == "" {
		return fmt.Errorf("name: обязателен")
	}
	// fixtures.soulprint (single-host сахар) и fixtures.hosts (multi-host roster)
	// взаимоисключены — оба описывают факты хостов прогона; одновременная подача
	// неоднозначна (один синтетический хост vs N) → strict-ошибка, как unknown-key.
	if c.Fixtures.Soulprint != nil && len(c.Fixtures.Hosts) > 0 {
		return fmt.Errorf("fixtures.soulprint и fixtures.hosts взаимоисключены: soulprint — single-host сахар, hosts — multi-host roster")
	}
	for i, h := range c.Fixtures.Hosts {
		if h.SID == "" {
			return fmt.Errorf("fixtures.hosts[%d]: sid обязателен", i)
		}
	}
	if len(c.Assert.RenderedTasks) == 0 {
		return fmt.Errorf("assert.rendered_tasks: пуст (пилот L0 сверяет только эту секцию)")
	}
	for i, et := range c.Assert.RenderedTasks {
		if et.Module == "" {
			return fmt.Errorf("assert.rendered_tasks[%d]: module обязателен", i)
		}
	}
	return nil
}
