package rbac

import "sort"

// Purview — типизированный результат [Enforcer.ResolvePurview]: верхняя граница
// scope-видимости/таргетинга оператора для конкретного (resource, action),
// разложенная по измерениям (ADR-047).
//
// Covens + терминальные флаги Unrestricted/Deny заполнены с S0 (обобщение
// прежнего [Enforcer.CovenScope]); измерения Regexes (S2a) / SoulprintExprs
// (S2b) / StateExprs (S2c) добавлялись additive-слайсами без смены сигнатуры и
// потребителей — каждый лишь начинал класть значения в своё поле.
//
// Семантика терминальных флагов:
//   - Unrestricted=true — у оператора нет ограничений по scope для этого
//     действия (bare-permission, `*`-permission или `coven=*`). Измерения
//     при этом пусты и игнорируются.
//   - Deny — НЕ используется и в S1 (всегда false). default-deny S1 выражен
//     через ОТСУТСТВИЕ Unrestricted (роль с default_scope → bare-permissions
//     ограничены этим scope, а не unrestricted). «Введённое пустое измерение»
//     в coven-MVP недостижимо (parseSelector требует непустой value-list, NULL
//     default_scope = измерение НЕ введено = unrestricted). Поле остаётся
//     заготовкой до S2, где появятся измерения с возможным пустым значением.
//
// Несколько значений в одном измерении — союз (OR внутри измерения).
type Purview struct {
	// Covens — точные coven-метки, на которые распространяется право
	// (дедуп, отсортировано). Заполняется в S0.
	Covens []string

	// Unrestricted — у оператора нет scope-ограничений для (resource, action).
	Unrestricted bool

	// Deny — доступ запрещён по scope. Заготовка под S1 (default-deny);
	// в S0 всегда false.
	Deny bool

	// Regexes — RE2-паттерны по SID. Заготовка под S2; в S0 всегда nil.
	Regexes []string

	// SoulprintExprs — CEL-предикаты `soulprint.self.*` (ADR-047 S2b, ADR-018
	// каноническая форма; дедуп, отсортировано). Union по ролям. Реальный
	// CEL-eval против фактов хоста — слайсы S3/S4 ([EvalSoulprintExpr]);
	// здесь Purview лишь несёт предикаты (как Covens несёт coven-метки).
	SoulprintExprs []string

	// StateExprs — CEL-предикаты по `incarnation.state` (ADR-047 S2c; дедуп,
	// отсортировано). Union по ролям. Реальный CEL-eval против state инкарнации —
	// слайс S3b ([EvalStateExpr] через keeper/internal/statepredicate); здесь
	// Purview лишь несёт предикаты (как Covens несёт coven-метки).
	StateExprs []string
}

