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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/console"
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
	KindFloat    Kind = "float"
	KindInt      Kind = "int"
	KindDuration Kind = "duration"
	KindBool     Kind = "bool"
	KindString   Kind = "string"
)

// Field is one admitted overlay key. The key ↔ yaml-path mapping is explicit
// rather than a mechanical dot→underscore transliteration: flattening is
// ambiguous (`a.b_c` and `a_b.c` both give `a_b_c`).
//
// Min/Max (numbers) and MinDur/MaxDur (durations) are the write-gate range
// bounds (ADR-0073(i)) — mandatory, because a type validator is happy to accept
// `tempo rate=1e9` or `reaper interval=1ms`. They are at least as strict as the
// schema validator of `shared/config`, so PUT and a reload agree on what is
// acceptable. A `bool` field has no range: both values are in-domain.
type Field struct {
	Key      string
	YAMLPath string
	Kind     Kind

	Min          float64
	Max          float64
	MinExclusive bool

	MinDur time.Duration
	MaxDur time.Duration

	// Allowed, when set, is the closed set a string field accepts — the same
	// enum the schema validator enforces, published so the UI renders a select
	// rather than a free-text box.
	Allowed []string

	// Default is the built-in value used when neither Postgres nor the file
	// says anything — the bottom layer of the precedence in ADR-0073(b). An
	// empty string means the field HAS no built-in default: until someone sets
	// it, the feature it configures is simply unavailable (`cloud_init.*`).
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

// fields is the admission set (ADR-0073(j)). Every entry below was verified to
// have a LIVE apply path — a consumer that re-resolves it from the current
// `config.Store` snapshot (a per-tick read, a per-request read or an OnReload
// callback). A key whose consumer reads it once at startup is deliberately NOT
// here: the overlay would accept the edit and silently do nothing until the next
// restart, which is worse than not offering the field at all.
//
// Nothing here is a security gate (ADR-0073(j.2)) and nothing carries a secret —
// [TestRegistry_AdmissionGuard] enforces both against the yaml path, so a
// sensitive value cannot reach the catalog, the events or the logs by
// construction rather than by masking.
var fields = []Field{
	// --- Toll: reinjected into the live leader by applyTollReload -----------
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
		Key:         "cfg_toll_window_size",
		YAMLPath:    "$.toll.window_size",
		Kind:        KindDuration,
		MinDur:      time.Second,
		MaxDur:      time.Hour,
		Default:     formatDuration(config.DefaultTollWindow),
		Description: "Toll: length of the aggregation window over which the disconnect rate is computed.",
		Read: func(c *config.KeeperConfig) any {
			return tollDuration(c, func(t *config.KeeperToll) string { return t.WindowSize }, config.DefaultTollWindow)
		},
	},
	{
		Key:         "cfg_toll_degraded_ttl",
		YAMLPath:    "$.toll.degraded_ttl",
		Kind:        KindDuration,
		MinDur:      time.Second,
		MaxDur:      24 * time.Hour,
		Default:     formatDuration(config.DefaultTollDegradedTTL),
		Description: "Toll: how long the degraded verdict stays in effect before it has to be re-confirmed.",
		Read: func(c *config.KeeperConfig) any {
			return tollDuration(c, func(t *config.KeeperToll) string { return t.DegradedTTL }, config.DefaultTollDegradedTTL)
		},
	},
	{
		Key:         "cfg_toll_clear_grace",
		YAMLPath:    "$.toll.clear_grace",
		Kind:        KindDuration,
		MinDur:      time.Second,
		MaxDur:      time.Hour,
		Default:     formatDuration(config.DefaultTollClearGrace),
		Description: "Toll: quiet period a recovered cluster must hold before the degraded verdict is cleared.",
		Read: func(c *config.KeeperConfig) any {
			return tollDuration(c, func(t *config.KeeperToll) string { return t.ClearGrace }, config.DefaultTollClearGrace)
		},
	},

	// --- Tempo: the limiter re-reads the snapshot on every request ----------
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
	{
		Key:          "cfg_tempo_voyage_preview_rate",
		YAMLPath:     "$.tempo.voyage_preview.rate",
		Kind:         KindFloat,
		Min:          0,
		MinExclusive: true,
		Max:          10000,
		Default:      config.DefaultTempoVoyagePreviewRate,
		Description:  "Tempo: refill rate (tokens/sec, per AID) of the POST /v1/voyages/preview bucket.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil {
				return config.DefaultTempoVoyagePreviewRate
			}
			rate, _ := c.Tempo.ResolvedVoyagePreview()
			return rate
		},
	},
	{
		Key:         "cfg_tempo_voyage_preview_burst",
		YAMLPath:    "$.tempo.voyage_preview.burst",
		Kind:        KindInt,
		Min:         1,
		Max:         10000,
		Default:     config.DefaultTempoVoyagePreviewBurst,
		Description: "Tempo: depth (per AID) of the POST /v1/voyages/preview bucket.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil {
				return config.DefaultTempoVoyagePreviewBurst
			}
			_, burst := c.Tempo.ResolvedVoyagePreview()
			return burst
		},
	},

	// --- Reaper: the Runner resolves these from a fresh snapshot per tick ---
	{
		Key:         "cfg_reaper_interval",
		YAMLPath:    "$.reaper.interval",
		Kind:        KindDuration,
		MinDur:      time.Minute,
		MaxDur:      24 * time.Hour,
		Default:     formatDuration(config.DefaultReaperInterval),
		Description: "Reaper: interval between cleanup passes. The floor is 1m — a sweep is a full table scan, not a poll.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil {
				return formatDuration(config.DefaultReaperInterval)
			}
			return formatDuration(c.Reaper.ResolvedInterval())
		},
	},
	{
		Key:         "cfg_reaper_batch_size",
		YAMLPath:    "$.reaper.batch_size",
		Kind:        KindInt,
		Min:         1,
		Max:         100000,
		Default:     config.DefaultReaperBatchSize,
		Description: "Reaper: maximum rows one rule deletes or updates per pass.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil {
				return config.DefaultReaperBatchSize
			}
			return c.Reaper.ResolvedBatchSize()
		},
	},
	{
		Key:         "cfg_reaper_dry_run",
		YAMLPath:    "$.reaper.dry_run",
		Kind:        KindBool,
		Default:     false,
		Description: "Reaper: report what a pass would delete without mutating anything.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil || c.Reaper == nil {
				return false
			}
			return c.Reaper.DryRun
		},
	},

	// --- Conductor: the poll corridor is re-derived on every tick -----------
	{
		Key:         "cfg_cadence_scheduler_poll_floor",
		YAMLPath:    "$.cadence_scheduler.poll_floor",
		Kind:        KindDuration,
		MinDur:      30 * time.Second,
		MaxDur:      time.Hour,
		Default:     formatDuration(config.DefaultCadenceSchedulerPollFloor),
		Description: "Conductor: lower bound of the adaptive poll step. The absolute floor is 30s — sub-30s cadence is the Beacons domain.",
		Read: func(c *config.KeeperConfig) any {
			return formatDuration(cadenceCfg(c).ResolvedPollFloor())
		},
	},
	{
		Key:         "cfg_cadence_scheduler_poll_ceiling",
		YAMLPath:    "$.cadence_scheduler.poll_ceiling",
		Kind:        KindDuration,
		MinDur:      30 * time.Second,
		MaxDur:      time.Hour,
		Default:     formatDuration(config.DefaultCadenceSchedulerPollCeiling),
		Description: "Conductor: upper bound of the adaptive poll step. Must be >= poll_floor and <= poll_idle.",
		Read: func(c *config.KeeperConfig) any {
			return formatDuration(cadenceCfg(c).ResolvedPollCeiling())
		},
	},
	{
		Key:         "cfg_cadence_scheduler_poll_idle",
		YAMLPath:    "$.cadence_scheduler.poll_idle",
		Kind:        KindDuration,
		MinDur:      30 * time.Second,
		MaxDur:      24 * time.Hour,
		Default:     formatDuration(config.DefaultCadenceSchedulerPollIdle),
		Description: "Conductor: poll step while the enabled Cadence registry is empty. Must be >= poll_ceiling.",
		Read: func(c *config.KeeperConfig) any {
			return formatDuration(cadenceCfg(c).ResolvedPollIdle())
		},
	},
	{
		Key:         "cfg_cadence_scheduler_lock_ttl",
		YAMLPath:    "$.cadence_scheduler.lock_ttl",
		Kind:        KindDuration,
		MinDur:      30 * time.Second,
		MaxDur:      time.Hour,
		Default:     formatDuration(config.DefaultCadenceSchedulerLockTTL),
		Description: "Conductor: TTL of the conductor:leader Redis lease; renewed at a third of it. Applies between re-acquires.",
		Read: func(c *config.KeeperConfig) any {
			return formatDuration(cadenceCfg(c).ResolvedLockTTL())
		},
	},

	// --- Onboarding barrier ceiling: read per scenario step -----------------
	{
		Key:         "cfg_max_await_timeout",
		YAMLPath:    "$.max_await_timeout",
		Kind:        KindDuration,
		MinDur:      time.Minute,
		MaxDur:      24 * time.Hour,
		Default:     formatDuration(config.DefaultMaxAwaitTimeout),
		Description: "Ceiling on await_timeout of the core.soul.registered onboarding barrier; a step asking for more ends failed.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil {
				return formatDuration(config.DefaultMaxAwaitTimeout)
			}
			return formatDuration(c.ResolvedMaxAwaitTimeout())
		},
	},
}

