package audit

import (
	"log/slog"
	"reflect"
)

// Декларативный secret-маскинг ([ADR-010] §7.4, три слоя AUGMENT/OR):
//
//  1. schema — поле маскируется, если его путь объявлен secret:true в активной
//     схеме источника (input_schema/state_schema/manifest InputParamDef.Secret).
//     Декларатив primary: оператор/автор явно помечает поле секретом, маскинг не
//     зависит от угадывания по имени ключа.
//  2. vault-происхождение — string-значение содержит `vault:<mount>/` ([vaultRefRe],
//     mask.go). Это маскинг по СОДЕРЖИМОМУ (vault-ref в значении), не по имени.
//  3. seal (render-time provenance) — путь ячейки помечен sealed на render-фазе
//     (CEL-выражение читало secret-источник: secret-input/vault()/транзитивно
//     vars/compute). seal-набор приходит из render-прохода (keeper/internal/render),
//     передаётся сюда как набор dot-путей [SealOpts.Sealed].
//  4. regex-LAST-RESORT — [sensitiveKeyRe]/[isSensitiveKey] (mask.go). Ловит
//     класс sensitive-by-name, не покрытый декларативом (внутренние bootstrap_token/
//     jwt/creds без схемы). Срабатывание ИСКЛЮЧИТЕЛЬНО этого слоя (schema/vault/seal
//     молчали) → аларм [SealOpts.RegexFallback] + warn-лог: сигнал пробела
//     декларатива, чтобы класс закрывать структурно, а не полагаться на имя ключа.
//
// Слои OR-объединены: любой сработавший → ячейка маскируется. Старая
// [MaskSecrets] (mask.go) — слои vault+regex без schema/seal — сохранена
// (additive, её зовут ~46 точек); [MaskSecretsWithSchema] добавляет schema-слой
// для read-path, [MaskSecretsSealed] — seal-слой для render write-точек.

// SecretSchema — узкая поверхность «является ли путь secret»: декларативный слой
// (1). Реализуется keeper-side над config.InputSchemaMap (input/scenario-схема) и
// над flat state_schema (`properties.<field>.secret: true`). dot-путь — как
// человекочитаемый path ячейки render (`acl[0].password`, `config.tls_key`).
//
// shared/audit не импортирует shared/config (слоистость): caller строит
// SecretSchema из своей схемы. Простейшая реализация — [SecretPathSet].
type SecretSchema interface {
	// IsSecret — путь поля (dot/idx-форма, как ключи payload) объявлен secret.
	IsSecret(path string) bool
}

// SecretPathSet — множество dot-путей, объявленных secret. Простейшая
// [SecretSchema]: keeper строит его обходом своей схемы один раз. Индексы среза
// нормализуются — путь `acl[0].password` сверяется по обоим: точному `acl[0].
// password` И обобщённому `acl[].password` (схема описывает элемент массива без
// конкретного индекса), см. [normalizeIdx].
type SecretPathSet map[string]bool

// IsSecret — путь в наборе (точная форма или с обобщёнными индексами `[]`).
func (s SecretPathSet) IsSecret(path string) bool {
	if s[path] {
		return true
	}
	return s[normalizeIdx(path)]
}

// SealOpts — параметры layered-маскинга поверх [MaskSecrets].
type SealOpts struct {
	// Schema — декларативный слой (1); nil → слой выключен.
	Schema SecretSchema

	// Sealed — слой seal (3): набор dot-путей ячеек, помеченных sealed на
	// render-фазе. nil/пуст → слой выключен. Сверка — точная по пути ячейки И по
	// обобщённой idx-форме (симметрично SecretPathSet).
	Sealed map[string]bool

	// RegexFallback — аларм (4): вызывается, когда ячейку поймал ТОЛЬКО
	// regex-last-resort (schema/vault/seal по этому пути молчали). nil → аларм не
	// зовётся (метрику/лог подключает keeper через DefaultSealHooks). Идемпотентно
	// не требуется: вызывается один раз на каждую такую ячейку.
	RegexFallback func(path string)

	// Logger — канал warn-лога regex-fallback. nil → лог подавлен (аларм-метрика
	// всё равно зовётся через RegexFallback).
	Logger *slog.Logger
}

// MaskSecretsWithSchema — read-path вариант ([ADR-010] §7.4): layered-маскинг
// payload по схеме источника (slice 1) поверх vault+regex слоёв [MaskSecrets].
// schema nil → деградирует к [MaskSecrets] байт-в-байт (schema-слой выключен).
// regex-fallback аларм берётся из [DefaultSealHooks] (keeper подключает метрику/
// лог; в тестах/офлайне nil → no-op).
//
// payload не мутируется; форма результата — как у [MaskSecrets].
func MaskSecretsWithSchema(payload map[string]any, schema SecretSchema) map[string]any {
	return MaskSecretsSealed(payload, SealOpts{
		Schema:        schema,
		RegexFallback: DefaultSealHooks.RegexFallback,
		Logger:        DefaultSealHooks.Logger,
	})
}

// MaskSecretsSealed — полный layered-маскинг: schema (opts.Schema) + vault +
// seal (opts.Sealed) + regex-last-resort с алармом. Все слои OR. Используется
// render write-точками (error_summary/status_details/dispatch), которым доступен
// seal-набор прохода. opts zero-value → эквивалент [MaskSecrets] + аларм
// отключён.
//
// payload не мутируется; nil-вход → nil-выход.
func MaskSecretsSealed(payload map[string]any, opts SealOpts) map[string]any {
	if payload == nil {
		return nil
	}
	return maskMapLayered(payload, "", opts)
}

