package util

import (
	"fmt"
	"sort"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/plugin"
	"google.golang.org/protobuf/types/known/structpb"
)

// ValidateAgainstManifest — runtime-валидация ValidateRequest против embed-
// манифеста core-модуля (shared/coremanifest). Единый источник правды для
// per-field-проверок: known-state и наличие required-параметров декларированы
// в manifest.yaml модуля, а не захардкожены отдельно в каждом Module.Validate
// (раньше это был дубль между линтером и runtime-кодом).
//
// `coreName` — каноническое имя core-модуля (`core.exec`, `core.file`).
// Возвращает список ошибок (пустой = всё валидно). Тип-проверка значений в
// runtime НЕ делается тут: её выполняют типизированные param-getter-ы в Apply
// (StringParam/OptStringSliceParam/…) — это императивная per-field-проверка,
// которую manifest-DSL не выражает. Cross-field-инварианты (если появятся у
// модуля) добавляются в его Module.Validate поверх этого вызова.
func ValidateAgainstManifest(coreName string, req *pluginv1.ValidateRequest) []string {
	m, ok := coremanifest.Default().Lookup(coreName)
	if !ok {
		// Манифест core-модуля обязан существовать (баг сборки иначе); но в
		// runtime не паникуем — отдаём диагностику как обычную ошибку валидации.
		return []string{fmt.Sprintf("internal: no manifest for %q", coreName)}
	}
	def, ok := m.Spec.States[req.State]
	if !ok {
		return []string{fmt.Sprintf("unknown state %q (want one of %v)", req.State, sortedStates(m))}
	}
	var errs []string
	for _, name := range sortedRequiredParams(def) {
		if !paramPresent(req, name) {
			errs = append(errs, fmt.Sprintf("param %q: missing", name))
		}
	}
	return errs
}

func paramPresent(req *pluginv1.ValidateRequest, name string) bool {
	if req == nil || req.Params == nil || req.Params.Fields == nil {
		return false
	}
	v, ok := req.Params.Fields[name]
	if !ok || v == nil {
		return false
	}
	// Явный null трактуется как отсутствие — симметрично Apply-time getter-ам
	// (Opt*Param в params.go), где NullValue-kind → значения нет. Иначе
	// required-параметр со значением null проходил бы lint, но падал в runtime.
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return false
	}
	return true
}

func sortedStates(m *plugin.Manifest) []string {
	out := make([]string, 0, len(m.Spec.States))
	for s := range m.Spec.States {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sortedRequiredParams(def plugin.StateDef) []string {
	var out []string
	for name, p := range def.Input {
		if p.Required {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
