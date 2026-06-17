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

// HeraldType — closed-enum типа канала. В MVP только webhook (ADR-052(a));
// slack/email — additive post-MVP (новое значение enum + CHECK heralds_type_enum).
type HeraldType string

const HeraldWebhook HeraldType = "webhook"

// ValidHeraldType — true для известного типа канала.
func ValidHeraldType(t HeraldType) bool {
	return t == HeraldWebhook
}

// Herald — runtime-представление строки реестра `heralds`.
//
// Config — per-type конфигурация канала (для webhook — url + опц. headers +
// опц. opt-out флаги http_allowed/allow_private). SecretRef — vault-ref секрета
// канала (signing-token), nullable: не каждому webhook нужна подпись.
type Herald struct {
	Name         string         `json:"name"`
	Type         HeraldType     `json:"type"`
	Config       map[string]any `json:"config"`
	SecretRef    *string        `json:"secret_ref,omitempty"`
	Enabled      bool           `json:"enabled"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	CreatedByAID *string        `json:"created_by_aid,omitempty"`
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
// покрывает — JSONB shape зависит от type). Для webhook:
//   - `url` обязателен и строка;
//   - SSRF-контур взведён по умолчанию (ADR-052(e), паттерн core.url):
//     https-only через [netguard.ValidateEndpoint], если в config не задан
//     явный opt-out `http_allowed: true`; при opt-out — проверяется только
//     корректность URL (http допускается);
//   - литеральный private-IP в host блокируется (часть ValidateEndpoint),
//     если не задан opt-out `allow_private: true` (DNS-резолв приватки —
//     dial-фаза доставки, S3).
//
// fail-closed: неизвестный/битый config отвергается на CRUD-этапе, до записи.
func ValidateConfig(t HeraldType, config map[string]any) error {
	switch t {
	case HeraldWebhook:
		return validateWebhookConfig(config)
	default:
		return fmt.Errorf("herald: unknown type %q (must be webhook)", t)
	}
}

func validateWebhookConfig(config map[string]any) error {
	rawURL, ok := config["url"]
	if !ok {
		return fmt.Errorf("herald: webhook config requires %q", "url")
	}
	urlStr, ok := rawURL.(string)
	if !ok || urlStr == "" {
		return fmt.Errorf("herald: webhook config %q must be a non-empty string", "url")
	}

	httpAllowed := configBool(config, "http_allowed")
	allowPrivate := configBool(config, "allow_private")

	if !httpAllowed && !allowPrivate {
		// Дефолтный контур: https-only + literal-private-IP блок.
		return netguard.ValidateEndpoint(urlStr)
	}

	// Хотя бы один opt-out — проверяем поэлементно, чтобы не снимать лишнего.
	u, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("herald: webhook config invalid url %q", urlStr)
	}
	if u.Host == "" {
		return fmt.Errorf("herald: webhook config url %q has no host", urlStr)
	}
	if !httpAllowed && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("herald: webhook config: only https:// allowed (set http_allowed=true to opt out), got scheme %q", u.Scheme)
	}
	if u.Scheme != "" && !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return fmt.Errorf("herald: webhook config: unsupported url scheme %q", u.Scheme)
	}
	return nil
}

// configBool читает bool-флаг из config (отсутствие/не-bool → false). JSON-числа
// и строки флагом не считаются — флаг безопасности взводится только явным `true`.
func configBool(config map[string]any, key string) bool {
	v, ok := config[key].(bool)
	return ok && v
}

// ValidateSecretRef проверяет, что secret_ref (если задан) — корректный
// vault-ref (`vault:<mount>/<path>`), тем же парсером, что omens.auth_ref. nil
// допустим — секрет канала опционален (подпись webhook-а не обязательна).
func ValidateSecretRef(ref *string) error {
	if ref == nil {
		return nil
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
