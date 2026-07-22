package main

import (
	"errors"
	"strings"

	"github.com/souls-guild/soul-stack/sdk/clouddriver"
)

// classifyProxmox is the per-provider [clouddriver.ClassifyFunc] for Proxmox VE.
// It maps REST API HTTP status into the shared SDK taxonomy. Proxmox does not
// publish a closed-enum ErrorCode (unlike AWS), but HTTP status is stable:
//   - 401              -> auth (missing/expired ticket, invalid token)
//   - 403              -> auth (no permission on resource/action)
//   - 404              -> not_found
//   - 400              -> invalid_params
//   - 500              -> parse body text: "does not exist" -> not_found,
//     everything else -> transient (Proxmox likes 500 for temporary lock
//     contention and operational conflicts)
//   - other 5xx        -> transient
//   - 429              -> transient (rate-limit; Proxmox usually does not have
//     it, but handle defensively)
//
// This is the only provider-specific part of error handling; backoff/retry/event
// mapping is done by the SDK (sdk/clouddriver), shared by all rollout drivers.
func classifyProxmox(err error) clouddriver.FailClass {
	var hErr *pveHTTPError
	if !errors.As(err, &hErr) {
		// Non-HTTP error (network/DNS/EOF/TLS handshake) is transient.
		return clouddriver.FailTransient
	}
	body := strings.ToLower(hErr.Body)
	switch hErr.Status {
	case 401, 403:
		return clouddriver.FailAuth
	case 404:
		return clouddriver.FailNotFound
	case 400:
		return clouddriver.FailInvalidParams
	case 429:
		return clouddriver.FailTransient
	case 500:
		// Proxmox convention: "<resource> does not exist" comes as 500, not 404.
		// This is the most common source of false-positive "invalid_params" in
		// drivers without the heuristic - map it to not_found so destroy
		// idempotency works.
		if strings.Contains(body, "does not exist") ||
			strings.Contains(body, "no such") ||
			strings.Contains(body, "not found") {
			return clouddriver.FailNotFound
		}
		// Other Proxmox 500s are transient (lock contention, storage I/O).
		return clouddriver.FailTransient
	}
	if hErr.Status >= 500 && hErr.Status < 600 {
		return clouddriver.FailTransient
	}
	return clouddriver.FailUnknown
}
