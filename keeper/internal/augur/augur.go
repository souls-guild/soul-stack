// Package augur — Postgres-реестр Augur-а (ADR-025, docs/keeper/augur.md):
// две таблицы omens (внешние системы) и rites (grant-ы доступа).
//
// Слой содержит типы + CRUD (Insert / Select* / Delete) + service-валидацию,
// которую БД-CHECK не покрывает: формат vault-ref auth_ref, форма allow по
// source_type, token-поля только для vault-delegate, формат token_ttl.
// Авторизация AugurRequest, минтинг токенов и EventStream-проводка — отдельный
// слайс (здесь их НЕТ).
package augur

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Sentinel-ошибки storage-валидаторов формы Rite-а. Service-слой маппит их в 422
// через errors.Is — не по строковому префиксу текста (переименование диагностики
// не должно молча сломать маппинг 422→500). Эти валидаторы живут в storage-
// слайсе и не знают про management-Service [ErrValidation]; Service оборачивает
// их результат сам.
var (
	// ErrAllowShape — allow-JSONB не соответствует форме source_type (ValidateAllow).
	ErrAllowShape = errors.New("augur: allow shape invalid")
	// ErrTokenFields — нарушен инвариант token-полей (ValidateTokenFields).
	ErrTokenFields = errors.New("augur: token fields invalid")
)

// SourceType — descriptive closed enum типа внешней системы (omens.source_type,
// augur.md §7). Расширение — propose-and-wait + PR в augur.md и naming-rules.md.
type SourceType string

const (
	SourceVault      SourceType = "vault"
	SourcePrometheus SourceType = "prometheus"
	SourceELK        SourceType = "elk"
)

// ValidSourceType — членство в closed enum. Дублирует CHECK
// omens_source_type_enum (032), но позволяет отбить bad value до round-trip-а.
func ValidSourceType(s SourceType) bool {
	switch s {
	case SourceVault, SourcePrometheus, SourceELK:
		return true
	default:
		return false
	}
}

// NamePattern — каноническая форма имени Omen-а: kebab-case, длина 1..63. То же,
// что CHECK omens_name_format в 032-миграции (как providers.NamePattern).
const NamePattern = `^[a-z0-9-]{1,63}$`

// CovenPattern — форма Coven-метки субъекта Rite-а. То же, что CHECK
// rites_coven_format в 033-миграции.
const CovenPattern = `^[a-z0-9][a-z0-9-]*$`

var (
	nameRe  = regexp.MustCompile(NamePattern)
	covenRe = regexp.MustCompile(CovenPattern)
)

// ValidName проверяет имя Omen-а на каноническую форму (kebab 1..63).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// ValidCoven проверяет Coven-метку субъекта Rite-а.
func ValidCoven(coven string) bool { return covenRe.MatchString(coven) }

// ValidAuthRef проверяет, что auth_ref — корректный vault-ref
// (`vault:<mount>/<path>`), тем же парсером, что providers.credentials_ref и
// прочие `*_ref`-поля keeper.yml. Master-credential в БД не хранится — только
// ссылка (инвариант augur.md §4.1).
func ValidAuthRef(ref string) bool {
	_, err := vault.ParseRef(ref)
	return err == nil
}

