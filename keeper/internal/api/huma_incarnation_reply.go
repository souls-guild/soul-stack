package api

// HUMA-NATIVE reply-DTO for the INCARNATION domain (T5d-2c-full handler-native). Reply/output
// Body of huma operations — native Go structs in package api, WITHOUT legacy-gen. The register
// func (huma_incarnation.go) projects FLAT domain handlers.*View → these native types DIRECTLY
// (newX(view)) — there are no more legacy-gen→native converters. Key points for incarnation:
//
//   - FORM byte-for-byte = former legacy-gen (json tags / omitempty (nil → key omitted) vs
//     without omitempty (nil `*map`/`*string` → `null`) / date-time RFC3339Nano / categories A-D ADR-051).
//   - SCHEMA NAME = contractual (IncarnationCreateReply / IncarnationGetReply / ...): huma
//     DefaultSchemaNamer takes reflect.Type.Name() and capitalizes the first letter → schema gets
//     the same name the former legacy-gen gave. The aggregator spec (TestFullSpec_) is unchanged.
//   - STATUS FIELDS — NATIVE enum IncarnationStatus (huma_enums.go) with a $ref to the named
//     schema "IncarnationStatus" (SchemaProvider). The projection casts the domain status string →
//     native enum (same underlying string → byte-exact). The former alias IncarnationStatus →
//     native is no longer needed (not a single oapi field remains in the reflected Body).
//
// OUTPUT-PATTERN (documentation only, NOT runtime validation): huma does NOT validate the
// response body against the schema (writeResponse → Transform → Marshal, no Validate;
// empirically 200, not 500). `pattern:` on output ID fields is purely format documentation
// for client codegen. apply_id/history_id — machine-generated ULIDs
// (audit.NewULID, migration 006: "history_id (ULID …)"), format guaranteed.
// *_by_aid ← operator.AIDPattern (migration 058 — the current pattern is a superset of the
// old one, legacy AIDs match too). golden byte-exact stays intact: the pattern tag doesn't
// affect json.Marshal.
//
// OUTPUT-PATTERN NAMES (batch 5): incarnation_name (Name + echo Incarnation) ←
// incarnation.IDPattern; covens[] ← soul.CovenPattern (per-element, output covens in
// Incarnation* View/Reply). Reply types are output-only (create/run/upgrade/rerun-last —
// separate *Request/*Input) → no input-422 risk. service — FK to serviceregistry,
// format covered by the INPUT domain (incarnation.create service, batch 4) — output echo is
// NOT tagged (outside the name-scope of Service-View for this batch).

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (form 1:1 with the former legacy-gen form) ===

// === nested reply-DTO ===

// === projection of domain handlers.*View → native wire-DTO (byte-exact passthrough of form) ===

func newIncarnationCreateReply(v handlers.IncarnationCreateView) IncarnationCreateReply {
	return IncarnationCreateReply{ApplyID: v.ApplyID, Incarnation: v.Incarnation}
}

func newIncarnationRunReply(v handlers.IncarnationRunView) IncarnationRunReply {
	return IncarnationRunReply{ApplyID: v.ApplyID, Incarnation: v.Incarnation, Scenario: v.Scenario}
}

func newIncarnationUnlockReply(v handlers.IncarnationUnlockView) IncarnationUnlockReply {
	return IncarnationUnlockReply{
		ID:             v.ID,
		PreviousStatus: IncarnationStatus(v.PreviousStatus),
		Status:         IncarnationStatus(v.Status),
		UnlockedAt:     v.UnlockedAt,
		UnlockedByAID:  v.UnlockedByAID,
	}
}

func newIncarnationUpgradeReply(v handlers.IncarnationUpgradeView) IncarnationUpgradeReply {
	return IncarnationUpgradeReply{ApplyID: v.ApplyID, RunApplyID: v.RunApplyID}
}

