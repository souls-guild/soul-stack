package render

import (
	"fmt"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// resolveCompute вычисляет scenario-level `compute:`-блок (ADR-009 amendment
// 2026-06-23) ОДИН раз на прогон и возвращает map имя→значение для
// [RenderInput.Compute]. Каждая запись резолвится в порядке объявления; ранее
// вычисленный compute виден следующему как `compute.<name>` (накопление в acc).
//
// ★ Барьер изоляции #2 (host-инвариантность compute):
// резолв-контекст — РУН-УРОВНЕВЫЙ (input/register/incarnation/essence + state из
// incarnationVars), БЕЗ soulprint.self и БЕЗ soulprint.hosts (AllowHosts=false).
// Ссылка на soulprint.* в compute-выражении → CEL no-such-key (структурный барьер,
// не текстовый guard): compute по построению не зависит от per-host фактов, поэтому
// одно и то же значение корректно уходит и в apply.input (резолв на targeted[0]),
// и в state_changes (per-run, не per-host) — drift между ними исключён.
//
// Резолвится один раз: входной in.Compute (если уже посчитан caller-ом/предыдущим
// проходом) возвращается как есть — идемпотентность для повторных вызовов в
// staged-render-е. Пустой/отсутствующий блок → nil (`compute.<name>` =
// штатный no-such-key, backward-compat бит-в-бит).
//
// non-string значение записи (число/bool/коллекция) проходит литералом — CEL-фаза
// трогает только строки (симметрично resolveTaskVars/renderValue).
func (p *Pipeline) resolveCompute(in RenderInput) (map[string]any, error) {
	if in.Compute != nil {
		return in.Compute, nil
	}
	block := in.Scenario.Compute
	if len(block) == 0 {
		return nil, nil
	}

	acc := make(map[string]any, len(block))
	// База резолва: рун-уровневый контекст без soulprint. Compute копится в acc и
	// подкладывается на каждой итерации → compute[i] видит compute[j<i].
	base := cel.Vars{
		Input:       in.Input,
		Register:    in.Register,
		Incarnation: incarnationVars(in, len(in.Hosts)),
		Essence:     in.Essence,
		Ctx:         in.Ctx,
	}
	for _, cv := range block {
		s, ok := cv.Value.(string)
		if !ok {
			acc[cv.Name] = cv.Value // литерал — насквозь
			continue
		}
		base.Compute = acc
		val, err := p.cel.EvalInterpolation(s, base)
		if err != nil {
			return nil, fmt.Errorf("render: compute.%s: %w", cv.Name, err)
		}
		acc[cv.Name] = val
	}
	return acc, nil
}
