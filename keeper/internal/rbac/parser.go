package rbac

import (
	"fmt"
	"regexp"
	"strings"
)

// reResourceAction — грамматика `<resource>.<action>` из rbac.md.
// Resource: `[a-z][a-z0-9-]*` (kebab-case); action: `*` или
// `[a-z][a-z0-9-]*`. Дефис допустим (`issue-token`), пробел/underscore — нет.
var (
	reResource = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	reAction   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	reSelKey   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	reSelValue = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
)

// maxRegexLen — верхняя граница длины regex-значения селектора (ADR-047 S2a).
// RE2 (Go regexp) не подвержен catastrophic backtracking, но cap длины — дешёвая
// страховка против раздутых паттернов в снимке (compile-cost/память на load).
const maxRegexLen = 256

// ParsePermission парсит permission-строку в Permission. Невалидные формы
// возвращают error с указанием причины.
//
// Принимаемые формы:
//
//	"*"                                    → full wildcard.
//	"incarnation.create"                   → resource+action, no selector.
//	"incarnation.*"                        → resource+wildcard-action.
//	"incarnation.create on service=foo"    → +selector.
//	"incarnation.* on service=foo,bar"     → +selector с множественными значениями.
//
// Отказы:
//   - Пустая строка.
//   - Три и более сегментов (`keeper.incarnation.create` — это MCP-tool, не permission).
//   - Wildcard в resource (`*.create`).
//   - Unknown selector key (rbac.md закрытый enum: service/coven/incarnation/host).
//   - Malformed selector (нет `=`, пустое значение, недопустимые символы).
//   - Unknown permission в каталоге [AllowedPermissions] — проверяется здесь,
//     чтобы load `keeper.yml` фейлился до runtime (PM-decision M0.6b #7).
func ParsePermission(raw string) (Permission, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Permission{}, fmt.Errorf("permission: empty string")
	}

	// Full wildcard.
	if s == "*" {
		return Permission{IsWildcard: true}, nil
	}

	// Опциональный `on <selector>`. Разделитель — ровно ` on ` (один пробел
	// с обеих сторон); rbac.md прибит к этой форме.
	var head, tail string
	if idx := strings.Index(s, " on "); idx >= 0 {
		head = strings.TrimSpace(s[:idx])
		tail = strings.TrimSpace(s[idx+len(" on "):])
		if tail == "" {
			return Permission{}, fmt.Errorf("permission %q: empty selector after \" on \"", raw)
		}
	} else {
		head = s
	}

	// Head: `<resource>.<action>`. Ровно один разделитель `.`.
	dotIdx := strings.IndexByte(head, '.')
	if dotIdx < 0 {
		return Permission{}, fmt.Errorf("permission %q: expected <resource>.<action>, no '.' found", raw)
	}
	if strings.IndexByte(head[dotIdx+1:], '.') >= 0 {
		return Permission{}, fmt.Errorf("permission %q: expected exactly two segments (<resource>.<action>); three-segment names are MCP tools, not permissions", raw)
	}
	resource := head[:dotIdx]
	action := head[dotIdx+1:]

	if resource == "" || action == "" {
		return Permission{}, fmt.Errorf("permission %q: empty resource or action segment", raw)
	}
	if resource == "*" {
		return Permission{}, fmt.Errorf("permission %q: wildcard in <resource> is not supported (use bare '*' for full wildcard)", raw)
	}
	if !reResource.MatchString(resource) {
		return Permission{}, fmt.Errorf("permission %q: resource %q does not match [a-z][a-z0-9-]*", raw, resource)
	}
	if action != "*" && !reAction.MatchString(action) {
		return Permission{}, fmt.Errorf("permission %q: action %q does not match [a-z][a-z0-9-]* or '*'", raw, action)
	}

	if !IsAllowedPermission(resource, action) {
		return Permission{}, fmt.Errorf("permission %q: unknown_permission (resource.action not in catalog rbac.md → §Каталог permissions)", raw)
	}

	// DEPRECATED-alias канонизируется в новое имя (selector сохраняется):
	// роли со старым именем матчат запрос по каноническому resource.action
	// без правки [Permission.Matches]. Wildcard-action (`incarnation.*`) не
	// канонизируется — он и так покрывает каноническое имя.
	if action != "*" {
		if canon, ok := deprecatedActionAliases[resource+"."+action]; ok {
			dotIdx := strings.IndexByte(canon, '.')
			resource, action = canon[:dotIdx], canon[dotIdx+1:]
		}
	}

	p := Permission{Resource: resource, Action: action}

	if tail != "" {
		sel, err := parseSelector(tail, raw)
		if err != nil {
			return Permission{}, err
		}
		p.Selector = sel
	}

	return p, nil
}

