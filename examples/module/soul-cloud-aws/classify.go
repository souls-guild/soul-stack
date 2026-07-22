package main

import (
	"errors"
	"strings"

	"github.com/aws/smithy-go"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
)

// classifyAWS is a per-provider [clouddriver.ClassifyFunc] for AWS: maps
// smithy API errors from EC2 to a common SDK taxonomy by ErrorCode. This
// is the only provider-specific part of error handling; backoff/retry/event-mapping
// are done by SDK (sdk/clouddriver), common to all drivers.
//
// Codes are stable string ErrorCodes from AWS EC2/STS; we group by suffix/prefix
// family to avoid enumerating hundreds of specific codes.
func classifyAWS(err error) clouddriver.FailClass {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		// Non-API error (network/DNS/EOF) is transient: retry is justified.
		return clouddriver.FailTransient
	}
	code := apiErr.ErrorCode()
	switch {
	// Throttle is checked BEFORE quota: `RequestLimitExceeded` is a rate-limit
	// (transient), not resource quota exhaustion, even though it contains the substring
	// `LimitExceeded` that isQuota would match.
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
	// EC2 not-found family: InvalidAMIID.NotFound, InvalidSubnetID.NotFound,
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
	// Other Invalid*Malformed/Value (but NOT .NotFound — it's above as not_found).
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
