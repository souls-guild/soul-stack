package oracle

import (
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// SubjectMatches checks the Decree's subject binding against the sending host
// (ADR-030(b)). A Decree's subject is strictly XOR (schema CHECK decrees_subject_xor):
//   - SubjectSID is set → match if subjectSID == *d.SubjectSID;
//   - SubjectCoven is set → match if there's an intersection of SubjectCoven ∩ covens.
//
// subjectSID is the authoritative host SID (from the mTLS peer cert, NOT PortentEvent.sid).
// covens are the host's covens from the souls registry (authoritative, NOT from the payload).
// The subject binding is a defense layer: it restricts which hosts can even
// trigger the rule (untrusted input, ADR-030(b)).
func SubjectMatches(d *Decree, subjectSID string, covens []string) bool {
	if d.SubjectSID != nil {
		return *d.SubjectSID == subjectSID
	}
	if len(d.SubjectCoven) == 0 {
		// The schema's XOR invariant guarantees we never reach here (one of the
		// subjects is non-empty). Fail-safe: no subject → no match (default-deny).
		return false
	}
	want := make(map[string]struct{}, len(d.SubjectCoven))
	for _, c := range d.SubjectCoven {
		want[c] = struct{}{}
	}
	for _, c := range covens {
		if _, ok := want[c]; ok {
			return true
		}
	}
	return false
}

// WithinCooldown reports whether the (decree, subject) pair is within the cooldown
// window: whether less time has passed since lastFired than the Decree's cooldown
// (ADR-030(a), loop-prevention). now is the single reference time of firing.
//
//   - hasFired=false (the pair hasn't fired yet) → false (cooldown is not active);
//   - cooldown <= 0 (disabled, default "0s") → false;
//   - now - lastFired < cooldown → true (blocked, skip);
//   - otherwise → false (can fire).
//
// An invalid cooldown format is treated as 0 (cooldown disabled): format
// validation happens at the service layer (S3); fail-open on cooldown here does NOT
// weaken security (cooldown is loop-prevention, not authz; subject + default-deny
// have already run).
func WithinCooldown(cooldown string, lastFired time.Time, hasFired bool, now time.Time) bool {
	if !hasFired {
		return false
	}
	d, err := config.ParseDuration(cooldown)
	if err != nil || d <= 0 {
		return false
	}
	return now.Sub(lastFired) < d
}