// loggingLevels is the closed enum of `logging.level`, mirroring the schema
// validator of `shared/config`.
var loggingLevels = []string{"debug", "info", "warn", "error"}

// levelAndCloudInit — the two blocks admitted by explicit decision rather than
// by the four rules alone.
//
// `logging.level` has a live apply path (the daemon's OnReload calls
// `logLevel.Set`) and is the single most common cluster-wide operation: turn
// debug on everywhere, look, turn it back off. ADR-0073(b) keeps `logging.*` in
// the file because logging must work BEFORE Postgres — that argument covers
// building the writer (`format`/`file`/`rotation`, which stay file-only), not
// the level of an already-running logger.
//
// `cloud_init.*` is admitted at the user's explicit request, ahead of a redesign
// of the block. It deserves the warning it got: these fields decide where a new
// VM downloads its `soul` binary and which CA it trusts, so `setting.update`
// becomes the right to redirect that download. It is a supply-chain surface
// rather than an operational tunable — see the note in
// [`docs/keeper/config.md`](docs/keeper/config.md).
var levelAndCloudInit = []Field{
	{
		Key:         "cfg_logging_level",
		YAMLPath:    "$.logging.level",
		Kind:        KindString,
		Allowed:     loggingLevels,
		Default:     "info",
		Description: "Log level of every Keeper instance. Applied live — the running logger switches without a restart.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil || c.Logging.Level == "" {
				return "info"
			}
			return c.Logging.Level
		},
	},
	{
		Key:         "cfg_cloud_init_bootstrap_endpoint",
		YAMLPath:    "$.cloud_init.bootstrap_endpoint",
		Kind:        KindString,
		Default:     "",
		Description: "cloud-init: Keeper `host:port` of the Bootstrap listener a freshly created VM calls home to.",
		Read:        cloudInitField(func(ci *config.KeeperCloudInit) any { return ci.BootstrapEndpoint }, ""),
	},
	{
		Key:         "cfg_cloud_init_event_stream_port",
		YAMLPath:    "$.cloud_init.event_stream_port",
		Kind:        KindInt,
		Min:         1,
		Max:         65535,
		Default:     0,
		Description: "cloud-init: port of the EventStream listener written into the userdata (0 — the template default).",
		Read:        cloudInitField(func(ci *config.KeeperCloudInit) any { return ci.EventStreamPort }, 0),
	},
	{
		Key:         "cfg_cloud_init_tls_ca_ref",
		YAMLPath:    "$.cloud_init.tls_ca_ref",
		Kind:        KindString,
		Default:     "",
		Description: "cloud-init: vault-ref (`vault:path#field`) of the Keeper CA a new VM is told to trust. A POINTER into Vault, never the certificate itself.",
		Read:        cloudInitField(func(ci *config.KeeperCloudInit) any { return ci.TLSCARef }, ""),
	},
	{
		Key:         "cfg_cloud_init_soul_binary_url",
		YAMLPath:    "$.cloud_init.soul_binary_url",
		Kind:        KindString,
		Default:     "",
		Description: "cloud-init: HTTPS URL a new VM downloads the `soul` binary from.",
		Read:        cloudInitField(func(ci *config.KeeperCloudInit) any { return ci.SoulBinaryURL }, ""),
	},
	{
		Key:         "cfg_cloud_init_soul_binary_ca",
		YAMLPath:    "$.cloud_init.soul_binary_ca",
		Kind:        KindString,
		Default:     "",
		Description: "cloud-init: CA the VM verifies the binary download against (empty — the system trust store).",
		Read:        cloudInitField(func(ci *config.KeeperCloudInit) any { return ci.SoulBinaryCA }, ""),
	},
	{
		Key:         "cfg_cloud_init_soul_version",
		YAMLPath:    "$.cloud_init.soul_version",
		Kind:        KindString,
		Default:     "",
		Description: "cloud-init: `soul` version recorded in the userdata; empty — whatever the URL serves.",
		Read:        cloudInitField(func(ci *config.KeeperCloudInit) any { return ci.SoulVersion }, ""),
	},

	// --- Console: the interactive plane and its operator envelope (NIM-292) ---
	//
	// `cfg_console_enabled` is admitted AGAINST THE LETTER of (j.2), which keeps
	// security gates out of a fail-soft overlay, and the decision is recorded in
	// ADR-0073 rather than left to be re-derived from this table. Two things make
	// it survivable where `audit.enabled` did not:
	//
	//   - the failure (j.2) guards against cannot occur here. A gate served from
	//     Postgres degrades toward the more permissive file value when Postgres
	//     goes away — but the console plane cannot run without Postgres either:
	//     recording is mandatory (ADR-0074(g)) and lives in `console_recordings`,
	//     so a session opened during the outage is refused anyway;
	//   - the file still outranks the overlay (b). A cluster that must never carry
	//     consoles pins `enabled: false` in its own keeper.yml, where no
	//     `setting.update` can reach it.
	//
	// What is bought with that: `setting.update` becomes the right to switch the
	// plane back ON cluster-wide. That is the real cost of the admission, it is
	// documented in rbac.md, and pinning the key in the file is the answer to it.
	{
		Key:         "cfg_console_enabled",
		YAMLPath:    "$.console.enabled",
		Kind:        KindBool,
		Default:     true,
		Description: "Console: whether this cluster carries an interactive console plane at all. False removes both halves — the GET /v1/console WebSocket and the MCP keeper.soul.run-command tool — and closes the sessions already open. Recorded sessions stay readable.",
		Read: func(c *config.KeeperConfig) any {
			return c.ConsolePlaneEnabled()
		},
	},
	{
		Key:         "cfg_console_max_sessions_per_archon",
		YAMLPath:    "$.console.max_sessions_per_archon",
		Kind:        KindInt,
		Min:         1,
		Max:         1000,
		Default:     console.DefaultMaxSessionsPerAID,
		Description: "Console: live consoles one Archon may hold across all their sockets.",
		Read: func(c *config.KeeperConfig) any {
			return consoleInt(c, func(cc *config.KeeperConsole) int { return cc.MaxSessionsPerArchon },
				console.DefaultMaxSessionsPerAID)
		},
	},
	{
		Key:      "cfg_console_max_sessions_global",
		YAMLPath: "$.console.max_sessions_global",
		Kind:     KindInt,
		// Floor of 1, not 0: in this block `0` means "the default 256", so a
		// range that accepted it would offer an operator a value that reads as
		// "none" and resolves to the maximum. Switching the plane off is
		// `cfg_console_enabled`, which says what it does.
		Min:         1,
		Max:         100000,
		Default:     console.DefaultMaxSessionsGlobal,
		Description: "Console: live consoles on ONE Keeper instance across all operators. Not an off switch — see cfg_console_enabled.",
		Read: func(c *config.KeeperConfig) any {
			return consoleInt(c, func(cc *config.KeeperConsole) int { return cc.MaxSessionsGlobal },
				console.DefaultMaxSessionsGlobal)
		},
	},
	{
		Key:         "cfg_console_idle_timeout",
		YAMLPath:    "$.console.idle_timeout",
		Kind:        KindDuration,
		MinDur:      time.Minute,
		MaxDur:      24 * time.Hour,
		Default:     formatDuration(console.DefaultIdleTimeout),
		Description: "Console: how long a session may go without operator input before it is closed. Output does not count as activity.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil || c.Console == nil || c.Console.IdleTimeout == "" {
				return formatDuration(console.DefaultIdleTimeout)
			}
			d, err := config.ParseDuration(c.Console.IdleTimeout)
			if err != nil || d <= 0 {
				return formatDuration(console.DefaultIdleTimeout)
			}
			return formatDuration(d)
		},
	},
	{
		Key:      "cfg_console_recording_max_session_bytes",
		YAMLPath: "$.console.recording.max_session_bytes",
		Kind:     KindInt,
		// The floor is a real one: reaching the cap CLOSES the session, so a
		// value small enough to be hit by a login banner would make consoles
		// unusable through a setting that reads as a storage knob.
		Min:         1 << 20,
		Max:         1 << 34,
		Default:     int(console.DefaultMaxRecordingBytes),
		Description: "Console: cap on one session's recording. Reaching it closes the session — a console that can no longer be recorded may not keep running.",
		Read: func(c *config.KeeperConfig) any {
			if c == nil || c.Console == nil || c.Console.Recording == nil || c.Console.Recording.MaxSessionBytes == 0 {
				return int(console.DefaultMaxRecordingBytes)
			}
			return int(c.Console.Recording.MaxSessionBytes)
		},
	},
	// `console.recording.retention` is deliberately NOT here. It is stamped into
	// the row when a recording is created, by the store in internal/consolepg
	// which takes it once at construction — so it has no live apply path and
	// (j.5) refuses it. Giving it one is a follow-up, not a line in this table.
	//
	// `console.errand_shell_gate` is also absent, and for the original reason:
	// it is the NIM-197 gate over the Errand path, with its own deprecation
	// window and its own deadline. Nothing about this admission decides it.
}

