package clouddriver

import (
	"context"
	"errors"
	"fmt"
)

// FailClass — единая таксономия причин отказа CloudDriver-операции, общая для
// всех провайдеров (AWS/GCP/Azure/YC/Proxmox/OpenStack). Per-provider код
// маппит свои API-ошибки в один из этих классов через [ClassifyFunc], а
// SDK формирует консистентный человекочитаемый message — иначе тираж 5
// драйверов даст 5 диалектов error-сообщений.
type FailClass int

const (
	// FailUnknown — причина не распознана классификатором провайдера.
	// Транзиентность неизвестна → не ретраится (см. [FailClass.Transient]).
	FailUnknown FailClass = iota

	// FailNotFound — запрошенный ресурс отсутствует (образ/subnet/VM/квота-объект).
	FailNotFound

	// FailQuota — превышена квота/лимит провайдера (instances, vCPU, IP).
	FailQuota

	// FailAuth — отказ аутентификации/авторизации (битые/просроченные
	// credentials, нет прав на действие).
	FailAuth

	// FailInvalidParams — параметры профиля невалидны на стороне провайдера
	// (несовместимый instance_type для AMI, неверный формат и т.п.).
	FailInvalidParams

	// FailTransient — временная ошибка (throttling, 5xx, сетевой сбой):
	// ретраится с backoff (см. [Retry]).
	FailTransient
)

// String — стабильный машинно-читаемый код класса (идёт в message-префикс,
// используется тестами тиража для assert-ов).
func (c FailClass) String() string {
	switch c {
	case FailNotFound:
		return "not_found"
	case FailQuota:
		return "quota_exceeded"
	case FailAuth:
		return "auth"
	case FailInvalidParams:
		return "invalid_params"
	case FailTransient:
		return "transient"
	default:
		return "unknown"
	}
}

// Transient — true для классов, которые имеет смысл ретраить ([Retry]
// опирается на это). Только [FailTransient] транзиентен; auth/quota/not_found/
// invalid_params — детерминированные отказы, ретрай их не починит.
func (c FailClass) Transient() bool { return c == FailTransient }

// ClassifyFunc — per-provider классификатор: разбирает нативную ошибку
// провайдерского SDK в [FailClass]. Единственное, что драйвер обязан написать
// сам; backoff/retry/маппинг-в-event берёт на себя SDK. ctx-ошибки
// (Canceled/DeadlineExceeded) драйвер классифицировать НЕ должен — их ловит
// [Classify] до вызова func.
type ClassifyFunc func(err error) FailClass

// Classify — обёртка над per-provider [ClassifyFunc] с общей предобработкой:
// nil → [FailUnknown]; context.Canceled/DeadlineExceeded → [FailTransient]
// (отмена/таймаут — повод свернуться, но это не детерминированный отказ).
// Остальное делегируется fn (nil fn → [FailUnknown]).
func Classify(fn ClassifyFunc, err error) FailClass {
	if err == nil {
		return FailUnknown
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return FailTransient
	}
	if fn == nil {
		return FailUnknown
	}
	return fn(err)
}

// FailMessage — консолидированный message для failed-event: `<class>: <op>: <err>`.
// `op` — короткое имя фазы («RunInstances», «wait-until-ready»). Формат един
// для всех драйверов, чтобы Keeper/оператор видели одинаковую структуру.
func FailMessage(class FailClass, op string, err error) string {
	return fmt.Sprintf("%s: %s: %v", class, op, err)
}
