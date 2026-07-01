package herald

// Декларативный дескриптор типа канала Herald (ADR-052 amendment) — ЕДИНЫЙ
// источник, из которого КОДОМ выводятся: (1) generic-валидатор config, (2)
// список типов для huma-enum, (3) сверка-тест с PG-CHECK, (4) каталог-эндпоинт
// (следующий слайс). Устраняет три раздельных места под один список типов.
//
// Доменная валидация транспорта (SSRF-guard URL webhook) — НЕ часть generic-
// обхода: она живёт в per-type domain-хуке ([HeraldTypeSpec.validate]), потому
// что зависит от opt-out-флагов и netguard-контура, которые дескриптору полей
// знать незачем.
//
// NB(имена на подтверждении PM/юзера, propose-and-wait): HeraldTypeSpec /
// HeraldFieldSpec / FieldKind предложены architect; в naming-rules пока НЕ
// фиксируются.

import (
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// FieldKind — вид значения config-поля для generic-валидатора и каталога
// (имя на подтверждении). string/int/bool/enum — скаляры; map/list —
// контейнеры; url — строка-URL (SSRF-guard живёт в domain-хуке, здесь только
// «строка»); vault_ref — строка-vault-ref (значение обязано парситься
// vault.ParseRef, секрет в PG cleartext не хранится).
type FieldKind string

const (
	KindString   FieldKind = "string"
	KindInt      FieldKind = "int"
	KindBool     FieldKind = "bool"
	KindEnum     FieldKind = "enum"
	KindMap      FieldKind = "map"
	KindList     FieldKind = "list"
	KindURL      FieldKind = "url"
	KindVaultRef FieldKind = "vault_ref"
)

// HeraldFieldSpec — описание одного config-поля типа (имя на подтверждении).
// Secret=true ⟹ поле держит vault-ref (Kind обязан быть KindVaultRef; секрет
// в config, не в top-level secret_ref — разводка ADR-052 amendment). EnumValues
// заполняется только для Kind==KindEnum (допустимый набор строк, пустой элемент
// = «поле опущено/plain» разрешён явным включением "" в набор).
type HeraldFieldSpec struct {
	Name       string
	Label      string
	Required   bool
	Secret     bool
	Kind       FieldKind
	EnumValues []string
}

// HeraldTypeSpec — дескриптор типа канала (имя на подтверждении): тип + поля +
// доменный per-type хук транспорт-валидации. validate вызывается generic-
// валидатором ПОСЛЕ обхода полей (config уже прошёл required/kind/secret-
// проверки), получает уже провалидированный по форме config.
type HeraldTypeSpec struct {
	Type   HeraldType
	Fields []HeraldFieldSpec
	// validate — доменный хук транспорта (nil = нет доп. проверок). Для webhook —
	// SSRF-guard URL с учётом opt-out-флагов; для telegram — chat_id непустой.
	validate func(config map[string]any) error
}

// heraldTypeSpecs — канонический реестр дескрипторов, ЕДИНЫЙ источник типов.
// Новый тип добавляется ОДНОЙ записью сюда (+ CHECK-миграция + huma-enum, оба
// сверяются guard-тестом с AllHeraldTypes).
var heraldTypeSpecs = map[HeraldType]HeraldTypeSpec{
	HeraldWebhook: {
		Type: HeraldWebhook,
		Fields: []HeraldFieldSpec{
			{Name: "url", Label: "URL", Required: true, Kind: KindURL},
			{Name: "headers", Label: "HTTP-заголовки", Kind: KindMap},
			{Name: "http_allowed", Label: "Разрешить http://", Kind: KindBool},
			{Name: "allow_private", Label: "Разрешить приватные IP", Kind: KindBool},
		},
		validate: validateWebhookConfig,
	},
	HeraldTelegram: {
		Type: HeraldTelegram,
		Fields: []HeraldFieldSpec{
			{Name: "bot_token_ref", Label: "Vault-ref токена бота", Required: true, Secret: true, Kind: KindVaultRef},
			{Name: "chat_id", Label: "ID чата/канала", Required: true, Kind: KindString},
			{Name: "parse_mode", Label: "Формат текста", Kind: KindEnum, EnumValues: []string{"", "MarkdownV2", "HTML"}},
		},
		validate: validateTelegramConfig,
	},
}

// typeSpec возвращает дескриптор типа. ok=false — тип неизвестен (не в реестре).
func typeSpec(t HeraldType) (HeraldTypeSpec, bool) {
	s, ok := heraldTypeSpecs[t]
	return s, ok
}

// AllHeraldTypes — отсортированный список известных типов канала (единый
// источник для huma-enum-сверки и PG-CHECK-сверки; guard-тест ловит расхождение
// трёх мест). Сортировка детерминирует enum-строку.
func AllHeraldTypes() []HeraldType {
	out := make([]HeraldType, 0, len(heraldTypeSpecs))
	for t := range heraldTypeSpecs {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// validateBySpec — generic-обход дескриптора: required-присутствие + kind-
// соответствие + secret-поле → валидный vault-ref. Доменный per-type хук
// (spec.validate) вызывается ПОСЛЕ обхода. Неизвестные ключи config НЕ
// отвергаются (forward-compat: JSONB мог прийти из более новой версии; huma-
// уровень режет unknown-поля на wire, а domain остаётся терпимым).
func validateBySpec(spec HeraldTypeSpec, config map[string]any) error {
	for _, f := range spec.Fields {
		raw, present := config[f.Name]
		if !present {
			if f.Required {
				return fmt.Errorf("herald: %s config requires %q", spec.Type, f.Name)
			}
			continue
		}
		if err := checkFieldKind(spec.Type, f, raw); err != nil {
			return err
		}
	}
	if spec.validate != nil {
		return spec.validate(config)
	}
	return nil
}

// checkFieldKind проверяет одно присутствующее поле по его Kind. Пустая строка в
// required-поле трактуется как отсутствие значения (required нарушен). Для
// vault_ref дополнительно проверяется парсинг vault.ParseRef (секрет-поле обязано
// быть vault-ref, не plaintext).
func checkFieldKind(t HeraldType, f HeraldFieldSpec, raw any) error {
	switch f.Kind {
	case KindString, KindURL:
		s, ok := raw.(string)
		if !ok || (f.Required && s == "") {
			return fmt.Errorf("herald: %s config %q must be a non-empty string", t, f.Name)
		}
	case KindVaultRef:
		s, ok := raw.(string)
		if !ok || s == "" {
			return fmt.Errorf("herald: %s config %q must be a non-empty vault-ref", t, f.Name)
		}
		if _, err := vault.ParseRef(s); err != nil {
			return fmt.Errorf("herald: %s config %q must be a vault-ref (vault:<mount>/<path>): %w", t, f.Name, err)
		}
	case KindEnum:
		s, ok := raw.(string)
		if !ok {
			return fmt.Errorf("herald: %s config %q must be a string", t, f.Name)
		}
		if !containsString(f.EnumValues, s) {
			return fmt.Errorf("herald: %s config %q must be one of %v, got %q", t, f.Name, f.EnumValues, s)
		}
	case KindBool:
		if _, ok := raw.(bool); !ok {
			return fmt.Errorf("herald: %s config %q must be a boolean", t, f.Name)
		}
	case KindInt:
		// JSON-числа приходят float64; целостность тут не проверяем (нет int-полей
		// в MVP-типах) — достаточно «число».
		switch raw.(type) {
		case float64, int, int64:
		default:
			return fmt.Errorf("herald: %s config %q must be a number", t, f.Name)
		}
	case KindMap:
		if _, ok := raw.(map[string]any); !ok {
			return fmt.Errorf("herald: %s config %q must be an object", t, f.Name)
		}
	case KindList:
		if _, ok := raw.([]any); !ok {
			return fmt.Errorf("herald: %s config %q must be an array", t, f.Name)
		}
	}
	return nil
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