// Omen — runtime-представление строки реестра `omens` (внешняя система).
type Omen struct {
	Name         string     `json:"name"`
	SourceType   SourceType `json:"source_type"`
	Endpoint     string     `json:"endpoint"`
	AuthRef      string     `json:"auth_ref"`
	CreatedByAID *string    `json:"created_by_aid,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Rite — runtime-представление строки реестра `rites` (grant доступа).
//
// Субъект — строго XOR: ровно одно из Coven / SID непусто. allow — сырой JSONB
// (форма зависит от SourceType Omen-а, валидируется через ValidateAllow).
// TokenTTL / TokenNumUses осмысленны только для vault-Omen с Delegate=true.
type Rite struct {
	ID           int64           `json:"id"`
	Omen         string          `json:"omen"`
	Coven        *string         `json:"coven,omitempty"`
	SID          *string         `json:"sid,omitempty"`
	Allow        json.RawMessage `json:"allow"`
	Delegate     bool            `json:"delegate"`
	TokenTTL     *string         `json:"token_ttl,omitempty"`
	TokenNumUses *int            `json:"token_num_uses,omitempty"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// allow-shape по source_type (augur.md §4.2). Формы — закрытые: посторонние
// ключи отвергаются, чтобы опечатка в allow не приводила к молчаливо пустому
// allow-list-у (security-инвариант — слишком широкий grant из-за тайпо).
//
// vault     → {paths?, policies?}  (хотя бы одно непустое)
// prometheus→ {queries}            (непустой)
// elk       → {indices}            (непустой)

type allowVault struct {
	Paths    []string `json:"paths"`
	Policies []string `json:"policies"`
}

type allowPrometheus struct {
	Queries []string `json:"queries"`
}

type allowELK struct {
	Indices []string `json:"indices"`
}

// ValidateAllow проверяет форму allow-JSONB против source_type Omen-а. Возврат
// nil — форма валидна; иначе диагностика с указанием ожидаемой формы.
//
// Service-слой: декларативным CHECK-ом JSONB-форму к source_type другого Omen-а
// без триггера не привязать, поэтому проверка живёт здесь (augur.md §4.2).
func ValidateAllow(src SourceType, allow json.RawMessage) error {
	if len(allow) == 0 {
		return fmt.Errorf("%w: allow is empty", ErrAllowShape)
	}
	switch src {
	case SourceVault:
		var a allowVault
		if err := strictUnmarshal(allow, &a); err != nil {
			return fmt.Errorf("%w: allow for vault must be {paths?, policies?}: %s", ErrAllowShape, err)
		}
		if len(a.Paths) == 0 && len(a.Policies) == 0 {
			return fmt.Errorf("%w: allow for vault must carry at least one of paths/policies", ErrAllowShape)
		}
		return nil
	case SourcePrometheus:
		var a allowPrometheus
		if err := strictUnmarshal(allow, &a); err != nil {
			return fmt.Errorf("%w: allow for prometheus must be {queries}: %s", ErrAllowShape, err)
		}
		if len(a.Queries) == 0 {
			return fmt.Errorf("%w: allow for prometheus must carry non-empty queries", ErrAllowShape)
		}
		return nil
	case SourceELK:
		var a allowELK
		if err := strictUnmarshal(allow, &a); err != nil {
			return fmt.Errorf("%w: allow for elk must be {indices}: %s", ErrAllowShape, err)
		}
		if len(a.Indices) == 0 {
			return fmt.Errorf("%w: allow for elk must carry non-empty indices", ErrAllowShape)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown source_type %q", ErrAllowShape, src)
	}
}

// strictUnmarshal отвергает посторонние ключи (DisallowUnknownFields) — опечатка
// в allow не должна молча давать слишком широкий grant.
func strictUnmarshal(data json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// ValidateTokenFields реализует вторую половину инварианта token-полей, которую
// БД-CHECK не ловит: token_ttl / token_num_uses допустимы ТОЛЬКО при
// Delegate=true И SourceType=vault (CHECK rites_token_fields_vault_only ловит
// лишь ⇒delegate; ⇒vault требует join к omens — augur.md §4.2). Дополнительно
// проверяет формат token_ttl через config.ParseDuration.
func ValidateTokenFields(src SourceType, r *Rite) error {
	hasTTL := r.TokenTTL != nil
	hasUses := r.TokenNumUses != nil
	if !hasTTL && !hasUses {
		return nil
	}
	if !r.Delegate {
		return fmt.Errorf("%w: token_ttl/token_num_uses require delegate=true", ErrTokenFields)
	}
	if src != SourceVault {
		return fmt.Errorf("%w: token_ttl/token_num_uses are vault-only, got source_type %q", ErrTokenFields, src)
	}
	if hasTTL {
		if _, err := config.ParseDuration(*r.TokenTTL); err != nil {
			return fmt.Errorf("%w: invalid token_ttl %q: %s", ErrTokenFields, *r.TokenTTL, err)
		}
	}
	if hasUses && *r.TokenNumUses < 0 {
		return fmt.Errorf("%w: token_num_uses must be >= 0, got %d", ErrTokenFields, *r.TokenNumUses)
	}
	return nil
}

// ValidateSubjectXOR проверяет XOR-инвариант субъекта Rite-а: ровно одно из
// Coven / SID непусто, и заданный Coven проходит формат. SID-формат на этом
// слое не нормируется (FQDN-семантика SID — registry-сторона).
func ValidateSubjectXOR(r *Rite) error {
	hasCoven := r.Coven != nil && *r.Coven != ""
	hasSID := r.SID != nil && *r.SID != ""
	if hasCoven == hasSID {
		return fmt.Errorf("augur: rite subject must be exactly one of coven / sid (XOR)")
	}
	if hasCoven && !ValidCoven(*r.Coven) {
		return fmt.Errorf("augur: invalid coven %q (must match %s)", *r.Coven, CovenPattern)
	}
	return nil
}
