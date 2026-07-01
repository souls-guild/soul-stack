package herald

// Двухклассовая channel-модель доставки Herald (ADR-052 amendment, вердикт
// architect). HTTP-класс (webhook/telegram/slack/mattermost/discord/custom):
// каждый тип — [channelDriver] с тремя обязанностями: (1) валидация config по
// его дескриптору полей, (2) объявление, использует ли он top-level secret_ref
// (только webhook), (3) резолв доставки в готовый [httpDelivery] (URL + метод +
// тело + заголовки + опц. подпись + SSRF-opt-out-флаги). SMTP-класс (email) —
// НЕ channelDriver (нет httpDelivery/HTTP-транспорта), живёт отдельной осью
// ([email.go], своя ветка в [DeliveryWorker.deliver]).
//
// ЕДИНЫЙ SSRF-контур: driver ТОЛЬКО строит httpDelivery, а guard
// (validateDeliveryEndpoint + guardedDeliveryClient, egress.go) и client.Do
// зовёт сам [DeliveryWorker.deliver]. Новый HTTP-тип НЕ может обойти SSRF-guard
// by construction.
//
// ЕДИНЫЙ источник типов: [channelDrivers] (+ HeraldEmail) — из него выводятся
// (1) generic-валидатор config по дескриптору, (2) список типов для huma-enum,
// (3) PG-CHECK-сверка, (4) каталог GET /v1/herald-types. Добавление HTTP-типа —
// одна запись в [channelDrivers] (+ CHECK-миграция + huma-enum, сверяются
// guard-тестом с [AllHeraldTypes]).
//
// NB(имена на подтверждении PM/юзера, propose-and-wait): channelDriver /
// httpDelivery / HeraldFieldSpec / FieldKind предложены architect; в naming-
// rules пока НЕ фиксируются.

