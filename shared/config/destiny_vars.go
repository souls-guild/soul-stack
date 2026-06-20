package config

import (
	"fmt"
	"os"
	"sort"

	"github.com/goccy/go-yaml"
)

// LoadDestinyVars парсит `vars.yml` destiny — top-level YAML-map
// destiny-локалов (docs/destiny/vars.md). Возвращает RAW-карту имя→значение
// БЕЗ схемо-валидации: vars не типизированы спекой (plain map значений от
// автора destiny, см. таблицу `vars` vs `input` в vars.md), поэтому путь
// parseAndValidate (как у destiny.yml/scenario.yml) тут неуместен — простой
// yaml.Unmarshal.
//
// Значения проходят насквозь как Go-данные (string/число/bool/коллекция);
// CEL-выражения `${ … }` в строковых ячейках резолвятся ПОЗЖЕ, в render-фазе
// (renderApplyDestiny над input+soulprint.self), не здесь.
//
// Отсутствие файла — НЕ ошибка: destiny без локалов (vars.yml не обязателен)
// даёт nil-карту, обращение `${ vars.<x> }` тогда упадёт штатным no-such-key.
func LoadDestinyVars(path string) (map[string]any, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: чтение %s: %w", path, err)
	}
	return LoadDestinyVarsFromBytes(path, src)
}

// LoadDestinyVarsFromBytes — точка входа без I/O (снапшот destiny уже прочитан,
// тесты с in-memory фикстурами). Пустой документ (только комментарии/пробелы) →
// nil-карта, не ошибка. filename используется лишь как метка в сообщении.
func LoadDestinyVarsFromBytes(filename string, data []byte) (map[string]any, error) {
	data = stripBOM(data)
	if len(data) == 0 {
		return nil, nil
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("config: парсинг %s (vars.yml — top-level YAML-map): %w", filename, err)
	}
	return out, nil
}

// DestinyVarsCollisions возвращает отсортированный список имён, объявленных И в
// file-level `vars.yml` (fileVars), И в task-level `vars:` хотя бы одной задачи
// (tasks). Это не ошибка — Вариант A (vars.md «Слияние file-vars ↔ task-vars»)
// детерминирован: task-var переопределяет одноимённый file-var. Но коллизия —
// частый источник недоразумений («почему мой file-var игнорируется?»), поэтому
// soul-lint поднимает warn. Пустой результат → пересечений нет.
func DestinyVarsCollisions(fileVars map[string]any, tasks []Task) []string {
	if len(fileVars) == 0 || len(tasks) == 0 {
		return nil
	}
	clash := make(map[string]struct{})
	for i := range tasks {
		for name := range tasks[i].Vars {
			if _, ok := fileVars[name]; ok {
				clash[name] = struct{}{}
			}
		}
	}
	if len(clash) == 0 {
		return nil
	}
	out := make([]string, 0, len(clash))
	for name := range clash {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
