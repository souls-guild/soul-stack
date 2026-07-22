package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
)

// classifyAzure is the per-provider [clouddriver.ClassifyFunc] for Azure: it
// maps `*azcore.ResponseError` (ARM errors) into the shared SDK taxonomy by
// StatusCode + ErrorCode. Backoff/retry/event mapping is done by the SDK
// (sdk/clouddriver), shared by all rollout drivers.
//
// Mapping strategy is two-level: StatusCode (HTTP, reliable) sets the "family",
// ErrorCode (string, ARM-specific) refines within the family. Non-API errors
// (DNS/TLS/EOF/Azure SDK pipeline) are transient.
func classifyAzure(err error) clouddriver.FailClass {
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		// Non-ARM error (network/DNS/EOF/token pipeline) is transient.
		return clouddriver.FailTransient
	}
	code := respErr.ErrorCode

	// Throttle is checked BEFORE quota: 429 is rate-limit (transient), and 5xx is
	// server-side, also transient.
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
		// 409 Conflict (`ResourceGroupNotFound` also sometimes lands here for
		// Azure - override through ErrorCode).
		if isAzureNotFound(code) {
			return clouddriver.FailNotFound
		}
		return clouddriver.FailInvalidParams
	}

	// Without an explicit marker, decide by ErrorCode.
	switch {
	case isAzureAuth(code):
		return clouddriver.FailAuth
	case isAzureNotFound(code):
		return clouddriver.FailNotFound
	case isAzureInvalidParams(code):
		return clouddriver.FailInvalidParams
	}

	// 5xx without an explicit throttle marker is still transient.
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
