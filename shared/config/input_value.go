package config

// Runtime-резолв значений input: применение `input:`-схемы к фактически
// переданным оператором значениям ([`docs/input.md`] → «Резолв значений»).
// Симметрично schema-валидатору (input_schema.go валидирует САМУ схему): здесь
// схема уже валидна, проверяются переданные значения.
//
// Используется и прод-путём (keeper scenario-runner перед render), и L0
// (soul-trial) — один источник правды эффективного input, чтобы L0 не
// маскировал отсутствие merge-фазы.

import (
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ResolveInputValues строит эффективный input из схемы и переданных значений:
//
//  1. merge дефолтов: для каждого параметра со `default:`, отсутствующего в
//     provided (или пустая строка для type=string без allow_empty — трактуется
//     как отсутствие, docs/input.md §«Пустые строки»), подставляется default;
//  2. required: параметр с `required: true` без значения и без default → ошибка;
//  3. value-валидация переданных значений против схемы: type-match, enum,
//     pattern (type=string) — рекурсивно вглубь array (items) и object
//     (properties + required-поля). Дефолты-выражения (`${ … }`/`{{ … }}`) и
//     значения-выражения не валидируются против enum/pattern на любом уровне
//     (финальное значение появляется только после CEL/template-фазы).
//
// provided — оператор-переданный input (incarnation.spec.input или L0
// fixtures.input); nil безопасен. Возвращает НОВУЮ map (provided не мутируется);
// неизвестные ключи provided (без схемы) пробрасываются как есть — грамматику
// «unknown input key» MVP не запрещает.
//
// Первая же ошибка прерывает резолв: понятное сообщение оператору важнее списка.
//
// vault-резолв input-ref (`vault:`-значение secret-поля с `vault_scope`) ЗДЕСЬ
// НЕ выполняется — это keeper-side scoped-фаза (см. [ResolveInputValuesVault]).
// Этот путь используют L0-trial (vault-refs не резолвятся) и контексты без
// vault-клиента; `vault:`-значение пройдёт обычную value-валидацию как строка.
func ResolveInputValues(schema InputSchemaMap, provided map[string]any) (map[string]any, error) {
	merged := mergeInputDefaults(schema, provided)
	if err := requireInputValues(schema, merged); err != nil {
		return nil, err
	}
	if err := validateInputValues(schema, merged); err != nil {
		return nil, err
	}
	return merged, nil
}

// InputVaultResolver резолвит ОДИН input-vault-ref в значение секрета.
//
// name — имя input-поля (для аудита/диагностики), s — его схема (несёт
// VaultScope + Secret), raw — переданное оператором значение (строка с
// `vault:`-префиксом). Реализация (keeper-side) проверяет scope+deny, читает
// Vault KV и пишет audit. Возвращает зарезолвленное значение, которое заменит
// `vault:`-строку ДО value-валидации (pattern/enum проверяются на нём).
type InputVaultResolver func(name string, s *InputSchema, raw string) (any, error)

// ResolveInputValuesVault — keeper-side резолв эффективного input с врезанной
// scoped vault-фазой (docs/input.md → «vault_scope»). Порядок строго:
//
//	merge дефолтов + required → vault-resolve input-ref → value-валидация.
//
// vault-resolve идёт ДО value-валидации намеренно: pattern/enum/type
// проверяются на УЖЕ зарезолвленном значении, не на строке `vault:...`.
// resolve вызывается ровно один раз на каждый input-ref (значение читается из
// Vault один раз, дальше попадает в render N раз уже резолвнутым).
//
// resolve может быть nil — тогда поведение совпадает с [ResolveInputValues]
// (vault-refs не трогаются). При nil-resolve `vault:`-значение поля проходит
// как строка (back-compat для путей без vault-клиента).
func ResolveInputValuesVault(schema InputSchemaMap, provided map[string]any, resolve InputVaultResolver) (map[string]any, error) {
	merged := mergeInputDefaults(schema, provided)
	if err := requireInputValues(schema, merged); err != nil {
		return nil, err
	}
	if resolve != nil {
		if err := resolveInputVaultRefs(schema, merged, resolve); err != nil {
			return nil, err
		}
	}
	if err := validateInputValues(schema, merged); err != nil {
		return nil, err
	}
	return merged, nil
}

// reSID — FQDN-форма SID (ADR-044 S-T1): начинается с буквы/цифры, далее
// [a-z0-9.-], до 254 символов. Совпадает с формой идентичности Soul (SID = FQDN).
// Проверяет только синтаксис значения; принадлежность каталогу — backend.
var reSID = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,253}$`)

// Регэкспы для format-значений, у которых нет точного парсера в stdlib
// (docs/input.md → «Допустимые значения format»). IP/CIDR/email/uri/duration
// валидируются парсерами net/net-mail/net-url/time — точнее и без ручного regex.
var (
	// reHostnameLabel — одна метка hostname (RFC 1123): буква/цифра по краям,
	// внутри допустим дефис, 1..63 символа.
	reHostnameLabel = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

	// reUUID — UUID любой версии (8-4-4-4-12 hex), case-insensitive.
	reUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

	// reSemver — Semantic Versioning 2.0.0 (semver.org BNF): MAJOR.MINOR.PATCH
	// + опц. pre-release (`-rc1`, `-alpha.1`) + опц. build-metadata (`+build`).
	reSemver = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-(0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(\.(0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*)?(\+[0-9a-zA-Z-]+(\.[0-9a-zA-Z-]+)*)?$`)
)

