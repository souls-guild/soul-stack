// Package serviceregistry — Postgres-реестр Service-ов и cluster-wide
// key-value настроек Keeper-а (миграции 034 service_registry / 035
// keeper_settings). Переносит каталог `services[]` и top-level скаляры из
// статического keeper.yml в managed-через-OpenAPI/MCP таблицы, симметрично
// RBAC-storage (ADR-028).
//
// Слой slice S1: типы + raw-SQL CRUD ([repository.go]) + service-обёртка с
// валидацией ([service.go]). holder-снимок и cluster-wide-инвалидация (S2),
// transport-фасад OpenAPI/MCP (S3), hard-cut конфига и переключение
// потребителей (S4) — отдельные слайсы, здесь их НЕТ.
package serviceregistry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Sentinel-ошибки CRUD-слоя. Transport-сторона (отдельный слайс S3) маппит на
// HTTP-коды:
//   - ErrAlreadyExists  → 409 (UNIQUE по PK service_registry.name);
//   - ErrNotFound       → 404 (нет строки по PK);
//   - ErrInvalidName    → 422 (name не по формату);
//   - ErrInvalidGit     → 422 (git пустой);
//   - ErrInvalidRef     → 422 (ref пустой);
//   - ErrInvalidRefresh → 422 (refresh не парсится как duration);
//   - ErrOperatorNotFound → 404 (FK-violation на created_by_aid/updated_by_aid:
//     ссылающийся оператор не существует).
var (
	ErrAlreadyExists    = errors.New("serviceregistry: service name already exists")
	ErrNotFound         = errors.New("serviceregistry: service name not found")
	ErrInvalidName      = errors.New("serviceregistry: invalid service name")
	ErrInvalidGit       = errors.New("serviceregistry: git is empty")
	ErrInvalidRef       = errors.New("serviceregistry: ref is empty")
	ErrInvalidRefresh   = errors.New("serviceregistry: invalid refresh duration")
	ErrOperatorNotFound = errors.New("serviceregistry: referenced operator (AID) not found")

	// ErrSettingNotFound — нет строки в keeper_settings по key (GetSetting).
	ErrSettingNotFound = errors.New("serviceregistry: setting key not found")
	// ErrInvalidSettingKey — key не по формату keeper_settings_key_format.
	ErrInvalidSettingKey = errors.New("serviceregistry: invalid setting key")

	// ErrEmptyProvisioningMethods — ключ provisioning_allowed_methods ЗАДАН, но
	// распарсенный набор пуст (значение пустое / только пробелы / только запятые).
	// Это config-ERROR (anti-lockout): пустая политика заблокировала бы СОЗДАНИЕ
	// операторов всеми методами (user/ldap/oidc) — оператор не должен залочить
	// себя молча. Дефолт «всё разрешено» сигнализируется ОТСУТСТВИЕМ ключа, а не
	// пустым значением.
	ErrInvalidProvisioningMethod = errors.New("serviceregistry: provisioning method must be one of {user,ldap,oidc}")
	ErrEmptyProvisioningMethods  = errors.New("serviceregistry: provisioning_allowed_methods is set but empty (anti-lockout)")
)

// NamePattern — каноническая форма имени Service-а: совпадает с CHECK
// service_registry_name_format в 034-миграции (как rbac.reRoleName). Дублируется
// в Go для прикладной валидации до round-trip-а (better error, нет лишнего
// обращения к БД на битом имени).
const NamePattern = `^[a-z][a-z0-9-]*$`

// SettingKeyPattern — форма ключа keeper_settings: совпадает с CHECK
// keeper_settings_key_format в 035-миграции (snake_case).
const SettingKeyPattern = `^[a-z][a-z0-9_]*$`

var (
	nameRe       = regexp.MustCompile(NamePattern)
	settingKeyRe = regexp.MustCompile(SettingKeyPattern)
)

