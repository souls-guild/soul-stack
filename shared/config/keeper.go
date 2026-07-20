package config

import "time"

// ApplyRequest size limits (Keeper↔Soul EventStream contract, ADR-012).
//
// The field is in MiB, symmetric with `logging.rotation.max_size_mb` (one style
// of size fields in the project, no separate human-readable parser). Both sides
// share default and minimum: the Keeper-send limit must be ≤ the Soul-recv
// limit, and the defaults match (8 MiB) so out of the box Keeper never sends
// what Soul would reject.
const (
	// DefaultMaxApplySizeMB is the default for both limits (Keeper-send and
	// Soul-recv) when the field is omitted. 8 MiB > the gRPC recv default
	// (4 MiB), which is too small for a large Destiny.
	DefaultMaxApplySizeMB = 8

	// MinMaxApplySizeMB is the validation floor. Below 1 MiB is rejected: even a
	// modest batch of RenderedTask with inline template content fits in single
	// MiB, whereas a sub-megabyte limit would turn any real Destiny into
	// fail-fast (Keeper) / rejection (Soul).
	MinMaxApplySizeMB = 1

	// bytesPerMiB converts MiB→bytes for grpc call options.
	bytesPerMiB = 1024 * 1024
)

// KeeperConfig is the typed representation of `keeper.yml`.
//
// Normative block spec — [docs/keeper/config.md]. Each block is a separate
// struct below. All `duration` fields are stored as strings and validated in
// the semantic phase (`time.ParseDuration`); enums as strings with an
// allowed-values list in the schema phase.
//
// `reactor:` is deliberately absent — the name is not yet fixed
// ([open Q №23](docs/architecture.md)); the parser must reject the key via
// `unknown_key` in strict mode.
//
// `rbac:` is also absent: the RBAC catalog moved to Postgres (ADR-028(g)
// hard-cut), managed via the `role.*` API/MCP. An `rbac:` key in keeper.yml is
// rejected by the same `unknown_key` (no struct field → the reflect-walker in
// `walk.go` raises a diagnostic).
//
// `services:` / `default_destiny_source:` / `default_module_source:` are also
// absent: the Service registry and well-known scalars moved to Postgres
// (`service_registry` + `keeper_settings`, ADR-029 hard-cut), managed via the
// `service.*` API/MCP. Source of truth is the DB; consumers (scenario service-
// registry / destiny-source) read the runtime snapshot `serviceregistry.Holder`.
// `default_module_source:` was dropped without replacement (no consumer). All
// three keys are rejected by the same `unknown_key`.
type KeeperConfig struct {
	KID string `yaml:"kid"`

	Listen   KeeperListen   `yaml:"listen"`
	Postgres KeeperPostgres `yaml:"postgres"`
	Redis    KeeperRedis    `yaml:"redis"`
	Vault    KeeperVault    `yaml:"vault"`
	Auth     *KeeperAuth    `yaml:"auth,omitempty"`
	OTel     *KeeperOTel    `yaml:"otel,omitempty"`
	Logging  KeeperLogging  `yaml:"logging"`

	Metrics *KeeperMetrics `yaml:"metrics,omitempty"`

	Plugins       *KeeperPlugins `yaml:"plugins,omitempty"`
	PluginRuntime *PluginRuntime `yaml:"plugin_runtime,omitempty"`
	Sigil         *KeeperSigil   `yaml:"sigil,omitempty"`
	Audit         *KeeperAudit   `yaml:"audit,omitempty"`

	// SecretIngest is dual-mode ingest of operator plaintext secrets (ADR-064,
	// NIM-11). Optional; absent/nil == secure default (plaintext forbidden, only
	// *_ref accepted). See [KeeperSecretIngest].
	SecretIngest *KeeperSecretIngest `yaml:"secret_ingest,omitempty"`

	// Push is the pilot wire-up of SshDispatcher (S6, 2026-05-26). Pilot path:
	// inline `targets[]` + `providers[]` + single `host_ca_ref` in keeper.yml,
	// single-provider routing. Long-term canon (S7): migration to
	// souls.ssh_target jsonb + push_providers PG table + push.host_ca_refs[] —
	// a separate slice, after which this block is deprecated. Optional: without
	// the block (or with empty targets/host_ca_ref) the push orchestrator does
	// not start — `/v1/push/*` and `keeper.push.apply` return "not configured".
	Push *KeeperPush `yaml:"push,omitempty"`

	// SigilAnchorsReloadInterval is the TTL-fallback re-read period for the set
	// of Sigil signing trust-anchor keys (ADR-026(h), R3 known-gap). The
	// `sigil:anchors-changed` channel (Redis pub/sub) is best-effort: a missed
	// signal would leave a lagging node with the old anchor set until restart
	// (fail-open on Retire). A periodic re-read (`reloadAnchors` on a ticker,
	// modeled on rbac.DefaultRefreshInterval / Summons poll-fallback) self-heals
	// a missed signal within the interval. Type `duration`, empty/0 → default
	// [DefaultSigilAnchorsReloadInterval] (30s). Format validation — semantic
	// phase; range (>0) — via default resolution in the daemon (style `acolyte_*`).
	SigilAnchorsReloadInterval string        `yaml:"sigil_anchors_reload_interval,omitempty"`
	HotReload                  *HotReload    `yaml:"hot_reload,omitempty"`
	Reaper                     *KeeperReaper `yaml:"reaper,omitempty"`

	// CadenceScheduler is the Conductor, the leader-elected executor of Cadence
	// schedules (ADR-048). Its own tick interval, independent of reaper.interval
	// (Cadence's scheduling domain needs a frequent tick ~15–30s, Reaper's
	// cleanup domain a rare ~1h). Default-ON when Redis is present (footgun-guard
	// ADR-048 §5: Cadence without a working scheduler silently never spawns
	// Voyage). Optional: without the block Conductor starts with defaults if
	// Redis is configured.
	CadenceScheduler *KeeperCadenceScheduler `yaml:"cadence_scheduler,omitempty"`

	// Acolytes is the number of workers in the apply execution pool (ADR-027,
	// Acolyte). Feature flag: 0 (default) — pool does not start, execution runs
	// the old scenario-runner run-goroutine path; >0 — pool active. Cutover to
	// the pool is a separate slice (Phase 1.4). Validation (>= 0) — schema phase.
	Acolytes int `yaml:"acolytes,omitempty"`

	// AcolyteLease is the TTL of the Ward claim on a planned job (ADR-027(d):
	// claim_expires_at = NOW()+lease). An expired Ward is re-claimed by the
	// recovery scan (Phase 2). Type `duration` (Go `time.ParseDuration` or
	// `<N>d`), empty → default [DefaultAcolyteLease] (30s). Format validation —
	// semantic phase; range (>0) — after parsing in the daemon.
	AcolyteLease string `yaml:"acolyte_lease,omitempty"`

	// AcolyteBatch is the max number of planned jobs claimed in one claim tick
	// (claim-query LIMIT, ADR-027(d)). Workers of different instances share the
	// queue via FOR UPDATE SKIP LOCKED — the batch only caps one tick's appetite.
	// 0/omitted → default [DefaultAcolyteBatch] (10). Validation (>= 0) — schema
	// phase.
	AcolyteBatch int `yaml:"acolyte_batch,omitempty"`

	// AcolytePollInterval is the worker's poll-tick period: fallback to the
	// Summons signal (ADR-027(a)). Even if the pub/sub signal is lost the job is
	// picked up on the next tick. Type `duration`, empty → default
	// [DefaultAcolytePollInterval] (2s). Format validation — semantic phase;
	// range (>0) — after parsing.
	AcolytePollInterval string `yaml:"acolyte_poll_interval,omitempty"`

	// AcolyteDrainGrace is the graceful-drain window of the Acolyte pool on
	// Keeper stop (ADR-027 Phase 2): from the "stop claiming" signal to hard
	// cancellation of the claim ctx for in-flight workers that did not finish. An
	// interrupted claim leaves a Ward in the DB (claimed/running) — picked up by
	// the recovery scan. Type `duration`, empty → default
	// [DefaultAcolyteDrainGrace] (5s). Format validation — semantic phase; range
	// (>0) — after parsing in the daemon.
	AcolyteDrainGrace string `yaml:"acolyte_drain_grace,omitempty"`

	// OracleCircuitMaxFires is the Oracle circuit-breaker threshold (ADR-030(a),
	// beacons S4): how many fires of one Decree within the [OracleCircuitWindow]
	// are allowed before auto-disable (enabled=false). Global threshold across
	// all Decrees; per-Decree override is a separate pass.
	//
	// Distinguishing "empty" from "explicit 0" requires a pointer: `nil` (field
	// omitted) → default [DefaultOracleCircuitMaxFires] (5), resolved in the
	// daemon; explicit `0` (escape-hatch) → breaker OFF (BumpCircuit not called,
	// Decree never auto-disabled). A flat `int` could not tell these two apart
	// (both = 0), while the spec requires "empty=default, 0=off" — hence `*int`
	// (the overlay-`*int` pattern, as MaxAgeDays had before flattening, but here
	// the "0 vs unset" distinction is deliberately KEPT). Validation (>= 0 when
	// set) — schema phase (nil-safe).
	OracleCircuitMaxFires *int `yaml:"oracle_circuit_max_fires,omitempty"`

	// OracleCircuitWindow is the fixed-window length of the Oracle circuit
	// breaker (ADR-030(a)): the window in which Decree fires are counted before
	// comparison with [OracleCircuitMaxFires]. Type `duration`, empty → default
	// [DefaultOracleCircuitWindow] (10m). Format validation — semantic phase;
	// range (>0) — via default resolution in the daemon (style `acolyte_*`).
	OracleCircuitWindow string `yaml:"oracle_circuit_window,omitempty"`

	// WatchmanInterval is the Watchman probe-tick period (isolation detection +
	// soul-shedding S2): how often the instance pings PG+Redis (the same
	// dependencies as `/readyz`) to check for isolation. Type `duration`, empty →
	// default [DefaultWatchmanInterval] (5s). Format validation — semantic phase;
	// range (>0) resolved by default in the daemon (style `acolyte_*` /
	// `oracle_circuit_window`).
	WatchmanInterval string `yaml:"watchman_interval,omitempty"`

	// WatchmanFailThreshold is the number of consecutive Watchman probe failures
	// before declaring isolation and actively closing (shedding) all local
	// EventStream streams. Debounce/flap-guard: a single network spike does not
	// drop the whole fleet of streams (thundering-herd reconnect across the
	// cluster). 0/omitted → default [DefaultWatchmanFailThreshold] (3).
	// Validation (>= 0) — schema phase (symmetric with `acolyte_batch`).
	WatchmanFailThreshold int `yaml:"watchman_fail_threshold,omitempty"`

	// AllowUnsafeSinglePathMultiKeeper is the explicit opt-out from the
	// soul-shedding refuse-guard (Finding-A, ADR-027(h)): when `acolytes == 0`
	// AND OTHER live Keeper instances are present in the Conclave
	// (`CountLive > 1`), Keeper REFUSES to start by default — the run-goroutine
	// path (`acolytes: 0`) is single-keeper-only, otherwise an apply on Keeper-A
	// for a Soul on Keeper-B's stream hangs forever in `applying` (see the HA
	// invariant in docs/keeper/config.md).
	//
	// `true` is a deliberate operator choice (e.g. an intentional
	// single-keeper-behind-LB during migration / rolling restart, where the
	// "other" instance is the one leaving): refuse becomes a loud WARN and start
	// proceeds. Default `false` (safe: refuse). Mirrored by the env flag
	// `KEEPER_ALLOW_UNSAFE_MULTI_KEEPER` (truthy-OR, for container/CI
	// environments), resolved in the daemon.
	//
	// Any `bool` value is valid — there is no schema range check; the field just
	// has to exist in the struct so the strict-walker does not reject the key as
	// `unknown_key`.
	AllowUnsafeSinglePathMultiKeeper bool `yaml:"allow_unsafe_single_path_multi_keeper,omitempty"`

	// Toll is the cluster-wide detector of mass Soul churn (ADR-038). With a
	// nil/empty block it starts with defaults (enabled): per-instance tollwatcher
	// + Redis-leader aggregator. `enabled: false` is an explicit opt-out (dev
	// builds without Redis / debugging); all other fields default from the
	// [DefaultToll*] constants. Toll works ONLY with a non-nil Redis — in
	// single-instance/dev without Redis it degrades to a no-op (gated in the
	// daemon, not in config).
	Toll *KeeperToll `yaml:"toll,omitempty"`

	// Voyage holds the VoyageWorker pool parameters (ADR-043): claim+execute of
	// unified batch runs (kind=scenario|command). A separate worker pool (NOT
	// shared with Acolyte).
	//
	// Config-gated OFF BY DEFAULT: with a nil/omitted block the pool does NOT
	// start; the worker starts ONLY on an explicit `voyage.workers: N > 0`. Other
	// fields default from the [DefaultVoyage*] constants.
	Voyage *KeeperVoyage `yaml:"voyage,omitempty"`

	// Tempo is the per-AID rate limiter for resolver-heavy write endpoints
	// (ADR-050). With nil → defaults + enabled=true (starts WHEN Redis is
	// present; without Redis limiter=nil → middleware passthrough, gated in the
	// daemon). Explicit `enabled: false` — opt-out (dev/debugging). The
	// `voyage_create.{rate,burst}` fields default from the [DefaultTempo*]
	// constants; hot-reloadable (atomic swap, new limit from the next request,
	// [ADR-021]).
	Tempo *KeeperTempo `yaml:"tempo,omitempty"`

	// Herald holds the claim-queue notification-delivery worker parameters
	// (ADR-052(d), S3). With nil → defaults + starts WHEN Redis is present
	// (delivery lives in a Redis queue, hot→Redis); without Redis delivery
	// degrades (jobs are dropped, keeper stays up — fail-open). Workers default
	// to [DefaultHeraldWorkers].
	Herald *KeeperHerald `yaml:"herald,omitempty"`

	// WebUIEnabled toggles the built-in UI on the `/ui` route (ADR-055). `*bool`
	// to distinguish "unset" from explicit `false`: nil/omitted → default-ON
	// (true) — the beta wants a single-binary UI out of the box; explicit
	// `false` → opt-out (the `/ui` static assets are NOT mounted, the `/v1` API
	// is unaffected). Symmetric with the footgun-guard of neighboring subsystems
	// (`tempo.enabled`/Toll default-ON), but WITHOUT an infrastructure
	// dependency: the UI is embedded in the binary (go:embed) and needs no
	// external backend. Hot-reloadable (ADR-021) — re-mount of the router.
	// Resolved by [KeeperConfig.WebUIEnabled].
	WebUIEnabled *bool `yaml:"web_ui_enabled,omitempty"`

	// CloudInit holds the cloud-init userdata render parameters for VMs created
	// by `core.cloud.provisioned` (ADR-017(h) amendment 2026-05-27, B-flat
	// locked). With nil, userdata generation is unavailable: a scenario with
	// `generate_userdata: true` fails with an explicit error; an explicit
	// `userdata` in params keeps working unchanged.
	//
	// All fields resolve at the GenerateUserdata call (not at daemon start), so a
	// `keeper.yml` hot-reload is picked up by the next cloud-create step without
	// restart — via `config.Store.Get()`.
	//
	// Userdata does NOT carry bootstrap tokens: the per-VM token is generated
	// after Create in `applyCreated` and lands in register-output for delivery by
	// a separate scenario step (typically `keeper.push` via an SSH provider).
	// See the ADR-017(h) amendment and docs/keeper/cloud.md → "Cloud-init
	// bootstrap (MVP)".
	CloudInit *KeeperCloudInit `yaml:"cloud_init,omitempty"`

	// MaxAwaitTimeout is the operator ceiling on `await_timeout` of the
	// `core.soul.registered` onboarding barrier (ADR-061). A step with
	// `await_online: true` blocks waiting for presence (Redis SID-lease) up to
	// `await_min_count`/timeout; without a ceiling a malicious/mistaken
	// `await_timeout: 100h` would keep a run-goroutine/Acolyte worker busy (DoS).
	// Fail-closed: a step with `await_timeout` > ceiling ends `failed` (explicit
	// error, NOT silent truncation). Type `duration` (Go `time.ParseDuration` or
	// `<N>d`), empty → default [DefaultMaxAwaitTimeout] (30m). Format validation —
	// semantic phase; range (>0) resolved by default in
	// [KeeperConfig.ResolvedMaxAwaitTimeout] (style `acolyte_*`).
	MaxAwaitTimeout string `yaml:"max_await_timeout,omitempty"`
}