// newIncarnationUpgradePathsReply projects the domain handlers.IncarnationUpgradePaths-
// View into native (ADR-0068 §6). Paths/Target are mutually exclusive (omitempty): cheap →
// paths, on-demand → target. state_migrations reuse StateSchemaMigration.
func newIncarnationUpgradePathsReply(v handlers.IncarnationUpgradePathsView) IncarnationUpgradePathsReply {
	out := IncarnationUpgradePathsReply{
		CurrentVersion:            v.CurrentVersion,
		CurrentStateSchemaVersion: v.CurrentStateSchemaVersion,
	}
	if v.Paths != nil {
		paths := make([]UpgradePathRef, 0, len(v.Paths))
		for _, p := range v.Paths {
			paths = append(paths, UpgradePathRef{Ref: p.Ref, Type: p.Type, Commit: p.Commit, IsCurrent: p.IsCurrent})
		}
		out.Paths = paths
	}
	if v.Target != nil {
		t := v.Target
		migs := make([]StateSchemaMigration, 0, len(t.StateMigrations))
		for _, m := range t.StateMigrations {
			migs = append(migs, StateSchemaMigration{From: m.From, Path: m.Path, To: m.To})
		}
		out.Target = &UpgradePathTarget{
			To:                       t.To,
			ResolvedCommit:           t.ResolvedCommit,
			TargetStateSchemaVersion: t.TargetStateSchemaVersion,
			Direction:                t.Direction,
			Mode:                     t.Mode,
			Slug:                     t.Slug,
			Downgrade:                t.Downgrade,
			Reachable:                t.Reachable,
			UnreachableReason:        t.UnreachableReason,
			StateMigrations:          migs,
		}
	}
	return out
}

func newIncarnationRerunLastReply(v handlers.IncarnationRerunLastView) IncarnationRerunLastReply {
	return IncarnationRerunLastReply{ApplyID: v.ApplyID, Incarnation: v.Incarnation, Scenario: v.Scenario}
}

func newIncarnationDestroyReply(v handlers.IncarnationDestroyView) IncarnationDestroyReply {
	out := IncarnationDestroyReply{ApplyID: v.ApplyID}
	if v.Unreleased != nil {
		out.Unreleased = &UnreleasedResourcesReply{
			Provider: v.Unreleased.Provider,
			VMIDs:    v.Unreleased.VMIDs,
			SIDs:     v.Unreleased.SIDs,
		}
	}
	return out
}

// newIncarnationGetReply projects the flat domain handlers.IncarnationGetView into native.
// map fields spec/state/status_details are wrapped in *map (nil → `null` WITHOUT omitempty).
// status — native enum cast (same underlying string).
func newIncarnationGetReply(v handlers.IncarnationGetView) IncarnationGetReply {
	return IncarnationGetReply{
		ApplyingApplyID:    v.ApplyingApplyID,
		Covens:             v.Covens,
		CreatedAt:          v.CreatedAt,
		CreatedByAID:       v.CreatedByAID,
		CreatedScenario:    v.CreatedScenario,
		Label:              v.Label,
		ID:                 v.ID,
		Service:            v.Service,
		ServiceVersion:     v.ServiceVersion,
		State:              ptrMap(v.State),
		StateSchemaVersion: v.StateSchemaVersion,
		Status:             IncarnationStatus(v.Status),
		StatusDetails:      ptrMap(v.StatusDetails),
		Traits:             v.Traits,
		UpdatedAt:          v.UpdatedAt,
	}
}

// newStateHistoryEntry projects the domain handlers.StateHistoryView into native. state_before/
// state_after are wrapped in *map (nil → `null`); changed_by_aid as-is (nil → key omitted).
func newStateHistoryEntry(v handlers.StateHistoryView) StateHistoryEntry {
	return StateHistoryEntry{
		ApplyID:      v.ApplyID,
		ChangedByAID: v.ChangedByAID,
		CreatedAt:    v.CreatedAt,
		HistoryID:    v.HistoryID,
		Scenario:     v.Scenario,
		StateAfter:   ptrMap(v.StateAfter),
		StateBefore:  ptrMap(v.StateBefore),
	}
}

