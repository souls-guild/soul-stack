package handlers

// HUMA-NATIVE domain view-DTOs of the INCARNATION domain (T5d-2c-full handler-native). The *Typed
// functions (incarnation_typed.go) return the FLAT domain view structs in this file —
// no legacy generator. Package api projects them into native reply-DTOs (huma_incarnation_reply.go →
// newIncarnationGetReply / newStateHistoryEntry). This cuts the last live handler-layer
// dependency on the legacy generator (the former toIncarnationGetReply/toStateHistoryEntry built
// legacy output; the api converters are gone — the register func builds native directly from the view).
//
// INVARIANTS (★ wire byte-exact, the api projection keeps shape 1:1):
//   - The view carries DOMAIN types (time.Time as-is, map[string]any, string status). The api
//     projection casts the status string → native enum (same underlying string → byte-exact) and
//     wraps map → *map (nil-distinguishability preserved).
//   - date-time created_at/updated_at/last_drift_check_at/scanned_at — NANOSECOND wire
//     (.UTC() without Truncate; incarnation fields are a bare time.Time — truncation would break the byte).
//   - covens — non-nil slice (coalesceCoven → `[]` when nil), like the former DTO.
//   - spec/state run through [audit.MaskSecrets] (defense-in-depth, variant D) exactly
//     like the former toDTO — secrets leave masked, the original stays stored in the DB.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// IncarnationGetView — FLAT domain projection of incarnation for the 200 body of GET /v1/incarnations/
// {name} (also list element and PATCH .../hosts). Package api projects it into native IncarnationGetReply.
// Status — RAW domain string (the native api type holds the enum form). Spec/State/StatusDetails —
// map[string]any (nil → `null` via *map in the projection). CreatedByAID/LastDriftCheckAt/
// LastDriftSummary — pointer-optional. covens — non-nil slice. Traits (operator-set
// labels, ADR-060) and CreatedScenario (start scenario, multi-create mechanism)
// project with omitempty (empty map / empty string → key omitted).
type IncarnationGetView struct {
	ApplyingApplyID    *string
	Covens             []string
	CreatedAt          time.Time
	CreatedByAID       *string
	CreatedScenario    string
	LastDriftCheckAt   *time.Time
	LastDriftSummary   *DriftScanSummaryView
	Name               string
	Service            string
	ServiceVersion     string
	Spec               map[string]any
	State              map[string]any
	StateSchemaVersion int32
	Status             string
	StatusDetails      map[string]any
	Traits             map[string]any
	UpdatedAt          time.Time
}

// DriftScanSummaryView — native counts aggregate of last_drift_summary (domain form). int (not
// int32) — wire parity. ScannedAt — nanosecond time-wire.
type DriftScanSummaryView struct {
	HostsClean       int
	HostsDrifted     int
	HostsFailed      int
	HostsUnsupported int
	ScannedAt        time.Time
	TotalHosts       int
}

// StateHistoryView — native history.items element (domain form). ChangedByAID — *string
// (empty string → nil → key omitted in the projection). StateBefore/StateAfter — map (nil → `null`).
// CreatedAt — nanosecond time-wire.
type StateHistoryView struct {
	ApplyID      string
	ChangedByAID *string
	CreatedAt    time.Time
	HistoryID    string
	Scenario     string
	StateAfter   map[string]any
	StateBefore  map[string]any
}

// maskWithSchema — the single read-path masking point for spec/state/history payloads
// ([ADR-010] §7.4): when a service secret schema is present, the declarative layer via
// [audit.MaskSecretsWithSchema] (schema+vault+regex with a regex alarm), otherwise
// [audit.MaskSecrets] (vault+regex, no alarm). nil schema → byte-for-byte the former
// behavior (List does not thread a schema — the snapshot is not materialized per element).
func maskWithSchema(payload map[string]any, schema audit.SecretSchema) map[string]any {
	if schema == nil {
		return audit.MaskSecrets(payload)
	}
	return audit.MaskSecretsWithSchema(payload, schema)
}

// toIncarnationGetView projects incarnation into the domain [IncarnationGetView].
// spec/state masking goes through [maskWithSchema] (declarative layer ADR-010
// §7.4 + vault+regex; single defense-in-depth source, parity with the former toDTO).
// schema — the service secret schema ([secretSchemaForIncarnation]); nil → degrades
// to MaskSecrets, BIT-FOR-BIT. date-time — `.UTC()` without Truncate. covens nil → `[]`.
func toIncarnationGetView(inc *incarnation.Incarnation, schema audit.SecretSchema) IncarnationGetView {
	view := IncarnationGetView{
		ApplyingApplyID:    inc.ApplyingApplyID, // ADR-068 §A1: link to the live run
		Covens:             coalesceCoven(inc.Covens),
		CreatedAt:          inc.CreatedAt.UTC(),
		CreatedByAID:       inc.CreatedByAID,
		CreatedScenario:    derefString(inc.CreatedScenario),
		Name:               inc.Name,
		Service:            inc.Service,
		ServiceVersion:     inc.ServiceVersion,
		Spec:               maskWithSchema(inc.Spec, schema),
		State:              maskWithSchema(inc.State, schema),
		StateSchemaVersion: int32(inc.StateSchemaVersion),
		Status:             string(inc.Status),
		StatusDetails:      inc.StatusDetails,
		Traits:             inc.Traits,
		UpdatedAt:          inc.UpdatedAt.UTC(),
		LastDriftSummary:   toDriftScanSummaryView(inc.LastDriftSummary),
	}
	if inc.LastDriftCheckAt != nil {
		t := inc.LastDriftCheckAt.UTC()
		view.LastDriftCheckAt = &t
	}
	return view
}

// toDriftScanSummaryView projects the typed domain [incarnation.DriftScanSummary] into the domain
// view. nil (NULL column) → nil (the api projection omits the key via omitempty). ScannedAt —
// nanosecond wire (the same json contract that scry writes).
func toDriftScanSummaryView(s *incarnation.DriftScanSummary) *DriftScanSummaryView {
	if s == nil {
		return nil
	}
	return &DriftScanSummaryView{
		HostsDrifted:     s.HostsDrifted,
		HostsClean:       s.HostsClean,
		HostsUnsupported: s.HostsUnsupported,
		HostsFailed:      s.HostsFailed,
		TotalHosts:       s.TotalHosts,
		ScannedAt:        s.ScannedAt,
	}
}

// toStateHistoryView projects a state_history row into the domain [StateHistoryView].
// state_before/state_after — via [maskWithSchema] (declarative layer ADR-010
// §7.4 + vault+regex; parity with the former toHistoryDTO). schema — the service
// secret schema (nil → MaskSecrets, BIT-FOR-BIT). changed_by_aid — *string (empty → nil →
// key omitted). created_at — `.UTC()` without Truncate.
func toStateHistoryView(e *incarnation.HistoryEntry, schema audit.SecretSchema) StateHistoryView {
	view := StateHistoryView{
		HistoryID:   e.HistoryID,
		Scenario:    e.Scenario,
		StateBefore: maskWithSchema(e.StateBefore, schema),
		StateAfter:  maskWithSchema(e.StateAfter, schema),
		ApplyID:     e.ApplyID,
		CreatedAt:   e.At.UTC(),
	}
	if e.ChangedByAID != nil && *e.ChangedByAID != "" {
		view.ChangedByAID = e.ChangedByAID
	}
	return view
}