import (
	"context"
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// channelDriver — драйвер одного HTTP-класс-типа канала. validateConfig
// проверяет config на CRUD-этапе (форма полей + доменные инварианты), не
// читая Vault (секрет держится как vault-ref, резолвится только на доставке).
// secretRequired — использует ли тип top-level secret_ref (HMAC signing-token);
// true только у webhook, у прочих credential — vault-ref-поле ВНУТРИ config
// (разводка ADR-052 amendment). resolveDelivery резолвит канал в готовый
// httpDelivery на момент доставки (config мог измениться после create).
type channelDriver interface {
	// validateConfig — CRUD-валидация config по типу (форма + доменные инварианты).
	validateConfig(config map[string]any) error
	// secretRequired — true, если тип использует top-level secret_ref (webhook).
	secretRequired() bool
	// resolveDelivery — сборка httpDelivery на момент доставки. Ошибка резолва
	// секрета из Vault пробрасывается как есть (caller сохраняет terminal/transient
	// классификацию: Vault-сбой transient, битый config — terminal).
	resolveDelivery(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*httpDelivery, error)
	// fields — дескриптор config-полей типа (единый источник для generic-валидатора
	// и каталога GET /v1/herald-types; каталог и валидация НЕ разъезжаются).
	fields() []HeraldFieldSpec
}

// httpDelivery — готовый результат резолва доставки HTTP-класса: request-заготовка
// (url + метод + тело + заголовки) + SSRF-opt-out-флаги + опц. signing-key для
// HMAC-подписи. [DeliveryWorker.deliver] строит *http.Request из этих полей,
// прогоняет ЕДИНЫЙ SSRF-guard по url и опционально подписывает тело.
//
// httpAllowed/allowPrivate — per-Herald opt-out (webhook — из config; фиксированные
// публичные endpoint-ы telegram/slack/… — оба false). signingKey nil → подпись не
// ставится (только webhook с secret_ref). method пуст → POST; contentType пуст →
// application/json (нормализует deliver()).
type httpDelivery struct {
	url          string
	method       string
	contentType  string
	body         []byte
	headers      map[string]string
	httpAllowed  bool
	allowPrivate bool
	signingKey   []byte
}

// channelDrivers — канонический реестр драйверов HTTP-класса, ЕДИНЫЙ источник
// HTTP-типов. email — НЕ здесь (SMTP-класс, отдельная ось). Новый HTTP-тип
// добавляется ОДНОЙ записью сюда (+ CHECK-миграция + huma-enum, оба сверяются
// guard-тестом с AllHeraldTypes).
var channelDrivers = map[HeraldType]channelDriver{
	HeraldWebhook:    webhookChannel{},
	HeraldTelegram:   telegramChannel{},
	HeraldSlack:      slackChannel{},
	HeraldMattermost: mattermostChannel{},
	HeraldDiscord:    discordChannel{},
	HeraldCustom:     customChannel{},
}

// driverFor возвращает драйвер HTTP-класса для типа. ok=false — тип не
// HTTP-класса (email) или неизвестен. caller ([DeliveryWorker.deliver]) для
// email идёт своей SMTP-веткой ДО driverFor; для неизвестного — terminal-fail.
func driverFor(t HeraldType) (channelDriver, bool) {
	d, ok := channelDrivers[t]
	return d, ok
}

// resolveDelivery диспетчеризует резолв доставки HTTP-класса по типу канала.
// Единая точка входа для [DeliveryWorker.deliver] (HTTP-ветка). Неизвестный /
// не-HTTP-тип → terminal-no-retry (доставлять нечем). Email сюда не попадает
// (deliver отводит его в SMTP-ветку раньше).
func resolveDelivery(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*httpDelivery, error) {
	d, ok := driverFor(h.Type)
	if !ok {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: channel %q type %q has no HTTP driver", h.Name, h.Type)}
	}
	return d.resolveDelivery(ctx, h, job, kv)
}

// --- дескриптор config-полей (единый источник валидации + каталога) ----------

// FieldKind — вид значения config-поля для generic-валидатора и каталога
// (имя на подтверждении). string/int/bool/enum — скаляры; map/list —
// контейнеры; url — строка-URL (SSRF-guard живёт в доставке, здесь — «строка»);
// list_string — список строк (email to); vault_ref — строка-vault-ref
// (значение обязано парситься vault.ParseRef, секрет в PG cleartext не хранится).
type FieldKind string

const (
	KindString     FieldKind = "string"
	KindInt        FieldKind = "int"
	KindBool       FieldKind = "bool"
	KindEnum       FieldKind = "enum"
	KindMap        FieldKind = "map"
	KindList       FieldKind = "list"
	KindListString FieldKind = "list_string"
	KindURL        FieldKind = "url"
	KindVaultRef   FieldKind = "vault_ref"
)

// HeraldFieldSpec — описание одного config-поля типа (имя на подтверждении).
// Secret=true ⟹ поле держит vault-ref (Kind обязан быть KindVaultRef; секрет
// в config, не в top-level secret_ref — разводка ADR-052 amendment). EnumValues
// заполняется только для Kind==KindEnum (допустимый набор строк; пустой элемент
// "" в наборе = «поле опущено/plain» разрешён).
type HeraldFieldSpec struct {
	Name       string
	Label      string
	Required   bool
	Secret     bool
	Kind       FieldKind
	EnumValues []string
}

// validateBySpec — generic-обход дескриптора полей: required-присутствие + kind-
// соответствие + secret-поле → валидный vault-ref. Неизвестные ключи config НЕ
// отвергаются (forward-compat: JSONB мог прийти из более новой версии; huma-
// уровень режет unknown-поля на wire, domain остаётся терпимым).
func validateBySpec(t HeraldType, fields []HeraldFieldSpec, config map[string]any) error {
	for _, f := range fields {
		raw, present := config[f.Name]
		if !present {
			if f.Required {
				return fmt.Errorf("herald: %s config requires %q", t, f.Name)
			}
			continue
		}
		if err := checkFieldKind(t, f, raw); err != nil {
			return err
		}
	}
	return nil
}

// checkFieldKind проверяет одно присутствующее поле по его Kind. Пустая строка в
// required-поле трактуется как отсутствие значения (required нарушен). Для
// vault_ref дополнительно проверяется vault.ParseRef (секрет-поле обязано быть
// vault-ref, не plaintext).
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
		if !isJSONInt(raw) {
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
	case KindListString:
		xs, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("herald: %s config %q must be an array of strings", t, f.Name)
		}
		if f.Required && len(xs) == 0 {
			return fmt.Errorf("herald: %s config %q must be a non-empty array of strings", t, f.Name)
		}
		for _, el := range xs {
			s, ok := el.(string)
			if !ok || s == "" {
				return fmt.Errorf("herald: %s config %q must contain only non-empty strings", t, f.Name)
			}
		}
	}
	return nil
}