// validateFormat проверяет строковое значение против предопределённого format
// (docs/input.md). Возвращает true, если значение валидно. Неизвестный format
// (schema-validate его не пропустит) трактуется как «нечего проверять» → true.
//
// Семантика hostname vs fqdn (docs/input.md примеры): hostname = одиночная
// RFC1123-метка без точек (`redis-01`); fqdn = ≥2 меток через точку
// (`redis-01.prod.wb.local`). sid — отдельная FQDN-форма (reSID), enforce которой
// существовал до этой доработки.
func validateFormat(format, v string) bool {
	switch format {
	case "sid":
		return reSID.MatchString(v)
	case "hostname":
		return reHostnameLabel.MatchString(v)
	case "fqdn":
		return isFQDNValue(v)
	case "ipv4":
		ip := net.ParseIP(v)
		return ip != nil && ip.To4() != nil
	case "ipv6":
		ip := net.ParseIP(v)
		return ip != nil && ip.To4() == nil
	case "cidr":
		_, _, err := net.ParseCIDR(v)
		return err == nil
	case "email":
		addr, err := mail.ParseAddress(v)
		// mail.ParseAddress принимает форму "Name <a@b>"; для format:email нужен
		// голый адрес — сверяем, что распарсенный адрес совпал со входом.
		return err == nil && addr.Address == v
	case "uri":
		u, err := url.Parse(v)
		// URI обязан нести схему (docs пример https://…); относительный путь без
		// схемы — не URI в смысле format.
		return err == nil && u.Scheme != "" && (u.Host != "" || u.Opaque != "")
	case "uuid":
		return reUUID.MatchString(v)
	case "semver":
		return reSemver.MatchString(v)
	case "duration":
		_, err := time.ParseDuration(v)
		return err == nil
	}
	return true
}

// isFQDNValue — format:fqdn (docs/input.md): ≥2 RFC1123-меток через точку, каждая
// метка валидна, общая длина ≤253. Отличие от hostname — обязательная ≥1 точка.
// Отдельна от semantic.isFQDN (тот валидирует SID coven-config: lowercase-only,
// принимает одиночную метку) — у format:fqdn своя семантика «полное имя».
func isFQDNValue(v string) bool {
	if len(v) == 0 || len(v) > 253 {
		return false
	}
	labels := strings.Split(v, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !reHostnameLabel.MatchString(l) {
			return false
		}
	}
	return true
}

// vaultRefMarker — префикс input-значения-ссылки на Vault KV. Совпадает с
// формой авторских ref-ов (render.vaultRefPrefix / vault.ParseRef); здесь
// используется только для детекта «значение поля — vault-ref».
const vaultRefMarker = "vault:"

// resolveInputVaultRefs обходит schema и для каждого присутствующего
// string-значения, начинающегося на `vault:`, вызывает resolve, заменяя
// значение результатом. Поле без `vault_scope`, несущее `vault:`-значение, —
// ошибка (default-deny): сам resolve обязан её поднять, но детект ref-а нужен
// здесь, чтобы вызвать resolve вообще. Только top-level secret-поля (vault_scope
// применим лишь к ним) — вглубь array/object не спускаемся.
func resolveInputVaultRefs(schema InputSchemaMap, merged map[string]any, resolve InputVaultResolver) error {
	for name, s := range schema {
		if s == nil || s.Type != "string" {
			continue
		}
		raw, ok := merged[name]
		if !ok {
			continue
		}
		str, ok := raw.(string)
		if !ok || !strings.HasPrefix(str, vaultRefMarker) {
			continue
		}
		val, err := resolve(name, s, str)
		if err != nil {
			return err
		}
		merged[name] = val
	}
	return nil
}