// ResolvePurview резолвит [Purview] оператора для (resource, action) —
// разрешённый scope, разложенный по измерениям (ADR-047).
//
// S1 (default_scope роли + наследование/override + default-deny по введённым
// измерениям). Для каждой матчащей (resource, action) permission итоговый
// scope = эффективный селектор:
//   - `*`-permission → ВСЕГДА [Purview.Unrestricted] (cluster-admin не залочен
//     default-deny — ADR-047(б), исключение №1).
//   - per-perm-селектор (`on coven=X`) → ПЕРЕОПРЕДЕЛЯЕТ default_scope роли
//     целиком (не merge) — эффективный селектор = per-perm.
//   - bare-permission (Selector==nil):
//     · роль БЕЗ default_scope → [Purview.Unrestricted] (backcompat,
//     исключение №2: NULL scope = измерение НЕ введено);
//     · роль С default_scope → НАСЛЕДУЕТ его (НЕ unrestricted, ограничена).
//
// Эффективный селектор раскладывается по измерениям так же, как S0: ключ
// `coven` даёт covens; `coven=*` → Unrestricted; селектор без `coven`
// (host/incarnation/service) вклада в covens не даёт (симметрия с Matches).
//
// Union по ролям: если ХОТЬ ОДНА матчащая permission даёт Unrestricted → итог
// Unrestricted; иначе covens — union конкретных coven-значений (дедуп,
// отсортировано).
//
// Deny в S1 НЕ выставляется (coven-MVP): NULL default_scope = unrestricted, а
// «введённое пустое coven-измерение» в грамматике недостижимо (parseSelector
// требует непустой value-list). Поле [Purview.Deny] остаётся заготовкой до S2.
func (e *Enforcer) ResolvePurview(aid, resource, action string) Purview {
	// Revoked-shortcut (ADR-047 G1, зеркало [Enforcer.Check] revoked-ветки):
	// ревокнутый Архонт получает Deny независимо от ролей — ПЕРЕД любым
	// сбором измерений, иначе bare `*`-роль вернула бы Unrestricted. Это единая
	// точка revoked-aware-резолва для всех read-souls-потребителей: gate
	// (HoldsAction→Deny→false→403), single-read (soulpurview.Resolve→Empty→404),
	// InScope (Deny→false). На read revoked = «нет доступа» (403/404), а НЕ 401-
	// паритет Check-а: видимость флота не должна различать revoked и no-permission.
	if _, ok := e.revoked[aid]; ok {
		return Purview{Deny: true}
	}
	roles, ok := e.rolesByAID[aid]
	if !ok {
		return Purview{}
	}
	seen := make(map[string]struct{})
	seenRegex := make(map[string]struct{})
	seenSoulprint := make(map[string]struct{})
	seenState := make(map[string]struct{})
	for _, role := range roles {
		for _, p := range role.Permissions {
			if p.IsWildcard {
				return Purview{Unrestricted: true}
			}
			if p.Resource != resource {
				continue
			}
			if p.Action != "*" && p.Action != action {
				continue
			}

			// Эффективный селектор: per-perm переопределяет default_scope
			// целиком; bare наследует default_scope роли (nil → unrestricted).
			eff := p.Selector
			if eff == nil {
				eff = role.DefaultScope
			}
			if eff == nil {
				// bare-permission без default_scope роли → unrestricted
				// (backcompat, ADR-047(б) исключение №2).
				return Purview{Unrestricted: true}
			}

			// regex-измерение (ADR-047 S2a): паттерны селектора — union по
			// ролям. Реальный матчинг против SID — S3/S4; здесь Purview лишь
			// несёт паттерны (как Covens несёт coven-метки).
			for _, pat := range eff["regex"] {
				seenRegex[pat] = struct{}{}
			}

			// soulprint-измерение (ADR-047 S2b): CEL-предикаты селектора — union
			// по ролям (как regex). Реальный CEL-eval против фактов хоста — S3/S4
			// (резолвер list/target подаёт SoulprintFacts); здесь Purview несёт
			// предикаты для S3/S4 и least-privilege subset.
			for _, expr := range eff["soulprint"] {
				seenSoulprint[expr] = struct{}{}
			}

			// state-измерение (ADR-047 S2c): CEL-предикаты по incarnation.state —
			// union по ролям (как soulprint). Реальный CEL-eval против state
			// инкарнации — S3b (резолвер list/target подаёт incarnation.state);
			// здесь Purview несёт предикаты для S3b и least-privilege subset.
			for _, expr := range eff["state"] {
				seenState[expr] = struct{}{}
			}

			vals, hasCoven := eff["coven"]
			if !hasCoven {
				// Эффективный селектор без ключа coven (host/incarnation/
				// service/regex/soulprint) — ограничение в другом измерении: по
				// coven не unrestricted, вклада в covens нет (симметрия с Matches).
				continue
			}
			for _, v := range vals {
				// `coven=*` — wildcard-значение, снимает scope как bare.
				// Defensive/недостижимо: parseSelector не допускает `*` как
				// значение (отвергается на load снимка); ветка оставлена для
				// симметрии с wildcard-конвенцией.
				if v == "*" {
					return Purview{Unrestricted: true}
				}
				seen[v] = struct{}{}
			}
		}
	}
	if len(seen) == 0 && len(seenRegex) == 0 && len(seenSoulprint) == 0 && len(seenState) == 0 {
		return Purview{}
	}
	return Purview{
		Covens:         sortedKeys(seen),
		Regexes:        sortedKeys(seenRegex),
		SoulprintExprs: sortedKeys(seenSoulprint),
		StateExprs:     sortedKeys(seenState),
	}
}

// sortedKeys — отсортированный slice ключей set-а (nil при пустом set-е, чтобы
// сохранить «измерение не введено» = nil-семантику Purview).
func sortedKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
