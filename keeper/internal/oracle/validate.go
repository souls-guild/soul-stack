package oracle

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/souls-guild/soul-stack/keeper/internal/subject"
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
//   - IncarnationPattern — decrees_incarnation_name_format (= incarnation.name).
//   - ScenarioPattern    — decrees_scenario_format (snake_case named scenario).
//
// The SUBJECT's own forms are not restated here — they belong to
// [subject.Validate], the one validator all three registries share.
const (
	NamePattern        = `^[a-z0-9-]{1,63}$`
	IncarnationPattern = `^[a-z0-9][a-z0-9-]{0,62}$`
	ScenarioPattern    = `^[a-z][a-z0-9_]*$`
)

var (
	nameRe        = regexp.MustCompile(NamePattern)
	incarnationRe = regexp.MustCompile(IncarnationPattern)
	scenarioRe    = regexp.MustCompile(ScenarioPattern)
)

// ValidName checks a Vigil / Decree name against the canonical form (kebab 1..63).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// ValidCoven checks a single Coven label. Kept as the package's spelling of
// [subject.ValidCoven] for callers that check one label outside a selector.
func ValidCoven(coven string) bool { return subject.ValidCoven(coven) }

// ValidIncarnationName checks a Decree's TARGET incarnation — the one the
// reaction acts on, not the subject (the subject's incarnation is a
// service+name pair, checked by [subject.Validate]).
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

// validateSubject checks the subject's exactly-one-of-four invariant and the
// form of every populated element, through the validator all three registries
// share ([subject.Validate]). Symmetric with the vigils_subject_one_of /
// decrees_subject_one_of CHECKs (defence in depth).
//
// An `incarnation=` subject additionally has to name an incarnation that
// EXISTS. There is no FK on those columns on purpose, so nothing else would
// catch a typo: the rule would just never match, silently, forever.
//
// Wrapped in [ErrValidation] so the management service keeps mapping it to 422
// by errors.Is — the diagnostic text from [subject.Validate] is already public
// (identifiers and operator-set labels, no SQL and no stack).
func (s *Service) validateSubject(ctx context.Context, sel subject.Selector) error {
	if err := subject.Validate(sel, "oracle:"); err != nil {
		return fmt.Errorf("%w: %s", ErrValidation, err)
	}
	if sel.Dimension() != subject.DimIncarnation {
		return nil
	}
	ok, err := subject.ExistsIncarnation(ctx, s.pool, sel.Service, sel.Incarnation)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: subject incarnation %s.%s does not exist", ErrValidation, sel.Service, sel.Incarnation)
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
