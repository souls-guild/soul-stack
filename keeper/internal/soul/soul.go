// Package soul — типы реестра Soul-агентов (`souls`) под ADR-002 / ADR-012
// и docs/soul/identity.md.
//
// M2.1.a: типы + CRUD (Insert / SelectBySID / UpdateStatus / List).
// gRPC Bootstrap-handler, EventStream, выдача SoulSeed-сертификатов —
// следующие slice-ы (M2.1.b, M2.2+).
package soul

import (
	"regexp"
	"time"
)

// Transport — способ доставки команд Soul-у (ADR-002).
//
// `agent` — pull-модель, демон Soul держит долгоживущий gRPC-стрим.
// `ssh` — push-модель, Keeper ходит на хост по SSH (keeper.push); у такого
// хоста нет bootstrap_token / soul_seed.
//
// Совпадает с CHECK `souls_transport_valid` в 007_create_souls.up.sql.
type Transport string

const (
	TransportAgent Transport = "agent"
	TransportSSH   Transport = "ssh"
)

// Status — состояние Soul-а в реестре. Узкий MVP-enum (docs/soul/identity.md):
//
//   - `pending` — оператор выписал bootstrap-токен, Soul ещё не подключался.
//   - `connected` — стрим жив, Keeper держит lease в Redis.
//   - `disconnected` — стрим закрыт, lease истёк (Soul может вернуться).
//   - `revoked` — оператор отозвал, новые подключения отвергаются на mTLS-уровне.
//   - `expired` — Жнец передвинул `pending` после TTL bootstrap-токена.
//   - `destroyed` — хост физически удалён через `core.cloud.provisioned destroyed`
//     (ADR-017 cascade). Terminal-state: переходов исходящих нет; в default-set
//     `purge_souls.statuses` намеренно НЕ включён (forensic > GC).
//
// Совпадает с CHECK `souls_status_valid` в 007_create_souls.up.sql
// (расширен миграцией 016 — добавлен `destroyed`).
type Status string

const (
	StatusPending      Status = "pending"
	StatusConnected    Status = "connected"
	StatusDisconnected Status = "disconnected"
	StatusRevoked      Status = "revoked"
	StatusExpired      Status = "expired"
	StatusDestroyed    Status = "destroyed"
)

// SIDPattern — каноническая форма SID (= FQDN хоста).
//
// Lowercase letters/digits, dots, hyphens; стартует с alnum; 1..254
// символа. Дублирует CHECK `souls_sid_format` из миграции — нужно для
// валидации до round-trip-а (better error messages, нет лишнего PG-запроса).
//
// Точное RFC-1035 ограничение длины label-а 63 символа здесь не
// форсируется (FQDN с label > 63 не существует на практике, но PG-CHECK
// держит общую длину строки).
const SIDPattern = `^[a-z0-9][a-z0-9.-]{0,253}$`

var sidRe = regexp.MustCompile(SIDPattern)

// ValidSID проверяет соответствие SID канонической форме.
func ValidSID(sid string) bool { return sidRe.MatchString(sid) }

// CovenPattern — каноническая форма Coven-метки: одноуровневый kebab-case
// (ADR-008 — стабильные логические теги). Совпадает по форме с именем
// сервиса/сценария и с `reCovenName` в shared/config (валидация `on:`-меток
// сценария): стартует с буквы, сегменты через дефис, без trailing/double
// дефиса. Лимит длины 1..63 — RFC-1035-совместимый порог для метки.
const CovenPattern = `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`

var covenRe = regexp.MustCompile(CovenPattern)

const covenMaxLen = 63

// ValidCoven проверяет одну Coven-метку (kebab-case, 1..63 символа).
func ValidCoven(label string) bool {
	if len(label) == 0 || len(label) > covenMaxLen {
		return false
	}
	return covenRe.MatchString(label)
}

// Soul — runtime-представление строки реестра `souls`.
//
// JSON-теги — для будущего Operator API (M0.7+). NULL-семантика SQL
// маппится в указатели: LastSeenAt = nil для никогда не подключавшегося
// Soul-а, CreatedByAID = nil для seed-импорта из cli-bootstrap-а.
type Soul struct {
	SID           string     `json:"sid"`
	Transport     Transport  `json:"transport"`
	Status        Status     `json:"status"`
	Coven         []string   `json:"coven"`
	RegisteredAt  time.Time  `json:"registered_at"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	LastSeenByKID *string    `json:"last_seen_by_kid,omitempty"`
	CreatedByAID  *string    `json:"created_by_aid,omitempty"`
	RequestedAt   *time.Time `json:"requested_at,omitempty"`
	Note          string     `json:"note,omitempty"`
}

// ValidStatus / ValidTransport — экспортированные closed-enum проверки
// для list-filter в handler-слое (симметрично [incarnation.ValidStatus]).
// Делегируют приватным валидаторам, дублирующим SQL-CHECK.
func ValidStatus(s Status) bool       { return validStatus(s) }
func ValidTransport(t Transport) bool { return validTransport(t) }

// validStatus / validTransport — closed enum проверки, дублирующие
// SQL-CHECK для отказа до round-trip-а.
func validStatus(s Status) bool {
	switch s {
	case StatusPending, StatusConnected, StatusDisconnected, StatusRevoked, StatusExpired, StatusDestroyed:
		return true
	}
	return false
}

func validTransport(t Transport) bool {
	switch t {
	case TransportAgent, TransportSSH:
		return true
	}
	return false
}
