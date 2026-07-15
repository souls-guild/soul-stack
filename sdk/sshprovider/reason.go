package sshprovider

import "fmt"

// DenyReason is the stable machine-readable Authorize denial code, shared
// across all SshProvider plugins in the lineup (static-key / Vault SSH CA /
// Teleport). Placed in [pluginv1.AuthorizeReply.Reason]; Keeper writes it to
// the audit trail for a fail-closed denial. Without a shared vocabulary, a
// lineup of providers would produce N dialects of deny messages — Keeper
// wouldn't be able to aggregate denial reasons across Souls.
//
// This is NOT part of the proto contract: an open string
// [pluginv1.AuthorizeReply.Reason] goes on the wire; the vocabulary is an
// SDK-side convention, extensible without touching proto.
type DenyReason string

const (
	// DenyExplicitDeny means the (host, user) pair is explicitly on the
	// provider's deny-list.
	DenyExplicitDeny DenyReason = "explicit_deny"

	// DenyNotInAllowlist means the provider runs in allowlist mode and the
	// pair isn't in it.
	DenyNotInAllowlist DenyReason = "not_in_allowlist"

	// DenyPolicy means the provider's policy refused it (e.g. root login is forbidden).
	DenyPolicy DenyReason = "policy"
)

// DenyMessage builds the consolidated reason field for AuthorizeReply:
// `<reason>: <detail>`. The format is uniform across all providers, so
// Keeper/the operator sees the same denial structure. `detail` is a
// human-readable clarification (username, the rule that fired); an empty
// detail is fine (then reason has no suffix).
func DenyMessage(reason DenyReason, detail string) string {
	if detail == "" {
		return string(reason)
	}
	return fmt.Sprintf("%s: %s", reason, detail)
}

// SignFailReason is the stable Sign error code, shared across the lineup.
// Sign returns an error (not a reply), so the code travels inside the error
// message via [SignError]; Keeper maps the step to failed and writes the
// code to the diagnostic channel.
type SignFailReason string

const (
	// SignFailReadKey means the provider couldn't read/parse the key
	// material (broken PEM, missing file, Vault unavailable). Fail-closed:
	// Keeper does NOT open the SSH session.
	SignFailReadKey SignFailReason = "read_key"

	// SignFailIssue means the provider couldn't issue/sign credentials
	// (CA providers: Vault SSH CA refusal, expired role).
	SignFailIssue SignFailReason = "issue"
)

// SignError builds an error for Sign with a stable [SignFailReason] stitched
// in: `<reason>: <err>`. Returned by the provider from Sign on any
// fail-closed branch; the format is uniform across the lineup.
func SignError(reason SignFailReason, err error) error {
	return fmt.Errorf("%s: %w", reason, err)
}