// === runs reply-DTO (list of incarnation runs + per-host details) ===

// newRunSummaryEntry projects the domain handlers.RunSummaryView into native.
func newRunSummaryEntry(v handlers.RunSummaryView) RunSummaryEntry {
	return RunSummaryEntry{
		ApplyID:      v.ApplyID,
		Scenario:     v.Scenario,
		Status:       v.Status,
		StartedAt:    v.StartedAt,
		FinishedAt:   v.FinishedAt,
		StartedByAID: v.StartedByAID,
	}
}

// newRunDetailReply projects the domain handlers.RunDetailView into native (header +
// hosts). hosts is always materialized as a non-nil slice (byte-exact `[]` at 0
// length doesn't occur — see RunDetailReply).
func newRunDetailReply(v handlers.RunDetailView) RunDetailReply {
	hosts := make([]RunHostStatusEntry, len(v.Hosts))
	for i, hs := range v.Hosts {
		// nil rather than an empty slice when there is nothing to report:
		// omitempty then drops the key entirely, keeping the body of an
		// unaffected run byte-for-byte what it was.
		var notices []RunNoticeEntry
		for _, n := range hs.Notices {
			notices = append(notices, RunNoticeEntry{
				Code:    n.Code,
				Module:  n.Module,
				Param:   n.Param,
				Message: n.Message,
			})
		}
		hosts[i] = RunHostStatusEntry{
			SID:             hs.SID,
			Status:          hs.Status,
			Passage:         hs.Passage,
			FailedTaskIdx:   hs.FailedTaskIdx,
			FailedPlanIndex: hs.FailedPlanIndex,
			ErrorSummary:    hs.ErrorSummary,
			Attempt:         hs.Attempt,
			CancelRequested: hs.CancelRequested,
			Notices:         notices,
		}
	}
	var input *map[string]interface{}
	if v.Input != nil {
		input = &v.Input
	}
	return RunDetailReply{
		ApplyID:      v.ApplyID,
		Scenario:     v.Scenario,
		Status:       v.Status,
		StartedAt:    v.StartedAt,
		FinishedAt:   v.FinishedAt,
		StartedByAID: v.StartedByAID,
		Hosts:        hosts,
		Input:        input,
	}
}

// === run tasks reply-DTO (run plan + per-host results) — NIM-37 ===

// newRunTasksReply projects the domain handlers.RunTasksView into native. tasks/hosts
// are materialized as non-nil slices (empty plan → `[]`).
func newRunTasksReply(v handlers.RunTasksView) RunTasksReply {
	tasks := make([]RunTaskEntry, len(v.Tasks))
	for i, t := range v.Tasks {
		hosts := make([]RunTaskHostEntry, len(t.Hosts))
		for j, hs := range t.Hosts {
			he := RunTaskHostEntry{SID: hs.SID, Status: hs.Status, Output: ptrMap(hs.Output)}
			if hs.Error != nil {
				he.Error = &RunTaskErrorEntry{Code: hs.Error.Code, Module: hs.Error.Module, Message: hs.Error.Message}
			}
			hosts[j] = he
		}
		tasks[i] = RunTaskEntry{
			PlanIndex: t.PlanIndex,
			Passage:   t.Passage,
			ID:        t.Name,
			Module:    t.Module,
			Params:    ptrMap(t.Params),
			Hosts:     hosts,
		}
	}
	return RunTasksReply{Tasks: tasks}
}

// ptrMap wraps a domain `map[string]any` into `*map[string]interface{}`, preserving nil-distinguishability:
// nil map → nil pointer (json tag without omitempty → `null`), non-empty → pointer to the same map.
func ptrMap(m map[string]any) *map[string]interface{} {
	if m == nil {
		return nil
	}
	cp := map[string]interface{}(m)
	return &cp
}