// isJSONInt — JSON-число (float64 из decode, либо native int/int64). Целостность
// не проверяем строго (порты валидируются доменным хуком email отдельно).
func isJSONInt(raw any) bool {
	switch raw.(type) {
	case float64, int, int64:
		return true
	default:
		return false
	}
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// AllHeraldTypes — отсортированный список ВСЕХ известных типов канала (HTTP-класс
// из channelDrivers + email). ЕДИНЫЙ источник для huma-enum-сверки, PG-CHECK-
// сверки и каталога; guard-тест ловит расхождение мест. Сортировка детерминирует
// enum-строку.
func AllHeraldTypes() []HeraldType {
	out := make([]HeraldType, 0, len(channelDrivers)+1)
	for t := range channelDrivers {
		out = append(out, t)
	}
	out = append(out, HeraldEmail)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// fieldsFor возвращает дескриптор config-полей типа для каталога GET /v1/herald-
// types. HTTP-класс берёт из драйвера; email — из [emailFields] (SMTP-ось вне
// channelDrivers). ok=false — неизвестный тип.
func fieldsFor(t HeraldType) ([]HeraldFieldSpec, bool) {
	if d, ok := channelDrivers[t]; ok {
		return d.fields(), true
	}
	if t == HeraldEmail {
		return emailFields(), true
	}
	return nil, false
}

// HeraldTypeDescriptor — публичный дескриптор одного типа канала для каталог-
// эндпоинта GET /v1/herald-types: тип + его config-поля + признак top-level
// secret_ref. ЕДИНЫЙ источник с валидацией (те же [HeraldFieldSpec], что
// валидирует CRUD; SecretRequired — тот же [channelDriver.secretRequired], что
// сверяет [ValidateSecretRef]) — каталог и валидация не разъезжаются.
// SecretRequired=true ⟹ у типа есть top-level secret_ref (HMAC signing-token,
// только webhook); UI показывает поле secret_ref по этому признаку, не по
// хардкоду типа.
type HeraldTypeDescriptor struct {
	Type           HeraldType
	Fields         []HeraldFieldSpec
	SecretRequired bool
}

// TypeCatalog собирает дескрипторы ВСЕХ известных типов канала (отсортированы как
// [AllHeraldTypes]) для каталог-эндпоинта. Источник полей — драйверы (HTTP-класс)
// и [emailFields] (SMTP); тот же набор, что валидирует CRUD. SecretRequired берётся
// из драйвера (email — не channelDriver, top-level secret_ref не использует → false).
func TypeCatalog() []HeraldTypeDescriptor {
	types := AllHeraldTypes()
	out := make([]HeraldTypeDescriptor, 0, len(types))
	for _, t := range types {
		fields, _ := fieldsFor(t) // t ∈ AllHeraldTypes ⟹ дескриптор всегда есть
		d, ok := driverFor(t)     // email не HTTP-класс ⟹ ok=false ⟹ secret_ref не для него
		out = append(out, HeraldTypeDescriptor{Type: t, Fields: fields, SecretRequired: ok && d.secretRequired()})
	}
	return out
}