// DefaultMaxAwaitTimeout is the default ceiling on `await_timeout` of the
// `core.soul.registered` onboarding barrier (ADR-061). 30m: a cloud VM usually
// onboards in single minutes (boot + cloud-init + CSR-bootstrap); 30m is a
// generous margin for a slow provider/large batch, but not "forever".
const DefaultMaxAwaitTimeout = 30 * time.Minute

// ResolvedMaxAwaitTimeout returns the effective `await_timeout` ceiling:
// empty/0/invalid → [DefaultMaxAwaitTimeout] (30m); otherwise the parsed value.
// Format is already checked in the semantic phase; here the range (>0) is
// resolved, like the other duration ceilings (style `acolyte_*`).
func (c *KeeperConfig) ResolvedMaxAwaitTimeout() time.Duration {
	if c == nil || c.MaxAwaitTimeout == "" {
		return DefaultMaxAwaitTimeout
	}
	d, err := ParseDuration(c.MaxAwaitTimeout)
	if err != nil || d <= 0 {
		return DefaultMaxAwaitTimeout
	}
	return d
}

// WebUIMounted returns the effective built-in UI toggle (`/ui`, ADR-055):
// nil/omitted → true (default-ON, footgun-guard like Tempo/Toll); explicit
// `false` → false (opt-out). A pure pointer resolve; the UI is embedded in the
// binary and needs no external backend (unlike Tempo/Toll, which need Redis).
// The name differs from the WebUIEnabled field (Go forbids a method and a field
// with the same name).
func (c *KeeperConfig) WebUIMounted() bool {
	if c == nil || c.WebUIEnabled == nil {
		return true
	}
	return *c.WebUIEnabled
}

// VoyageWorker pool defaults (ADR-043). Applied in the daemon for an empty
// field (once the voyage block is present and workers > 0). Semantics source of
// truth is the voyageorch package; declared here for config resolution without
// a shared→keeper import cycle. IMPORTANT: there is NO `workers` default —
// absence of the block means "pool OFF", explicit opt-in via `voyage.workers: N`.
const (
	// DefaultVoyageLeaseTTL is the default TTL of the PG claim-lease for a row in
	// `voyages` (claim_expires_at = NOW() + lease_ttl). 60s parity with ErrandRun.
	DefaultVoyageLeaseTTL = 60 * time.Second

	// DefaultVoyageLeaseRenewInterval is the default renewal-CAS-UPDATE period
	// (~1/3 TTL). 20s parity with ErrandRun.
	DefaultVoyageLeaseRenewInterval = 20 * time.Second

	// DefaultVoyagePollInterval is the default idle-poll period of the claim
	// loop. 5s parity with ErrandRun.
	DefaultVoyagePollInterval = 5 * time.Second

	// DefaultVoyageMaxScope is the default upper limit on the resolved scope size
	// of one Voyage (run units: incarnations for kind=scenario, hosts for
	// kind=command). DoS-guard S-med-3: without a ceiling one POST could resolve
	// the whole fleet (100k) → 100k per-row INSERT in one transaction +
	// uncontrolled blast radius. 10000 is an architect recommendation adopted as
	// the default; the operator overrides in keeper.yml::voyage.max_scope.
	DefaultVoyageMaxScope = 10000

	// DefaultVoyageMaxBatchSize is the default upper limit on the batch/window
	// size of one Voyage (ADR-043 amendment 2026-06-01, S-W4): batch_size for a
	// barrier, concurrency for a window. DoS-guard parity with voyage.max_scope —
	// without a ceiling the operator could set a giant batch/window and defeat
	// the point of a rolling rollout (whole scope in one wave). Equal to
	// [DefaultVoyageMaxScope] (10000): a batch cannot exceed the whole scope
	// ceiling; the operator overrides in keeper.yml::voyage.max_batch_size.
	DefaultVoyageMaxBatchSize = 10000
)

// KeeperVoyage holds the VoyageWorker pool parameters (ADR-043, S1).
//
// Optional block. UNLIKE ErrandRun: with a nil/omitted block the pool does NOT
// start (config-gated OFF by default — the S1 foundation alongside the old
// paths). The pool starts ONLY on an explicit `voyage.workers: N > 0`. When the
// block is present, the other fields resolve to the [DefaultVoyage*] constants.
type KeeperVoyage struct {
	// Workers is the number of VoyageWorker pool workers per instance. 0/omitted
	// → pool does NOT start (even if the voyage block is set). Explicit `N > 0`
	// starts N workers (config-gated OFF by default).
	Workers int `yaml:"workers,omitempty" json:"workers,omitempty"`

	// LeaseTTL is the TTL of the PG claim-lease for a row in `voyages`. Type
	// `duration`, empty → default [DefaultVoyageLeaseTTL] (60s).
	LeaseTTL string `yaml:"lease_ttl,omitempty" json:"lease_ttl,omitempty"`

	// LeaseRenewInterval is the renewal-CAS-UPDATE period for the current lease.
	// 0 rows affected → ErrLeaseLost, the VoyageWorker drops the work. Type
	// `duration`, empty → default [DefaultVoyageLeaseRenewInterval]
	// (20s = ~1/3 LeaseTTL).
	LeaseRenewInterval string `yaml:"lease_renew_interval,omitempty" json:"lease_renew_interval,omitempty"`

	// PollInterval is the idle-poll period of the claim loop (when there are no
	// pending Voyages). Type `duration`, empty → default
	// [DefaultVoyagePollInterval] (5s).
	PollInterval string `yaml:"poll_interval,omitempty" json:"poll_interval,omitempty"`

	// MaxScope is the upper limit on the resolved scope size of one Voyage
	// (DoS-guard S-med-3). `*int` to distinguish "unset" (→ default
	// [DefaultVoyageMaxScope] = 10000) from explicit 0 ("unlimited" — for tests /
	// backward compatibility). Exceeding the limit at target resolution →
	// 422 voyage_scope_too_large (handler invariant, not a CHECK). Unlike the
	// other fields of the block, MaxScope acts INDEPENDENTLY of Workers: the cap
	// lives in the API handler (POST /v1/voyages), not the pool, so it protects
	// even with `workers: 0`.
	MaxScope *int `yaml:"max_scope,omitempty" json:"max_scope,omitempty"`

	// MaxBatchSize is the upper limit on the batch/window size of one Voyage
	// (ADR-043 amendment 2026-06-01, S-W4): effective batch_size for a barrier,
	// concurrency for a window. `*int` to distinguish "unset" (→ default
	// [DefaultVoyageMaxBatchSize] = 10000) from explicit 0 ("no limit").
	// Exceeding → 422 voyage_batch_size_too_large (handler invariant, parity with
	// voyage_scope_too_large). Symmetric with MaxScope, acts INDEPENDENTLY of
	// Workers (the cap lives in the API handler).
	MaxBatchSize *int `yaml:"max_batch_size,omitempty" json:"max_batch_size,omitempty"`
}

// ResolvedMaxScope returns the effective scope-size ceiling: unset (nil block /
// nil field) → [DefaultVoyageMaxScope] (10000); explicit 0 → 0 (unlimited, for
// tests / backward compatibility); an explicit value → itself.
func (v *KeeperVoyage) ResolvedMaxScope() int {
	if v == nil || v.MaxScope == nil {
		return DefaultVoyageMaxScope
	}
	return *v.MaxScope
}

// ResolvedMaxBatchSize returns the effective batch/window-size ceiling: unset
// (nil block / nil field) → [DefaultVoyageMaxBatchSize] (10000); explicit 0 → 0
// (no limit); an explicit value → itself. Parity with [ResolvedMaxScope].
func (v *KeeperVoyage) ResolvedMaxBatchSize() int {
	if v == nil || v.MaxBatchSize == nil {
		return DefaultVoyageMaxBatchSize
	}
	return *v.MaxBatchSize
}

// Herald claim-queue delivery worker defaults (ADR-052(d), S3).
const (
	// DefaultHeraldWorkers is the number of delivery worker goroutines per
	// instance when Redis is present. Concurrent claims are safe (at-least-once).
	// 2 is moderate parallelism without a storm: notifications are few relative
	// to runs.
	DefaultHeraldWorkers = 2

	// DefaultHeraldDeliveryTimeout is the default overall timeout of one webhook
	// POST (dial+TLS+POST+response read). Mirror of [herald.DefaultDeliveryTimeout]
	// (10s) — kept as a string here so the daemon resolves it uniformly with the
	// other duration fields (ParseDuration).
	DefaultHeraldDeliveryTimeout = "10s"
)

// KeeperHerald holds the claim-queue notification-delivery worker parameters
// (ADR-052(d), S3). Optional block: with nil the fields default and the worker
// starts WHEN Redis is present. `workers: 0` explicitly disables delivery (jobs
// accumulate in the Redis queue but are not delivered).
type KeeperHerald struct {
	// Workers is the number of delivery worker goroutines per instance.
	// nil/omitted → [DefaultHeraldWorkers] (2). Explicit 0 → delivery disabled.
	Workers *int `yaml:"workers,omitempty" json:"workers,omitempty"`

	// DeliveryTimeout is the overall timeout of one webhook POST. Type
	// `duration`, empty/invalid → [DefaultHeraldDeliveryTimeout] (10s).
	DeliveryTimeout string `yaml:"delivery_timeout,omitempty" json:"delivery_timeout,omitempty"`
}

