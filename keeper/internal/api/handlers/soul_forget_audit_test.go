package handlers

import (
	"fmt"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// Every audit row is passed through [audit.MaskSecrets] before it is stored
// (auditpg.Writer.Write), and that mask matches key names by SUBSTRING,
// case-insensitively: a key containing `token`, `secret`, `credential`,
// `password`, `key`, … has its value replaced with [audit.MaskedValue] —
// whatever the type, integers and booleans included.
//
// For `soul.forgotten` that is not a cosmetic redaction. Every row this
// payload describes is already deleted by the time it is written: the
// bootstrap tokens counted here were burned and then cascaded away with the
// `souls` row, so the audit event is the ONLY durable record that they ever
// existed. A masked count is not a secret withheld, it is the permanent loss
// of the one number the operator came to the audit trail for.
//
// The neighbouring IssueToken payload dodges the same trap by construction
// (see [SoulForgetReply] siblings in soul.go and mcp/soul_tools_test.go →
// assertNoTokenNamedKey). This is that guard for the forget payload, written
// against the whole map rather than one named key, so a field added later is
// covered without anyone having to remember the rule.

// findMasked reports the path of the first value that came back masked.
func findMasked(v any, path string) (string, bool) {
	switch t := v.(type) {
	case string:
		if t == audit.MaskedValue {
			return path, true
		}
	case map[string]any:
		for k, sub := range t {
			if p, ok := findMasked(sub, path+"."+k); ok {
				return p, true
			}
		}
	case []any:
		for i, sub := range t {
			if p, ok := findMasked(sub, fmt.Sprintf("%s[%d]", path, i)); ok {
				return p, true
			}
		}
	}
	return "", false
}

func TestSoulForgetAuditPayload_SurvivesTheSecretMask(t *testing.T) {
	// Counts are deliberately non-zero: a zero would still be a number, but a
	// masked field is only obviously wrong next to the value it replaced.
	reply := SoulForgetReply{Body: SoulForgetView{
		SID:                "host1.example.com",
		StatusBefore:       "disconnected",
		SeedsRevoked:       2,
		BootstrapsBurned:   1,
		MembershipsSevered: 3,
		ChoirVoicesRemoved: 2,
		LocalStreamClosed:  true,
		Broadcast:          true,
		CacheKeysPurged:    3,
		Warnings:           []string{},
	}}

	payload := map[string]any(reply.AuditPayload())
	masked := audit.MaskSecrets(payload)

	if path, ok := findMasked(masked, "payload"); ok {
		t.Errorf("%s came back as %q after audit.MaskSecrets — the stored `soul.forgotten` row "+
			"loses this value, and it is the only durable record left of what was released; "+
			"rename the key so it carries no substring the mask matches", path, audit.MaskedValue)
	}

	// The count keys specifically: the operator reads these to tell "forgotten
	// and released" from "forgotten, and something is still held".
	for _, k := range []string{"seeds_revoked", "bootstraps_burned", "memberships_severed",
		"choir_voices_removed", "cache_keys_purged"} {
		if masked[k] != payload[k] {
			t.Errorf("count %q: stored %#v, reply says %#v — the audit trail and the 200 body "+
				"disagree about what happened", k, masked[k], payload[k])
		}
	}
}