// ParseDefaultScope парсит role default_scope-строку (ADR-047 S1) в селектор
// той же формы, что per-permission `on <selector>` — `key=v1,v2,…`. Пустая
// строка → nil (измерение НЕ введено, роль без scope-ограничения). Невалидная
// форма → error (load снимка фейлится, как и на битом permission-е).
//
// Переиспользует [parseSelector] — синтаксис и closed enum ключей
// (service/coven/incarnation/host) общие с per-perm-селектором, дублировать
// грамматику нельзя.
func ParseDefaultScope(raw string) (map[string][]string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	return parseSelector(s, "default_scope "+raw)
}

// parseSelector — `key=v1,v2,…`. Ровно один key (multi-key через запятую
// не поддерживается; grammar rbac.md). raw — оригинальная permission-строка
// для message-context-а в error-ах.
func parseSelector(s, raw string) (map[string][]string, error) {
	eqIdx := strings.Index(s, "=")
	if eqIdx < 0 {
		return nil, fmt.Errorf("permission %q: selector %q missing '='", raw, s)
	}
	key := s[:eqIdx]
	values := s[eqIdx+1:]
	if key == "" {
		return nil, fmt.Errorf("permission %q: selector key is empty", raw)
	}
	if !reSelKey.MatchString(key) {
		return nil, fmt.Errorf("permission %q: selector key %q does not match [a-z][a-z0-9-]*", raw, key)
	}
	if !IsAllowedSelectorKey(key) {
		return nil, fmt.Errorf("permission %q: unknown selector key %q (allowed: service|coven|incarnation|host|regex|soulprint|state|trait)", raw, key)
	}
	if values == "" {
		return nil, fmt.Errorf("permission %q: selector value-list is empty", raw)
	}

	// regex-ключ (ADR-047 S2a) — отдельная грамматика: значение в одинарных
	// кавычках `regex='^web-.*'`. Кавычки нужны, чтобы запятая внутри regex
	// (`{1,3}`) не интерпретировалась как `,`-разделитель value-list; спецсимволы
	// regex не проходят reSelValue, поэтому незакавыченная форма запрещена.
	// Одно значение на ключ (multi-regex через `,` неоднозначен с regex-запятой).
	if key == "regex" {
		pat, err := parseRegexValue(values, raw)
		if err != nil {
			return nil, err
		}
		return map[string][]string{key: {pat}}, nil
	}

	// soulprint-ключ (ADR-047 S2b) — CEL-предикат по фактам хоста
	// (`soulprint.self.*`, ADR-018 каноническая форма) в одинарных кавычках:
	// `soulprint='soulprint.self.os.family == "debian"'`. Кавычки нужны, чтобы
	// пробелы/запятые/двойные кавычки внутри CEL не интерпретировались как
	// `,`-разделитель value-list. Один предикат на ключ; union — несколькими
	// ролями/permission-ами. Компиляция валидируется shared/cel на load.
	if key == "soulprint" {
		expr, err := parseSoulprintValue(values, raw)
		if err != nil {
			return nil, err
		}
		return map[string][]string{key: {expr}}, nil
	}

	// state-ключ (ADR-047 S2c) — CEL-предикат по incarnation.state в одинарных
	// кавычках: `state='state.redis_version == "8.0"'`. Кавычки нужны по той же
	// причине, что у soulprint: пробелы/запятые/двойные кавычки внутри CEL не
	// должны рваться `,`-разделителем value-list. Один предикат на ключ; union —
	// несколькими ролями/permission-ами. Компиляция валидируется через
	// keeper/internal/statepredicate на load (migration-sandbox корень `state`).
	if key == "state" {
		expr, err := parseStateValue(values, raw)
		if err != nil {
			return nil, err
		}
		return map[string][]string{key: {expr}}, nil
	}

	// trait-ключ (ADR-047 amendment, ADR-060 п.7 slice 1) — exact key:value-match
	// по incarnation.traits. Форма `trait=key:value`: ровно ОДНА `:` (разделитель
	// ключа и значения), обе половины — непустые [a-zA-Z0-9_.-]+ (scalar-only, как
	// обычные exact-значения). Хранится в Selector["trait"] одной строкой
	// `key:value` (не разбивается) — match сравнивает её целиком против пары
	// traits-ключа инкарнации (slice 1 п.7). Один trait на ключ (multi-key через
	// `,` — follow-up: AND-сужение по нескольким парам).
	if key == "trait" {
		pair, err := parseTraitValue(values, raw)
		if err != nil {
			return nil, err
		}
		return map[string][]string{key: {pair}}, nil
	}

	parts := strings.Split(values, ",")
	out := make([]string, 0, len(parts))
	for _, v := range parts {
		// rbac.md: значения через запятую без пробелов; whitespace внутри
		// разделителей — невалидно, не trim-аем.
		if v == "" {
			return nil, fmt.Errorf("permission %q: empty value in selector %q", raw, s)
		}
		if !reSelValue.MatchString(v) {
			return nil, fmt.Errorf("permission %q: selector value %q does not match [a-zA-Z0-9_.-]+", raw, v)
		}
		out = append(out, v)
	}
	return map[string][]string{key: out}, nil
}