// ResolvedWorkers returns the effective worker count: nil block / nil field →
// [DefaultHeraldWorkers]; an explicit value (incl. 0) → itself.
func (h *KeeperHerald) ResolvedWorkers() int {
	if h == nil || h.Workers == nil {
		return DefaultHeraldWorkers
	}
	return *h.Workers
}

// Tempo per-AID rate-limiter defaults (ADR-050(e)). Applied for an
// omitted/zero field when the config snapshot is read (middleware reads the live
// config.Store, hot-reload). Chosen so "a human/normal automaton has ample
// headroom, loop-abuse is cut off".
const (
	// DefaultTempoVoyageCreateRate is the refill rate of the voyage-create
	// bucket, tokens per second (rps).
	DefaultTempoVoyageCreateRate = 10.0

	// DefaultTempoVoyageCreateBurst is the depth of the voyage-create bucket.
	DefaultTempoVoyageCreateBurst = 20

	// DefaultTempoVoyagePreviewRate is the refill rate of the voyage-preview
	// bucket, tokens per second (rps). Softer than create (preview is read-like
	// in effect — no persist/audit) but NOT unlimited: dry-resolve of scope is
	// just as resolver-heavy (Purview resolution + page-CEL over the fleet), so
	// loop-abuse is cut off by a separate, wider bucket (ADR-050 amendment
	// 2026-06-17).
	DefaultTempoVoyagePreviewRate = 30.0

	// DefaultTempoVoyagePreviewBurst is the depth of the voyage-preview bucket.
	DefaultTempoVoyagePreviewBurst = 60
)

// KeeperTempo is the Tempo per-AID rate limiter (ADR-050).
//
// Optional block. With nil → defaults + enabled=true (default-ON, footgun-guard
// like Conductor/Toll). Explicit `enabled: false` — opt-out. Actually starts
// ONLY with a Redis client (the limiter lives in Redis); without Redis —
// middleware passthrough (gated in the daemon, not in config). The
// `voyage_create.{rate,burst}` / `voyage_preview.{rate,burst}` fields resolve to
// [DefaultTempo*] when omitted.
type KeeperTempo struct {
	// Enabled toggles Tempo. nil/omitted → true (default-ON); explicit `false` —
	// operator disables (dev/debugging). `*bool` to distinguish "unset"
	// (→ default-on) from explicit `false` (KeeperToll.Enabled pattern).
	Enabled *bool `yaml:"enabled,omitempty"`

	// VoyageCreate is the rate/burst of the `POST /v1/voyages` (create) bucket.
	// Omitted fields resolve to [DefaultTempoVoyageCreate*] in
	// [ResolvedVoyageCreate].
	VoyageCreate KeeperTempoBucket `yaml:"voyage_create,omitempty"`

	// VoyagePreview is the rate/burst of the `POST /v1/voyages/preview`
	// (dry-resolve scope) bucket. A SEPARATE bucket from voyage_create (ADR-050
	// amendment 2026-06-17): preview is read-like in effect but resolver-heavy in
	// cost → its own, softer limit so preview and create do not share a quota.
	// Omitted fields resolve to [DefaultTempoVoyagePreview*] in
	// [ResolvedVoyagePreview].
	VoyagePreview KeeperTempoBucket `yaml:"voyage_preview,omitempty"`
}

// KeeperTempoBucket is the rate/burst of one logical Tempo bucket.
//
// Rate — refill rate (tokens per second, rps). Burst — bucket depth (capacity).
// Both must be > 0 when the block is set (validated in the schema phase);
// 0/omitted resolves to the default in [KeeperTempo.ResolvedVoyageCreate].
type KeeperTempoBucket struct {
	Rate  float64 `yaml:"rate,omitempty"`
	Burst int     `yaml:"burst,omitempty"`
}