// ValidName проверяет имя Service-а на каноническую форму.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// ValidSettingKey проверяет ключ keeper_settings на каноническую форму.
func ValidSettingKey(key string) bool { return settingKeyRe.MatchString(key) }

// Well-known ключи keeper_settings. Семантика и набор живут здесь, не в схеме
// (таблица untyped — см. 035-миграцию). default_module_source НЕ заводится: у
// него нет потребителя в keeper-коде (мёртвое поле прежнего конфига).
const (
	// SettingDefaultDestinySource — дефолтный git-источник Destiny.
	SettingDefaultDestinySource = "default_destiny_source"

	// SettingProvisioningAllowedMethods — CSV из домена {user,ldap,oidc}: список
	// разрешённых способов СОЗДАНИЯ оператора (created_via-методов). Гейтит только
	// ветку создания (POST /v1/operators → "user"; federated auto-provision →
	// "ldap"/"oidc"); bootstrap/system НЕ гейтятся никогда. ОТСУТСТВИЕ ключа =
	// всё разрешено (back-compat); ЗАДАН-но-пустой = config-error
	// ([ErrEmptyProvisioningMethods], anti-lockout). Семантика парсинга —
	// [ParseProvisioningMethods].
	SettingProvisioningAllowedMethods = "provisioning_allowed_methods"
)

// provisioningMethodDomain — закрытый набор created_via-методов, которые можно
// указать в политике provisioning_allowed_methods. bootstrap/system сюда НЕ
// входят: они не гейтятся (bootstrap первого Архонта через `keeper init`,
// system — внутренние записи) — попытка задать их в политике = ошибка.
var provisioningMethodDomain = map[string]struct{}{
	"user": {},
	"ldap": {},
	"oidc": {},
}

// ParseProvisioningMethods парсит CSV-значение политики provisioning_allowed_methods
// в set разрешённых методов. Семантика:
//   - split по ',', trim пробелов, отбрасывание пустых элементов, lowercase;
//   - каждый элемент валидируется против [provisioningMethodDomain] ({user,ldap,
//     oidc}); невалидный → [ErrInvalidProvisioningMethod];
//   - пустой результат (csv="" / только запятые-пробелы) → [ErrEmptyProvisioningMethods].
//
// «Ключ отсутствует» обрабатывается ВЫШЕ (PoolSource.Load: ErrSettingNotFound →
// политика не задана), сюда передаётся только заданное значение.
func ParseProvisioningMethods(csv string) (map[string]bool, error) {
	out := make(map[string]bool)
	for _, part := range strings.Split(csv, ",") {
		m := strings.ToLower(strings.TrimSpace(part))
		if m == "" {
			continue
		}
		if _, ok := provisioningMethodDomain[m]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrInvalidProvisioningMethod, m)
		}
		out[m] = true
	}
	if len(out) == 0 {
		return nil, ErrEmptyProvisioningMethods
	}
	return out, nil
}

// ServiceEntry — runtime-представление строки реестра service_registry. Несёт
// git-координаты Service-а (Name/Git/Ref, ADR-007) плюс audit-метаданные;
// реестр перенесён из удалённого `keeper.yml::services[]` (ADR-029).
//
// Refresh — duration-строка авто-refresh ("5m"); nil = без авто-refresh (NULL в
// БД). CreatedByAID / UpdatedByAID — AID оператора-автора/последнего правщика;
// nil = NULL (seed / без инициатора-Архонта / до первого update).
type ServiceEntry struct {
	Name         string    `json:"name"`
	Git          string    `json:"git"`
	Ref          string    `json:"ref"`
	Refresh      *string   `json:"refresh,omitempty"`
	CreatedByAID *string   `json:"created_by_aid,omitempty"`
	UpdatedByAID *string   `json:"updated_by_aid,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Setting — runtime-представление строки keeper_settings.
type Setting struct {
	Key          string    `json:"key"`
	Value        string    `json:"value"`
	UpdatedByAID *string   `json:"updated_by_aid,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}
