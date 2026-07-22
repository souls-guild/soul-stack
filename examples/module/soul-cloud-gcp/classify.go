package main

import (
	"errors"

	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/api/googleapi"
)

// classifyGCP is a per-provider [clouddriver.ClassifyFunc] for GCP: maps
// googleapi.Error by HTTP status to the SDK's common taxonomy. This is the only
// provider-specific part of error handling; backoff/retry/event-mapping
// is done by the SDK (sdk/clouddriver), common to all drivers.
//
// GCP errors arrive as *googleapi.Error with HTTP Code populated; reason codes
// inside ErrorItem.Reason exist, but for MVP taxonomy HTTP status is sufficient
// (finer-grained reason discrimination can be added later without breaking changes).
func classifyGCP(err error) clouddriver.FailClass {
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) {
		// Non-API error (network/DNS/EOF/timeout) — transient: retry is justified.
		return clouddriver.FailTransient
	}
	switch apiErr.Code {
	case 401, 403:
		// 401 Unauthorized / 403 Forbidden — invalid/insufficient credentials.
		// Quota-exceeded in GCP also returns 403 with reason="quotaExceeded" —
		// we distinguish by the reason of the first error-item.
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
		// 409 Conflict — usually alreadyExists/resourceInUse: invalid_params
		// for our idempotent flow (we should have found the VM earlier).
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

// hasReason checks if at least one ErrorItem.Reason from apiErr.Errors
// matches one of the provided reasons.
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
