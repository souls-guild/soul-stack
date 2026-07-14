package audit

// Source is the closed enum of audit-event initiators per ADR-022(b).
// Maps to the `audit_log.source` column in Postgres. Extension
// (`policy_check`, …) is propose-and-wait via docs/naming-rules.md.
//
// Casting an arbitrary string (`Source("hax0r")`) is technically possible — the
// invariant is validated by the [Writer] write-path implementation before
// INSERT (see `keeper/internal/auditpg.NewWriter`).
type Source string

const (
	// SourceSignal — the hot-reload pipeline reacting to SIGHUP (ADR-021).
	// `archon_aid` is always NULL: the operator is at the keyboard on the host,
	// identity is not authenticated by Keeper.
	SourceSignal Source = "signal"

	// SourceAPI — the Operator API HTTP middleware. `archon_aid` comes from the
	// JWT claim `sub`.
	SourceAPI Source = "api"

	// SourceMCP — the MCP handler. `archon_aid` comes from the JWT.
	SourceMCP Source = "mcp"

	// SourceKeeperInternal — Reaper, scheduled tasks, bootstrap.
	// `archon_aid` is always NULL.
	SourceKeeperInternal Source = "keeper_internal"

	// SourceSoulGRPC — the Keeper-side forwarder of events from a Soul over the
	// gRPC EventStream (ADR-012). `archon_aid` is always NULL;
	// `correlation_id` = `apply_id`.
	SourceSoulGRPC Source = "soul_grpc"

	// SourceBackground — a background periodic Reaper rule that initiates a
	// Scry drift check (ADR-031 Slice C, `scry_background`). Semantically
	// distinct from [SourceKeeperInternal]: a background dry_run run is a
	// security signal (an `ApplyRequest{dry_run:true}` is placed on hosts
	// without operator initiative) and must not fold into the general
	// `keeper_internal` audit filter. `archon_aid` is always NULL (no
	// identified initiator), `correlation_id` = the Scry run's `apply_id`.
	SourceBackground Source = "background"

	// SourceConfigBootstrap — a one-shot legacy import on Keeper startup
	// (ADR-032 amendment 2026-05-26, S7-4): migration of inline `keeper.yml`
	// blocks (`push.targets[]` / `push.providers[]`) into PG sources under an
	// explicit opt-in flag (`push.auto_import_legacy_*`). Semantically separate
	// from [SourceKeeperInternal]: a data migration by operator consent is its
	// own security signal (to distinguish from Reaper / scheduled tasks when
	// filtering `GET /v1/audit`). `archon_aid` is always NULL (system action,
	// initiated by the config, not an operator); `correlation_id` is empty.
	SourceConfigBootstrap Source = "config_bootstrap"
)

// Valid reports whether the value is one of the closed enum's MVP values.
func (s Source) Valid() bool {
	switch s {
	case SourceSignal, SourceAPI, SourceMCP, SourceKeeperInternal, SourceSoulGRPC, SourceBackground, SourceConfigBootstrap:
		return true
	}
	return false
}

// RequiresArchonAID reports true for sources where `archon_aid` must be
// **non-NULL** (`SourceAPI` / `SourceMCP` — an operator authenticated via JWT).
// For `signal` / `keeper_internal` / `soul_grpc` the operator is not
// identified, `archon_aid` = NULL.
//
// The helper is not checked inside [Writer.Write]: the invariant is the
// initiator's responsibility (like the ReloadSource cast in shared/config). A
// caller may `assert` via this helper before building an [Event].
func (s Source) RequiresArchonAID() bool {
	return s == SourceAPI || s == SourceMCP
}