// TempoEnabled returns the effective Tempo toggle: nil block / nil field → true
// (default-ON, footgun-guard). Actually starting additionally requires Redis
// (gated in the daemon).
func (t *KeeperTempo) TempoEnabled() bool {
	if t == nil || t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// ResolvedVoyageCreate returns the effective rate/burst of the voyage-create
// bucket: omitted/zero fields → defaults [DefaultTempoVoyageCreate*]. Read by
// the middleware on every request (hot-reload), hence a cheap allocation-free
// pure resolve.
func (t *KeeperTempo) ResolvedVoyageCreate() (rate float64, burst int) {
	rate, burst = DefaultTempoVoyageCreateRate, DefaultTempoVoyageCreateBurst
	if t == nil {
		return rate, burst
	}
	if t.VoyageCreate.Rate > 0 {
		rate = t.VoyageCreate.Rate
	}
	if t.VoyageCreate.Burst > 0 {
		burst = t.VoyageCreate.Burst
	}
	return rate, burst
}

// ResolvedVoyagePreview returns the effective rate/burst of the voyage-preview
// bucket: omitted/zero fields → defaults [DefaultTempoVoyagePreview*]. A
// separate bucket from voyage-create (ADR-050 amendment 2026-06-17). Read by the
// middleware on every request (hot-reload), hence a cheap allocation-free pure
// resolve.
func (t *KeeperTempo) ResolvedVoyagePreview() (rate float64, burst int) {
	rate, burst = DefaultTempoVoyagePreviewRate, DefaultTempoVoyagePreviewBurst
	if t == nil {
		return rate, burst
	}
	if t.VoyagePreview.Rate > 0 {
		rate = t.VoyagePreview.Rate
	}
	if t.VoyagePreview.Burst > 0 {
		burst = t.VoyagePreview.Burst
	}
	return rate, burst
}

// Acolyte pool defaults (ADR-027). Equal to the former hardcoded values (main
// constants / acolyte.defaultPollInterval) so an empty config does not change
// behavior. Applied in the daemon for an empty/zero field.
const (
	// DefaultAcolyteLease is the default TTL of the Ward claim (claim_expires_at).
	// A moderate window: enough for render→MarkDispatched→SendApply, and short
	// enough for the recovery scan to quickly return a dead owner's job to the
	// queue.
	DefaultAcolyteLease = 30 * time.Second

	// DefaultAcolyteBatch is the default batch size of one claim tick.
	DefaultAcolyteBatch = 10

	// DefaultAcolytePollInterval is the default worker poll-fallback period.
	// Frequent enough for failover in single seconds, rare enough not to flood
	// PG with empty claim queries on an idle cluster.
	DefaultAcolytePollInterval = 2 * time.Second

	// DefaultAcolyteDrainGrace is the default graceful-drain window of the
	// Acolyte pool on stop (ADR-027 Phase 2). Enough for an already-started claim
	// (render → MarkDispatched → SendApply) to finish on a healthy PG/Soul, and
	// not so large as to slow the SIGTERM exit on a stuck in-flight (whose Ward
	// survives the restart — the lease expires and the recovery scan picks it up).
	DefaultAcolyteDrainGrace = 5 * time.Second
)

// Oracle circuit-breaker defaults (ADR-030(a), beacons S4). Applied in the
// daemon for an empty (omitted) field; an explicit 0 in max_fires is NOT a
// default but the "breaker OFF" escape-hatch (see
// [KeeperConfig.OracleCircuitMaxFires]).
const (
	// DefaultOracleCircuitMaxFires is the default Decree auto-disable threshold.
	// 5 fires per window: a rule with an idempotent action + cooldown normally
	// does not reach 5 repeats; a steady climb to the threshold means the rule
	// broke into a loop and should be silenced.
	DefaultOracleCircuitMaxFires = 5

	// DefaultOracleCircuitWindow is the default fixed-window length. 10m is wide
	// enough for a "rule in a loop" burst of repeats to fit one window (the
	// cooldown between them is usually minutes), and narrow enough that rare
	// legitimate fires across a day do not accumulate into a false trip.
	DefaultOracleCircuitWindow = 10 * time.Minute
)

// Watchman defaults (isolation detection + soul-shedding S2). Applied in the
// daemon for an empty/zero field. Equal to the watchman.Default* package
// constants (defaults' source of truth is the watchman package, duplicated here
// for config resolution without a shared→keeper import cycle).
const (
	// DefaultWatchmanInterval is the default Watchman probe-tick period. 5s:
	// a balance between reaction speed to isolation (≈ interval × fail_threshold
	// until shedding) and the load of pings on PG/Redis.
	DefaultWatchmanInterval = 5 * time.Second

	// DefaultWatchmanFailThreshold is the default number of consecutive probe
	// failures before shedding. 3: debounce against single spikes (one or two
	// ticks are survived), a steady loss (>=3) triggers.
	DefaultWatchmanFailThreshold = 3
)

// Toll defaults (cluster-wide mass-churn detector, ADR-038). Applied in the
// daemon for an empty/zero field. Source of truth is the toll package,
// duplicated here for config resolution without a shared→keeper import cycle.
const (
	// DefaultTollThreshold is the fraction of the `souls.status='connected'`
	// baseline above which, within a window, Toll raises cluster:degraded.
	// 0.20 = 20% (a moderate threshold: unresponsive to single disconnects in a
	// small cluster, but catches a DC outage / massive split).
	DefaultTollThreshold = 0.20

	// DefaultTollWindow is the Toll sliding-window length. 60s balances reaction
	// speed to mass churn against resilience to bursts (a rolling restart of one
	// batch of Souls fits in the window).
	DefaultTollWindow = 60 * time.Second

	// DefaultTollDegradedTTL is the TTL of the `cluster:degraded` Redis key.
	// Equal to the window length: if the leader dies and fails to renew, the flag
	// clears itself and the lock is released.
	DefaultTollDegradedTTL = 60 * time.Second

	// DefaultTollClearGrace is the sustained low-rate window before clearing
	// (asymmetric hysteresis per ADR-038): fire on the first excess, clear only
	// after a grace below the threshold — flap protection.
	DefaultTollClearGrace = 60 * time.Second

	// DefaultTollLeaseTTL is the TTL of the `cluster:toll:leader` Redis lease.
	// 30s (symmetric with Reaper's): the leader renews every TTL/3, and on crash
	// the next candidate takes over within ≤ TTL.
	DefaultTollLeaseTTL = 30 * time.Second

	// DefaultTollWarmup is the immunity window after instance start. Disconnects
	// in the first 60s after tollwatcher start are counted (the metric grows) but
	// NOT published to the Redis sorted-set (cluster-restart false-positive
	// defense — all Souls reconnect at once, which is not churn).
	DefaultTollWarmup = 60 * time.Second

	// DefaultTollWebhookTimeout is the ceiling on one webhook-channel POST call
	// (ADR-038 amendment 2026-05-27, extensions). 10s is enough for the PagerDuty
	// Events API / Slack incoming webhook (typical latency is hundreds of ms),
	// short enough that a hung remote does not delay the leader tick. The webhook
	// is best-effort: a timeout on the alert channel does not block Set/Clear.
	DefaultTollWebhookTimeout = 10 * time.Second
)

// Allowed webhook-channel formats (ADR-038 amendment, extensions).
const (
	// TollWebhookFormatGeneric is a generic JSON POST with a flat payload
	// `{event_type, leader_kid, rate, baseline_connected, threshold,
	// window_seconds, timestamp, coven_name?}`. Fits an arbitrary HTTP receiver
	// (including self-hosted alertmanager relays).
	TollWebhookFormatGeneric = "generic"

	// TollWebhookFormatPagerDutyV2 is the PagerDuty Events API v2 schema:
	// `{routing_key, event_action, dedup_key, payload:{summary, source,
	// severity, custom_details}}`. The URL must be
	// `https://events.pagerduty.com/v2/enqueue` (or an integration equivalent),
	// `routing_key` is the integration key from Vault KV (under `routing_key`).
	TollWebhookFormatPagerDutyV2 = "pagerduty_v2"

	// TollWebhookFormatSlack is the Slack incoming-webhook schema:
	// `{text, attachments:[{color, fields:[...]}]}`. The URL is a slack-issued
	// webhook (`https://hooks.slack.com/services/...`). Auth is in the URL
	// itself, no separate headers.
	TollWebhookFormatSlack = "slack"
)

// KeeperToll is the Toll cluster-wide detector (ADR-038).
//
// Optional block. With nil → defaults + enabled=true (starts); explicit
// `enabled: false` → fully disabled. All other fields default from the
// [DefaultToll*] constants; they can be tuned without rewriting the whole block
// (omitted fields resolve to defaults in the daemon).
type KeeperToll struct {
	// Enabled toggles Toll. nil/omitted → true (enabled by default); explicit
	// `false` — operator disables (dev mode / debugging). `*bool` to distinguish
	// "unset" (→ default-on) from explicit `false`.
	Enabled *bool `yaml:"enabled,omitempty"`

	// Threshold is the disconnect_rate / baseline_connected fraction above which
	// the Toll leader raises cluster:degraded. 0/omitted → default
	// [DefaultTollThreshold] (0.20). Allowed range (0, 1] — a "more than 100% of
	// baseline" threshold is meaningless. Range checked in the schema phase.
	Threshold float64 `yaml:"threshold,omitempty"`

	// WindowSize is the sliding-window length (per-second buckets in a Redis
	// sorted-set). Type `duration`, empty → [DefaultTollWindow] (60s). Format —
	// semantic phase.
	WindowSize string `yaml:"window_size,omitempty"`

	// DegradedTTL is the TTL of the `cluster:degraded` Redis key. Type
	// `duration`, empty → [DefaultTollDegradedTTL] (60s). If the leader dies and
	// does not renew, the flag clears itself.
	DegradedTTL string `yaml:"degraded_ttl,omitempty"`

	// ClearGrace is the sustained low-rate window before clearing (asymmetric
	// hysteresis). Type `duration`, empty → [DefaultTollClearGrace] (60s).
	ClearGrace string `yaml:"clear_grace,omitempty"`

	// LeaseTTL is the TTL of the `cluster:toll:leader` Redis lease. Type
	// `duration`, empty → [DefaultTollLeaseTTL] (30s). Renew every TTL/3.
	LeaseTTL string `yaml:"lease_ttl,omitempty"`

	// WarmupDelay is the immunity window after instance start (cluster-restart
	// false-positive defense). Type `duration`, empty → [DefaultTollWarmup]
	// (60s).
	WarmupDelay string `yaml:"warmup_delay,omitempty"`

	// Webhook is an optional alert channel for cluster.degraded_set /
	// cluster.degraded_cleared (ADR-038 amendment 2026-05-27, extensions).
	// Supports a generic JSON POST plus the specific PagerDuty Events API v2 /
	// Slack incoming-webhook formats. With nil or `enabled: false` the notifier
	// does not start; degraded set/clear proceeds as before (audit + gauge +
	// metrics) without an alert-out. The webhook is best-effort: a POST error is
	// logged but does not block Set/Clear (cluster-degraded is the primary goal,
	// the webhook is secondary).
	Webhook *KeeperTollWebhook `yaml:"webhook,omitempty" json:"webhook,omitempty"`

	// PerCovenThresholds is an optional per-coven threshold override (ADR-038
	// amendment 2026-05-27, extensions). If set and non-empty, the leader also
	// counts disconnect_rate per-coven (ZRANGEBYSCORE → split the member value on
	// `|`) and raises cluster:degraded if EITHER the global threshold is exceeded
	// OR a per-coven threshold for a specific coven. The trigger's audit payload
	// stores `coven_name` (if the trigger is per-coven). Map key — coven name,
	// value — threshold (0, 1].
	//
	// The cardinality risk of ADR-038(§5) is mitigated by per-coven thresholds
	// being an explicit operator opt-in: the key list is finite and controlled in
	// keeper.yml. The Prometheus per-coven counter
	// (keeper_toll_disconnects_total{coven}) already carries the same cardinality.
	PerCovenThresholds map[string]float64 `yaml:"per_coven_thresholds,omitempty" json:"per_coven_thresholds,omitempty"`
}

// KeeperTollWebhook is the Toll webhook alert channel (ADR-038 amendment,
// extensions).
//
// With `enabled: false` the notifier does not start (default-off — adding the
// field does not change existing configs' behavior). With `enabled: true`,
// `url_ref` (a vault-ref to a secret URL OR an inline plain URL — operator's
// choice) + `format` are required (`format` validated in the schema phase
// against the closed enum [TollWebhookFormatGeneric] /
// [TollWebhookFormatPagerDutyV2] / [TollWebhookFormatSlack]).
//
// SECURITY: integration keys / Slack webhook URLs are secrets (they expose the
// pager). Prefer `url_ref: vault:secret/keeper/toll-webhook-url` (Vault KV field
// `url`). An inline URL is allowed for compatibility with local receivers
// without Vault, but for a prod form use a vault-ref.
type KeeperTollWebhook struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	URLRef  string `yaml:"url_ref" json:"url_ref"`
	Format  string `yaml:"format,omitempty" json:"format,omitempty"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// KeeperListen holds the four Keeper listeners.
type KeeperListen struct {
	GRPC    KeeperListenGRPC   `yaml:"grpc"`
	OpenAPI KeeperListenSimple `yaml:"openapi"`
	MCP     KeeperListenSimple `yaml:"mcp"`
	Metrics KeeperListenSimple `yaml:"metrics"`
}

// KeeperListenGRPC holds Keeper's two sub-listeners under ADR-012(b):
// `bootstrap` (server-only TLS, a separate listener for the Bootstrap RPC, since
// a Soul has no SoulSeed cert yet) and `event_stream` (mTLS, the long-lived bidi
// stream after onboarding).
//
// TLS parameters are architecturally independent, but the grammar allows the
// same cert/key paths — the same server certificate may be used on both
// listeners.
type KeeperListenGRPC struct {
	Bootstrap   KeeperListenGRPCBootstrap   `yaml:"bootstrap"`
	EventStream KeeperListenGRPCEventStream `yaml:"event_stream"`
}

// KeeperListenGRPCBootstrap is the Bootstrap listener.
//
// No CA goes here: Bootstrap per ADR-012(b) is server-only TLS, and a Soul has
// no client certificate before onboarding. A `listen.grpc.bootstrap.tls.ca` key
// in YAML is rejected in the schema phase via `unknown_key`.
type KeeperListenGRPCBootstrap struct {
	Addr string                       `yaml:"addr"`
	TLS  KeeperListenGRPCBootstrapTLS `yaml:"tls"`
}

type KeeperListenGRPCBootstrapTLS struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// KeeperListenGRPCEventStream is the EventStream listener (mTLS).
//
// MaxApplySizeMB is the size ceiling of one outgoing FromKeeper message,
// primarily `ApplyRequest` with a batch of rendered `RenderedTask` (Destiny
// render is Keeper-side, ADR-012). Applied as `grpc.MaxSendMsgSize` on the
// EventStream server: on an attempt to send more, Keeper fails fast with a clear
// error instead of an opaque rejection on the Soul side. 0/omitted → default
// [DefaultMaxApplySizeMB] (8 MiB). This is NOT the recv limit for incoming
// FromSoul (that is the internal invariant `eventStreamMaxRecvMsgSize`, 1 MiB,
// not config-controlled).
//
// Keeper↔Soul contract invariant: this send limit must be ≤ the Soul recv limit
// (`keeper.max_apply_size_mb` in `soul.yml`), otherwise Keeper sends what Soul
// would reject. Both sides' defaults match (8 MiB).
type KeeperListenGRPCEventStream struct {
	Addr           string                         `yaml:"addr"`
	TLS            KeeperListenGRPCEventStreamTLS `yaml:"tls"`
	MaxApplySizeMB int                            `yaml:"max_apply_size_mb,omitempty"`
}

// ResolvedMaxApplySize returns the effective EventStream-server send limit in
// bytes: 0/omitted → [DefaultMaxApplySizeMB]. Validation (>0, ≥ minimum) — in
// the schema phase; here only default resolution.
func (e KeeperListenGRPCEventStream) ResolvedMaxApplySize() int {
	mb := e.MaxApplySizeMB
	if mb <= 0 {
		mb = DefaultMaxApplySizeMB
	}
	return mb * bytesPerMiB
}

type KeeperListenGRPCEventStreamTLS struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
	// CA is the root certificate validating incoming Souls' SoulSeed.
	CA string `yaml:"ca"`
}

type KeeperListenSimple struct {
	Addr string `yaml:"addr"`
}

// KeeperPostgres is the cold storage.
type KeeperPostgres struct {
	DSNRef string             `yaml:"dsn_ref"`
	Pool   KeeperPostgresPool `yaml:"pool"`
}

type KeeperPostgresPool struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

// KeeperRedis is the hot layer and coordination.
//
// `mode` selects the Redis topology (ADR-006 amendment):
//   - `standalone` (default, empty/omitted = standalone — forward-compat for old
//     configs) — a single node, address in `addr`.
//   - `sentinel` — Redis Sentinel HA: the client finds the master via sentinel
//     nodes. Requires `master_name` + `sentinels` (sentinel node addresses
//     host:port); `addr` is unused in this mode. The sentinel nodes' own
//     password is `sentinel_password_ref` (optional), the Redis password is
//     `password_ref`.
//   - `cluster` — Redis Cluster: slot-based sharding. Requires `nodes` (cluster
//     node addresses host:port for bootstrap discovery); `addr` is unused.
//     `sentinel_*` do not apply to cluster mode.
//
// `password_ref` / `sentinel_password_ref` are vault-refs of the form
// `vault:<mount>/<path>[#field]` (or plaintext in tests); resolved at
// `keeper/internal/redis.NewClient` via the keeper-vault client.
type KeeperRedis struct {
	Mode                string   `yaml:"mode,omitempty"`
	Addr                string   `yaml:"addr"`
	PasswordRef         string   `yaml:"password_ref"`
	MasterName          string   `yaml:"master_name,omitempty"`
	Sentinels           []string `yaml:"sentinels,omitempty"`
	Nodes               []string `yaml:"nodes,omitempty"`
	SentinelPasswordRef string   `yaml:"sentinel_password_ref,omitempty"`
}

// KeeperVault is a mandatory Keeper dependency.
//
// `auth.method` selects how Keeper authenticates to Vault:
//   - `token` (default, dev-shortcut) — a static token from the `token` field;
//     `dev/docker-compose.yml` runs Vault in dev mode with a root token.
//   - `approle` — the prod path (ADR-014): Keeper does `auth/approle/login` with
//     `role_id` + `secret_id` and gets a renewable client token, which
//     TokenRenewer (renewer.go) then keeps alive.
//
// Forward-compat: keeper.yml without an `auth` block (or with an empty
// `auth.method`) is treated as `method=token` — old configs work unchanged.
//
// `kv_mount` is the mount point for the Vault KV v1/v2 secrets engine, default
// "secret" (version auto-detected via `sys/internal/ui/mounts`; override —
// `vault.kv_version`).
//
// `kv_version` is an optional escape-hatch: empty/omitted → the KV mount version
// is resolved by a probe via `sys/internal/ui/mounts/<mount>`; a set `"1"`/`"2"`
// forces the version without a probe (needed when ACL blocks the probe endpoint).
//
// `pki_mount` / `pki_role` are the mount + role of the PKI engine through which
// Keeper signs Souls' CSRs at onboarding (`Bootstrap` RPC, ADR-012(b)). Default
// `pki_role` is empty; the semantic phase does not validate the ROLE — Vault
// itself rejects a request for a nonexistent role.
type KeeperVault struct {
	Addr      string          `yaml:"addr"`
	Token     string          `yaml:"token,omitempty"`
	KVMount   string          `yaml:"kv_mount,omitempty"`
	KVVersion string          `yaml:"kv_version,omitempty"`
	Auth      KeeperVaultAuth `yaml:"auth"`
	PKIMount  string          `yaml:"pki_mount"`
	PKIRole   string          `yaml:"pki_role,omitempty"`

	// InputDenyPaths is an optional extension of the hard deny-list for scoped
	// resolution of `vault:`-refs in operator input (docs/input.md → "vault_scope",
	// fork C). Logical-path prefixes (`<mount>/<prefix>`) that are NEVER resolved
	// via an input-ref, even if a field declared a `vault_scope` covering them.
	// Augments the system-floor [config.VaultInputFloor] (`secret/keeper/*`,
	// `secret/internal/*`); the system-floor itself is NOT disabled by config,
	// only extended. This does NOT affect authored `vault:`-refs in task params.
	InputDenyPaths []string `yaml:"input_deny_paths,omitempty"`
}

// KeeperSecretIngest is dual-mode ingest of an operator secret (ADR-064,
// NIM-11). The operator may pass a secret by value (plaintext
// `secret`/`credentials`) instead of a vault-ref; keeper writes it to Vault at a
// deterministic path and stores only the ref in PG.
type KeeperSecretIngest struct {
	// AcceptPlaintext allows ingesting a plaintext secret in Herald/Provider CRUD
	// (Operator API + MCP). Default false (secure): when false, plaintext is
	// rejected with 422 and only `*_ref` is accepted. Enable ONLY when the
	// Operator API and MCP are behind TLS (ADR-064 mitigation a): plaintext
	// travels operator→keeper over the wire, a cleartext hop is unacceptable.
	// Keeper does not terminate the Operator API TLS itself (behind a proxy), so
	// the guarantee is an operator declaration.
	AcceptPlaintext bool `yaml:"accept_plaintext"`
}

// KeeperVaultAuth is the Vault auth-method selection and parameters.
//
// AppRole credentials are NOT read from Vault (`vault:`-ref) — that is
// chicken-and-egg: these are the very credentials Keeper logs in with in order
// to resolve any vault-refs (postgres.dsn_ref, signing_key_ref, …). So the
// source is local, before the Vault client comes up:
//
//   - `role_id` is NOT a secret (a role identifier), acceptable in the clear
//     right in keeper.yml.
//   - `secret_id` is a secret, its plaintext is NOT stored in the main config.
//     The source is one of (priority top to bottom):
//   - `secret_id_file` — a path to a mode-restricted file (recommended
//     0400/0600), contents = secret_id (trailing newline stripped);
//   - `secret_id_env` — the name of an env var with secret_id (dev/CI/secret
//     injectors like Vault Agent / k8s-secret-as-env).
//
// With `method=token` all approle fields are ignored.
type KeeperVaultAuth struct {
	Method       string `yaml:"method,omitempty"`
	RoleID       string `yaml:"role_id,omitempty"`
	SecretIDFile string `yaml:"secret_id_file,omitempty"`
	SecretIDEnv  string `yaml:"secret_id_env,omitempty"`
}

// AuthMethodToken / AuthMethodAppRole are the allowed `vault.auth.method`
// values. An empty method is equivalent to token (forward-compat).
const (
	AuthMethodToken   = "token"
	AuthMethodAppRole = "approle"
)

// ResolvedAuthMethod returns the effective auth method: empty → token.
func (a KeeperVaultAuth) ResolvedAuthMethod() string {
	if a.Method == "" {
		return AuthMethodToken
	}
	return a.Method
}

// KeeperAuth is operator (Archon) authentication.
// Soul has no `auth:` block — a Soul authenticates via mTLS / SoulSeed.
//
// `jwt` is the internal JWT issuer (ADR-014), the active part.
//
// `ldap` is federated LDAP authentication (ADR-058, stage 1 implemented:
// semantic validation + resolution into auth/ldap.Config + endpoint
// /auth/ldap/login). `oidc` is federated OAuth2/OIDC authentication (ADR-058
// stage 2 implemented: semantic validation + discovery + resolution into
// auth/oidc.Config + endpoints /auth/oidc/{login,callback}; PKCE always-on;
// requires Redis). Both blocks are optional: unset → the login method is
// unavailable and Keeper still starts (ADR-053 OPTIONAL-tier). Secret fields —
// via `*_ref` (`vault:<mount>/<path>[#field]`, resolved load-time like
// `redis.password_ref`).
type KeeperAuth struct {
	JWT  *KeeperAuthJWT  `yaml:"jwt,omitempty"`
	LDAP *KeeperAuthLDAP `yaml:"ldap,omitempty"` // ADR-058 stage 1
	OIDC *KeeperAuthOIDC `yaml:"oidc,omitempty"` // ADR-058 stage 2

	// RateLimit is the anti-bruteforce guard for public login endpoints
	// (ADR-058(g), HIGH-3). Per-IP + per-username attempt-rate throttle + lockout
	// after a series of failures. Optional: nil/omitted → defaults
	// [DefaultAuthLoginRL*] (default-ON, footgun-guard like Tempo/Toll). Actually
	// starts only with Redis (the limiter is cluster-shared, gated in the
	// daemon); without Redis the login endpoints degrade without throttle (like
	// Tempo passthrough), consistent with OIDC already requiring Redis.
	// Hot-reloadable.
	RateLimit *KeeperAuthLoginRateLimit `yaml:"rate_limit,omitempty"`
}

// Anti-bruteforce login-limit defaults (ADR-058(g), HIGH-3). Chosen so "a
// person with a mistyped password is not blocked, brute-force/username
// enumeration is cut off".
const (
	// DefaultAuthLoginRLRate is the attempt-throttle refill rate (attempts/sec)
	// per principal (IP or username). 0.5 rps = on average one attempt per 2s.
	DefaultAuthLoginRLRate = 0.5

	// DefaultAuthLoginRLBurst is the attempt-bucket depth (a one-time burst).
	DefaultAuthLoginRLBurst = 10

	// DefaultAuthLoginLockoutThreshold is the number of consecutive failed logins
	// in the window after which the principal is locked out.
	DefaultAuthLoginLockoutThreshold = 5

	// DefaultAuthLoginLockoutWindow is the failure-counting window (sliding by
	// TTL).
	DefaultAuthLoginLockoutWindow = 15 * time.Minute

	// DefaultAuthLoginLockoutBackoff is the lockout duration after the failure
	// threshold is reached.
	DefaultAuthLoginLockoutBackoff = 15 * time.Minute
)

// KeeperAuthLoginRateLimit is the anti-bruteforce login-endpoint configuration
// (ADR-058(g), HIGH-3). All fields optional, omitted/zero → [Default*].
type KeeperAuthLoginRateLimit struct {
	// Enabled toggles the guard. nil/omitted → true (default-ON, footgun-guard).
	// Explicit `false` — opt-out (dev). Actually starting requires Redis (daemon).
	Enabled *bool `yaml:"enabled,omitempty"`

	// Rate / Burst throttle the ATTEMPT rate per principal (token-bucket).
	Rate  float64 `yaml:"rate,omitempty"`
	Burst int     `yaml:"burst,omitempty"`

	// LockoutThreshold is the number of failures in the window before lockout.
	LockoutThreshold int `yaml:"lockout_threshold,omitempty"`

	// LockoutWindow / LockoutBackoff are the failure-counting window and lockout
	// duration (duration-convention: Go duration or `<N>d`).
	LockoutWindow  string `yaml:"lockout_window,omitempty"`
	LockoutBackoff string `yaml:"lockout_backoff,omitempty"`
}

// LoginRateLimitEnabled is the effective toggle (nil block/nil field → true).
func (a *KeeperAuth) LoginRateLimitEnabled() bool {
	if a == nil || a.RateLimit == nil || a.RateLimit.Enabled == nil {
		return true
	}
	return *a.RateLimit.Enabled
}

// ResolvedLoginRateLimit returns the effective throttle+lockout parameters:
// omitted/zero fields → [DefaultAuthLogin*]. Duration fields are parsed; invalid
// (should not get here — semantic validation) → default. A pure resolve for
// daemon wiring (read once at middleware assembly, not hot-path).
func (a *KeeperAuth) ResolvedLoginRateLimit() (rate float64, burst, threshold int, window, backoff time.Duration) {
	rate, burst = DefaultAuthLoginRLRate, DefaultAuthLoginRLBurst
	threshold = DefaultAuthLoginLockoutThreshold
	window, backoff = DefaultAuthLoginLockoutWindow, DefaultAuthLoginLockoutBackoff
	if a == nil || a.RateLimit == nil {
		return rate, burst, threshold, window, backoff
	}
	rl := a.RateLimit
	if rl.Rate > 0 {
		rate = rl.Rate
	}
	if rl.Burst > 0 {
		burst = rl.Burst
	}
	if rl.LockoutThreshold > 0 {
		threshold = rl.LockoutThreshold
	}
	if rl.LockoutWindow != "" {
		if d, err := ParseDuration(rl.LockoutWindow); err == nil && d > 0 {
			window = d
		}
	}
	if rl.LockoutBackoff != "" {
		if d, err := ParseDuration(rl.LockoutBackoff); err == nil && d > 0 {
			backoff = d
		}
	}
	return rate, burst, threshold, window, backoff
}

type KeeperAuthJWT struct {
	SigningKeyRef string `yaml:"signing_key_ref,omitempty"`
	Issuer        string `yaml:"issuer,omitempty"`
	TTLDefault    string `yaml:"ttl_default,omitempty"`
	TTLBootstrap  string `yaml:"ttl_bootstrap,omitempty"`
	// ExchangeTTL — short-lived Bearer TTL issued by POST /auth/token in exchange for a
	// session-cookie (NIM-77/ADR-058 Variant B). Separate from ttl_default (24h);
	// default 10m, floor 1m (a value below the minimum is raised). Duration string.
	ExchangeTTL string `yaml:"exchange_ttl,omitempty" json:"exchange_ttl,omitempty"`
}

// KeeperAuthLDAP is the LDAP-authentication config (ADR-058(c)/(e), stage 1
// implemented). TLS is required: `ldaps://` OR `ldap://` + `start_tls: true`.
// Secrets — `bind_password_ref` (Vault). `tls.ca_ref` — optional CA bundle for
// LDAPS.
//
// Semantic validation (semantic.go::checkAuthLDAP): ldaps-vs-start_tls mutually
// exclusive, bind_mode=search ⇒ bind_dn+bind_password_ref, TLS-required,
// insecure_skip_verify → WARN. Resolution of *_ref + ca_ref → auth/ldap.Config —
// load-time in the daemon (setupLDAPAuth). bind_mode=direct (the
// user_dn_template field) is deferred (stage 1 — search only).
type KeeperAuthLDAP struct {
	URL             string              `yaml:"url"`                         // ldaps://host:636 | ldap://host:389
	StartTLS        bool                `yaml:"start_tls,omitempty"`         // StartTLS over ldap://
	TLS             KeeperAuthLDAPTLS   `yaml:"tls,omitempty"`               //
	BindMode        string              `yaml:"bind_mode,omitempty"`         // search | direct
	BindDN          string              `yaml:"bind_dn,omitempty"`           // service-account DN (search)
	BindPasswordRef string              `yaml:"bind_password_ref,omitempty"` // vault-ref (search)
	BaseDN          string              `yaml:"base_dn,omitempty"`           //
	UserFilter      string              `yaml:"user_filter,omitempty"`       // (uid=%s)
	UserDNTemplate  string              `yaml:"user_dn_template,omitempty"`  // uid=%s,ou=people,... (direct)
	GroupFilter     string              `yaml:"group_filter,omitempty"`      // (member=%s)
	GroupAttr       string              `yaml:"group_attr,omitempty"`        // cn
	AIDAttr         string              `yaml:"aid_attr,omitempty"`          // uid | mail → AID
	Timeout         string              `yaml:"timeout,omitempty"`           // duration
	GroupRoleMap    map[string][]string `yaml:"group_role_map,omitempty"`    // external group → RBAC roles
}

type KeeperAuthLDAPTLS struct {
	CARef              string `yaml:"ca_ref,omitempty"`               // vault-ref CA-bundle
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify,omitempty"` // dev-only (WARN)
}

// KeeperAuthOIDC is the OIDC-authentication config (ADR-058(b)/(e), stage 2
// implemented). `issuer` — HTTPS only (discovery base). Secret —
// `client_secret_ref` (Vault, optional for a public client). `tls.ca_ref` —
// optional custom IdP CA.
//
// PKCE (S256) is always enabled by the implementation (auth/oidc), not left to
// the operator's choice (ADR-058 fork №6 resolved in favor of "mandatory"); so
// there is no use_pkce config flag. OIDC requires a live Redis (cluster-shared
// flow-state store for nonce/PKCE) — without Redis the endpoints are not mounted
// (ADR-053 OPTIONAL-tier).
//
// Semantic validation (semantic.go::checkAuthOIDC): issuer https-only,
// client_id/redirect_url required, vault-ref form of client_secret_ref/ca_ref.
// Resolution of *_ref + discovery → auth/oidc.Config — load-time in the daemon
// (setupOIDCAuth).
type KeeperAuthOIDC struct {
	Issuer          string              `yaml:"issuer"`                      // https://idp/realms/...
	ClientID        string              `yaml:"client_id"`                   //
	ClientSecretRef string              `yaml:"client_secret_ref,omitempty"` // vault-ref (optional, public-client)
	RedirectURL     string              `yaml:"redirect_url"`                // https://keeper/auth/oidc/callback
	Scopes          []string            `yaml:"scopes,omitempty"`            // openid, email, profile, groups
	TLS             KeeperAuthOIDCTLS   `yaml:"tls,omitempty"`               //
	AIDClaim        string              `yaml:"aid_claim,omitempty"`         // sub | email | preferred_username (default sub)
	GroupsClaim     string              `yaml:"groups_claim,omitempty"`      // claim with groups (default groups)
	GroupRoleMap    map[string][]string `yaml:"group_role_map,omitempty"`    // external group → RBAC roles
}

type KeeperAuthOIDCTLS struct {
	CARef string `yaml:"ca_ref,omitempty"` // vault-ref of a custom IdP CA
}

// KeeperSigil is plugin-clearance signing (ADR-026, the Sigil seal of trust).
//
// Optional block: if unset (or signing_key_ref empty), signing is unavailable —
// keeper starts normally, but the allow operation (slice S4) returns "sigil key
// not configured". Key loading is nil-safe.
//
// SigningKeyRef is the config path to the Vault KV holding the ed25519 signing
// private key (`vault:<mount>/<path>`, field `signing_key`). Symmetric with
// auth.jwt.signing_key_ref (ADR-014), but the key is ASYMMETRIC (ed25519): the
// private key signs on Keeper, the public part travels to a Soul at bootstrap as
// a trust anchor.
type KeeperSigil struct {
	SigningKeyRef string `yaml:"signing_key_ref,omitempty"`
}

// DefaultSigilAnchorsReloadInterval is the default TTL-fallback re-read of the
// set of Sigil signing trust-anchor keys (ADR-026(h), R3 known-gap). 30s — the
// same window as the `rbac.DefaultRefreshInterval` family (key mutations are
// rare, the staleness window is small), short enough for a missed
// `sigil:anchors-changed` to self-heal before a prod rotation, and rare enough
// not to poke Vault/PG with a Signer re-build on an idle cluster.
const DefaultSigilAnchorsReloadInterval = 30 * time.Second

// KeeperMetrics holds extra settings for Keeper's `/metrics` endpoint. The bind
// address itself is `listen.metrics.addr` (a dedicated listener, not the openapi
// router); this block only carries optional endpoint protection (ADR-024).
//
// Soul has no symmetric auth: the Soul agent has no vault client (ADR-012) to
// resolve a password_ref; Soul metrics are protected by loopback
// (`metrics.listen` = 127.0.0.1). Auth for Soul is a separate future task.
type KeeperMetrics struct {
	Auth *KeeperMetricsAuth `yaml:"auth,omitempty"`
}

type KeeperMetricsAuth struct {
	Basic *KeeperMetricsBasicAuth `yaml:"basic,omitempty"`
}

// KeeperMetricsBasicAuth is HTTP Basic auth on `/metrics`.
//
// PasswordRef is a vault-ref (`vault:<mount>/<path>`), resolved by the same
// keeper-vault client that reads the JWT signing key (Vault KV field
// `password`). A plaintext password in the config is not allowed ("security
// first"): only a vault-ref. When Enabled, both Username and PasswordRef are
// required (validated in the schema phase).
type KeeperMetricsBasicAuth struct {
	Enabled     bool   `yaml:"enabled"`
	Username    string `yaml:"username,omitempty"`
	PasswordRef string `yaml:"password_ref,omitempty"`
}

// KeeperOTel is the shared Keeper/Soul shape (separate structs so future enum
// drift does not couple them).
type KeeperOTel struct {
	Enabled  bool   `yaml:"enabled"`
	Exporter string `yaml:"exporter,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`

	// ExportMetrics optionally pushes metrics over OTLP in addition to the
	// Prometheus scrape (ADR-024 §1.2 / observability.md §5). A stub for Slice 2:
	// read from config, but the OTLP metrics pipeline does not start yet (in
	// Slice 0 only traces are exported).
	ExportMetrics bool `yaml:"export_metrics,omitempty"`
}

// KeeperLogging is logs with rotation.
type KeeperLogging struct {
	Level    string           `yaml:"level,omitempty"`
	Format   string           `yaml:"format,omitempty"`
	File     string           `yaml:"file,omitempty"`
	Rotation *LoggingRotation `yaml:"rotation,omitempty"`
}

// LoggingRotation is shared by Keeper and Soul (different defaults, identical
// schema).
//
// MaxAgeDays semantics: empty/0 → builder default (7 days), any >0 — the exact
// number of days. "0 = no age-based deletion" is NOT expressible in the schema —
// for that MaxAgeDays moved from overlay-`*int` to a flat int, and the "0 vs
// unset" distinction was deliberately dropped (see shared/log → defaultMaxAgeDays).
type LoggingRotation struct {
	MaxSizeMB  int  `yaml:"max_size_mb,omitempty"`
	MaxFiles   int  `yaml:"max_files,omitempty"`
	MaxAgeDays int  `yaml:"max_age_days,omitempty"`
	Compress   bool `yaml:"compress,omitempty"`
}

// DefaultPluginFetchTimeout is the default for `plugins.fetch_timeout`: the
// ceiling on one chain of plugin-resolve git commands (clone→checkout→rev-parse,
// ADR-026 F-fetch). git-egress is an external call, so a timeout is mandatory;
// 120s covers a large repo on a slow link without turning Keeper start into an
// endless wait on an unreachable remote.
const DefaultPluginFetchTimeout = 120 * time.Second

// Plugin-resolve size limits (ADR-026(g) git-egress hardening). Protects the
// keeper host's disk from a hostile/huge repository: the timeout bounds
// git-egress by time, these two fields by size. Both are in MiB, symmetric with
// `listen.grpc.event_stream.max_apply_size_mb` (one style of size fields in the
// project, no separate human-readable parser). Exceeding is fail-closed: the
// slot is not created, so there is nothing for the plugin to clear (Sigil).
const (
	// DefaultPluginMaxArtifactSizeMB is the default for
	// `plugins.max_artifact_size_mb`: the size ceiling of one extracted binary
	// `dist/<binary-name>` (and manifest.yaml, which is clearly smaller). 256 MiB
	// amply covers real Go plugin binaries (tens of MiB) while cutting off a junk
	// artifact a hostile repository would use to fill the cache.
	DefaultPluginMaxArtifactSizeMB = 256

	// DefaultPluginMaxCloneSizeMB is the default for `plugins.max_clone_size_mb`:
	// the ceiling on the total working-tree size of the clone (checkout + `.git`)
	// before artifact extraction. Necessarily larger than the artifact limit: the
	// tree carries the artifact itself plus the repository's other files and a
	// shallow `.git`. 1024 MiB — a large repository with built binaries, but not
	// unbounded.
	DefaultPluginMaxCloneSizeMB = 1024

	// MinPluginSizeMB is the validation floor for both limits. Below 1 MiB is
	// rejected: a sub-megabyte ceiling would reject any real Go plugin binary
	// (tens of MiB), turning the hardening into a permanent fail-closed.
	MinPluginSizeMB = 1
)

// KeeperPlugins holds the Keeper-side plugin catalogs.
//
// `CacheRoot` is the path to the plugin-artifact cache directory on the keeper
// host (see [docs/keeper/plugins.md](../../docs/keeper/plugins.md)). Empty →
// `pluginhost.DefaultCacheRoot`. Must be absolute; validated in the schema
// phase.
//
// `WorkRoot` is the root of the plugin resolver's working git clones (ADR-026
// F-fetch, A1-S1). STRICTLY outside `CacheRoot`: .git and the checkout must not
// land in the cache directory that Discover/ReadSlot read. Empty → the built-in
// default `/var/lib/soul-stack-keeper/plugin-src`. Must be absolute; validated
// in the schema phase.
//
// `FetchTimeout` is the ceiling on one chain of resolve git commands (type
// `duration`, empty → [DefaultPluginFetchTimeout] (120s)). Format validated in
// the semantic phase (style `acolyte_*`).
//
// `MaxArtifactSizeMB` / `MaxCloneSizeMB` are git-egress hardening size limits
// (ADR-026(g)): the ceiling on one extracted binary and on the total clone
// working tree. Both in MiB (style `max_apply_size_mb`); 0/omitted → defaults
// [DefaultPluginMaxArtifactSizeMB] / [DefaultPluginMaxCloneSizeMB]; a set value
// must be ≥ [MinPluginSizeMB] (validated in the schema phase). Exceeding at
// resolve time is fail-closed (the slot is not created).
type KeeperPlugins struct {
	CacheRoot         string               `yaml:"cache_root,omitempty"`
	WorkRoot          string               `yaml:"work_root,omitempty"`
	FetchTimeout      string               `yaml:"fetch_timeout,omitempty"`
	MaxArtifactSizeMB int                  `yaml:"max_artifact_size_mb,omitempty"`
	MaxCloneSizeMB    int                  `yaml:"max_clone_size_mb,omitempty"`
	CloudDrivers      []PluginCatalogEntry `yaml:"cloud_drivers,omitempty"`
	SSHProviders      []PluginCatalogEntry `yaml:"ssh_providers,omitempty"`
	SoulModules       []PluginCatalogEntry `yaml:"soul_modules,omitempty"`
}

// ResolvedFetchTimeout returns the effective `plugins.fetch_timeout`:
// empty/invalid → [DefaultPluginFetchTimeout]. Format is already validated in
// the semantic phase (checkDuration); we default on any non-positive result just
// in case (symmetric with the acolyte_* resolvers in keeper/cmd/keeper).
func (p *KeeperPlugins) ResolvedFetchTimeout() time.Duration {
	if p == nil || p.FetchTimeout == "" {
		return DefaultPluginFetchTimeout
	}
	d, err := ParseDuration(p.FetchTimeout)
	if err != nil || d <= 0 {
		return DefaultPluginFetchTimeout
	}
	return d
}

// ResolvedMaxArtifactSize returns the effective single-binary size limit in
// bytes: 0/omitted/invalid → [DefaultPluginMaxArtifactSizeMB]. The range
// (≥ minimum) is validated in the schema phase; here only default resolution.
func (p *KeeperPlugins) ResolvedMaxArtifactSize() int64 {
	mb := DefaultPluginMaxArtifactSizeMB
	if p != nil && p.MaxArtifactSizeMB > 0 {
		mb = p.MaxArtifactSizeMB
	}
	return int64(mb) * bytesPerMiB
}

// ResolvedMaxCloneSize returns the effective clone working-tree size limit in
// bytes: 0/omitted/invalid → [DefaultPluginMaxCloneSizeMB].
func (p *KeeperPlugins) ResolvedMaxCloneSize() int64 {
	mb := DefaultPluginMaxCloneSizeMB
	if p != nil && p.MaxCloneSizeMB > 0 {
		mb = p.MaxCloneSizeMB
	}
	return int64(mb) * bytesPerMiB
}

type PluginCatalogEntry struct {
	Name   string `yaml:"name"`
	Source string `yaml:"source"`
	Ref    string `yaml:"ref"`
}

// PluginRuntime is shared by Keeper and Soul (ADR-020).
// Symmetric schema, different `socket_dir` defaults.
type PluginRuntime struct {
	SocketDir           string   `yaml:"socket_dir,omitempty"`
	StartupTimeout      string   `yaml:"startup_timeout,omitempty"`
	ShutdownGrace       string   `yaml:"shutdown_grace,omitempty"`
	AllowedCapabilities []string `yaml:"allowed_capabilities,omitempty"`
	ConflictPolicy      string   `yaml:"conflict_policy,omitempty"`
	EnableTLS           bool     `yaml:"enable_tls,omitempty"`
}

// KeeperAudit is the audit block (ADR-022).
type KeeperAudit struct {
	Enabled       bool `yaml:"enabled"`
	OTelExport    bool `yaml:"otel_export"`
	RetentionDays int  `yaml:"retention_days"`
}

// HotReload is the Keeper/Soul-shared block controlling reload triggers
// (ADR-021).
type HotReload struct {
	EnableSignal       bool `yaml:"enable_signal"`
	EnableInotify      bool `yaml:"enable_inotify"`
	AuditCorrelationID bool `yaml:"audit_correlation_id"`
}

// Conductor defaults (ADR-048). Applied in the daemon for an empty/zero field.
const (
	// DefaultCadenceSchedulerLockTTL is the default TTL of the conductor:leader
	// Redis lease. 5m parity with reaper lock_ttl: large enough to survive a
	// temporary leader stall without losing leadership, and short enough for fast
	// failover to another instance on leader death. Renew is at lock_ttl/3 inside
	// leaderloop.
	DefaultCadenceSchedulerLockTTL = 5 * time.Minute

	// The "Calm" profile of Conductor's adaptive poll step (ADR-048 "Adaptive
	// interval", 2026-06-07). Step = clamp(derivedMinPeriod, poll_floor,
	// poll_ceiling); empty registry → poll_idle.

	// DefaultCadenceSchedulerPollFloor is the poll-step floor (30s). This is also
	// the absolute minimum (poll_floor < 30s is a config error; the DB-CHECK
	// floor interval_seconds ≥ 30 closes Pass B / ADR-046).
	DefaultCadenceSchedulerPollFloor = 30 * time.Second

	// DefaultCadenceSchedulerPollCeiling is the poll-step ceiling (60s): rare
	// schedules (interval=1h) do not stretch polling so far that a NextRunAnchored
	// missed-slot becomes the only safety net.
	DefaultCadenceSchedulerPollCeiling = 60 * time.Second

	// DefaultCadenceSchedulerPollIdle is the poll step when the enabled registry
	// is empty (120s): nothing to spawn, so we poll less often than the normal
	// corridor.
	DefaultCadenceSchedulerPollIdle = 120 * time.Second
)

// KeeperCadenceScheduler is the Conductor config (ADR-048). Its own tick
// interval and Redis lease (`conductor:leader`), independent of Reaper.
//
// Enabled is *bool to distinguish "unset" (→ default-ON when Redis is present,
// footgun-guard ADR-048 §5) from explicit `false` (the operator deliberately
// silences the whole scheduler — disabling a single Cadence is done per-Cadence
// via `enabled: false` on the row itself, ADR-046 §3, not by globally silencing
// Conductor).
type KeeperCadenceScheduler struct {
	// Enabled — nil (omitted) → default-ON when Redis is present; explicit false →
	// Conductor does not start; explicit true → starts (requires Redis for lease
	// leadership, like Reaper).
	Enabled *bool `yaml:"enabled,omitempty"`

	// Interval is a backcompat alias for the adaptive-poll ceiling (ADR-048
	// "Adaptive interval"). Before the 2026-06-07 amendment this was a fixed tick
	// period. Now the step is adaptive (clamped by poll_floor/poll_ceiling);
	// `interval` is kept as an alias: if set while `poll_ceiling` is not →
	// ceiling = interval (old keeper.yml do not break). Type `duration`,
	// hot-reload.
	Interval string `yaml:"interval,omitempty"`

	// LockTTL is the TTL of the conductor:leader Redis lease (hot-reload between
	// re-acquires). Type `duration`, empty → default
	// [DefaultCadenceSchedulerLockTTL] (5m).
	LockTTL string `yaml:"lock_ttl,omitempty"`

	// PollFloor / PollCeiling / PollIdle are the adaptive poll-step corridor
	// (ADR-048 "Adaptive interval", "Calm" profile 30s/60s/120s). Step =
	// clamp(derivedMinPeriod, poll_floor, poll_ceiling); empty registry →
	// poll_idle. All three type `duration`, hot-reload (read from the config
	// snapshot in IntervalFn, like Interval/LockTTL). Empty/invalid → default.
	// Invariants (semantic validation): poll_floor ≥ 30s (absolute minimum),
	// poll_floor ≤ poll_ceiling, poll_idle ≥ poll_ceiling (idle no more frequent
	// than the normal poll).
	PollFloor   string `yaml:"poll_floor,omitempty"`
	PollCeiling string `yaml:"poll_ceiling,omitempty"`
	PollIdle    string `yaml:"poll_idle,omitempty"`
}

// CadenceSchedulerEnabled returns the effective Conductor enabled flag with the
// footgun-guard (ADR-048 §5): unset (nil block / nil field) → ON; explicit
// `false` → OFF; explicit `true` → ON. Actually starting in the daemon
// additionally requires a non-nil Redis (lease leadership), like Reaper.
func (c *KeeperCadenceScheduler) CadenceSchedulerEnabled() bool {
	if c == nil || c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ResolvedLockTTL returns the effective TTL of the Conductor lease key:
// empty/invalid → default.
func (c *KeeperCadenceScheduler) ResolvedLockTTL() time.Duration {
	if c == nil || c.LockTTL == "" {
		return DefaultCadenceSchedulerLockTTL
	}
	d, err := ParseDuration(c.LockTTL)
	if err != nil || d <= 0 {
		return DefaultCadenceSchedulerLockTTL
	}
	return d
}

// ResolvedPollFloor is the adaptive-poll floor (ADR-048): empty/invalid →
// default 30s.
func (c *KeeperCadenceScheduler) ResolvedPollFloor() time.Duration {
	if c == nil {
		return DefaultCadenceSchedulerPollFloor
	}
	return resolveDuration(c.PollFloor, DefaultCadenceSchedulerPollFloor)
}

// ResolvedPollCeiling is the adaptive-poll ceiling (ADR-048). Backcompat: if
// `poll_ceiling` is unset but `interval` is set (old format) → ceiling =
// max(interval, poll_floor) — clamped UP to floor. An old small `interval`
// (dev configs set sub-30s) does NOT break the config via the
// `poll_floor ≤ poll_ceiling` invariant: the alias is always ≥ floor. The
// warning about the bump is emitted by the semantic phase
// ([cadenceIntervalBelowFloorWarn]). Otherwise empty/invalid → default 60s.
func (c *KeeperCadenceScheduler) ResolvedPollCeiling() time.Duration {
	if c == nil {
		return DefaultCadenceSchedulerPollCeiling
	}
	if c.PollCeiling == "" && c.Interval != "" {
		ceiling := resolveDuration(c.Interval, DefaultCadenceSchedulerPollCeiling)
		if floor := c.ResolvedPollFloor(); ceiling < floor {
			return floor
		}
		return ceiling
	}
	return resolveDuration(c.PollCeiling, DefaultCadenceSchedulerPollCeiling)
}

// ResolvedPollIdle is the poll step when the enabled registry is empty
// (ADR-048): empty/invalid → default 120s.
func (c *KeeperCadenceScheduler) ResolvedPollIdle() time.Duration {
	if c == nil {
		return DefaultCadenceSchedulerPollIdle
	}
	return resolveDuration(c.PollIdle, DefaultCadenceSchedulerPollIdle)
}

// resolveDuration is the shared duration-field resolver: empty/invalid/
// non-positive → fallback (style ResolvedInterval/ResolvedLockTTL).
func resolveDuration(val string, fallback time.Duration) time.Duration {
	if val == "" {
		return fallback
	}
	d, err := ParseDuration(val)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// KeeperReaper is the background cleanup.
type KeeperReaper struct {
	Enabled   bool                  `yaml:"enabled"`
	Interval  string                `yaml:"interval,omitempty"`
	DryRun    bool                  `yaml:"dry_run,omitempty"`
	BatchSize int                   `yaml:"batch_size,omitempty"`
	LockTTL   string                `yaml:"lock_ttl,omitempty"`
	Rules     map[string]ReaperRule `yaml:"rules,omitempty"`
}

// ReaperRule is one Reaper rule. The rule schema is loosely typed because the 5
// predefined rules have different required fields (`statuses`, `stale_after`,
// `target_status`); strict per-rule typing is a deferred reaper.md normalization
// task.
//
// KeepLastN / KeepVersionBumpSnapshots are fields of the `archive_state_history`
// rule (ADR-Q19 retention). Other rules ignore them. KeepLastN uses *int to tell
// "unset" (→ the runner supplies default 50) from explicit 0 (semantic-validate
// rejects: 0 = "archive everything"). KeepVersionBumpSnapshots is *bool for the
// same reason: tell "unset" (→ default true, migration protection) from explicit
// false (the operator deliberately archives version-bump snapshots too).
type ReaperRule struct {
	Enabled                  bool     `yaml:"enabled"`
	MaxAge                   string   `yaml:"max_age,omitempty"`
	Action                   string   `yaml:"action,omitempty"`
	Statuses                 []string `yaml:"statuses,omitempty"`
	StaleAfter               string   `yaml:"stale_after,omitempty"`
	TargetStatus             string   `yaml:"target_status,omitempty"`
	KeepLastN                *int     `yaml:"keep_last_n,omitempty"`
	KeepVersionBumpSnapshots *bool    `yaml:"keep_version_bump_snapshots,omitempty"`

	// MaxConcurrentInFlight is a field of the `scry_background` rule (ADR-031
	// Slice C): the upper bound on concurrent dry_run scans initiated by a Reaper
	// tick. Other rules ignore it. `*int` to distinguish "unset" (→ the runner
	// supplies default 10) from explicit 0 ("mute the rule without clearing
	// enabled"). The active-scan counter is the number of apply_runs rows with
	// recipe->>'dry_run'='true' and finished_at IS NULL.
	MaxConcurrentInFlight *int `yaml:"max_concurrent_in_flight,omitempty"`

	// MinIntervalPerIncarnation is a field of the `scry_background` rule (ADR-031
	// Slice C): the minimum interval between background scans of one incarnation.
	// Other rules ignore it. Empty string / zero duration = "no lower bound" (the
	// iterator sort `last_drift_check_at NULLS FIRST` naturally gives round-robin
	// across incarnations).
	MinIntervalPerIncarnation string `yaml:"min_interval_per_incarnation,omitempty"`

	// RotateThreshold is a field of the `rotate_due_certs` rule (cert-rotation
	// Var1): how long before not_after a cert is considered due (e.g. "720h").
	// Other rules ignore it. Empty string / zero duration → the rule rotates
	// nothing (guard: without a threshold there is nothing to scan). The rotation
	// on-switch is a meaningful threshold + enabled:true.
	RotateThreshold string `yaml:"rotate_threshold,omitempty"`

	// RotateJitter is a field of the `rotate_due_certs` rule: the spread width of
	// the effective threshold (e.g. "168h") so certs with the same not_after are
	// not rotated in one tick (anti-thundering-herd). Empty/zero → no spread.
	RotateJitter string `yaml:"rotate_jitter,omitempty"`

	// MaxRotationsPerTick is a field of the `rotate_due_certs` rule: the ceiling
	// on the number of rotations per tick (anti-avalanche on mass expiry). `*int`
	// to distinguish "unset" (→ default in CertRotator) from an explicit value.
	MaxRotationsPerTick *int `yaml:"max_rotations_per_tick,omitempty"`

	// DryRun is the per-rule R1 barrier of the `rotate_due_certs` rule
	// (cert-rotation Var1): auto-swapping live TLS without an operator is
	// dangerous, so live rotation must NOT be enabled together with the global
	// reaper.dry_run:false (which permits a live purge). `*bool` with default-true
	// semantics: "unset" → dry_run (no rotation; the first run after enable is
	// dry); live rotation only on an EXPLICIT `dry_run: false`, independent of
	// reaper.dry_run. Other rules ignore this field (their dry_run is global).
	DryRun *bool `yaml:"dry_run,omitempty"`
}

// KeeperPush is the pilot wire-up of SshDispatcher (S6, 2026-05-26, [ADR-032
// amendment]).
//
// The pilot path is deliberately in keeper.yml for fast end-to-end progress:
// per-host SSH credentials in `targets[]` (without migrating to
// `souls.ssh_target jsonb` — that is S7), per-provider params in `providers[]`
// (without the push_providers PG table — S7). S7-3 introduced multi-CA
// `host_ca_refs[]`; the deprecated single-CA `host_ca_ref` stays under a
// 1-release WARN deprecation window (auto-adapt into the singleton, see below).
// Multi-provider routing is deferred — the pilot starts the first discovered
// ssh plugin.
//
// Optional: without the block (or with empty fields) the push orchestrator does
// not start.
type KeeperPush struct {
	// Targets — per-SID SSH credentials. The resolver looks up the record by SID
	// and returns [push.SSHTarget]; a SID with no record → fail with
	// `target_not_configured`.
	Targets []KeeperPushTarget `yaml:"targets,omitempty"`
	// Providers — per-provider params for the env-payload `SOUL_SSH_<NAME>_PARAMS`
	// (ADR-020 amendment l). On plugin spawn `params` is serialized to JSON and
	// placed in the env var named `SOUL_SSH_<UPPER(name)>_PARAMS`. Entries with no
	// match in `plugins.ssh_providers[].name` are ignored.
	Providers []KeeperPushProvider `yaml:"providers,omitempty"`
	// HostCARef — deprecated singular vault-ref to the public host CA (PEM-encoded
	// SSH public key, Vault KV field `public_key`). S7-3 introduced multi-CA
	// `host_ca_refs[]` (ADR-032 amendment 2026-05-26); the singular stays under a
	// 1-release WARN deprecation window. At daemon start it auto-adapts the
	// singular into `HostCARefs[0]` with auto-name `default` and a one-time WARN.
	// Mutually exclusive with `host_ca_refs[]` — simultaneous presence is rejected
	// in the schema phase (`mutually_exclusive_keys`).
	//
	// Deprecated: use [HostCARefs] (multi-CA).
	HostCARef string `yaml:"host_ca_ref,omitempty" json:"host_ca_ref,omitempty"`
	// HostCARefs — multi-CA for verifying host keys over SSH (S7-3, ADR-032
	// amendment 2026-05-26). Each element is a vault-ref + operator-defined `name`
	// (for logs / OTel attrs / metrics cardinality). At handshake
	// `ssh.CertChecker.IsHostAuthority` does an OR check over all loaded CAs: a
	// host-cert signed by any of them is trusted.
	//
	// Plaintext inline PEM is rejected as a security-policy violation (symmetric
	// with `auth.jwt.signing_key_ref` / `sigil.signing_key_ref`); every `ref`
	// must be a vault-ref. Names in the set must be unique (lookup by name in logs
	// / metrics without ambiguity).
	HostCARefs []KeeperPushCARef `yaml:"host_ca_refs,omitempty" json:"host_ca_refs,omitempty"`
	// AllowLegacyPushTargets — fallback flag for the S7-1 deprecation window
	// (ADR-032 amendment 2026-05-26): the PG source (souls.ssh_target jsonb) is
	// canonical, keeper.yml::push.targets[] is legacy. With false (default), a
	// record absent from PG → `ErrTargetNotConfigured`; with true → fallback to
	// ConfigTargetResolver over Targets[] with a one-time WARN at start.
	AllowLegacyPushTargets bool `yaml:"allow_legacy_push_targets,omitempty" json:"allow_legacy_push_targets,omitempty"`
	// AllowLegacyPushProviders — fallback flag for the S7-2 deprecation window
	// (ADR-032 amendment 2026-05-26): the PG source (push_providers table) is
	// canonical, keeper.yml::push.providers[] is legacy. With false (default), a
	// plugin with no PG record → the plugin starts without an env-payload
	// (behavior depends on the plugin itself: soul-ssh-static works with defaults,
	// soul-ssh-vault requires params); with true → fallback to
	// keeper.yml::push.providers[] with a one-time WARN at start. Symmetric with
	// [AllowLegacyPushTargets].
	AllowLegacyPushProviders bool `yaml:"allow_legacy_push_providers,omitempty" json:"allow_legacy_push_providers,omitempty"`

	// AutoImportLegacyTargets — opt-in one-shot migration of inline
	// `push.targets[]` → `souls.ssh_target` jsonb at Keeper start (ADR-032
	// amendment 2026-05-26, S7-4). Default false (silent data migration is
	// forbidden without explicit operator consent). With true, at start the
	// daemon walks `Targets[]`: for each SID with `ssh_target IS NULL` in `souls`
	// it writes the SSH credentials and emits audit-event
	// `soul.ssh-target.imported_from_config` (source `config_bootstrap`).
	// Idempotent: a record with a non-empty PG target is skipped, a repeated start
	// is a no-op. A missing `souls` row is a WARN-skip (not fatal).
	AutoImportLegacyTargets bool `yaml:"auto_import_legacy_targets,omitempty" json:"auto_import_legacy_targets,omitempty"`

	// AutoImportLegacyProviders — opt-in one-shot migration of inline
	// `push.providers[]` → the `push_providers` PG table at Keeper start (ADR-032
	// amendment 2026-05-26, S7-4). Default false. Symmetric with
	// [AutoImportLegacyTargets]: records absent from PG are created under the
	// `archon-system` AID; already-existing names are skipped (the PG record is
	// the canonical source, not overwritten). Audit-event —
	// `push-provider.imported_from_config`.
	AutoImportLegacyProviders bool `yaml:"auto_import_legacy_providers,omitempty" json:"auto_import_legacy_providers,omitempty"`

	// CovenDefaultProviders — Level 2 multi-provider routing (P2 W-4, ADR-032
	// amendment 2026-05-27). A map coven-name → SshProvider-plugin name. Used when
	// a Soul has no per-SID `ssh_target.ssh_provider` (Level 1). Tiebreak on
	// multiple coven matches — alphabetical order of coven names (determinism). An
	// empty map → fall through to Level 3.
	//
	// Hot-reload supported: on each config.Store.OnReload the router reads a fresh
	// snapshot via RouterConfigSource.
	CovenDefaultProviders map[string]string `yaml:"coven_default_providers,omitempty" json:"coven_default_providers,omitempty"`

	// ClusterDefaultProvider — Level 3 multi-provider routing (P2 W-4). The
	// default SshProvider-plugin name for all Souls for which neither Level 1 nor
	// Level 2 matched. Empty → ErrProviderNotRouted (fail per-host).
	//
	// Hot-reload supported (see CovenDefaultProviders).
	ClusterDefaultProvider string `yaml:"cluster_default_provider,omitempty" json:"cluster_default_provider,omitempty"`

	// Transport is the bootstrap-token delivery mode for
	// `core.bootstrap.delivered` (ADR-063 amendment "Teleport by-name
	// transport"): `direct` (default) or `teleport`. Affects ONLY the keeper-side
	// token-delivery core module, not the Destiny push run (that is always
	// generic).
	//
	//   - direct: generic push.Dial by primary_ip — plugin Authorize/Sign +
	//     CA-signed host-cert verify (host-CA from `host_ca_refs[]`).
	//   - teleport: by-name via the Teleport Proxy (target = SID/FQDN, NOT IP).
	//     Transport+auth+host-verify entirely through the Teleport identity-file
	//     (`teleport.*` below); plugin Authorize/Sign and Vault host-CA are NOT
	//     used. A fresh VM appears in Teleport in ~3-5 min → the module retries
	//     connect (scenario param `join_wait_timeout`).
	//
	// Empty is treated as `direct` (backward-compat). With `teleport` the
	// `teleport.*` block is required (schema-phase validation).
	Transport string `yaml:"transport,omitempty" json:"transport,omitempty"`

	// Teleport holds Teleport creds for `transport: teleport` (ADR-063
	// amendment). Required with `transport: teleport`, ignored with `direct`. The
	// creds live in the keeper.yml push block (NOT in the plugin): in teleport
	// mode the soul-ssh-teleport plugin does not take part in the delivery flow.
	Teleport *KeeperPushTeleport `yaml:"teleport,omitempty" json:"teleport,omitempty"`
}

// Allowed `push.transport` values (ADR-063 amendment). Empty string =
// PushTransportDirect (backward-compat).
const (
	PushTransportDirect   = "direct"
	PushTransportTeleport = "teleport"
)

// KeeperPushTeleport holds Teleport identity creds for by-name bootstrap
// transport (`push.transport: teleport`, ADR-063 amendment). All three fields
// are required in teleport mode (schema-phase validation): the identity-file
// carries TLS cert+key for mTLS to the Proxy AND an SSH user-cert + host-CA
// (known_hosts) for the target handshake — transport+auth+host-verify entirely
// from it.
type KeeperPushTeleport struct {
	// ProxyAddr is the `host:port` of the Teleport Proxy (gRPC sshgrpc listener,
	// usually `<proxy>:443`).
	ProxyAddr string `yaml:"proxy_addr" json:"proxy_addr"`
	// IdentityFile is the path to the Teleport identity-file (`tctl auth sign` for
	// a bot/role with access to the target nodes). A file secret on the keeper's
	// disk.
	IdentityFile string `yaml:"identity_file" json:"identity_file"`
	// Cluster is the name of the Teleport cluster in which node names are
	// resolved.
	Cluster string `yaml:"cluster" json:"cluster"`
	// UseSystemTrust — the Teleport Proxy sits BEHIND a public L7-TLS load
	// balancer (ADR-063 amendment). Optional, default false (back-compat).
	//
	// false: the proxy server-cert is verified via the identity-CA + sentinel
	// ServerName `teleport.cluster.local` (valid for a Teleport-issued
	// proxy-cert).
	//
	// true: the balancer presents its own public cert (e.g. `*.tp.rwb.ru`, with no
	// SAN `teleport.cluster.local`) — verification shifts to the system trust
	// store by host from `proxy_addr`. The mTLS client-cert (auth to the proxy)
	// and the SSH host-CA from the identity are preserved: the proxy server-cert
	// is not a Soul Stack trust boundary.
	UseSystemTrust bool `yaml:"use_system_trust,omitempty" json:"use_system_trust,omitempty"`
	// AlpnUpgrade — the L7-TLS load balancer in front of the Proxy terminates TLS
	// and does NOT proxy a raw gRPC/SSH stream (ADR-063 amendment). Optional,
	// default false (back-compat).
	//
	// true: the connection to the Proxy is wrapped in an ALPN conn-upgrade (a
	// WebSocket tunnel on `/webapi/connectionupgrade`), which the L7-LB passes —
	// without it DialHost fails with `403 / content-type "text/plain"`. The inner
	// gRPC-mTLS is configured by the alpn branch itself (combined trust:
	// identity-CA ∪ system), so for a proxy-behind-L7-LB a single
	// `alpn_upgrade: true` suffices; `use_system_trust` is a no-op when alpn is
	// on — it applies only with alpn off (the LB proxies raw gRPC as-is).
	AlpnUpgrade bool `yaml:"alpn_upgrade,omitempty" json:"alpn_upgrade,omitempty"`
}

// KeeperPushCARef is one element of the multi-CA `push.host_ca_refs[]` (S7-3).
//
// `Ref` — a vault-ref (`vault:<mount>/<path>`) to the public host CA (Vault KV
// field `public_key`, symmetric with the singular `host_ca_ref`).
//
// `Name` — an operator-defined kebab-case name, used as a label value in
// `keeper_push_host_ca_used_total{ca_name=...}` and in diag messages. Must be
// unique in the set (schema-phase validation).
type KeeperPushCARef struct {
	Ref  string `yaml:"ref"  json:"ref"`
	Name string `yaml:"name" json:"name"`
}

// DefaultHostCAName is the auto-name for the backward-compat auto-adapt of the
// singular `push.host_ca_ref` into `host_ca_refs[0]` (S7-3 deprecation window,
// ADR-032 amendment 2026-05-26). A kebab-case name that passes the same
// validation as operator-defined names in the multi-CA set.
const DefaultHostCAName = "default"

// KeeperPushTarget holds the SSH credentials of one push host (`sid` = FQDN, the
// same as in the `souls` registry). Pilot form: inline in keeper.yml; S7 will
// replace it with `souls.ssh_target jsonb`.
//
// The SSHPort / SSHUser / SoulPath defaults are applied at resolve time (see
// keeper/internal/push.ConfigTargetResolver), not in the schema phase: the
// operator may omit any field, and the standard value is then substituted
// (22 / root / /usr/local/bin/soul).
type KeeperPushTarget struct {
	SID      string `yaml:"sid"                json:"sid"`
	SSHPort  int    `yaml:"ssh_port,omitempty" json:"ssh_port,omitempty"`
	SSHUser  string `yaml:"ssh_user,omitempty" json:"ssh_user,omitempty"`
	SoulPath string `yaml:"soul_path,omitempty" json:"soul_path,omitempty"`
}

// KeeperCloudInit holds the cloud-init userdata render parameters (ADR-017(h)
// amendment 2026-05-27, B-flat). All fields are required at use time (a scenario
// with `generate_userdata: true`); an empty value → fail-fast with a clear error
// at the GenerateUserdata call, not a silent render of "under-userdata".
//
// `BootstrapEndpoint` — the `host:port` of the LB through which the Soul agent
// will call the Bootstrap RPC (ADR-012(b), a separate listener) AFTER install.
// It renders into userdata as `keeper.endpoints[0]` (host + bootstrap_port).
//
// `EventStreamPort` — the TCP port of the EventStream phase (mTLS listener) for
// the same host; goes into `keeper.endpoints[0].event_stream_port` in soul.yml.
// 0/omitted → the port from `bootstrap_endpoint` is used (back-compat: single-
// port LB). Without it, soul dialed EventStream on the Bootstrap port
// ("Unimplemented: method EventStream") — the 6th wall of ADR-063.
//
// `TLSCARef` — a vault-ref (`vault:<mount>/<path>`) to the Keeper PEM CA. At the
// GenerateUserdata call it is resolved via the keeper-vault client (Vault KV
// field `ca`), and the result is baked into userdata as
// `write_files: /etc/soul/tls/keeper-ca.pem`. The CA is public material (not a
// secret), but a single source of truth in Vault is needed for rotation without
// editing keeper.yml.
//
// `SoulBinaryURL` — the HTTPS URL from which the VM downloads the `soul` binary
// (curl in runcmd). Plain http is rejected at GenerateUserdata (TLS only).
//
// `SoulBinaryCA` — which trust store curl uses when downloading the binary:
//   - `keeper` (default, empty value — back-compat secure-default) — pin to the
//     Keeper PEM CA (`--cacert /etc/soul/tls/keeper-ca.pem`); suits a self-hosted
//     artifact host with the same CA as Keeper.
//   - `system` — the OS trust bundle (curl without `--cacert`); for artifact
//     hosts with a public CA (e.g. a binary on Nexus behind a GlobalSign cert).
//
// `soul_binary_ca: system` weakens ONLY the artifact-host certificate
// verification during the curl binary download. The bootstrap channel
// (souls↔keeper mTLS) is ALWAYS pinned to the keeper CA, independent of this
// field, and the binary SHA256-verify is unaffected. `system` is still
// system-CA-over-TLS, not plain-http.
//
// `SoulVersion` — an optional string that lands in userdata as a comment (for
// diagnostics); the fingerprint check is deferred (see the ADR-017(h) amendment,
// soul-binary signature verification — a separate slice).
type KeeperCloudInit struct {
	BootstrapEndpoint string `yaml:"bootstrap_endpoint"`
	EventStreamPort   int    `yaml:"event_stream_port,omitempty"`
	TLSCARef          string `yaml:"tls_ca_ref"`
	SoulBinaryURL     string `yaml:"soul_binary_url"`
	SoulBinaryCA      string `yaml:"soul_binary_ca,omitempty"`
	SoulVersion       string `yaml:"soul_version,omitempty"`
}

// KeeperPushProvider holds per-provider params for the SSH-provider plugin's
// env-payload. Pilot form: inline in keeper.yml; S7 will replace it with the
// `push_providers` PG table.
//
// `Name` — a reference to `plugins.ssh_providers[].name` (kebab-case); the exact
// same string the git catalog resolver uses. `Params` is serialized to JSON and
// injected into the plugin's `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS` env var
// (ADR-020 amendment l): `vault-bastion` → `SOUL_SSH_VAULT_BASTION_PARAMS`. The
// contents of `Params` are the provider's own opaque form
// (vault_addr/role/proxy_addr/…).
type KeeperPushProvider struct {
	Name   string         `yaml:"name"   json:"name"`
	Params map[string]any `yaml:"params" json:"params"`
}