// parseRegexValue извлекает RE2-паттерн из quoted-формы `'…'` и валидирует его
// на load снимка (ADR-047 S2a): пустой/незакавыченный/over-long/некомпилируемый
// regex → error (load фейлится, как unknown-permission). Возвращает паттерн без
// кавычек.
func parseRegexValue(values, raw string) (string, error) {
	if len(values) < 2 || values[0] != '\'' || values[len(values)-1] != '\'' {
		return "", fmt.Errorf("permission %q: regex value %q must be single-quoted (regex='^web-.*')", raw, values)
	}
	pat := values[1 : len(values)-1]
	if pat == "" {
		return "", fmt.Errorf("permission %q: regex value is empty", raw)
	}
	if len(pat) > maxRegexLen {
		return "", fmt.Errorf("permission %q: regex too long (%d > %d, length cap)", raw, len(pat), maxRegexLen)
	}
	if _, err := regexp.Compile(pat); err != nil {
		return "", fmt.Errorf("permission %q: regex %q does not compile: %w", raw, pat, err)
	}
	return pat, nil
}

// parseSoulprintValue извлекает CEL-предикат из quoted-формы `'…'` и валидирует
// его компиляцию на load снимка (ADR-047 S2b): пустой/незакавыченный/over-long/
// некомпилируемый/использующий запрещённый sandbox-корень предикат → error (load
// фейлится, как битый regex / unknown-permission). Возвращает предикат без
// кавычек. Компиляция — через shared/cel (sandbox FlowControl-env, см.
// [validateSoulprintExpr]); CEL-движок не дублируется.
func parseSoulprintValue(values, raw string) (string, error) {
	if len(values) < 2 || values[0] != '\'' || values[len(values)-1] != '\'' {
		return "", fmt.Errorf("permission %q: soulprint value %q must be single-quoted (soulprint='soulprint.self.os.family == \"debian\"')", raw, values)
	}
	expr := values[1 : len(values)-1]
	if expr == "" {
		return "", fmt.Errorf("permission %q: soulprint predicate is empty", raw)
	}
	if len(expr) > maxSoulprintExprLen {
		return "", fmt.Errorf("permission %q: soulprint predicate too long (%d > %d, length cap)", raw, len(expr), maxSoulprintExprLen)
	}
	if err := validateSoulprintExpr(expr); err != nil {
		return "", fmt.Errorf("permission %q: soulprint predicate %q does not compile: %w", raw, expr, err)
	}
	return expr, nil
}

