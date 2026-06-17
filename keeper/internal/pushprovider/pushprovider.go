// Package pushprovider — реестр SSH-Provider-params в Postgres (ADR-032
// amendment 2026-05-26, S7-2).
//
// Push-Provider — per-provider params для env-payload SSH-плагинов
// push-flow (ADR-020 amendment l, env-convention `SOUL_SSH_<UPPER_SNAKE
// (name)>_PARAMS`). Долговременный канон вместо `keeper.yml::push.
// providers[]` inline (pilot S6/S7-1).
//
// Сущностно — SSH-Provider variant of Provider (PM-decision S7-1 #1, S7-2 #1):
// тот же концепт «Provider» (Cloud Provider + SSH Provider), но разные
// таблицы (`providers` для cloud, `push_providers` для ssh), разные
// схемы params и разные RBAC-permission-области (`provider.*` vs
// `push-provider.*`).
package pushprovider

import (
	"regexp"
	"time"
)

// NamePattern — каноническая форма имени Push-Provider-а: kebab-case,
// начинается с буквы, длина 1..63. Совпадает с CHECK
// `push_providers_name_format` в миграции 054 и pattern
// `^[a-z][a-z0-9-]{0,62}$` из `keeper.yml::push.providers[].name`.
//
// Дополнительное ограничение vs cloud-Provider (`^[a-z0-9-]{1,63}$`):
// имя обязано начинаться с буквы, потому что транслируется в env-имя
// (`SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS`) — лидирующая цифра/дефис
// сломает env-var-name.
const NamePattern = `^[a-z][a-z0-9-]{0,62}$`

var nameRe = regexp.MustCompile(NamePattern)

// ValidName проверяет соответствие name канонической форме.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// PushProvider — runtime-представление строки реестра `push_providers`.
//
// Params — opaque-форма провайдера: keys и values определяются самим
// плагином (vault_addr/role/proxy_addr/…). Sensitive keys
// (secret_id/token/password/private_key) ОБЯЗАНЫ быть vault-refs
// (`vault:<path>`) — валидация на service-слое (Service.validateSensitive),
// не storage.
type PushProvider struct {
	Name         string         `json:"name"`
	Params       map[string]any `json:"params"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	CreatedByAID string         `json:"created_by_aid"`
	UpdatedByAID *string        `json:"updated_by_aid,omitempty"`
}
