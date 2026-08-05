package oracle

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/subject"
	"github.com/souls-guild/soul-stack/shared/config"
)

// SubjectMatches checks the Decree's subject binding against the sending host
// (ADR-030(b)). The four dimensions and the two levels coven / trait read live
// in [subject.Selector.Matches] — one implementation for Vigil, Decree and Rite,
// so a rule spelled the same way in two registries cannot bind differently.
//
// host is the AUTHORITATIVE picture, resolved from the registries by the SID of
// the mTLS peer cert ([subject.LoadHost]) — never from PortentEvent, which the
// Soul controls and which carries the SID only as an echo for logs.
//
// The subject binding is a defense layer: it restricts which hosts can even
// trigger the rule (untrusted input, ADR-030(b)). It answers "may this rule see
// the host" — NOT "does the host belong to the Decree's TARGET incarnation",
// which stays a separate gate over `incarnation_membership`
// (incarnation.IsMember). The two must not collapse into one even now that a
// subject can name an incarnation: the subject's incarnation says who may fire
// the rule, the target says what the reaction acts on, and a rule fired by one
// incarnation's host against another's is exactly the case the gate exists for.
func SubjectMatches(d *Decree, host subject.Host) bool {
	return d.Subject().Matches(host)
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
