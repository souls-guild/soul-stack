// Package herald — реестры Herald (каналы доставки) и Tiding (правила
// подписки) уведомлений о событиях прогонов в Postgres (ADR-052, слайс S1).
//
// Herald — куда слать (webhook-канал в MVP), Tiding — на что реагировать и
// каким Herald-ом. Доставка / tap-декоратор поверх audit.Writer /
// notification-dispatcher — следующие слайсы (S2-S4); здесь только типы,
// валидации и CRUD-слой (паттерн keeper/internal/augur omens/rites).
package herald

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/netguard"
)

// NamePattern — каноническая форма имени Herald/Tiding: kebab-case, длина
// 1..63. То же, что CHECK heralds_name_format / tidings_name_format в миграции
// 071 (как omens.NamePattern).
const NamePattern = `^[a-z0-9-]{1,63}$`

var nameRe = regexp.MustCompile(NamePattern)

// ValidName проверяет соответствие name канонической форме.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// HeraldType — closed-enum типа канала (ADR-052 amendment). Канонический набор
// известных типов — реестр драйверов [channelDrivers] (HTTP-класс) + email
// (SMTP-класс); единый источник — [AllHeraldTypes]. Добавление HTTP-типа — одна
// запись в channelDrivers + CHECK-миграция + huma-enum (сверяются guard-тестом
// с AllHeraldTypes).
//
// Классы: webhook — HTTP с HMAC-подписью (top-level secret_ref); telegram/slack/
// mattermost/discord — HTTP-мессенджеры (auth через vault-ref-поле в config,
// человекочитаемый текст); custom — HTTP с фиксированным телом webhookPayload;
// email — SMTP (отдельная ось, НЕ channelDrivers).
type HeraldType string

const (
	HeraldWebhook    HeraldType = "webhook"
	HeraldTelegram   HeraldType = "telegram"
	HeraldSlack      HeraldType = "slack"
	HeraldMattermost HeraldType = "mattermost"
	HeraldDiscord    HeraldType = "discord"
	HeraldCustom     HeraldType = "custom"
	HeraldEmail      HeraldType = "email"
)

// ValidHeraldType — true для известного типа канала: HTTP-драйвер в
// [channelDrivers] ИЛИ email (SMTP-ось). Единый источник — [AllHeraldTypes].
func ValidHeraldType(t HeraldType) bool {
	if _, ok := driverFor(t); ok {
		return true
	}
	return t == HeraldEmail
}

