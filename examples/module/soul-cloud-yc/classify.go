package main

import (
	"strings"

	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// classifyYC is the per-provider [clouddriver.ClassifyFunc] for Yandex Cloud:
// it maps YC grpc-status errors into the common SDK taxonomy by grpc.Code plus
// message-text heuristics (YC does not publish closed-enum ErrorCodes like AWS,
// so we use the grpc canon plus substrings).
//
// This is the only provider-specific part of error handling; backoff/retry and
// event mapping are done by the SDK (sdk/clouddriver), shared by all drivers in
// the rollout.
func classifyYC(err error) clouddriver.FailClass {
	st, ok := status.FromError(err)
	if !ok {
		// Non-grpc error (network/DNS/EOF/TLS) is transient: retry is justified.
		return clouddriver.FailTransient
	}
	switch st.Code() {
	case codes.Unauthenticated, codes.PermissionDenied:
		return clouddriver.FailAuth
	case codes.NotFound:
		return clouddriver.FailNotFound
	case codes.ResourceExhausted:
		// YC returns ResourceExhausted for both rate-limit (429) and quota
		// exhaustion. Distinguish by message substring: throttling wording means
		// transient, everything else means quota.
		if isThrottleMsg(st.Message()) {
			return clouddriver.FailTransient
		}
		return clouddriver.FailQuota
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return clouddriver.FailInvalidParams
	case codes.AlreadyExists:
		// Happens during idempotent create by name. It is neither auth nor quota,
		// so classify it as invalid_params: the operator must choose another name
		// or delete the conflicting VM.
		return clouddriver.FailInvalidParams
	case codes.Unavailable, codes.DeadlineExceeded, codes.Aborted, codes.Internal:
		return clouddriver.FailTransient
	case codes.Canceled:
		return clouddriver.FailTransient
	default:
		return clouddriver.FailUnknown
	}
}

// isThrottleMsg checks whether ResourceExhausted is a rate-limit case. YC also
// uses ResourceExhausted for quota exhaustion. Per public docs, API rate-limit
// messages contain "throttl", "too many", or "rate limit". The heuristic is
// deliberately broad: a false-positive throttle causes a retry of a transient
// operation, which is safer than not retrying quota exhaustion.
func isThrottleMsg(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "throttl") ||
		strings.Contains(low, "too many requests") ||
		strings.Contains(low, "rate limit")
}