// maskMapLayered — layered walk одного map-уровня. path — dot-путь до этого map
// (пустой на корне). Порядок слоёв на КЛЮЧЕ:
//
//	schema(1) ∨ seal(3)  — по ПУТИ ячейки (декларатив/taint, не имя ключа);
//	regex-last-resort(4) — по ИМЕНИ ключа (isSensitiveKey).
//
// Если ключ поймал ТОЛЬКО regex (schema/seal по пути молчали), значение строковое
// и оно НЕ vault-ref (слой 2 тоже молчал бы) — это чистый regex-fallback: аларм
// (метрика+warn-лог), сигнал пробела декларатива. vault-слой(2) — по содержимому
// строкового значения внутри maskValueLayered.
func maskMapLayered(m map[string]any, path string, opts SealOpts) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		cell := joinPath(path, k)

		declarative := pathIsSecret(cell, opts) // слои 1 (schema) + 3 (seal)
		if declarative {
			out[k] = maskedValue
			continue
		}

		// Слой 4: regex-last-resort по имени ключа. Срабатывает ТОЛЬКО когда
		// декларатив молчал — иначе это не «единственный сработавший слой».
		if isSensitiveKey(k) {
			alarmRegexFallback(cell, v, opts)
			out[k] = maskedValue
			continue
		}

		out[k] = maskValueLayered(v, cell, opts)
	}
	return out
}

// alarmRegexFallback зовёт аларм (метрика+warn-лог), когда ключ поймал ТОЛЬКО
// regex-last-resort и значение НЕ vault-ref (vault-слой(2) поймал бы его сам —
// тогда regex не «единственный»). vault-ref-значение под sensitive-ключом
// маскируется обоими слоями, аларм не нужен (декларативного пробела нет:
// vault-происхождение — структурный сигнал). Аларм фиксирует именно класс
// sensitive-by-name без схемы/seal/vault (ожидаемый класс ii — внутренние
// bootstrap_token/jwt/creds), чтобы видеть, что декларатив его пока не покрывает.
func alarmRegexFallback(cell string, v any, opts SealOpts) {
	if s, ok := v.(string); ok && vaultRefRe.MatchString(s) {
		return // vault-слой поймал бы сам — regex не единственный
	}
	if opts.RegexFallback != nil {
		opts.RegexFallback(cell)
	}
	if opts.Logger != nil {
		// Путь ячейки — НЕ секрет (имя поля), значение НЕ логируем.
		opts.Logger.Warn("audit: secret пойман regex-last-resort, декларатив (schema/seal/vault) молчал — пробел декларатива",
			slog.String("path", cell))
	}
}

// maskValueLayered — layered walk значения ячейки cell. string → vault-слой (2) и
// regex-last-resort (4, с алармом); контейнеры → рекурсивный layered walk.
func maskValueLayered(v any, cell string, opts SealOpts) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return maskStringLayered(x, cell, opts)
	case map[string]any:
		return maskMapLayered(x, cell, opts)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = maskValueLayered(el, joinIdx(cell, i), opts)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, el := range x {
			sub := joinPath(cell, k)
			if pathIsSecret(sub, opts) {
				out[k] = maskedValue
				continue
			}
			if isSensitiveKey(k) {
				alarmRegexFallback(sub, el, opts)
				out[k] = maskedValue
				continue
			}
			out[k] = maskStringLayered(el, sub, opts)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = maskStringLayered(el, joinIdx(cell, i), opts)
		}
		return out
	default:
		// Типизированные контейнеры (struct, прочие map/slice/ptr) — reflect-walk
		// БЕЗ schema/seal-слоёв (их пути не выражаются reflect-именами полей
		// структур в терминах render-path). vault+regex по значению/ключу — как
		// MaskSecrets. Холодный путь (payload в этих точках — map[string]any).
		return maskReflect(reflect.ValueOf(v))
	}
}

// maskStringLayered маскирует string по слоям 2 (vault) и 4 (regex-last-resort).
// schema/seal уже проверены по пути выше (pathIsSecret). vault-слой — по
// содержимому. Чистый regex-fallback по СОДЕРЖИМОМУ строки не применяется
// (sensitiveKeyRe — по имени КЛЮЧА, не значению); значение-уровневый
// regex-fallback решается на уровне ключа в key-match (см. отдельную ветку
// maskKeyLayered не нужна — key-match идёт в maskMapLayered, но там приоритет у
// schema/seal). Здесь только vault-by-content.
func maskStringLayered(s, _ string, _ SealOpts) any {
	if vaultRefRe.MatchString(s) {
		return maskedValue
	}
	return s
}

// pathIsSecret — слой 1 (schema) ИЛИ слой 3 (seal) пометили путь секретом.
func pathIsSecret(path string, opts SealOpts) bool {
	if opts.Schema != nil && opts.Schema.IsSecret(path) {
		return true
	}
	if len(opts.Sealed) > 0 {
		if opts.Sealed[path] || opts.Sealed[normalizeIdx(path)] {
			return true
		}
	}
	return false
}