// Herald — runtime-представление строки реестра `heralds`.
//
// Config — per-type конфигурация канала (для webhook — url + опц. headers +
// опц. opt-out флаги http_allowed/allow_private). SecretRef — vault-ref секрета
// канала (signing-token), nullable: не каждому webhook нужна подпись.
type Herald struct {
	Name      string         `json:"name"`
	Type      HeraldType     `json:"type"`
	Config    map[string]any `json:"config"`
	SecretRef *string        `json:"secret_ref,omitempty"`
	// Secret — plaintext webhook signing-secret (dual-mode, ADR-064): оператор
	// передаёт значение вместо SecretRef; Service материализует его в Vault
	// ([materializeHeraldSecrets]) и заменяет на внутренний SecretRef. json:"-" —
	// НИКОГДА не сериализуется (не в PG/View/audit), request-scoped, стирается
	// после записи. XOR с SecretRef. Аналогично для config-полей канала (<base>
	// plaintext XOR <base>_ref) — их plaintext живёт в Config до материализации.
	Secret *string `json:"-"`
	// SecretWritten — request-scoped маркер: keeper записал plaintext-секрет в
	// Vault на этой операции (ADR-064 audit-event). json:"-"; читается audit-
	// payload-ом (ключ plaintext_ingested), в PG/View не попадает.
	SecretWritten bool      `json:"-"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	CreatedByAID  *string   `json:"created_by_aid,omitempty"`
}

// Tiding — runtime-представление строки реестра `tidings`.
//
// EventTypes — непустой список audit event-types с поддержкой area-glob
// (`scenario_run.*`); валидируется [ValidateEventTypes]. Incarnation/Cadence —
// опц. селекторы привязки к источнику прогона (nil = без фильтра).
//
// Ephemeral/VoyageID (ADR-052(g)) — разовое правило, привязанное к одному
// прогону: Ephemeral=true ⟺ VoyageID != nil (инвариант [ErrEphemeralRequiresVoyage],
// дублируется CHECK tidings_ephemeral_voyage_consistent). Постоянное правило —
// Ephemeral=false, VoyageID=nil.
//
// Annotations/Projection (ADR-052(h)) — управление телом webhook-доставки.
// Annotations — статические поля оператора (JSON-объект верхнего уровня),
// мержатся ключом `annotations` в тело. Projection — allow-list путей payload
// (пусто = полная форма). Оба применяет worker доставки off-path (N3); domain
// (N1) только хранит + валидирует синтаксис.
//
// Task (ADR-052 §l) — опц. селектор подписки на КОНКРЕТНУЮ задачу прогона по её
// адресу (register ∪ id из changed_tasks события incarnation.run_completed,
// ADR-052 §j). nil = без фильтра. Непустое значение → правило матчит
// incarnation.run_completed, только если в его changed_tasks есть запись с
// register == *Task ИЛИ id == *Task ([matchTask]). Самодостаточен: присутствие
// адреса в changed_tasks = задача изменилась хотя бы на одном хосте.
//
// CreatedFromCadenceID (ADR-052 §m / ADR-046 §9) — маркер ПРОИСХОЖДЕНИЯ: правило
// рождено блоком notify[] формы Cadence (POST /v1/cadences). nil = заведено иначе
// (CRUD Tiding вручную / ephemeral от Voyage). Непустое → FK на cadences(id) ON
// DELETE CASCADE: снос Cadence уносит порождённые правила. Ортогонален селектору
// Cadence (фильтр подписки «слать только про прогоны этого расписания»): каскад
// сносит ТОЛЬКО форм-правила, не трогая руками заведённые с тем же cadence-
// селектором. Привязка по ULID (cadences.id), а не имени — rename-safe.
type Tiding struct {
	Name                 string         `json:"name"`
	Herald               string         `json:"herald"`
	EventTypes           []string       `json:"event_types"`
	OnlyFailures         bool           `json:"only_failures"`
	OnlyChanges          bool           `json:"only_changes"`
	Incarnation          *string        `json:"incarnation,omitempty"`
	Cadence              *string        `json:"cadence,omitempty"`
	Task                 *string        `json:"task,omitempty"`
	Ephemeral            bool           `json:"ephemeral"`
	VoyageID             *string        `json:"voyage_id,omitempty"`
	CreatedFromCadenceID *string        `json:"created_from_cadence_id,omitempty"`
	Annotations          map[string]any `json:"annotations,omitempty"`
	Projection           []string       `json:"projection,omitempty"`
	Enabled              bool           `json:"enabled"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
	CreatedByAID         *string        `json:"created_by_aid,omitempty"`
}

// ValidateConfig проверяет config канала по его типу (то, что БД-CHECK не
// покрывает — JSONB shape зависит от type). Диспетчер по классу: HTTP-типы
// валидирует их [channelDriver.validateConfig] (generic-обход дескриптора полей
// + доменные инварианты — SSRF-контур URL webhook/custom, chat_id telegram);
// email — [validateEmailConfig] (SMTP-ось). Единый источник валидатора и
// каталога — те же дескрипторы полей.
//
// fail-closed: неизвестный тип / битый config отвергается на CRUD-этапе, до
// записи.
func ValidateConfig(t HeraldType, config map[string]any) error {
	if d, ok := driverFor(t); ok {
		return d.validateConfig(config)
	}
	if t == HeraldEmail {
		return validateEmailConfig(config)
	}
	return fmt.Errorf("herald: unknown type %q (known: %v)", t, AllHeraldTypes())
}

// validateWebhookURL — доменный SSRF-контур URL для HTTP-типов с оператор-заданным
// url (webhook/custom). Дефолтный контур (оба opt-out false): https-only +
// literal-private-IP блок ([netguard.ValidateEndpoint]). При opt-out — поэлементно,
// чтобы не снимать лишнего (http:// только при http_allowed; private-IP покрывает
// dial-guard на доставке). Присутствие/форму url уже проверил generic-обход
// дескриптора — здесь только транспорт-контур.
func validateWebhookURL(config map[string]any) error {
	urlStr, _ := config["url"].(string)
	if urlStr == "" {
		return fmt.Errorf("herald: config %q must be a non-empty string", "url")
	}

	httpAllowed := configBool(config, "http_allowed")
	allowPrivate := configBool(config, "allow_private")

	if !httpAllowed && !allowPrivate {
		return netguard.ValidateEndpoint(urlStr)
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("herald: config invalid url %q", urlStr)
	}
	if u.Host == "" {
		return fmt.Errorf("herald: config url %q has no host", urlStr)
	}
	if !httpAllowed && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("herald: config: only https:// allowed (set http_allowed=true to opt out), got scheme %q", u.Scheme)
	}
	if u.Scheme != "" && !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return fmt.Errorf("herald: config: unsupported url scheme %q", u.Scheme)
	}
	return nil
}

