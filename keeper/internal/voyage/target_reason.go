package voyage

import "github.com/souls-guild/soul-stack/shared/coremanifest"

// TargetReason is why a target ended without succeeding, stored in
// voyage_targets.error_code / error_message (migration 125). The zero value
// records neither: a target that succeeded, or one whose child run carries its
// own reason.
//
// Message is a sentence the Keeper composed for an operator. It is never a raw
// wrapped error, because a dispatch error can quote request input and this
// column is read by everyone who may read the Voyage.
type TargetReason struct {
	Code    string
	Message string
}

// Values of voyage_targets.error_code. docs/keeper/operator-api/voyages.md
// documents the same set and TestTargetErrorCodesMatchDocs compares the two.
const (
	// A scenario target refused before its run started.
	TargetErrorIncarnationErrorLocked = "incarnation_error_locked"
	TargetErrorIncarnationApplying    = "incarnation_applying"
	TargetErrorIncarnationNotRunnable = "incarnation_not_runnable"
	TargetErrorIncarnationNotFound    = "incarnation_not_found"
	TargetErrorServiceNotRegistered   = "service_not_registered"
	TargetErrorOrphanLockReleaseFail  = "orphan_lock_release_failed"
	TargetErrorAwaitFailed            = "await_failed"

	// A command target refused before its Errand was dispatched.
	TargetErrorSoulNotConnected           = "soul_not_connected"
	TargetErrorDryRunUnsupported          = coremanifest.ReasonDryRunUnsupported
	TargetErrorSoulCapabilityUnsupported  = "soul_capability_unsupported"
	TargetErrorSoulCapabilityUnverifiable = "soul_capability_unverifiable"
	TargetErrorInvalidRequest             = "invalid_request"

	// A command target whose Errand ended without succeeding, or was not
	// awaited to its end.
	TargetErrorErrandFailed     = "errand_failed"
	TargetErrorModuleNotAllowed = "module_not_allowed"
	TargetErrorTimedOut         = "timed_out"
	TargetErrorCancelled        = "cancelled"
	TargetErrorAwaitTimeout     = "await_timeout"
	TargetErrorAsyncEscalation  = "async_escalation"
	TargetErrorUnknownStatus    = "unknown_status"

	// A scenario Voyage the start gate failed before it started: its targets'
	// maintenance windows never meet, or cannot be read or evaluated
	// (ADR-0093 §3). Every target carries it.
	TargetErrorNoCommonWindow = "no_common_window"

	// Either kind.
	TargetErrorInputDecode          = "input_decode_error"
	TargetErrorOwnershipCheckFailed = "ownership_check_failed"
	TargetErrorSpawn                = "spawn_error"
)

// TargetErrorCodes lists every value written to voyage_targets.error_code.
var TargetErrorCodes = []string{
	TargetErrorIncarnationErrorLocked,
	TargetErrorIncarnationApplying,
	TargetErrorIncarnationNotRunnable,
	TargetErrorIncarnationNotFound,
	TargetErrorServiceNotRegistered,
	TargetErrorOrphanLockReleaseFail,
	TargetErrorAwaitFailed,
	TargetErrorSoulNotConnected,
	TargetErrorDryRunUnsupported,
	TargetErrorSoulCapabilityUnsupported,
	TargetErrorSoulCapabilityUnverifiable,
	TargetErrorInvalidRequest,
	TargetErrorErrandFailed,
	TargetErrorModuleNotAllowed,
	TargetErrorTimedOut,
	TargetErrorCancelled,
	TargetErrorAwaitTimeout,
	TargetErrorAsyncEscalation,
	TargetErrorUnknownStatus,
	TargetErrorNoCommonWindow,
	TargetErrorInputDecode,
	TargetErrorOwnershipCheckFailed,
	TargetErrorSpawn,
}

// IsTargetErrorCode reports whether code is one of [TargetErrorCodes].
func IsTargetErrorCode(code string) bool {
	for _, c := range TargetErrorCodes {
		if c == code {
			return true
		}
	}
	return false
}