// mergeInputDefaults строит новую map: provided + подстановка default для
// отсутствующих/пустых параметров (шаг 1 docs/input.md «Резолв значений»).
// required не проверяется здесь — это отдельная фаза requireInputValues,
// чтобы vault-резолв (если он есть) встал между merge и валидацией.
func mergeInputDefaults(schema InputSchemaMap, provided map[string]any) map[string]any {
	out := make(map[string]any, len(provided)+len(schema))
	for k, v := range provided {
		out[k] = v
	}
	for name, s := range schema {
		if s == nil {
			continue
		}
		raw, passed := out[name]
		if passed && isAbsentValue(raw, s) {
			passed = false
			delete(out, name)
		}
		if passed {
			continue
		}
		if hasDefault(s) {
			out[name] = s.Default
		}
	}
	return out
}

// requireInputValues — шаг 2: параметр с required:true (безусловно) или с
// required_when, чей предикат над смерженным input истинен (условно), без
// значения и без default → ошибка (после merge — значит ни provided, ни default
// не дали его).
//
// required_when вычисляется ПОСЛЕ mergeInputDefaults (дефолты материализованы) —
// предикат видит эффективный input. Контекст предиката — только input.* (узкий
// CEL-env, input_required_when.go); это input-валидация, не render. Сообщение
// несёт ту же узнаваемую форму «обязателен, но не передан и не имеет default»,
// что и безусловный required — downstream-детект (checkdrift.isInputRequiredErr)
// ловит оба единым матчингом.
func requireInputValues(schema InputSchemaMap, merged map[string]any) error {
	for name, s := range schema {
		if s == nil {
			continue
		}
		if _, present := merged[name]; present {
			continue
		}
		if s.requiredKind == requiredBool && s.Required {
			return fmt.Errorf("input %q обязателен, но не передан и не имеет default", name)
		}
		if s.RequiredWhen != "" {
			required, err := evalRequiredWhen(s.RequiredWhen, merged)
			if err != nil {
				return fmt.Errorf("input %q: вычисление required_when: %w", name, err)
			}
			if required {
				return fmt.Errorf("input %q обязателен, но не передан и не имеет default (required_when: %s)", name, s.RequiredWhen)
			}
		}
	}
	return nil
}

// validateInputValues — шаг 3: value-валидация присутствующих значений против
// схемы (type/enum/pattern, рекурсивно). Дефолты-значения тоже валидируются:
// прежняя ResolveInputValues пропускала default без проверки, но дефолт уже
// прошёл schema-time validateDefaultContent — повторная проверка эквивалентна и
// упрощает фазовую модель. Значения-выражения (`${…}`/`{{…}}`) освобождены от
// enum/pattern на своём уровне (см. validateValueAt).
func validateInputValues(schema InputSchemaMap, merged map[string]any) error {
	for name, s := range schema {
		if s == nil {
			continue
		}
		raw, present := merged[name]
		if !present {
			continue
		}
		if err := validateInputValue(name, s, raw); err != nil {
			return err
		}
	}
	return nil
}

// isAbsentValue — true, если переданное значение трактуется как «не передано»:
// пустая строка для type=string без allow_empty (docs/input.md §«Пустые
// строки»). Прочие значения (включая 0, false, пустой список) — переданы.
func isAbsentValue(v any, s *InputSchema) bool {
	if s.Type != "string" || s.AllowEmpty {
		return false
	}
	str, ok := v.(string)
	return ok && str == ""
}

// hasDefault — у параметра объявлен default (не nil-значение). nil-default
// неотличим от отсутствия default — это не дефолт-значение, а его отсутствие.
func hasDefault(s *InputSchema) bool {
	return s.Default != nil
}

// validateInputValue проверяет одно переданное значение против схемы параметра
// (граница доверия оператор→рендер). Тонкая обёртка над рекурсивным
// validateValueAt: задаёт корневой путь `$.<name>` для понятных сообщений.
func validateInputValue(name string, s *InputSchema, v any) error {
	return validateValueAt("$."+name, s, v)
}

