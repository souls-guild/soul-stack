package main

import (
	"errors"

	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/api/googleapi"
)

// classifyGCP — per-provider [clouddriver.ClassifyFunc] для GCP: маппит
// googleapi.Error по HTTP-status в общую таксономию SDK. Это единственная
// provider-specific часть error-обработки; backoff/retry/маппинг-в-event
// делает SDK (sdk/clouddriver), общий для всех драйверов тиража.
//
// GCP-ошибки приходят как *googleapi.Error с заполненным HTTP Code; reason-коды
// внутри ErrorItem.Reason есть, но для MVP-таксономии достаточно HTTP-статуса
// (granularity по reason можно добавить позднее без breaking change).
func classifyGCP(err error) clouddriver.FailClass {
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) {
		// Не-API ошибка (сеть/DNS/EOF/timeout) — транзиентна: ретрай оправдан.
		return clouddriver.FailTransient
	}
	switch apiErr.Code {
	case 401, 403:
		// 401 Unauthorized / 403 Forbidden — битые/без-прав credentials.
		// Quota-exceeded GCP тоже возвращает 403 с reason="quotaExceeded" —
		// различаем по reason-у первой error-item.
		if hasReason(apiErr, "quotaExceeded", "rateLimitExceeded") {
			if hasReason(apiErr, "rateLimitExceeded") {
				return clouddriver.FailTransient
			}
			return clouddriver.FailQuota
		}
		return clouddriver.FailAuth
	case 404:
		return clouddriver.FailNotFound
	case 409:
		// 409 Conflict — обычно alreadyExists/resourceInUse: invalid_params
		// для нашего идемпотентного flow (мы должны были найти VM раньше).
		return clouddriver.FailInvalidParams
	case 400, 412:
		return clouddriver.FailInvalidParams
	case 429:
		return clouddriver.FailTransient
	}
	if apiErr.Code >= 500 && apiErr.Code <= 599 {
		return clouddriver.FailTransient
	}
	return clouddriver.FailUnknown
}

// hasReason проверяет, что хотя бы один ErrorItem.Reason из apiErr.Errors
// совпадает с одним из переданных reason-ов.
func hasReason(apiErr *googleapi.Error, reasons ...string) bool {
	for _, item := range apiErr.Errors {
		for _, r := range reasons {
			if item.Reason == r {
				return true
			}
		}
	}
	return false
}
