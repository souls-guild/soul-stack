package artifact

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
)

// varsBaseFile — the conventional name of a service's baseline vars layer. Kept
// only so a caller can say WHICH file it expected when the whole directory turns
// out to be missing; the catalog itself is read from the assembled vars, not from
// this file.
const varsBaseFile = "vars/00-base.yaml"

// DirectiveCatalog — a snapshot directive catalog: the SHA1 of the
// materialized snapshot (serves as an ETag, the catalog is immutable at a
// given git ref) + a map of `series (major.minor) → sorted directive names`.
// The result shape of the /directives lister.
type DirectiveCatalog struct {
	SHA1       string
	Directives map[string][]string
}

// LoadDirectiveCatalog reads the service's catalog of valid directive names
// from the `vars/00-base.yaml` snapshot (key `redis_directives`, a
// series→[]name map) and, if version is non-empty, narrows it to that
// version's major.minor series (the same logic as the render phase's assert,
// see FilterDirectivesByVersion). serviceRoot — the absolute path to the
// snapshot (ServiceArtifact.LocalDir).
//
// A service without a catalog (no vars/00-base.yaml OR no
// redis_directives key) → a non-nil empty map + nil error (the frontend
// degrades gracefully, HTTP 200). A read error (other than NotExist) /
// invalid YAML → an error (the handler maps it to 502).
func LoadDirectiveCatalog(serviceRoot, version string) (map[string][]string, error) {
	full, err := loadDirectiveCatalogFull(serviceRoot)
	if err != nil {
		return nil, err
	}
	return FilterDirectivesByVersion(full, version), nil
}

// loadDirectiveCatalogFull reads the whole catalog (all series) from
// `vars/00-base.yaml`. Missing file/key → an empty non-nil map (soft).
func loadDirectiveCatalogFull(serviceRoot string) (map[string][]string, error) {
	// The ASSEMBLED vars, not one file: a service may spread its vars over as
	// many `NN-*.yaml` as it likes, and the catalog is wherever its author put
	// it. Lexical assembly specifically — a `_stack.yaml`'s conditionality is
	// per-incarnation by construction (its steps read `incarnation.*`), and this
	// endpoint answers a service-level question with no incarnation in hand.
	vars, err := servicevars.NewResolver(nil).ResolveLexical(serviceRoot)
	if err != nil {
		return nil, fmt.Errorf("artifact: assembling service vars: %w", err)
	}
	raw := struct {
		RedisDirectives map[string][]string
	}{}
	if v, ok := vars["redis_directives"]; ok {
		b, merr := json.Marshal(map[string]any{"redis_directives": v})
		if merr != nil {
			return nil, fmt.Errorf("artifact: re-encoding the directive catalog: %w", merr)
		}
		var decoded struct {
			RedisDirectives map[string][]string `json:"redis_directives"`
		}
		if derr := json.Unmarshal(b, &decoded); derr != nil {
			return map[string][]string{}, nil // not the shape a catalog takes — no catalog
		}
		raw.RedisDirectives = decoded.RedisDirectives
	}
	if raw.RedisDirectives == nil {
		return map[string][]string{}, nil
	}
	// Defensive sort (the catalog generator has usually already sorted the
	// names).
	for _, names := range raw.RedisDirectives {
		sort.Strings(names)
	}
	return raw.RedisDirectives, nil
}

// FilterDirectivesByVersion narrows the catalog to the series version belongs
// to (e.g. "8.2.2" → series "8.2"). version=="" → the whole catalog (the same
// map). The membership rule mirrors the create/update_config assert (the service-var
// #6): series s matches version if version ~ `^([0-9]+:)?<s>[.]` (optional
// epoch prefix of a distro pin `5:7.0.15…`; the trailing dot is the series
// boundary, so 7.0 does not catch 7.04). version with no known series → an
// empty non-nil map (we don't block, same as an assert-skip).
func FilterDirectivesByVersion(catalog map[string][]string, version string) map[string][]string {
	if version == "" {
		return catalog
	}
	out := make(map[string][]string, 1)
	for series, names := range catalog {
		if directiveSeriesMatchesVersion(series, version) {
			out[series] = names
		}
	}
	return out
}

// directiveSeriesMatchesVersion — a regex match of series against version,
// identical to the render phase's CEL assert (RE2 in both). series comes from
// the trusted catalog (major.minor), so Compile never fails; err → false
// (defensive).
func directiveSeriesMatchesVersion(series, version string) bool {
	re, err := regexp.Compile("^([0-9]+:)?" + series + "[.]")
	if err != nil {
		return false
	}
	return re.MatchString(version)
}