// consoleInt resolves one optional console ceiling the way the consumer does: an
// absent block or a zero means the built-in default, never zero.
func consoleInt(c *config.KeeperConfig, get func(*config.KeeperConsole) int, def int) int {
	if c == nil || c.Console == nil {
		return def
	}
	if v := get(c.Console); v != 0 {
		return v
	}
	return def
}

// cloudInitField builds a nil-safe Read for one cloud_init field: an absent
// block reads as the zero value, which is exactly what the generator sees.
func cloudInitField(get func(*config.KeeperCloudInit) any, zero any) func(*config.KeeperConfig) any {
	return func(c *config.KeeperConfig) any {
		if c == nil || c.CloudInit == nil {
			return zero
		}
		return get(c.CloudInit)
	}
}

// tollDuration resolves one optional Toll duration exactly like the consumer
// does in applyTollReload: empty/invalid/non-positive → the built-in default.
func tollDuration(c *config.KeeperConfig, get func(*config.KeeperToll) string, def time.Duration) string {
	if c == nil || c.Toll == nil {
		return formatDuration(def)
	}
	raw := get(c.Toll)
	if raw == "" {
		return formatDuration(def)
	}
	d, err := config.ParseDuration(raw)
	if err != nil || d <= 0 {
		return formatDuration(def)
	}
	return formatDuration(d)
}

