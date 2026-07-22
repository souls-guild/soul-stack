package oracle

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Sentinel errors for Vigil / Decree service validation (S3). The
// management-Service maps them to 422 via errors.Is (not by string prefix —
// renaming a diagnostic must not silently break the 422→500 mapping). The
// concrete diagnostic text comes from the wrapped error (already public —
// built here, with no internal SQL/stack).
var ErrValidation = errors.New("oracle: validation failed")

// Form patterns duplicating migration 041's CHECKs (reject a bad value
// before the round-trip — better diagnostics, no wasted call on bad input).
//
//   - NamePattern        — vigils_name_format / decrees_name_format (kebab 1..63).
//   - CovenPattern       — the format of one coven label element of the
//     subject (kebab; a per-element CHECK for text[] can't be expressed
//     declaratively without a trigger — migration 041).
//   - IncarnationPattern — decrees_incarnation_name_format (= incarnation.name,
//     the root Coven label, ADR-008).
//   - ScenarioPattern    — decrees_scenario_format (snake_case named scenario).
const (
	NamePattern        = `^[a-z0-9-]{1,63}$`
	CovenPattern       = `^[a-z0-9][a-z0-9-]*$`
	IncarnationPattern = `^[a-z0-9][a-z0-9-]{0,62}$`
	ScenarioPattern    = `^[a-z][a-z0-9_]*$`
)

var (
	nameRe        = regexp.MustCompile(NamePattern)
	covenRe       = regexp.MustCompile(CovenPattern)
	incarnationRe = regexp.MustCompile(IncarnationPattern)
	scenarioRe    = regexp.MustCompile(ScenarioPattern)
)

// ValidName checks a Vigil / Decree name against the canonical form (kebab 1..63).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// ValidCoven checks a single subject Coven label.
func ValidCoven(coven string) bool { return covenRe.MatchString(coven) }

// ValidIncarnationName checks a Decree's target incarnation (the
// incarnation.name format).
func ValidIncarnationName(name string) bool { return incarnationRe.MatchString(name) }

// ValidScenario checks a named scenario's name (a Decree's action_scenario).
func ValidScenario(name string) bool { return scenarioRe.MatchString(name) }

// knownBeaconAddrs — a closed enum of built-in core-beacon addresses
// (ADR-030, VigilDef.check). Built from the canonical [beaconaddr.All] list
// — a single source of truth shared with the soul-side registry
// `beacon.Default()` (`soul/internal/beacon`). keeper does NOT import soul
// (compiler isolation, ADR-011), so the shared list lives in the neutral
// `shared/beaconaddr`: this removes the old keeper↔soul duplication (an S3
// bug: drift produced a false 422 on a valid Vigil). The `soul_beacon`
// plugin-kind (community checks, S5) isn't introduced yet — until then
// check_addr is restricted to this set (an unknown check is a validation
// error, not a silently unexecutable Vigil).
var knownBeaconAddrs = buildKnownBeaconAddrs()

func buildKnownBeaconAddrs() map[string]struct{} {
	addrs := beaconaddr.All()
	m := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		m[a] = struct{}{}
	}
	return m
}

// ValidCheckAddr — membership of check_addr in [knownBeaconAddrs].
func ValidCheckAddr(addr string) bool {
	_, ok := knownBeaconAddrs[addr]
	return ok
}

// validateCovenList checks the per-element format of the subject's coven
// labels. An empty list is fine at the caller (the XOR check decides
// whether a subject is set); here it's only the format of non-empty
// elements.
func validateCovenList(coven []string) error {
	for _, c := range coven {
		if !ValidCoven(c) {
			return fmt.Errorf("%w: invalid coven %q (must match %s)", ErrValidation, c, CovenPattern)
		}
	}
	return nil
}

// validateSubjectXOR checks the subject's XOR invariant (coven-list XOR
// sid): exactly one of a non-empty coven list / a non-empty sid is set.
// Symmetric with the vigils_subject_xor / decrees_subject_xor CHECKs
// (defence in depth) and [augur.ValidateSubjectXOR]. SID format isn't
// normalized at this layer (FQDN semantics are the registry's side).
func validateSubjectXOR(coven []string, sid *string) error {
	hasCoven := len(coven) > 0
	hasSID := sid != nil && *sid != ""
	if hasCoven == hasSID {
		return fmt.Errorf("%w: subject must be exactly one of coven / sid (XOR)", ErrValidation)
	}
	if hasCoven {
		return validateCovenList(coven)
	}
	return nil
}

// validateInterval checks a Vigil's interval duration-string format through
// the same parser as other keeper.yml duration fields ([config.ParseDuration]).
func validateInterval(spec string) error {
	if spec == "" {
		return fmt.Errorf("%w: interval is empty", ErrValidation)
	}
	if _, err := config.ParseDuration(spec); err != nil {
		return fmt.Errorf("%w: invalid interval %q: %s", ErrValidation, spec, err)
	}
	return nil
}

// validateCooldown checks a Decree's cooldown format. An empty string is
// fine (the repository fills in DEFAULT '0s' — cooldown disabled).
func validateCooldown(spec string) error {
	if spec == "" {
		return nil
	}
	if _, err := config.ParseDuration(spec); err != nil {
		return fmt.Errorf("%w: invalid cooldown %q: %s", ErrValidation, spec, err)
	}
	return nil
}