// maskedSecretLiteral — плейсхолдер вместо сырого значения secret-поля в
// сообщении об ошибке валидации. Архитектура (ADR-010, secret-маскинг) требует
// не показывать секреты ни в одном выходном канале; ошибка валидации оседает в
// incarnation.StatusDetails / audit, поэтому маскинг нужен здесь, на источнике.
const maskedSecretLiteral = "<masked>"

// literalFor возвращает строку значения для диагностики ошибки: сырой литерал
// для обычного поля (диагностика важна), плейсхолдер для secret-поля. Тип не
// раскрывается отдельно — формат поля известен из самой схемы/пути.
func literalFor(s *InputSchema, v any) string {
	if s != nil && s.Secret {
		return maskedSecretLiteral
	}
	return formatLiteral(v)
}

// validateValueAt рекурсивно валидирует переданное значение против схемы на
// произвольной глубине вложенности (qa.1: top-level-only пропускал мусор внутри
// array/object вплоть до CEL/shell — дыра границы доверия). Симметрична
// schema-time validateDefaultValue (input_schema.go), но в runtime-форме:
// возвращает error с путём (`$.users[1].acl`) вместо diag, и применяет полный
// набор проверок переданного значения (type → enum → pattern → required-props),
// а не только type-match (default-литерал автору доверяем больше, чем
// оператор-вводу).
//
// Проверяется то, что выражено в схеме (type/enum/pattern); полный
// format-валидатор (ipv4/fqdn/…) — post-MVP, формы пока не используются в
// прод-сервисах (redis: только pattern).
//
// На каждом уровне строковое значение-выражение (`${ … }` / `{{ … }}`)
// освобождается от enum И pattern: финальная форма появится только после
// render-фазы (docs/input.md §«Резолв значений»).
func validateValueAt(path string, s *InputSchema, v any) error {
	if s == nil || s.Type == "" {
		return nil
	}

	// Строка-выражение освобождается от value-проверок данного уровня (enum +
	// pattern) — её финальная форма здесь неизвестна. Тип "string" при этом
	// формально соблюдён, поэтому type-проверку ниже она проходит штатно.
	exprString := s.Type == "string" && isStringExpr(v)

	if !valueMatchesType(v, s.Type) {
		return fmt.Errorf("input %s = %s не соответствует типу %q", path, literalFor(s, v), s.Type)
	}

	if !exprString && len(s.Enum) > 0 &&
		(s.Type == "string" || s.Type == "integer" || s.Type == "number" || s.Type == "boolean") {
		if !enumContains(s.Enum, v) {
			// enum-литералы поля тоже маскируются: для secret-поля сам список
			// допустимых значений — секрет (например, фикс-набор паролей).
			if s.Secret {
				return fmt.Errorf("input %s = %s не входит в enum", path, maskedSecretLiteral)
			}
			return fmt.Errorf("input %s = %s не входит в enum %s", path, formatLiteral(v), formatEnum(s.Enum))
		}
	}

	// format — каждое значение проверяется против предопределённого формата
	// (docs/input.md → «Допустимые значения format»: hostname/fqdn/ipv4/ipv6/
	// cidr/email/uri/uuid/semver/duration + sid). Проверяется только СТРУКТУРА
	// значения; принадлежность каталогу/доступность ресурса здесь НЕ проверяется
	// (для sid каталог hosts/Choir-партию резолвит backend). Для type=array
	// проверка применяется поэлементно через items (рекурсия validateValueAt).
	// Освобождена для значения-выражения по той же причине, что enum/pattern.
	if !exprString && s.Type == "string" && s.Format != "" {
		if !validateFormat(s.Format, v.(string)) {
			return fmt.Errorf("input %s = %s не соответствует формату %q", path, literalFor(s, v), s.Format)
		}
	}

	if !exprString && s.Type == "string" && s.Pattern != "" {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			// Схема уже провалидирована (input_pattern_invalid) — сюда невалидный
			// pattern долететь не должен; defensive.
			return fmt.Errorf("input %s: pattern %q не компилируется: %w", path, s.Pattern, err)
		}
		if !re.MatchString(v.(string)) {
			return fmt.Errorf("input %s = %s не соответствует pattern %q", path, literalFor(s, v), s.Pattern)
		}
	}

	// min_length / max_length — длина в Unicode-кодпоинтах (docs/input.md), не в
	// байтах: utf8.RuneCountInString. Освобождаются для значения-выражения по той
	// же причине, что enum/pattern — финальная длина появится только после render.
	// Schema-time уже гарантировала min_length >= 0, max_length >= min_length;
	// здесь проверяется ФАКТИЧЕСКОЕ значение оператора (раньше не enforced — drift
	// lint↔runtime).
	if !exprString && s.Type == "string" && (s.MinLength != nil || s.MaxLength != nil) {
		n := utf8.RuneCountInString(v.(string))
		if s.MinLength != nil && n < *s.MinLength {
			return fmt.Errorf("input %s = %s короче min_length %d (длина %d)", path, literalFor(s, v), *s.MinLength, n)
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			return fmt.Errorf("input %s = %s длиннее max_length %d (длина %d)", path, literalFor(s, v), *s.MaxLength, n)
		}
	}

	switch s.Type {
	case "array":
		return validateArrayItems(path, s, v)
	case "object":
		return validateObjectFields(path, s, v)
	}
	return nil
}

