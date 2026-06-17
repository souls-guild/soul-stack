package rbac

import "regexp"

// Permission — одна развёрнутая permission-строка.
//
// Грамматика (rbac.md § Формат permissions):
//
//	permission := "*" | <resource>.<action> ( " on " <selector> )?
//	selector   := <key>=<v1>,<v2>,…
//	key        ∈ {service, coven, incarnation, host}
//
// Wildcard варианты:
//   - `*` (full) → IsWildcard=true, Resource/Action="", Selector=nil.
//   - `<resource>.*` → Action="*", остальное как обычно.
//
// Selector — `nil` означает «без фильтра» (любой context матчит).
// Пустой map (`len==0`) при non-nil — невалидная форма (parsePermission
// возвращает error до конструирования).
type Permission struct {
	// IsWildcard — full-wildcard `*` (эквивалент cluster-admin).
	IsWildcard bool

	// Resource — первый сегмент (`incarnation`, `operator`, …).
	// Пуст при IsWildcard=true.
	Resource string

	// Action — второй сегмент или `*`. Пуст при IsWildcard=true.
	Action string

	// Selector — map ключ→список значений. nil = «без фильтра» (permission
	// действует на любой context). Множественные значения внутри одного
	// ключа — OR-логика по rbac.md § Семантика.
	Selector map[string][]string
}

// Matches — проверка, удовлетворяет ли permission запросу.
//
// Контракт:
//   - resource и action — non-empty строки, представляющие конкретное
//     действие (никаких wildcard в запросе).
//   - context — runtime-context из request-а: `{"service": "redis-cluster",
//     "incarnation": "redis-prod"}`. nil допустим (= нет ключей).
//
// Логика:
//   - IsWildcard → true для любого resource/action/context.
//   - Resource не совпадает → false.
//   - Action не совпадает (с учётом `*`) → false.
//   - Selector=nil → true (permission без фильтра).
//   - Selector → для каждой ключ-пары в селекторе: ключ должен быть в
//     context И значение из context должно входить в values селектора.
//     Все ключи селектора должны матчить (AND по ключам); внутри значений
//     одного ключа — OR.
//
// Ключи селектора, отсутствующие в context-е — permission не применима
// (deny). Это сознательно: `incarnation.create on service=foo` не должна
// случайно срабатывать на запросе без указания service в контексте.
func (p Permission) Matches(resource, action string, context map[string]string) bool {
	if p.IsWildcard {
		return true
	}
	if p.Resource != resource {
		return false
	}
	if p.Action != "*" && p.Action != action {
		return false
	}
	if p.Selector == nil {
		return true
	}
	for key, values := range p.Selector {
		if key == "regex" {
			// ADR-047 S2a: regex матчит по SID/имени хоста, источник —
			// host- или sid-ключ контекста (часть эндпоинтов кладёт `host`,
			// часть — `sid`). Нет ни одного → deny (как exact-ключ без своего
			// ключа в контексте). OR среди паттернов одного ключа.
			target, ok := regexTarget(context)
			if !ok {
				return false
			}
			if !regexAny(values, target) {
				return false
			}
			continue
		}
		if key == "soulprint" {
			// ADR-047 S2b: soulprint-предикат — CEL по фактам хоста
			// (`soulprint.self.*`). Текущий context (map[string]string) НЕ несёт
			// nested SoulprintFacts, поэтому в S2b soulprint-измерение fail-closed:
			// deny. РЕАЛЬНЫЙ CEL-eval против фактов — слайсы S3/S4 (резолвер
			// list/target подаёт факты в [EvalSoulprintExpr]); подача фактов в
			// Check-context потребовала бы расширения сигнатуры Matches и здесь НЕ
			// делается (граница S2b). Явная ветка фиксирует fail-closed как
			// намеренное (а не побочный эффект «ключ не в context»).
			return false
		}
		if key == "state" {
			// ADR-047 S2c: state-предикат — CEL по incarnation.state. Текущий
			// context (map[string]string) НЕ несёт nested incarnation.state, поэтому
			// в S2c state-измерение fail-closed: deny. РЕАЛЬНЫЙ CEL-eval против
			// state — слайс S3b (резолвер list/target инкарнаций подаёт state в
			// [EvalStateExpr]); подача state в Check-context потребовала бы
			// расширения сигнатуры Matches и здесь НЕ делается (граница S2c).
			// Симметрия с soulprint-веткой.
			return false
		}
		ctxVal, ok := context[key]
		if !ok {
			return false
		}
		matched := false
		for _, v := range values {
			if v == ctxVal {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// regexTarget извлекает строку, против которой матчится regex-селектор:
// host-ключ приоритетен, иначе sid. nil/отсутствие обоих → (_, false).
func regexTarget(context map[string]string) (string, bool) {
	if v, ok := context["host"]; ok {
		return v, true
	}
	if v, ok := context["sid"]; ok {
		return v, true
	}
	return "", false
}

// regexAny — true, если хотя бы один паттерн матчит target. Паттерны уже
// провалидированы [parseRegexValue] на load снимка (regexp.Compile прошёл),
// поэтому MustCompile здесь не паникует; повторная компиляция в hot-path
// допустима для MVP (Check — не самый горячий путь, scope-роли редки). При
// рассинхроне с битым паттерном — fail-closed (no match), не паника.
func regexAny(patterns []string, target string) bool {
	for _, pat := range patterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			continue
		}
		if re.MatchString(target) {
			return true
		}
	}
	return false
}
