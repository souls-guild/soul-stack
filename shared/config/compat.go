package config

// Engine compatibility window — the `compat:` block of a service/destiny
// manifest and the comparison rules for a keeper build version (ADR-0076).
//
// The block is author-declared per entity: `service.yml` and every
// `destiny.yml` state the keeper versions they were tested against. The
// effective window of a run is the INTERSECTION of all declared windows (the
// narrowest wins, ADR-0076(b)); a missing block is unbounded, so existing
// manifests keep working untouched.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// DevVersionSentinel — the un-injected build version of keeper/soul (`go build`
// without `-X main.version`). Carries no version information, so the compat
// window is NOT enforced against it (ADR-0076(e)) — otherwise plain `go build`
// would fail every declared window (`0.0.0` sorts below every min).
const DevVersionSentinel = "0.0.0-dev"

// CompatEntityService / CompatEntityDestiny — the `kind` of a window contributor
// ([CompatEntity.Kind]); the error text names the kind so an operator knows
// which artifact to re-pin.
const (
	CompatEntityService = "service"
	CompatEntityDestiny = "destiny"
)

// reCompatVersion — the only accepted form of a declared bound: plain
// `MAJOR.MINOR.PATCH` (ADR-0076(c)). No `v` prefix, no pre-release suffix, no
// `>=`/`<` operators — a free-form range is not closed under intersection, so
// there would be nothing to display as the effective window.
var reCompatVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// reGitDescribeDistance — the git-describe distance suffix `-<N>-g<sha>`
// (`v0.1.0-beta.1-12-gabc1234`). Build metadata, not precedence: stripped before
// comparison (ADR-0076(e)).
var reGitDescribeDistance = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+$`)

// ErrKeeperVersionUnsupported — sentinel behind [KeeperCompatError]: this keeper
// build is outside the declared window. The render path aborts with reason code
// `keeper_version_unsupported` (ADR-0076(f)); callers match with errors.Is to
// tell a version mismatch from a broken definition.
var ErrKeeperVersionUnsupported = errors.New("config: keeper version outside the declared compat window")

// CompatConfig — the `compat:` block of a service/destiny manifest (ADR-0076(a)).
// A nil block means unbounded on that entity — today's behavior is preserved
// with no migration (the nil-block-means-default precedent of `lifecycle:`/
// `telemetry:`). Shaped to accept a future `soul:` axis; the soul side is
// capability-based today (ADR-0076(n)).
type CompatConfig struct {
	Keeper *VersionWindow `yaml:"keeper,omitempty"`
}

// VersionWindow — a half-open `[min, max)` engine-version range (ADR-0076(c)).
// Min is inclusive; Max is EXCLUSIVE — it reads as "the first version I have
// NOT tested", which is the boundary an author can actually state, and
// `{min: 0.1.0, max: 0.3.0}` covers the whole `0.1.x`–`0.2.x` band without a
// bump on every patch. Both keys are optional (one-sided window); a window
// declaring neither is an authoring error.
type VersionWindow struct {
	Min string `yaml:"min,omitempty"`
	Max string `yaml:"max,omitempty"`
}

// KeeperWindow — nil-safe read of the declared keeper window: a nil block OR a
// nil `keeper:` entry → nil (unbounded).
func (c *CompatConfig) KeeperWindow() *VersionWindow {
	if c == nil {
		return nil
	}
	return c.Keeper
}

// CompatEntity — one contributor to the effective window: which artifact
// declared it, at which git ref (ADR-007), and the window itself (nil =
// unbounded). Carried into the error text and the API view so a narrow bound is
// attributable to the artifact that set it.
type CompatEntity struct {
	Kind   string
	Name   string
	Ref    string
	Window *VersionWindow
}

// String renders the window in the half-open form used in error text and the
// API view: `[0.1.0, 0.3.0)` for a two-sided window, `>= 0.1.0` / `< 0.3.0` for
// a one-sided one. A nil or empty-declaration window renders as `any`.
func (w *VersionWindow) String() string {
	switch {
	case w == nil || (w.Min == "" && w.Max == ""):
		return "any"
	case w.Max == "":
		return ">= " + w.Min
	case w.Min == "":
		return "< " + w.Max
	default:
		return "[" + w.Min + ", " + w.Max + ")"
	}
}

// Declared reports whether the window states at least one bound (a nil window
// and a `{}` window are both undeclared → unbounded).
func (w *VersionWindow) Declared() bool {
	return w != nil && (w.Min != "" || w.Max != "")
}

// IsEmpty reports whether the window can never be satisfied (min >= max, the
// `compat_window_empty` authoring error, ADR-0076(b)). A one-sided or
// undeclared window is never empty. Unparseable bounds are not empty either —
// the format diagnostic reports those.
func (w *VersionWindow) IsEmpty() bool {
	if w == nil || w.Min == "" || w.Max == "" {
		return false
	}
	min, max, ok := parseBounds(w.Min, w.Max)
	if !ok {
		return false
	}
	return !min.LessThan(max)
}

// Contains reports whether release (a comparable release core, see
// [NormalizeEngineVersion]) satisfies the half-open window: `min <= release <
// max`. An undeclared window contains everything; an unparseable bound is
// treated as absent (the format diagnostic reports it at validation, and a
// malformed manifest must not silently block a run).
func (w *VersionWindow) Contains(release string) bool {
	if !w.Declared() {
		return true
	}
	v, err := semver.StrictNewVersion(release)
	if err != nil {
		return true
	}
	if w.Min != "" {
		if min, err := semver.StrictNewVersion(w.Min); err == nil && v.LessThan(min) {
			return false
		}
	}
	if w.Max != "" {
		if max, err := semver.StrictNewVersion(w.Max); err == nil && !v.LessThan(max) {
			return false
		}
	}
	return true
}

