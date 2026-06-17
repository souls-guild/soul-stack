package main

import (
	"errors"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
)

// classifyOS — per-provider [clouddriver.ClassifyFunc] для OpenStack: маппит
// gophercloud-ошибки (ErrUnexpectedResponseCode + типизированные ErrDefault4xx)
// в общую таксономию SDK по HTTP-статусу. OpenStack не публикует closed-enum
// машинных кодов в теле ответа — единственный стабильный сигнал — HTTP-статус,
// дополняем эвристикой по тексту (throttle/quota), как в classifyYC.
//
// Это единственная provider-specific часть error-обработки;
// backoff/retry/маппинг-в-event делает SDK (sdk/clouddriver), общий для всех
// драйверов тиража.
func classifyOS(err error) clouddriver.FailClass {
	if err == nil {
		return clouddriver.FailUnknown
	}
	code := statusCode(err)
	switch code {
	case 401, 403:
		return clouddriver.FailAuth
	case 404:
		return clouddriver.FailNotFound
	case 409:
		// 409 Conflict у OpenStack — это и «invalid state transition» (нельзя
		// удалить инстанс в BUILD), и идемпотент-конфликт. Лечится оператором
		// либо естественно по времени; ретрай как transient не починит → invalid.
		return clouddriver.FailInvalidParams
	case 400, 422:
		return clouddriver.FailInvalidParams
	case 413:
		// 413 RequestEntityTooLarge у OpenStack — превышение rate-limit (Nova),
		// rate-limit headers с Retry-After. Не путать с quota.
		return clouddriver.FailTransient
	case 429:
		return clouddriver.FailTransient
	case 500, 502, 503, 504:
		// 5xx — серверная транзиентка, ретрай оправдан.
		return clouddriver.FailTransient
	}
	// Кода нет (не HTTP-ошибка). Эвристика по тексту: quota упоминается в
	// 403/413 сообщениях разных версий — отдельный класс полезен оператору.
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "quota") || strings.Contains(low, "limit exceeded") {
		return clouddriver.FailQuota
	}
	if strings.Contains(low, "throttl") || strings.Contains(low, "rate limit") || strings.Contains(low, "too many requests") {
		return clouddriver.FailTransient
	}
	// Не-HTTP ошибка (сеть/DNS/EOF/TLS) — транзиентна.
	return clouddriver.FailTransient
}

// statusCode возвращает HTTP-status из gophercloud-ошибки. v2 SDK предоставляет
// типизированную gophercloud.ErrUnexpectedResponseCode с полем StatusCode для
// всех ответов, не совпавших с ожиданием; типизированные ErrDefault4xx
// (Unauthorized/Forbidden/NotFound/Conflict/...) её embed-ят. 0 = код не
// определяется (не HTTP-ошибка).
func statusCode(err error) int {
	var ue gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &ue) {
		return ue.Actual
	}
	return 0
}
