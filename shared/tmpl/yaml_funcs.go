package tmpl

import (
	"fmt"
	"strings"
	"text/template"

	goyaml "github.com/goccy/go-yaml"
)

// customFuncs — собственные функции Soul Stack, добавляемые в FuncMap
// поверх sprig-allowlist-а. Это НЕ функции sprig: `toYaml`/`fromYaml` в
// upstream sprig отсутствуют (Helm-only), поэтому реализованы здесь через
// goccy/go-yaml. Учитываются в allowlist-инварианте отдельно от sprig
// ([templating.md §3.3]).
//
// [templating.md §3.3]: docs/templating.md
func customFuncs() template.FuncMap {
	return template.FuncMap{
		"toYaml":   toYaml,
		"fromYaml": fromYaml,
	}
}

// toYaml сериализует значение в YAML. В отличие от Helm-варианта (который
// глотает ошибку и возвращает пустую строку), здесь ошибка пробрасывается
// и проваливает рендер штатно — молчаливая подстановка мусора в конфиг
// опаснее упавшего шага ([templating.md §10]).
//
// Хвостовой перевод строки goccy-энкодера срезается: внутри шаблона
// результат обычно встраивается в более крупный YAML, лишний `\n` ломает
// отступы.
func toYaml(v any) (string, error) {
	out, err := goyaml.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("toYaml: %w", err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// fromYaml парсит YAML-строку в произвольную структуру (map/list/scalar),
// доступную дальше в шаблоне через индексацию. Ошибка парсинга проваливает
// рендер ([templating.md §10]).
func fromYaml(s string) (any, error) {
	var v any
	if err := goyaml.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("fromYaml: %w", err)
	}
	return v, nil
}
