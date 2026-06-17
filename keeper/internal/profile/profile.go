// Package profile — реестр Cloud-Profile-ей в Postgres (ADR-017,
// docs/keeper/cloud.md).
//
// Cloud.CRUD.a: типы + CRUD (Insert / SelectByName / SelectAll /
// SelectByProvider). Profile — VM-spec поверх конкретного Provider-а:
// `Params` (jsonb, произвольный VM-spec) + optional `CloudInit` (userdata).
//
// Валидация `Params` против CloudDriver.Schema живёт на service-слое
// (Cloud.CRUD.b), не здесь.
package profile

import (
	"regexp"
	"time"
)

// NamePattern — каноническая форма имени Profile: kebab-case, длина 1..63.
// То же, что CHECK profiles_name_format в 020-миграции.
const NamePattern = `^[a-z0-9-]{1,63}$`

var nameRe = regexp.MustCompile(NamePattern)

// ValidName проверяет соответствие name канонической форме (kebab 1..63).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Profile — runtime-представление строки реестра `profiles`.
//
// Params — `map[string]any` для freeform VM-spec; типизация по конкретному
// CloudDriver-у живёт в его schema, не в этом слое. CloudInit nil → колонка
// NULL (userdata отсутствует).
type Profile struct {
	Name         string         `json:"name"`
	Provider     string         `json:"provider"`
	Params       map[string]any `json:"params"`
	CloudInit    *string        `json:"cloud_init,omitempty"`
	CreatedByAID *string        `json:"created_by_aid,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
}