// cadenceCfg is the nil-safe accessor the Conductor uses (a nil block resolves
// to the defaults, it does not disable the corridor).
func cadenceCfg(c *config.KeeperConfig) *config.KeeperCadenceScheduler {
	if c == nil {
		return nil
	}
	return c.CadenceScheduler
}

// Fields returns the registry in catalog order.
func Fields() []Field {
	out := make([]Field, 0, len(fields)+len(levelAndCloudInit))
	out = append(out, fields...)
	return append(out, levelAndCloudInit...)
}

// Lookup finds a field by its `keeper_settings` key. An unknown key does not
// exist as far as the overlay is concerned — admission is enumerated, not
// pattern-matched.
func Lookup(key string) (Field, bool) {
	for _, f := range Fields() {
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
//
// A duration is returned in its NORMALIZED text form ("60s" → "1m"), which is
// what lands both in `keeper_settings` and in the merged YAML — the config
// fields themselves are `duration` strings.
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
	case KindDuration:
		d, err := config.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a duration (10s, 5m, 2h, 7d)", f.Key, raw)
		}
		if err := f.checkDuration(d); err != nil {
			return nil, err
		}
		return formatDuration(d), nil
	case KindBool:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a boolean (true, false)", f.Key, raw)
		}
		return v, nil
	case KindString:
		// An empty override is not a way to unset a key — DELETE is. Storing ""
		// would merge an empty value over the file one, which reads as
		// "configured to nothing".
		if raw == "" {
			return nil, fmt.Errorf("%s: an empty value is not accepted (use DELETE to drop the override)", f.Key)
		}
		if len(f.Allowed) > 0 && !slices.Contains(f.Allowed, raw) {
			return nil, fmt.Errorf("%s: %q is not one of %s", f.Key, raw, strings.Join(f.Allowed, ", "))
		}
		return raw, nil
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
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprint(v)
	}
}

