package audit

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// maskedValue — placeholder, которым заменяются sensitive значения в
// payload перед INSERT. Совпадает с маскировкой OTel-exporter Operator
// API (docs/keeper/operator-api.md → Secret masking).
const maskedValue = "***MASKED***"

// sensitiveKeyRe — регистронезависимый substring-матч по имени ключа.
// Маскирует любой ключ, СОДЕРЖАЩИЙ один из секрет-фрагментов, а не только
// точное совпадение: `bootstrap_token`, `aws_secret_access_key`,
// `db_password`, `tls_private_key`, `credentials_ref`, `jwt_signing_key`
// — все попадают под фильтр (security-review H1: exact-match на `"token"`
// пропускал `"bootstrap_token"` → plaintext leak).
//
// Расширение каталога — обычным PR в этот regex при появлении новой
// sensitive области; инвариант словаря не требует propose-and-wait
// (формализация наблюдаемого pattern-а, см. docs/architecture.md → Error
// codes / расширение каталога).
var sensitiveKeyRe = regexp.MustCompile(
	`(?i)(token|secret|password|passwd|private[_-]?key|privatekey|credential|signing[_-]?key|api[_-]?key|access[_-]?key)`,
)

// extraExactKeys — короткие ключи без секрет-фрагмента в имени, которые
// всё равно несут секрет. Substring-regex их не поймал бы (`jwt` не
// содержит ни token, ни secret), поэтому держим отдельным exact-set
// (регистронезависимое сравнение по lower-case ключу).
var extraExactKeys = map[string]struct{}{
	"jwt": {},
}

// isSensitiveKey — true, если ключ нужно маскировать целиком (по substring-
// regex или по extra-exact-set). Регистронезависимо.
func isSensitiveKey(key string) bool {
	if sensitiveKeyRe.MatchString(key) {
		return true
	}
	_, ok := extraExactKeys[strings.ToLower(key)]
	return ok
}

// CredentialsRefPrefix — каноническая форма vault-reference на секрет KV
// ([ADR-017]: `vault:<mount>/<path>`, дефолтный mount `secret`). Любое
// string-значение, СОДЕРЖАЩЕЕ этот маркер, маскируется целиком (vault-путь
// может leak в логи/observability через payload). Применяется к string-значениям
// независимо от ключа (отдельный второй фильтр поверх key-match).
//
// Substring-, не prefix-match (security-review: vault-ref утекает не только
// «голым» значением `vault:secret/x`, но и склеенным в строку — error-
// сообщения вида `render: ... vault:secret/db ...`, которые попадают в
// status_details (GET incarnation) и error_summary. Префиксный фильтр их
// пропускал → plaintext leak vault-пути в наблюдаемый канал).
//
// Маскинг делает [vaultRefRe] — регэксп по форме `vault:<mount>/` (любой mount,
// не только дефолтный `secret`): security-аудит K5 показал, что оператор вправе
// настроить кастомный KV-mount в `keeper.yml` (config.Vault.KVMount), и тогда
// ref-ы вида `vault:kv/…` / `vault:db-creds/…` утекали в audit/OTel/SSE/error в
// plaintext — маркер `vault:secret/` их не ловил. Регэксп требует mount-токен +
// `/`, поэтому легитимные строки без vault-ref не over-маскируются:
// `https://vault:8200` (нет `/` после токена-порта), `hashicorp/vault:1.18`
// (`1.18` без `/`), `vault: KV error` (пробел не в классе токена) — passthrough.
//
// CredentialsRefPrefix оставлен как дефолт-mount-константа для прочих
// потребителей (provider-ref-валидация); сам маскинг идёт через [vaultRefRe].
const CredentialsRefPrefix = "vault:secret/"

// vaultRefRe матчит каноничную форму vault-reference `vault:<mount>/<path>` с
// произвольным mount-токеном (`secret`, `kv`, `db-creds`, …). Mount-токен —
// `[A-Za-z0-9._-]+` (символы, допустимые в Vault-mount-path), за ним
// обязательный `/` (разделитель mount↔rel из vault.ParseRef). Это и закрывает
// K5-пробел (кастомный mount), и не over-маскирует строки без ref-формы.
var vaultRefRe = regexp.MustCompile(`vault:[A-Za-z0-9._-]+/`)