// parseBounds parses both bounds of a two-sided window; ok=false if either is
// malformed.
func parseBounds(minRaw, maxRaw string) (min, max *semver.Version, ok bool) {
	min, err := semver.StrictNewVersion(minRaw)
	if err != nil {
		return nil, nil, false
	}
	max, err = semver.StrictNewVersion(maxRaw)
	if err != nil {
		return nil, nil, false
	}
	return min, max, true
}

// NormalizeEngineVersion turns a raw build version string into the comparable
// release core, per ADR-0076(d)/(e). ok=false means "there is nothing to
// compare" and the window must NOT be enforced — the caller says so out loud
// instead (log + API view), it never silently passes.
//
//	release pipeline    v0.1.0-beta.1                 -> 0.1.0, true
//	make build past tag v0.1.0-beta.1-12-gabc1234     -> 0.1.0, true
//	  the same, dirty   ...-gabc1234-dirty            -> 0.1.0, true
//	go build, no ldflags 0.0.0-dev                    -> "",    false
//	checkout with no tags abc1234                     -> "",    false
//
// A build standing past a tag is enforced as the release it is based on: it
// genuinely lacks whatever a later version introduced, and exempting it would
// hide real breakage on the dev stand.
func NormalizeEngineVersion(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "v")
	if s == "" || s == DevVersionSentinel {
		return "", false
	}
	s = strings.TrimSuffix(s, "-dirty")
	s = reGitDescribeDistance.ReplaceAllString(s, "")
	// Release core per ADR-0076(d): a pre-release keeper sits inside the window
	// of its release, and `+build` metadata never affects precedence.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if !reCompatVersion.MatchString(s) {
		return "", false
	}
	return s, true
}

// IntersectKeeperWindows computes the effective window over a set of
// contributors: min = max of the declared mins, max = min of the declared maxes
// (ADR-0076(b), the narrowest wins). Returns nil when no entity declared a
// window (unbounded — nothing to enforce or display). The result may be empty
// ([VersionWindow.IsEmpty]) — that is the `compat_window_empty` authoring error
// and is reported, not silently widened.
func IntersectKeeperWindows(entities []CompatEntity) *VersionWindow {
	var out *VersionWindow
	for _, e := range entities {
		w := e.Window
		if !w.Declared() {
			continue
		}
		if out == nil {
			out = &VersionWindow{Min: w.Min, Max: w.Max}
			continue
		}
		out.Min = higherBound(out.Min, w.Min)
		out.Max = lowerBound(out.Max, w.Max)
	}
	return out
}

// higherBound — max of two mins ("" = unbounded below, loses to any declared min).
func higherBound(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	av, aerr := semver.StrictNewVersion(a)
	bv, berr := semver.StrictNewVersion(b)
	if aerr != nil || berr != nil {
		return a
	}
	if bv.GreaterThan(av) {
		return b
	}
	return a
}