// parseStateValue извлекает CEL-предикат из quoted-формы `'…'` и валидирует его
// компиляцию на load снимка (ADR-047 S2c): пустой/незакавыченный/over-long/
// некомпилируемый/использующий запрещённый sandbox-корень предикат → error (load
// фейлится, как битый soulprint / unknown-permission). Возвращает предикат без
// кавычек. Компиляция — через keeper/internal/statepredicate (migration-sandbox,
// корень `state`, см. [validateStateExpr]); CEL-движок не дублируется.
func parseStateValue(values, raw string) (string, error) {
	if len(values) < 2 || values[0] != '\'' || values[len(values)-1] != '\'' {
		return "", fmt.Errorf("permission %q: state value %q must be single-quoted (state='state.redis_version == \"8.0\"')", raw, values)
	}
	expr := values[1 : len(values)-1]
	if expr == "" {
		return "", fmt.Errorf("permission %q: state predicate is empty", raw)
	}
	if len(expr) > maxStateExprLen {
		return "", fmt.Errorf("permission %q: state predicate too long (%d > %d, length cap)", raw, len(expr), maxStateExprLen)
	}
	if err := validateStateExpr(expr); err != nil {
		return "", fmt.Errorf("permission %q: state predicate %q does not compile: %w", raw, expr, err)
	}
	return expr, nil
}

// parseTraitValue валидирует `key:value`-форму trait-селектора на load снимка
// (ADR-047 amendment, ADR-060 п.7 slice 1) и возвращает её нормализованной
// строкой `key:value`. Требования: РОВНО одна `:` (одно вхождение — разделитель
// ключа/значения), обе половины непустые и матчат [reSelValue] (scalar-only, тот
// же символьный класс, что у обычных exact-значений). Нарушение → error (load
// фейлится, как битый soulprint/state). Двоеточие внутри ключа/значения
// запрещено: оно не проходит reSelValue, поэтому неоднозначности «какая `:`
// разделитель» не возникает — допустимо ровно одно вхождение.
func parseTraitValue(values, raw string) (string, error) {
	key, value, found := strings.Cut(values, ":")
	if !found {
		return "", fmt.Errorf("permission %q: trait value %q must be key:value (single ':')", raw, values)
	}
	// redundant-defensive: вторую `:` ловит и reSelValue ниже (двоеточие вне
	// [a-zA-Z0-9_.-]+) — отказ был бы и без этой ветки. Оставлена ради точного
	// сообщения оператору на load keeper.yml («exactly one ':'» понятнее, чем
	// «value не матчит класс»); диагностика битого permission — системная граница.
	if strings.Contains(value, ":") {
		return "", fmt.Errorf("permission %q: trait value %q must contain exactly one ':' (key:value)", raw, values)
	}
	if key == "" || value == "" {
		return "", fmt.Errorf("permission %q: trait %q has empty key or value", raw, values)
	}
	if !reSelValue.MatchString(key) {
		return "", fmt.Errorf("permission %q: trait key %q does not match [a-zA-Z0-9_.-]+", raw, key)
	}
	if !reSelValue.MatchString(value) {
		return "", fmt.Errorf("permission %q: trait value %q does not match [a-zA-Z0-9_.-]+", raw, value)
	}
	return key + ":" + value, nil
}
