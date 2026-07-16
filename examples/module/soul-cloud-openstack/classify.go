package main

import (
	"errors"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
)

// classifyOS is the per-provider [clouddriver.ClassifyFunc] for OpenStack: it
// maps gophercloud errors (ErrUnexpectedResponseCode + typed ErrDefault4xx) into
// the shared SDK taxonomy by HTTP status. OpenStack does not publish a
// closed-enum of machine codes in the response body, so the only stable signal is
// HTTP status, supplemented by text heuristics (throttle/quota), as in
// classifyYC.
//
// This is the only provider-specific part of error handling; backoff/retry/event
// mapping is done by the SDK (sdk/clouddriver), shared by all rollout drivers.
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
		// 409 Conflict in OpenStack can mean both "invalid state transition"
		// (cannot delete an instance in BUILD) and an idempotency conflict. It is
		// fixed by the operator or naturally over time; retrying as transient will
		// not fix it -> invalid.
		return clouddriver.FailInvalidParams
	case 400, 422:
		return clouddriver.FailInvalidParams
	case 413:
		// 413 RequestEntityTooLarge in OpenStack is rate-limit exceeded (Nova),
		// rate-limit headers with Retry-After. Do not confuse with quota.
		return clouddriver.FailTransient
	case 429:
		return clouddriver.FailTransient
	case 500, 502, 503, 504:
		// 5xx is server-side transient, retry is justified.
		return clouddriver.FailTransient
	}
	// No code (not an HTTP error). Text heuristic: quota is mentioned in 403/413
	// messages across versions; a separate class is useful for the operator.
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "quota") || strings.Contains(low, "limit exceeded") {
		return clouddriver.FailQuota
	}
	if strings.Contains(low, "throttl") || strings.Contains(low, "rate limit") || strings.Contains(low, "too many requests") {
		return clouddriver.FailTransient
	}
	// Non-HTTP error (network/DNS/EOF/TLS) is transient.
	return clouddriver.FailTransient
}

// statusCode returns HTTP status from a gophercloud error. The v2 SDK provides a
// typed gophercloud.ErrUnexpectedResponseCode with StatusCode for all responses
// that did not match expectations; typed ErrDefault4xx
// (Unauthorized/Forbidden/NotFound/Conflict/...) embed it. 0 = code cannot be
// determined (not an HTTP error).
func statusCode(err error) int {
	var ue gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &ue) {
		return ue.Actual
	}
	return 0
}
