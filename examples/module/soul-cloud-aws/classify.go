package main

import (
	"errors"
	"strings"

	"github.com/aws/smithy-go"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
)

// classifyAWS — per-provider [clouddriver.ClassifyFunc] для AWS: маппит
// smithy-API-ошибки EC2 в общую таксономию SDK по ErrorCode. Это
// единственная provider-specific часть error-обработки; backoff/retry/маппинг-
// в-event делает SDK (sdk/clouddriver), общий для всех драйверов тиража.
//
// Коды — стабильные строковые ErrorCode AWS EC2/STS; группируем по семейству
// суффикса/префикса, чтобы не перечислять сотни частных кодов.
func classifyAWS(err error) clouddriver.FailClass {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		// Не-API ошибка (сеть/DNS/EOF) — транзиентна: ретрай оправдан.
		return clouddriver.FailTransient
	}
	code := apiErr.ErrorCode()
	switch {
	// Throttle проверяется ПЕРЕД quota: `RequestLimitExceeded` — это rate-limit
	// (транзиентный), а не исчерпание квоты ресурсов, хотя содержит подстроку
	// `LimitExceeded`, которую ловит isQuota.
	case isThrottle(code):
		return clouddriver.FailTransient
	case isQuota(code):
		return clouddriver.FailQuota
	case isAuth(code):
		return clouddriver.FailAuth
	case isNotFound(code):
		return clouddriver.FailNotFound
	case isInvalidParams(code):
		return clouddriver.FailInvalidParams
	default:
		return clouddriver.FailUnknown
	}
}

func isQuota(code string) bool {
	switch code {
	case "InstanceLimitExceeded", "VcpuLimitExceeded", "MaxSpotInstanceCountExceeded",
		"AddressLimitExceeded", "VolumeLimitExceeded":
		return true
	}
	return strings.Contains(code, "LimitExceeded") || strings.HasSuffix(code, "Quota")
}

func isAuth(code string) bool {
	switch code {
	case "AuthFailure", "UnauthorizedOperation", "AccessDenied", "AccessDeniedException",
		"Blocked", "PendingVerification", "OptInRequired", "SignatureDoesNotMatch",
		"InvalidClientTokenId", "ExpiredToken", "ExpiredTokenException":
		return true
	}
	return strings.HasPrefix(code, "UnauthorizedOperation")
}

func isNotFound(code string) bool {
	// EC2 not-found-семейство: InvalidAMIID.NotFound, InvalidSubnetID.NotFound,
	// InvalidInstanceID.NotFound, InvalidGroup.NotFound, …
	return strings.Contains(code, ".NotFound") ||
		strings.HasSuffix(code, "NotFound") ||
		code == "InvalidAMIID.Unavailable"
}

func isInvalidParams(code string) bool {
	switch code {
	case "InvalidParameterValue", "InvalidParameterCombination", "MissingParameter",
		"InvalidParameter", "InvalidInstanceType", "Unsupported", "VPCResourceNotSpecified":
		return true
	}
	// Прочие Invalid*Malformed/Value (но НЕ .NotFound — он выше как not_found).
	return (strings.HasPrefix(code, "Invalid") && !strings.Contains(code, ".NotFound")) ||
		strings.HasSuffix(code, "Malformed")
}

func isThrottle(code string) bool {
	switch code {
	case "Throttling", "ThrottlingException", "RequestLimitExceeded",
		"SlowDown", "TooManyRequestsException", "ServiceUnavailable",
		"InternalError", "InternalFailure", "Unavailable":
		return true
	}
	return strings.Contains(code, "Throttl")
}
