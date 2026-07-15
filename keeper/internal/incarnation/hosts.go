package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// UpdateHostsMode — operation over the declared `spec.hosts[]` for
// [UpdateHosts] (PATCH /v1/incarnations/{name}/hosts).
//
//   - ModeReplace — full replacement of the list with the given set.
//   - ModeAppend  — add the given hosts; on a matching SID, the existing
//     record's role is updated.
//   - ModeRemove  — remove records with the given SIDs (role in the payload
//     is ignored).
type UpdateHostsMode string

const (
	ModeReplace UpdateHostsMode = "replace"
	ModeAppend  UpdateHostsMode = "append"
	ModeRemove  UpdateHostsMode = "remove"
)

// ValidHostsMode — closed-enum check of mode (handler side maps to 422).
func ValidHostsMode(m UpdateHostsMode) bool {
	switch m {
	case ModeReplace, ModeAppend, ModeRemove:
		return true
	}
	return false
}

// SpecHost — typed record of the declared `spec.hosts[]`. Stored in the jsonb
// field `incarnation.spec.hosts`, its shape mirrors the parser in
// [topology.parseDeclaredRoles] (`{sid, role}`). Role is optional
// (`null`/empty string allowed — ADR-008: declared role can be null
// for hosts outside the declared spec).
type SpecHost struct {
	SID  string `json:"sid"`
	Role string `json:"role,omitempty"`
}

// ErrIncarnationNotEditable — the incarnation's status does not allow spec edits
// (destroying / destroy_failed). Handler side maps this to 409
// incarnation-locked (parity with the Run/Upgrade gate status).
var ErrIncarnationNotEditable = errors.New("incarnation: status does not allow spec edits")

// ErrUnknownSouls — the given SIDs are missing from the `souls` registry. The
// handler maps this to 422; the offending SIDs are returned in .Missing for the message.
type ErrUnknownSouls struct {
	Missing []string
}

func (e *ErrUnknownSouls) Error() string {
	return fmt.Sprintf("incarnation: %d unknown SID(s) in souls registry: %v", len(e.Missing), e.Missing)
}

// hostsEditableStatus — statuses from which editing spec.hosts[] is allowed.
// destroying / destroy_failed are excluded (the incarnation is being torn down, spec
// edits are meaningless). applying is allowed: the spec is the declared input of the
// next run, not the current one; concurrent edit and run are serialized via row-level
// FOR UPDATE (spec.hosts is applied on the next resolve).
func hostsEditableStatus(s Status) bool {
	switch s {
	case StatusDestroying, StatusDestroyFailed:
		return false
	}
	return true
}

// UpdateHostsInput — parameters for [UpdateHosts]. Hosts — the payload (validated
// by the caller for SID/role format and mode semantics); ChangedByAID — the initiating
// Archon (optional; nil → audit-neutral).
type UpdateHostsInput struct {
	Name         string
	Hosts        []SpecHost
	Mode         UpdateHostsMode
	ChangedByAID *string
}

// UpdateHostsResult — outcome of [UpdateHosts]: old/new snapshots for the audit
// payload + the full updated incarnation record for the response.
type UpdateHostsResult struct {
	OldHosts    []SpecHost
	NewHosts    []SpecHost
	Incarnation *Incarnation
}