// MaskSecrets возвращает копию payload с маскированными sensitive
// значениями. Walk рекурсивный — обходит вложенные maps и slices, включая
// типизированные контейнеры (`map[string]string`, `[]string`, struct-ы,
// указатели) через reflect.
//
// Правила маскировки:
//
//   - Ключ (case-insensitive) матчит [isSensitiveKey] → значение
//     заменяется на `"***MASKED***"` (тип теряется, это compliance-
//     требование).
//   - Строковое значение содержит `vault:secret/` (в любой позиции) → также
//     `"***MASKED***"` (защита от leak vault-ref-ов в логи/observability через
//     любой ключ, включая склеенные в error-строки; маркер сужен — см.
//     [CredentialsRefPrefix]).
//   - Map (любого key/value-типа) → рекурсивный walk; ключ нормализуется
//     стрингификацией для key-match.
//   - Slice / array → рекурсивный walk элементов.
//   - Struct → walk полей (имя поля = ключ); unexported-поля пропускаются.
//   - Pointer / interface → разыменование и walk.
//   - Остальные scalar-значения — копируются как есть.
//
// payload не мутируется; возвращается новая map того же shape (top-level
// всегда `map[string]any`, вложенность нормализуется к `map[string]any`/
// `[]any`/scalar при walk-е typed-контейнеров).
// nil-вход → nil-выход (caller обработает как пустой payload).
func MaskSecrets(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		if isSensitiveKey(k) {
			out[k] = maskedValue
			continue
		}
		out[k] = maskValue(v)
	}
	return out
}

// maskValue — walk-helper. Не экспортирован: формат и shape result-а
// совместимы только в рамках MaskSecrets.
//
// Fast-path для частых типов (`string`/`map[string]any`/`[]any`/
// `map[string]string`/`[]string`) — без reflect. Остальные контейнеры
// (struct, прочие map/slice/ptr) проходят через reflect-walk — это
// холодный путь (audit/SSE-payload), читаемость важнее аллокаций.
func maskValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return maskString(x)
	case map[string]any:
		return MaskSecrets(x)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = maskValue(el)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, el := range x {
			if isSensitiveKey(k) {
				out[k] = maskedValue
				continue
			}
			out[k] = maskString(el)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = maskString(el)
		}
		return out
	default:
		return maskReflect(reflect.ValueOf(v))
	}
}

// maskString маскирует string-значение, если оно содержит vault-ref-маркер
// (в любой позиции, не только префиксом — см. [CredentialsRefPrefix]).
func maskString(s string) any {
	if vaultRefRe.MatchString(s) {
		return maskedValue
	}
	return s
}

// maskReflect — reflect-fallback для типизированных контейнеров, которые
// не покрыты fast-path-ом maskValue (struct, map/slice произвольных типов,
// указатели). Возвращает нормализованную к `map[string]any`/`[]any`/scalar
// структуру.
func maskReflect(rv reflect.Value) any {
	if !rv.IsValid() {
		return nil
	}
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return maskReflect(rv.Elem())
	case reflect.String:
		return maskString(rv.String())
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			k := stringifyKey(iter.Key())
			if isSensitiveKey(k) {
				out[k] = maskedValue
				continue
			}
			out[k] = maskReflect(iter.Value())
		}
		return out
	case reflect.Slice, reflect.Array:
		n := rv.Len()
		out := make([]any, n)
		for i := 0; i < n; i++ {
			out[i] = maskReflect(rv.Index(i))
		}
		return out
	case reflect.Struct:
		t := rv.Type()
		out := make(map[string]any, rv.NumField())
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				// Unexported поле — reflect не даёт прочитать значение
				// безопасно; пропускаем (в payload секреты живут в
				// exported-полях).
				continue
			}
			name := structFieldName(f)
			if isSensitiveKey(name) {
				out[name] = maskedValue
				continue
			}
			out[name] = maskReflect(rv.Field(i))
		}
		return out
	default:
		return rv.Interface()
	}
}

// stringifyKey приводит reflect-ключ map-ы к строке для key-match.
// Не-string ключи (int, и т.п.) стрингифицируются через %v — секрет в
// таком ключе маловероятен, но key-match всё равно отрабатывает по имени.
func stringifyKey(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	return fmt.Sprintf("%v", k.Interface())
}

// structFieldName — имя поля для key-match: json-tag (без опций), если
// задан, иначе имя поля. json-tag важен, потому что payload-структуры
// сериализуются по тегам — секрет с тегом `json:"bootstrap_token"` должен
// матчиться так же, как map-ключ `bootstrap_token`.
func structFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return f.Name
	}
	if comma := strings.IndexByte(tag, ','); comma >= 0 {
		tag = tag[:comma]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}
