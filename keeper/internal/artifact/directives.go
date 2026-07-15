package artifact

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"

	yaml "gopkg.in/yaml.v3"
)

// essenceDefaultFile — the service essence baseline layer
// (`essence/_default.yaml`), host-agnostic. The directive catalog
// (`redis_directives`) lives here; full host resolution is not needed to
// read it. Parallel to typesCatalogFile.
const essenceDefaultFile = "essence/_default.yaml"

// DirectiveCatalog — a snapshot directive catalog: the SHA1 of the
// materialized snapshot (serves as an ETag, the catalog is immutable at a
// given git ref) + a map of `series (major.minor) → sorted directive names`.
// The result shape of the /directives lister.
type DirectiveCatalog struct {
	SHA1       string
	Directives map[string][]string
}

// LoadDirectiveCatalog reads the service's catalog of valid directive names
// from the `essence/_default.yaml` snapshot (key `redis_directives`, a
// series→[]name map) and, if version is non-empty, narrows it to that
// version's major.minor series (the same logic as the render phase's assert,
// see FilterDirectivesByVersion). serviceRoot — the absolute path to the
// snapshot (ServiceArtifact.LocalDir).
//
// A service without a catalog (no essence/_default.yaml OR no
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
// `essence/_default.yaml`. Missing file/key → an empty non-nil map (soft).
func loadDirectiveCatalogFull(serviceRoot string) (map[string][]string, error) {
	data, err := readSnapshotFile(serviceRoot, essenceDefaultFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string][]string{}, nil
		}
		return nil, err
	}
	// A narrow slice of top-level essence: yaml.Unmarshal ignores the
	// remaining keys.
	var raw struct {
		RedisDirectives map[string][]string `yaml:"redis_directives"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("artifact: парсинг %s: %w", essenceDefaultFile, err)
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
// map). The membership rule mirrors the create/update_config assert (essence
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