// Bounds renders the accepted range as operator-facing help, e.g. "(0, 1]" or
// "[30s, 1h]". A boolean has none.
func (f Field) Bounds() string {
	switch f.Kind {
	case KindBool:
		return "true | false"
	case KindDuration:
		return fmt.Sprintf("[%s, %s]", formatDuration(f.MinDur), formatDuration(f.MaxDur))
	case KindString:
		if len(f.Allowed) > 0 {
			return strings.Join(f.Allowed, " | ")
		}
		return "non-empty string"
	default:
		open := "["
		if f.MinExclusive {
			open = "("
		}
		return fmt.Sprintf("%s%s, %s]", open, trimFloat(f.Min), trimFloat(f.Max))
	}
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

func (f Field) checkDuration(d time.Duration) error {
	if d < f.MinDur {
		return fmt.Errorf("%s: must be >= %s, got %s", f.Key, formatDuration(f.MinDur), formatDuration(d))
	}
	if d > f.MaxDur {
		return fmt.Errorf("%s: must be <= %s, got %s", f.Key, formatDuration(f.MaxDur), formatDuration(d))
	}
	return nil
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// formatDuration renders a duration the way `keeper.yml` spells one ("1h", "5m",
// "30s") rather than Go's "1h0m0s": the value round-trips through the config
// parser and reads like the file it overrides.
func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	case d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	default:
		return d.String()
	}
}
