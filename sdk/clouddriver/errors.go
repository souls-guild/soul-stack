package clouddriver

import (
	"context"
	"errors"
	"fmt"
)

// FailClass is the unified taxonomy of CloudDriver operation failure
// reasons, shared across all providers (AWS/GCP/Azure/YC/Proxmox/OpenStack).
// Per-provider code maps its API errors to one of these classes via
// [ClassifyFunc], and the SDK builds a consistent human-readable message —
// otherwise a lineup of 5 drivers would produce 5 dialects of error
// messages.
type FailClass int

const (
	// FailUnknown means the reason wasn't recognized by the provider's
	// classifier. Transience is unknown → not retried (see [FailClass.Transient]).
	FailUnknown FailClass = iota

	// FailNotFound means the requested resource is missing (image/subnet/VM/quota object).
	FailNotFound

	// FailQuota means the provider's quota/limit was exceeded (instances, vCPU, IP).
	FailQuota

	// FailAuth means authentication/authorization was refused (broken/expired
	// credentials, no permission for the action).
	FailAuth

	// FailInvalidParams means the profile parameters are invalid on the
	// provider's side (incompatible instance_type for the AMI, bad format, etc.).
	FailInvalidParams

	// FailTransient means a temporary error (throttling, 5xx, network
	// failure): retried with backoff (see [Retry]).
	FailTransient
)

// String returns the stable machine-readable class code (goes into the
// message prefix, used by lineup tests for assertions).
func (c FailClass) String() string {
	switch c {
	case FailNotFound:
		return "not_found"
	case FailQuota:
		return "quota_exceeded"
	case FailAuth:
		return "auth"
	case FailInvalidParams:
		return "invalid_params"
	case FailTransient:
		return "transient"
	default:
		return "unknown"
	}
}

// Transient returns true for classes worth retrying ([Retry] relies on
// this). Only [FailTransient] is transient; auth/quota/not_found/
// invalid_params are deterministic failures that a retry won't fix.
func (c FailClass) Transient() bool { return c == FailTransient }

// ClassifyFunc is a per-provider classifier: parses a native error from the
// provider's SDK into a [FailClass]. The only thing a driver must write
// itself; backoff/retry/mapping-to-event is handled by the SDK. The driver
// must NOT classify ctx errors (Canceled/DeadlineExceeded) — [Classify]
// catches those before calling func.
type ClassifyFunc func(err error) FailClass

// Classify wraps a per-provider [ClassifyFunc] with common preprocessing:
// nil → [FailUnknown]; context.Canceled/DeadlineExceeded → [FailTransient]
// (a cancellation/timeout is a reason to wind down, but it's not a
// deterministic failure). Everything else is delegated to fn (nil fn →
// [FailUnknown]).
func Classify(fn ClassifyFunc, err error) FailClass {
	if err == nil {
		return FailUnknown
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return FailTransient
	}
	if fn == nil {
		return FailUnknown
	}
	return fn(err)
}

// FailMessage builds the consolidated message for a failed-event:
// `<class>: <op>: <err>`. `op` is a short phase name ("RunInstances",
// "wait-until-ready"). The format is uniform across all drivers, so
// Keeper/the operator sees the same structure.
func FailMessage(class FailClass, op string, err error) string {
	return fmt.Sprintf("%s: %s: %v", class, op, err)
}
