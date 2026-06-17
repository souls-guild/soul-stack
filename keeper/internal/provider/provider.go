// Package provider — реестр Cloud-Provider-ов в Postgres (ADR-017,
// docs/keeper/cloud.md).
//
// Cloud.CRUD.a: типы + CRUD (Insert / SelectByName / SelectAll). Provider —
// managed-через-API запись: CloudDriver-плагин (`Type`), регион и vault-ref
// до credentials. Сам secret в БД НЕ хранится.
//
// Соответствие `Type` ↔ keeper.yml::plugins.cloud_drivers[].name проверяется
// на service-слое (Cloud.CRUD.b), не здесь. Здесь — только формат kebab.
package provider

import (
	"regexp"
	"strings"
	"time"
)

// NamePattern — каноническая форма имени Provider / валидного `Type`:
// kebab-case, длина 1..63. То же, что CHECK providers_name_format /
// providers_type_format в 019-миграции.
const NamePattern = `^[a-z0-9-]{1,63}$`

// CredentialsRefPrefix — единственная поддерживаемая в MVP схема vault-ref
// (recon-crud.md развилка #2). env:/secret-store: — post-MVP ADR.
const CredentialsRefPrefix = "vault:"

var nameRe = regexp.MustCompile(NamePattern)

// ValidName проверяет соответствие name канонической форме (kebab 1..63).
// Используется и для имени Provider, и для поля `Type` (имя CloudDriver-плагина).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// ValidCredentialsRef проверяет, что ref начинается с [CredentialsRefPrefix]
// и несёт непустой path после него.
func ValidCredentialsRef(ref string) bool {
	return strings.HasPrefix(ref, CredentialsRefPrefix) &&
		len(ref) > len(CredentialsRefPrefix)
}

// Provider — runtime-представление строки реестра `providers`.
type Provider struct {
	Name           string    `json:"name"`
	Type           string    `json:"type"`
	Region         string    `json:"region"`
	CredentialsRef string    `json:"credentials_ref"`
	CreatedByAID   *string   `json:"created_by_aid,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}
