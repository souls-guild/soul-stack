// Package settingsstore is the SettingsStore of
// [ADR-0073](docs/adr/0073-keeper-runtime-config-pg.md): the cluster-wide store
// of Keeper runtime settings in Postgres (the `cfg_*` rows of `keeper_settings`)
// together with its overlay onto the file config.
//
// It owns the field-registry, the write-gate and the read-path snapshot; the
// merge itself is performed by `shared/config` through the injected
// [config.OverlaySource] hook, so no consumer of `config.Store` is rewritten.
//
// It does NOT own the ADR-029 well-known keys (`default_destiny_source`,
// `provisioning_allowed_methods`) — those keep their own consumers, which is
// exactly what the reserved `cfg_` prefix keeps them safe from.
package settingsstore

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/config"
)

// KeyPrefix is the reserved namespace of overlay keys inside `keeper_settings`
// (ADR-0073(e)): `cfg_<flattened yaml path>`.
const KeyPrefix = "cfg_"

// KeyFormat is the CHECK constraint of migration 035 — a key that violates it
// cannot be stored at all.
var KeyFormat = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Kind is the value domain of a field. Scalars only in this phase
// (ADR-0073(j.3)); structural values stay file-only.
type Kind string

const (
	KindFloat Kind = "float"
	KindInt   Kind = "int"
)

// Field is one admitted overlay key. The key ↔ yaml-path mapping is explicit
// rather than a mechanical dot→underscore transliteration: flattening is
// ambiguous (`a.b_c` and `a_b.c` both give `a_b_c`).
//
// Min/Max are the write-gate range bounds (ADR-0073(i)) — mandatory, because a
// type validator is happy to accept `tempo rate=1e9`. They are at least as
// strict as the schema validator of `shared/config`, so PUT and a reload agree
// on what is acceptable.
type Field struct {
	Key      string
	YAMLPath string
	Kind     Kind

	Min          float64
	Max          float64
	MinExclusive bool

	// Default is the built-in value used when neither Postgres nor the file
	// says anything — the bottom layer of the precedence in ADR-0073(b).
	Default any

	// Read resolves the field's EFFECTIVE value out of a loaded config,
	// mirroring how the consumer itself resolves it (an omitted/zero field
	// falls back to Default). Used by the read endpoint and by the guard test
	// that proves the yaml path actually lands in `KeeperConfig`.
	Read func(*config.KeeperConfig) any

	// Description is operator-facing help text; the settings catalog is
	// published to the UI rather than re-implemented there (ADR-042).
	Description string
}

// fields is the pilot admission set (ADR-0073(j)): the parameters that already
// have a live hot-reload consumer, so the overlay is all the new code they need.
// The set is meant to grow — each phase moves more of `keeper.yml` into
// Postgres.
var fields = []Field{
	{
		Key:          "cfg_toll_threshold",
		YAMLPath:     "$.toll.threshold",
		Kind:         KindFloat,
		Min:          0,
		MinExclusive: true,
		Max:          1,
		Default:      config.DefaultTollThreshold,
		Description:  "Toll: disconnect_rate / baseline_connected fraction above which the cluster is declared degraded.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil || c.Toll == nil || c.Toll.Threshold <= 0 {
				return config.DefaultTollThreshold
			}
			return c.Toll.Threshold
		},
	},
	{
		Key:          "cfg_tempo_voyage_create_rate",
		YAMLPath:     "$.tempo.voyage_create.rate",
		Kind:         KindFloat,
		Min:          0,
		MinExclusive: true,
		Max:          10000,
		Default:      config.DefaultTempoVoyageCreateRate,
		Description:  "Tempo: refill rate (tokens/sec, per AID) of the POST /v1/voyages bucket.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil {
				return config.DefaultTempoVoyageCreateRate
			}
			rate, _ := c.Tempo.ResolvedVoyageCreate()
			return rate
		},
	},
	{
		Key:         "cfg_tempo_voyage_create_burst",
		YAMLPath:    "$.tempo.voyage_create.burst",
		Kind:        KindInt,
		Min:         1,
		Max:         10000,
		Default:     config.DefaultTempoVoyageCreateBurst,
		Description: "Tempo: depth (per AID) of the POST /v1/voyages bucket.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil {
				return config.DefaultTempoVoyageCreateBurst
			}
			_, burst := c.Tempo.ResolvedVoyageCreate()
			return burst
		},
	},
}

// Fields returns the registry in catalog order.
func Fields() []Field {
	out := make([]Field, len(fields))
	copy(out, fields)
	return out
}

// Lookup finds a field by its `keeper_settings` key. An unknown key does not
// exist as far as the overlay is concerned — admission is enumerated, not
// pattern-matched.
func Lookup(key string) (Field, bool) {
	for _, f := range fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// Parse converts the stored TEXT into the field's typed value and enforces the
// range bounds. The single gate used by BOTH the write path (before the row is
// written) and the read path (before the snapshot is published), so a value
// that got into Postgres by other means cannot bypass the check.
func (f Field) Parse(raw string) (any, error) {
	switch f.Kind {
	case KindFloat:
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a number", f.Key, raw)
		}
		if err := f.checkRange(v); err != nil {
			return nil, err
		}
		return v, nil
	case KindInt:
		v, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not an integer", f.Key, raw)
		}
		if err := f.checkRange(float64(v)); err != nil {
			return nil, err
		}
		return v, nil
	default:
		return nil, fmt.Errorf("%s: unsupported kind %q", f.Key, f.Kind)
	}
}

// Format renders a typed value back into the TEXT form stored in
// `keeper_settings`.
func (f Field) Format(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int:
		return strconv.Itoa(t)
	default:
		return fmt.Sprint(v)
	}
}

// Bounds renders the range as an operator-facing interval, e.g. "(0, 1]".
func (f Field) Bounds() string {
	open := "["
	if f.MinExclusive {
		open = "("
	}
	return fmt.Sprintf("%s%s, %s]", open, trimFloat(f.Min), trimFloat(f.Max))
}

func (f Field) checkRange(v float64) error {
	if f.MinExclusive && v <= f.Min {
		return fmt.Errorf("%s: must be > %s, got %s", f.Key, trimFloat(f.Min), trimFloat(v))
	}
	if !f.MinExclusive && v < f.Min {
		return fmt.Errorf("%s: must be >= %s, got %s", f.Key, trimFloat(f.Min), trimFloat(v))
	}
	if v > f.Max {
		return fmt.Errorf("%s: must be <= %s, got %s", f.Key, trimFloat(f.Max), trimFloat(v))
	}
	return nil
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