// UpdateHosts atomically edits an incarnation's declared `spec.hosts[]`
// (ADR-008, UI Hosts editing). Same transactional pattern as
// [Unlock] / [Destroy]: one tx SELECT … FOR UPDATE → status guard →
// SID validation against souls → merge by mode → UPDATE spec/updated_at →
// commit.
//
// Validation:
//   - SID exists in `souls` (validated via a single batch SELECT, not
//     a per-host round-trip): unknown SIDs → [ErrUnknownSouls] (422).
//     For mode=remove the check runs too (guards against a silent no-op
//     on a mistyped SID). For empty hosts (legitimate for replace=empty
//     list) the check is skipped.
//   - Mode — closed enum (the caller side must call [ValidHostsMode]).
//
// Merge:
//   - replace — `spec.hosts` ← payload (including an empty array = clear);
//   - append  — payload is merged into existing; matched by SID, new role
//     overrides the old one (insert-or-update);
//   - remove  — payload SIDs are subtracted from existing (role in the payload
//     is ignored).
//
// Returns:
//   - [ErrIncarnationNotFound]   — name doesn't exist (404).
//   - [ErrIncarnationNotEditable] — status is destroying / destroy_failed (409).
//   - [ErrUnknownSouls]          — the given SIDs aren't in `souls` (422).
//
// Audit (`incarnation.hosts_updated`) is written by the handler (needs AID + source);
// ChangedByAID is passed here for future extension (no state_history row
// is written for hosts edits — this is a spec edit, not a state transition, ADR-009).
func UpdateHosts(ctx context.Context, pool TxBeginner, in UpdateHostsInput) (*UpdateHostsResult, error) {
	if !ValidName(in.Name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", in.Name)
	}
	if !ValidHostsMode(in.Mode) {
		return nil, fmt.Errorf("incarnation: invalid hosts mode %q", in.Mode)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin update-hosts tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// SELECT FOR UPDATE — serialize against concurrent Unlock / Upgrade / Destroy /
	// scenario-runner (they all lock the same row).
	const selectForUpdateSQL = `
SELECT name, service, service_version, state_schema_version,
       spec, state, status, status_details, created_by_aid,
       created_at, updated_at, covens, traits,
       last_drift_check_at, last_drift_summary, created_scenario,
       applying_apply_id
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	inc, err := scanIncarnation(tx.QueryRow(ctx, selectForUpdateSQL, in.Name))
	if err != nil {
		return nil, err
	}

	if !hostsEditableStatus(inc.Status) {
		return nil, ErrIncarnationNotEditable
	}

	// Snapshot of the existing spec.hosts[] for merge + audit payload.
	oldHosts := readSpecHosts(inc.Spec)

	// Validate the payload's SIDs against souls (replace+append+remove — all require
	// the SID to exist, so there's no silent no-op on a typo).
	if len(in.Hosts) > 0 {
		if err := validateSoulsExist(ctx, tx, in.Hosts); err != nil {
			return nil, err
		}
	}

	newHosts := mergeHosts(oldHosts, in.Hosts, in.Mode)

	// Merge only the `hosts` field: other spec keys are preserved. nil spec →
	// initialize it (NOT NULL DEFAULT '{}' guarantees non-null in the DB, but Spec in
	// scanIncarnation can be a nil map when unmarshaling `{}`).
	specOut := inc.Spec
	if specOut == nil {
		specOut = map[string]any{}
	}
	if len(newHosts) == 0 {
		// Save an empty array explicitly (replace with list []) — otherwise the resolver
		// can't distinguish "operator cleared the list" from "hosts never existed".
		specOut["hosts"] = []any{}
	} else {
		out := make([]any, 0, len(newHosts))
		for _, h := range newHosts {
			obj := map[string]any{"sid": h.SID}
			if h.Role != "" {
				obj["role"] = h.Role
			}
			out = append(out, obj)
		}
		specOut["hosts"] = out
	}

	specBytes, err := json.Marshal(specOut)
	if err != nil {
		return nil, fmt.Errorf("incarnation: marshal updated spec: %w", err)
	}

	const updateSQL = `
UPDATE incarnation
SET spec       = $2,
    updated_at = NOW()
WHERE name = $1
RETURNING updated_at
`
	if err := tx.QueryRow(ctx, updateSQL, in.Name, specBytes).Scan(&inc.UpdatedAt); err != nil {
		return nil, fmt.Errorf("incarnation: update hosts: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit update-hosts tx: %w", err)
	}

	inc.Spec = specOut
	return &UpdateHostsResult{
		OldHosts:    oldHosts,
		NewHosts:    newHosts,
		Incarnation: inc,
	}, nil
}

// readSpecHosts extracts the SpecHost list from the freeform jsonb spec. Symmetric with
// [topology.parseDeclaredRoles] (which returns a SID→role map, here it's an ordered
// list). Any shape deviation just skips the element, it is NOT an error (spec is freeform).
func readSpecHosts(spec map[string]any) []SpecHost {
	if spec == nil {
		return nil
	}
	raw, ok := spec["hosts"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]SpecHost, 0, len(arr))
	for _, el := range arr {
		obj, ok := el.(map[string]any)
		if !ok {
			continue
		}
		sid, _ := obj["sid"].(string)
		if sid == "" {
			continue
		}
		role, _ := obj["role"].(string)
		out = append(out, SpecHost{SID: sid, Role: role})
	}
	return out
}

// mergeHosts applies mode to the existing list. Preserves the existing
// order → new ones are appended at the end (append semantics are stable for the UI);
// remove preserves the order of what remains; replace fully overwrites.
func mergeHosts(existing, payload []SpecHost, mode UpdateHostsMode) []SpecHost {
	switch mode {
	case ModeReplace:
		// Copy of payload so the caller doesn't share a slice with tx.
		out := make([]SpecHost, len(payload))
		copy(out, payload)
		return out

	case ModeAppend:
		// Index existing by SID for O(1) lookup; updates override role,
		// new SIDs are appended at the end preserving payload order.
		idx := make(map[string]int, len(existing))
		for i, h := range existing {
			idx[h.SID] = i
		}
		out := make([]SpecHost, len(existing))
		copy(out, existing)
		for _, h := range payload {
			if i, ok := idx[h.SID]; ok {
				out[i].Role = h.Role
				continue
			}
			idx[h.SID] = len(out)
			out = append(out, h)
		}
		return out

	case ModeRemove:
		drop := make(map[string]struct{}, len(payload))
		for _, h := range payload {
			drop[h.SID] = struct{}{}
		}
		out := make([]SpecHost, 0, len(existing))
		for _, h := range existing {
			if _, rm := drop[h.SID]; rm {
				continue
			}
			out = append(out, h)
		}
		return out
	}
	// ValidHostsMode already filtered out unknown; unreachable.
	return existing
}

// validateSoulsExist checks that all SIDs exist in the `souls` registry.
// One batch SELECT (`= ANY($1)`), not a per-host round-trip. Empty in → no-op.
// Duplicates in the payload are harmless (PG IN handles them fine); the order of Missing
// matches the order of first occurrence in the payload (stable for tests).
func validateSoulsExist(ctx context.Context, db ExecQueryRower, payload []SpecHost) error {
	if len(payload) == 0 {
		return nil
	}
	// Dedup + preserve the order of first occurrence for Missing.
	seen := make(map[string]struct{}, len(payload))
	sids := make([]string, 0, len(payload))
	for _, h := range payload {
		if _, ok := seen[h.SID]; ok {
			continue
		}
		seen[h.SID] = struct{}{}
		sids = append(sids, h.SID)
	}

	const sql = `SELECT sid FROM souls WHERE sid = ANY($1)`
	rows, err := db.Query(ctx, sql, sids)
	if err != nil {
		return fmt.Errorf("incarnation: souls existence query: %w", err)
	}
	defer rows.Close()

	found := make(map[string]struct{}, len(sids))
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return fmt.Errorf("incarnation: souls existence scan: %w", err)
		}
		found[sid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("incarnation: souls existence iter: %w", err)
	}

	var missing []string
	for _, sid := range sids {
		if _, ok := found[sid]; !ok {
			missing = append(missing, sid)
		}
	}
	if len(missing) > 0 {
		return &ErrUnknownSouls{Missing: missing}
	}
	return nil
}