// validateArrayItems валидирует каждый элемент массива против items-схемы и
// проверяет лимиты длины (min_items/max_items). type-match массива уже проверен
// validateValueAt выше. Schema-time уже гарантировал min_items >= 0 и
// max_items >= min_items; здесь проверяется ФАКТИЧЕСКАЯ длина значения оператора
// (раньше не enforced — drift lint↔runtime; для sid-list ADR-044 S-T1 лимиты
// несут смысл «не меньше/не больше N выбранных хостов»).
func validateArrayItems(path string, s *InputSchema, v any) error {
	arr := v.([]any)
	n := len(arr)
	if s.MinItems != nil && n < *s.MinItems {
		return fmt.Errorf("input %s содержит %d элементов, меньше min_items %d", path, n, *s.MinItems)
	}
	if s.MaxItems != nil && n > *s.MaxItems {
		return fmt.Errorf("input %s содержит %d элементов, больше max_items %d", path, n, *s.MaxItems)
	}
	if s.Items == nil {
		// items обязателен для array (schema-validate ловит его отсутствие); без
		// схемы элементов вглубь идти не по чему.
		return nil
	}
	for i, el := range arr {
		if err := validateValueAt(fmt.Sprintf("%s[%d]", path, i), s.Items, el); err != nil {
			return err
		}
	}
	return nil
}

// validateObjectFields валидирует поля объекта против properties-схем и
// проверяет наличие required-полей. type-match объекта уже проверен выше.
//
// Поля без схемы в properties не валидируются (additional_properties — MVP не
// проверяет вглубь runtime-значения, симметрично validateDefaultValue).
func validateObjectFields(path string, s *InputSchema, v any) error {
	obj := v.(map[string]any)

	for _, req := range s.RequiredProps {
		fv, present := obj[req]
		if !present || isMissingField(s.Properties[req], fv) {
			return fmt.Errorf("input %s.%s обязателен, но не передан", path, req)
		}
	}

	for k, fv := range obj {
		prop := s.Properties[k]
		if prop == nil || prop.Type == "" {
			continue
		}
		if err := validateValueAt(path+"."+k, prop, fv); err != nil {
			return err
		}
	}
	return nil
}

// isMissingField — поле object трактуется как непереданное по той же семантике
// пустой строки, что и top-level isAbsentValue: пустая строка для type=string
// без allow_empty = «не передано» (docs/input.md §«Пустые строки»). prop может
// быть nil (required ссылается на поле вне properties — schema-validate такое
// ловит, defensive здесь: считаем переданным).
func isMissingField(prop *InputSchema, v any) bool {
	if prop == nil {
		return false
	}
	return isAbsentValue(v, prop)
}

// isStringExpr — значение является строкой-выражением (`${ … }` / `{{ … }}`).
// Удобная обёртка над isExprLiteral для any-значений: non-string → false.
func isStringExpr(v any) bool {
	str, ok := v.(string)
	return ok && isExprLiteral(str)
}

// isExprLiteral — строка является CEL-/template-выражением (`${ … }` / `{{ … }}`),
// финальное значение которого появится только после render-фазы. Такие значения
// нельзя проверять против pattern на этапе резолва. Та же эвристика, что в
// defaultMatchesType для default-выражений.
func isExprLiteral(s string) bool {
	t := strings.TrimSpace(s)
	return (strings.HasPrefix(t, "${") && strings.HasSuffix(t, "}")) ||
		(strings.HasPrefix(t, "{{") && strings.HasSuffix(t, "}}"))
}