// lowerBound — min of two maxes ("" = unbounded above, loses to any declared max).
func lowerBound(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	av, aerr := semver.StrictNewVersion(a)
	bv, berr := semver.StrictNewVersion(b)
	if aerr != nil || berr != nil {
		return a
	}
	if bv.LessThan(av) {
		return b
	}
	return a
}

// FirstIncompatibleEntity returns the first contributor whose declared window
// excludes this keeper release, or nil when every window admits it. Checking
// per entity is equivalent to checking the intersection (a version is in the
// intersection iff it is in every window) and yields a far better message: the
// bound is attributed to the artifact that set it (ADR-0076(g)).
//
// release must be the comparable core from [NormalizeEngineVersion]; an empty
// release means "not enforced" and always returns nil.
func FirstIncompatibleEntity(entities []CompatEntity, release string) *CompatEntity {
	if release == "" {
		return nil
	}
	for i := range entities {
		if !entities[i].Window.Contains(release) {
			return &entities[i]
		}
	}
	return nil
}

// KeeperCompatError — the versioned rejection that replaces the opaque render
// failure (ADR-0076(g)): which entity declared which window, what this keeper
// is, and what to do about it. Unwraps to [ErrKeeperVersionUnsupported].
type KeeperCompatError struct {
	// Entity — the contributor whose window excluded this build.
	Entity CompatEntity
	// KeeperVersion — the RAW runtime version string, verbatim (the release core
	// is what compared, but the operator needs the string their binary reports).
	KeeperVersion string
}

// NewKeeperCompatError builds the rejection for entity against the raw keeper
// version string.
func NewKeeperCompatError(entity CompatEntity, keeperVersion string) *KeeperCompatError {
	return &KeeperCompatError{Entity: entity, KeeperVersion: keeperVersion}
}

func (e *KeeperCompatError) Error() string {
	at := ""
	if e.Entity.Ref != "" {
		at = fmt.Sprintf(" (ref %s)", e.Entity.Ref)
	}
	return fmt.Sprintf(
		"%s %q%s supports keeper %s; this keeper is %s - pin a keeper inside the window or upgrade the %s definition (ADR-0076)",
		e.Entity.Kind, e.Entity.Name, at, e.Entity.Window.String(), e.KeeperVersion, e.Entity.Kind)
}

// Unwrap exposes the sentinel so the render path can tell a version mismatch
// from a broken definition (errors.Is).
func (e *KeeperCompatError) Unwrap() error { return ErrKeeperVersionUnsupported }

// validateCompat — schema validation of the optional `compat:` block, shared by
// `service.yml` and `destiny.yml` (identical grammar per ADR-0076(a)). A nil
// block is valid (unbounded, backcompat).
func validateCompat(root *ast.MappingNode, c *CompatConfig) []diag.Diagnostic {
	if c == nil {
		return nil
	}
	var out []diag.Diagnostic
	base := "$.compat"

	if c.Keeper == nil {
		out = append(out, atPath(root, base, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "compat_window_incomplete",
			Message: "compat: declares no engine window",
			Hint:    "declare keeper: {min: <M.m.p>, max: <M.m.p>} or drop the compat: block (absent = unbounded)",
		}))
		return out
	}

	w := c.Keeper
	if !w.Declared() {
		out = append(out, atPath(root, base+".keeper", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "compat_window_incomplete",
			Message: "compat.keeper declares neither min nor max",
			Hint:    "set at least one bound: min (inclusive) and/or max (EXCLUSIVE - the first untested version)",
		}))
		return out
	}

	for _, b := range []struct {
		key, value string
	}{{"min", w.Min}, {"max", w.Max}} {
		if b.value == "" || reCompatVersion.MatchString(b.value) {
			continue
		}
		out = append(out, atPath(root, base+".keeper."+b.key, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "compat_version_invalid",
			Message: fmt.Sprintf("compat.keeper.%s: %q is not a plain MAJOR.MINOR.PATCH version", b.key, b.value),
			Hint:    "use 0.3.0 - no v prefix, no pre-release suffix, no >=/< operators (ADR-0076)",
		}))
	}
	if diag.HasErrors(out) {
		return out
	}

	if w.IsEmpty() {
		out = append(out, atPath(root, base+".keeper", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "compat_window_empty",
			Message: fmt.Sprintf("compat.keeper window %s can never be satisfied: max is EXCLUSIVE, so min must be strictly below it", w.String()),
			Hint:    "to allow exactly 0.2.x write {min: 0.2.0, max: 0.3.0}",
		}))
	}
	return out
}