// configBool читает bool-флаг из config (отсутствие/не-bool → false). JSON-числа
// и строки флагом не считаются — флаг безопасности взводится только явным `true`.
func configBool(config map[string]any, key string) bool {
	v, ok := config[key].(bool)
	return ok && v
}

// ValidateSecretRef проверяет top-level secret_ref канала. Семантика (ADR-052
// amendment, разводка секрета): secret_ref — СТРОГО HMAC signing-token; его
// использует только тип, чей драйвер объявил [channelDriver.secretRequired]
// (webhook). У прочих типов auth-credential — vault-ref-поле ВНУТРИ config (напр.
// telegram bot_token_ref), а top-level secret_ref обязан быть ПУСТ. Правила:
//   - nil/пусто — всегда ок (подпись опциональна);
//   - тип без secret-поддержки + непустой secret_ref → ошибка (поле не для типа);
//   - тип c secret-поддержкой + непустой secret_ref → обязан быть корректным
//     vault-ref (`vault:<mount>/<path>`), тем же парсером, что omens.auth_ref.
func ValidateSecretRef(t HeraldType, ref *string) error {
	if ref == nil || *ref == "" {
		return nil
	}
	d, ok := driverFor(t)
	if !ok || !d.secretRequired() {
		return fmt.Errorf("herald: secret_ref is only for signing (webhook); %s uses a vault-ref field inside config", t)
	}
	if _, err := vault.ParseRef(*ref); err != nil {
		return fmt.Errorf("herald: invalid secret_ref %q (must be a vault-ref vault:<mount>/<path>): %w", *ref, err)
	}
	return nil
}

// marshalConfig сериализует config в JSON-bytes для JSONB-колонки. nil → `{}`
// (схема несёт DEFAULT, но pgx требует не-nil для NOT NULL). Симметрично
// pushprovider.marshalParams.
func marshalConfig(config map[string]any) ([]byte, error) {
	if config == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(config)
}

// projectionSegmentRe — допустимый сегмент projection-пути: непустой,
// строчные/цифры/`_`. Полный путь — сегменты через `.` (`summary.succeeded`).
var projectionSegmentRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// ValidateProjection проверяет СИНТАКСИС allow-list путей projection (ADR-052(h)):
// каждый путь — непустые сегменты `[a-z0-9_]`, разделённые `.`; запрещены пустые
// сегменты (ведущая/двойная/хвостовая точка → `..`/`.x`/`x.`) и сам `..`.
//
// Глубокая проверка пути ПРОТИВ payload-формы события здесь НЕ делается —
// allow-list резолвится лениво в worker-е доставки (N3): несуществующий путь
// просто не попадёт в тело, а каталог payload-форм эволюционирует и хрупкого
// статического матча против него тут быть не должно. nil/пустой projection
// допустим (= полная форма payload).
func ValidateProjection(paths []string) error {
	for _, p := range paths {
		if p == "" {
			return fmt.Errorf("herald: projection path is empty")
		}
		// strings.Split с пустым сегментом ловит ведущую/двойную/хвостовую точку
		// (включая литеральный `..` → два пустых соседа) одним проходом.
		for _, seg := range strings.Split(p, ".") {
			if seg == "" {
				return fmt.Errorf("herald: invalid projection path %q (empty segment — no leading/trailing/double dot)", p)
			}
			if !projectionSegmentRe.MatchString(seg) {
				return fmt.Errorf("herald: invalid projection path %q (segment %q must match [a-z0-9_])", p, seg)
			}
		}
	}
	return nil
}

// ValidateAnnotationsJSON проверяет, что сырой JSON annotations — ОБЪЕКТ
// верхнего уровня (ADR-052(h)/(i)): мержится в тело webhook ключом `annotations`,
// поэтому массив/скаляр/строка на верхнем уровне недопустимы. Зовётся handler/
// MCP-стороной (N2), декодирующей пользовательский JSON, ДО построения [Tiding]
// (где annotations уже типизирован map). Пустой/`null` JSON допустим (= нет
// статических полей). При нарушении возвращается обычная ошибка (без sentinel-а):
// handler-сторона (N2) оборачивает её в [ErrValidation] → 422. Отдельный sentinel
// здесь не нужен — N2 не различает причину невалидного annotations.
func ValidateAnnotationsJSON(raw json.RawMessage) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("herald: annotations is not valid JSON")
	}
	if _, ok := probe.(map[string]any); !ok {
		return fmt.Errorf("herald: annotations must be a JSON object (not an array or scalar)")
	}
	return nil
}

// marshalAnnotations сериализует annotations в JSON-bytes для JSONB-колонки.
// nil → `{}` (NOT NULL DEFAULT, как marshalConfig).
func marshalAnnotations(annotations map[string]any) ([]byte, error) {
	if annotations == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(annotations)
}
