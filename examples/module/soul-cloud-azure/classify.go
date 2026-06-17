package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
)

// classifyAzure — per-provider [clouddriver.ClassifyFunc] для Azure: маппит
// `*azcore.ResponseError` (ARM-ошибки) в общую таксономию SDK по StatusCode +
// ErrorCode. Backoff/retry/маппинг-в-event делает SDK (sdk/clouddriver), общий
// для всех драйверов тиража.
//
// Стратегия маппинга — двухуровневая: StatusCode (HTTP, надёжно) задаёт
// «семейство», ErrorCode (строковый, ARM-specific) уточняет в пределах семейства.
// Не-API ошибки (DNS/TLS/EOF/Azure SDK pipeline) — транзиентны.
func classifyAzure(err error) clouddriver.FailClass {
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		// Не-ARM ошибка (сеть/DNS/EOF/токен-pipeline) — транзиентна.
		return clouddriver.FailTransient
	}
	code := respErr.ErrorCode

	// Throttle проверяется ПЕРЕД quota: 429 — rate-limit (транзиентный),
	// 5xx — server-side, тоже транзиентны.
	if isAzureThrottle(respErr.StatusCode, code) {
		return clouddriver.FailTransient
	}
	if isAzureQuota(code) {
		return clouddriver.FailQuota
	}

	switch respErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return clouddriver.FailAuth
	case http.StatusNotFound:
		return clouddriver.FailNotFound
	case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		// 409 Conflict (`ResourceGroupNotFound` тоже сюда у Azure иногда падает —
		// перебиваем через ErrorCode).
		if isAzureNotFound(code) {
			return clouddriver.FailNotFound
		}
		return clouddriver.FailInvalidParams
	}

	// Без явного маркера — по ErrorCode.
	switch {
	case isAzureAuth(code):
		return clouddriver.FailAuth
	case isAzureNotFound(code):
		return clouddriver.FailNotFound
	case isAzureInvalidParams(code):
		return clouddriver.FailInvalidParams
	}

	// 5xx без явного throttle-маркера — всё равно транзиент.
	if respErr.StatusCode >= 500 && respErr.StatusCode < 600 {
		return clouddriver.FailTransient
	}
	return clouddriver.FailUnknown
}

func isAzureThrottle(status int, code string) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	switch code {
	case "TooManyRequests", "ThrottlingError", "OperationThrottled",
		"SubscriptionRequestsThrottled", "ServerBusy":
		return true
	}
	return strings.Contains(code, "Throttl")
}

func isAzureQuota(code string) bool {
	switch code {
	case "QuotaExceeded", "OperationNotAllowed", "SkuNotAvailable",
		"PublicIPCountLimitReached", "NetworkInterfaceCountLimitReached":
		return true
	}
	return strings.Contains(code, "QuotaExceeded") ||
		strings.Contains(code, "LimitReached") ||
		strings.Contains(code, "LimitExceeded")
}

func isAzureAuth(code string) bool {
	switch code {
	case "AuthenticationFailed", "AuthorizationFailed", "InvalidAuthenticationToken",
		"ExpiredAuthenticationToken", "Forbidden", "InvalidClientSecret",
		"InvalidTenantId", "Unauthorized":
		return true
	}
	return strings.HasPrefix(code, "Auth") || strings.HasPrefix(code, "Forbidden")
}

func isAzureNotFound(code string) bool {
	switch code {
	case "ResourceNotFound", "ResourceGroupNotFound", "SubscriptionNotFound",
		"NotFound", "ParentResourceNotFound":
		return true
	}
	return strings.HasSuffix(code, "NotFound")
}

func isAzureInvalidParams(code string) bool {
	switch code {
	case "InvalidParameter", "InvalidRequestFormat", "InvalidResourceName",
		"BadRequest", "InvalidTemplate", "InvalidIPAddressPrefix",
		"PropertyChangeNotAllowed", "OperationPreconditionFailedWithStatusCode":
		return true
	}
	return strings.HasPrefix(code, "Invalid") || strings.HasSuffix(code, "Malformed")
}
