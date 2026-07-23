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
// incarnation.NamePattern; covens[] ← soul.CovenPattern (per-element, output covens in
// Incarnation* View/Reply). Reply types are output-only (create/run/upgrade/rerun-last —
// separate *Request/*Input) → no input-422 risk. service — FK to serviceregistry,
// format covered by the INPUT domain (incarnation.create service, batch 4) — output echo is
// NOT tagged (outside the name-scope of Service-View for this batch).

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (form 1:1 with the former legacy-gen form) ===

// IncarnationCreateReply — native 202 body for POST /v1/incarnations. apply_id is optional
// (lifecycle.auto_create:false → incarnation goes ready without a run, apply_id omitted).
type IncarnationCreateReply struct {
	ApplyID     *string `json:"apply_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	Incarnation string  `json:"incarnation" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"`       // ← incarnation.NamePattern
}

// IncarnationRunReply — native 202 body for POST .../scenarios/{scenario} (apply_id + echo).
type IncarnationRunReply struct {
	ApplyID     string `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`     // ULID (audit.NewULID)
	Incarnation string `json:"incarnation" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.NamePattern
	Scenario    string `json:"scenario"`
}

// IncarnationUnlockReply — native 200 body for POST .../unlock. status/previous_status —
// native enum IncarnationStatus (exposed via SchemaProvider, wire form is a string). unlocked_at —
// nanosecond time-wire (handler gives .UTC()).
type IncarnationUnlockReply struct {
	Name           string            `json:"name"`
	PreviousStatus IncarnationStatus `json:"previous_status"`
	Status         IncarnationStatus `json:"status"`
	UnlockedAt     time.Time         `json:"unlocked_at"`
	UnlockedByAID  string            `json:"unlocked_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
}

// IncarnationUpgradeReply — native 202 body for POST .../upgrade. apply_id — M (ULID
// of the state migration, always). run_apply_id — R (ULID of the auto-started upgrade run,
// ADR-0068 §5); omitempty — omitted on the legacy branch (upgrade scenario not found).
type IncarnationUpgradeReply struct {
	ApplyID    string  `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`               // ULID (audit.NewULID)
	RunApplyID *string `json:"run_apply_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID of the Runner run (found branch)
}

// IncarnationUpgradePathsReply — native 200 body for GET .../upgrade-paths (ADR-0068 §6).
// Two modes (omitempty): without ?to= paths is filled (registry tags + is_current); with
// ?to= — target (analysis of a single target). current_* — the incarnation's current pin/schema.
type IncarnationUpgradePathsReply struct {
	CurrentVersion            string             `json:"current_version"`
	CurrentStateSchemaVersion int                `json:"current_state_schema_version"`
	Paths                     []UpgradePathRef   `json:"paths,omitempty"`
	Target                    *UpgradePathTarget `json:"target,omitempty"`
}

// UpgradePathRef — one git ref from the service registry (element of paths, cheap mode).
// is_current — ref == the incarnation's current pin (ADR-0068 §6).
type UpgradePathRef struct {
	Ref       string `json:"ref"`
	Type      string `json:"type"`
	Commit    string `json:"commit"`
	IsCurrent bool   `json:"is_current"`
}

// UpgradePathTarget — on-demand analysis of a single target (?to=). direction — no-op/downgrade/
// forward/same-schema; mode — found/legacy ONLY for forward/same-schema (omitempty:
// meaningless for downgrade/no-op → omitted); slug — present when found; downgrade — target is
// lower on the schema (chain not loaded, forward-only); reachable — target reachable via upgrade
// (false + unreachable_reason only for a broken migration chain — preview shows an
// unreachable target as DATA, not an HTTP error); state_migrations — the chain to apply
// (reuses the native StateSchemaMigration from the state-schema endpoint).
type UpgradePathTarget struct {
	To                       string                 `json:"to"`
	ResolvedCommit           string                 `json:"resolved_commit"`
	TargetStateSchemaVersion int                    `json:"target_state_schema_version"`
	Direction                string                 `json:"direction"`
	Mode                     string                 `json:"mode,omitempty"`
	Slug                     string                 `json:"slug,omitempty"`
	Downgrade                bool                   `json:"downgrade"`
	Reachable                bool                   `json:"reachable"`
	UnreachableReason        string                 `json:"unreachable_reason,omitempty"`
	StateMigrations          []StateSchemaMigration `json:"state_migrations,omitempty"`
}

// IncarnationRerunLastReply — native 202 body for POST .../rerun-last (apply_id + echo + the restarted scenario).
type IncarnationRerunLastReply struct {
	ApplyID     string `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`     // ULID (audit.NewULID)
	Incarnation string `json:"incarnation" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.NamePattern
	Scenario    string `json:"scenario" pattern:"^[a-z][a-z0-9_]*$"`            // name of the restarted scenario (the last one that failed)
}

// IncarnationDestroyReply — native 202 body for DELETE /v1/incarnations/{name} (apply_id).
type IncarnationDestroyReply struct {
	ApplyID string `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// IncarnationGetReply — native body for GET /v1/incarnations/{name} (and PATCH .../hosts, list element).
// Form is 1:1 with the former IncarnationGetReply: covens is always an array (WITHOUT omitempty, never nil);
// created_by_aid/spec/state/status_details — `*map`/`*string` WITHOUT omitempty (nil → `null`);
// last_drift_check_at/last_drift_summary/created_scenario/traits — WITH omitempty (nil/empty →
// key omitted; traits is a bare map, NOT `*map`, so an empty `{}` gets omitted). created_at/
// updated_at — nanosecond time-wire (handler gives .UTC() without Truncate).
type IncarnationGetReply struct {
	// ApplyingApplyID — apply_id of the in-progress run (ADR-068 §A1); omitempty: nil (no run
	// in progress / terminal) → key omitted. UI opens live-SSE using this apply_id.
	ApplyingApplyID    *string                 `json:"applying_apply_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	Covens             []string                `json:"covens" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"`                 // ← soul.CovenPattern (per-element)
	CreatedAt          time.Time               `json:"created_at"`
	CreatedByAID       *string                 `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CreatedScenario    string                  `json:"created_scenario,omitempty"`                             // starting scenario (multiple-create mechanism); empty → omitted
	LastDriftCheckAt   *time.Time              `json:"last_drift_check_at,omitempty"`
	LastDriftSummary   *DriftScanSummary       `json:"last_drift_summary,omitempty"`
	Name               string                  `json:"name" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.NamePattern
	Service            string                  `json:"service"`
	ServiceVersion     string                  `json:"service_version"`
	Spec               *map[string]interface{} `json:"spec"`
	State              *map[string]interface{} `json:"state"`
	StateSchemaVersion int32                   `json:"state_schema_version"`
	Status             IncarnationStatus       `json:"status"`
	StatusDetails      *map[string]interface{} `json:"status_details"`
	Traits             map[string]interface{}  `json:"traits,omitempty"` // operator-set labels (ADR-060); empty map → omitted
	UpdatedAt          time.Time               `json:"updated_at"`
}

// === nested reply-DTO ===

// DriftScanSummary — native counts aggregate for last_drift_summary (form 1:1 with the former
// DriftScanSummary; scanned_at — nanosecond time-wire). int (not int32) — parity.
type DriftScanSummary struct {
	HostsClean       int       `json:"hosts_clean"`
	HostsDrifted     int       `json:"hosts_drifted"`
	HostsFailed      int       `json:"hosts_failed"`
	HostsUnsupported int       `json:"hosts_unsupported"`
	ScannedAt        time.Time `json:"scanned_at"`
	TotalHosts       int       `json:"total_hosts"`
}

// StateHistoryEntry — native element of history.items (form 1:1 with the former StateHistoryEntry):
// changed_by_aid — `*string` WITH omitempty (nil → key omitted); state_before/state_after — `*map`
// WITHOUT omitempty (nil → `null`); created_at — nanosecond time-wire (.UTC()).
type StateHistoryEntry struct {
	ApplyID      string                  `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`                      // ULID (audit.NewULID)
	ChangedByAID *string                 `json:"changed_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CreatedAt    time.Time               `json:"created_at"`
	HistoryID    string                  `json:"history_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (migration 006)
	Scenario     string                  `json:"scenario"`
	StateAfter   *map[string]interface{} `json:"state_after"`
	StateBefore  *map[string]interface{} `json:"state_before"`
}

// === projection of domain handlers.*View → native wire-DTO (byte-exact passthrough of form) ===

func newIncarnationCreateReply(v handlers.IncarnationCreateView) IncarnationCreateReply {
	return IncarnationCreateReply{ApplyID: v.ApplyID, Incarnation: v.Incarnation}
}

func newIncarnationRunReply(v handlers.IncarnationRunView) IncarnationRunReply {
	return IncarnationRunReply{ApplyID: v.ApplyID, Incarnation: v.Incarnation, Scenario: v.Scenario}
}

func newIncarnationUnlockReply(v handlers.IncarnationUnlockView) IncarnationUnlockReply {
	return IncarnationUnlockReply{
		Name:           v.Name,
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
	return IncarnationDestroyReply{ApplyID: v.ApplyID}
}

// newDriftScanSummary projects the domain *handlers.DriftScanSummaryView into native (nil → nil:
// omitempty omits the key).
func newDriftScanSummary(v *handlers.DriftScanSummaryView) *DriftScanSummary {
	if v == nil {
		return nil
	}
	return &DriftScanSummary{
		HostsClean:       v.HostsClean,
		HostsDrifted:     v.HostsDrifted,
		HostsFailed:      v.HostsFailed,
		HostsUnsupported: v.HostsUnsupported,
		ScannedAt:        v.ScannedAt,
		TotalHosts:       v.TotalHosts,
	}
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
		LastDriftCheckAt:   v.LastDriftCheckAt,
		LastDriftSummary:   newDriftScanSummary(v.LastDriftSummary),
		Name:               v.Name,
		Service:            v.Service,
		ServiceVersion:     v.ServiceVersion,
		Spec:               ptrMap(v.Spec),
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

// RunSummaryEntry — native element of runs.items (GET /v1/incarnations/{name}/runs).
// status — aggregate run status (applying/success/failed/cancelled). finished_at
// / started_by_aid — omitempty (nil → key omitted: run still applying / initiator
// removed). Form is symmetric with StateHistoryEntry.
type RunSummaryEntry struct {
	ApplyID      string     `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Scenario     string     `json:"scenario"`
	Status       string     `json:"status" enum:"applying,success,failed,cancelled"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	StartedByAID *string    `json:"started_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"`
}

// RunHostStatusEntry — native element of runs/{apply_id}.hosts[]: status of one host in
// the run. failed_task_idx (local index of the failed task within its Passage) /
// failed_plan_index (global cross-cutting plan_index of the same task) / error_summary
// are filled ONLY on the failed host (omitempty: nil → key omitted on success/running).
// status — host-level status (planned/claimed/running/dispatched/success/failed/
// cancelled/orphaned/no_match).
type RunHostStatusEntry struct {
	SID             string  `json:"sid" pattern:"^(keeper|__run__|[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*)$" doc:"host FQDN OR synthetic run sid (keeper=on:keeper, __run__=run-sentinel abort to dispatch), not addressing a Soul (NIM-36)"`
	Status          string  `json:"status"`
	Passage         int     `json:"passage"`
	FailedTaskIdx   *int    `json:"failed_task_idx,omitempty"`
	FailedPlanIndex *int    `json:"failed_plan_index,omitempty"`
	ErrorSummary    *string `json:"error_summary,omitempty"`
	Attempt         int32   `json:"attempt"`
	CancelRequested bool    `json:"cancel_requested"`
}

// RunDetailReply — native body for GET /v1/incarnations/{name}/runs/{apply_id}: run
// header (apply_id/scenario/status/time/initiator) + a slice of hosts. hosts is non-nil
// (an empty run with no host rows is impossible — SelectRunDetail would return not-found).
// input omitempty — the masked snapshot of the operator input for the run (secret
// masking on the write path, ***MASKED*** for secrets); nil for old runs / input-less
// paths.
type RunDetailReply struct {
	ApplyID      string                  `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Scenario     string                  `json:"scenario"`
	Status       string                  `json:"status" enum:"applying,success,failed,cancelled"`
	StartedAt    time.Time               `json:"started_at"`
	FinishedAt   *time.Time              `json:"finished_at,omitempty"`
	StartedByAID *string                 `json:"started_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"`
	Hosts        []RunHostStatusEntry    `json:"hosts"`
	Input        *map[string]interface{} `json:"input,omitempty"`
}

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
		hosts[i] = RunHostStatusEntry{
			SID:             hs.SID,
			Status:          hs.Status,
			Passage:         hs.Passage,
			FailedTaskIdx:   hs.FailedTaskIdx,
			FailedPlanIndex: hs.FailedPlanIndex,
			ErrorSummary:    hs.ErrorSummary,
			Attempt:         hs.Attempt,
			CancelRequested: hs.CancelRequested,
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

// RunTaskErrorEntry — native error part of a per-host task outcome (FAILED/TIMED_OUT).
// message omitempty: suppressed for a no_log task (may carry a plaintext secret).
type RunTaskErrorEntry struct {
	Code    string `json:"code"`
	Module  string `json:"module"`
	Message string `json:"message,omitempty"`
}

// RunTaskHostEntry — native element of tasks[].hosts[]: per-host task outcome. output —
// register_data (omitempty: nil for tasks without register: / no_log). error — only on
// the failed host (omitempty). status — TASK_STATUS_* (keeperv1.TaskStatus).
type RunTaskHostEntry struct {
	SID    string                  `json:"sid" pattern:"^(keeper|__run__|[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*)$" doc:"host FQDN OR synthetic run sid (keeper=on:keeper)"`
	Status string                  `json:"status" enum:"TASK_STATUS_UNSPECIFIED,TASK_STATUS_OK,TASK_STATUS_CHANGED,TASK_STATUS_SKIPPED,TASK_STATUS_FAILED,TASK_STATUS_TIMED_OUT,TASK_STATUS_CANCELLED"`
	Output *map[string]interface{} `json:"output,omitempty"`
	Error  *RunTaskErrorEntry      `json:"error,omitempty"`
}

// RunTaskEntry — native element of tasks[]: plan of one task (host-invariant
// name/module/no_log/passage) + per-host results. params omitempty — masked
// operator input parameters of the task (NIM-37 S1b, secret masking on the write path);
// nil for no_log tasks and tasks without params. hosts — only hosts with a result in
// audit (pending hosts not included).
type RunTaskEntry struct {
	PlanIndex int                     `json:"plan_index"`
	Passage   int                     `json:"passage"`
	Name      string                  `json:"name"`
	Module    string                  `json:"module"`
	NoLog     bool                    `json:"no_log"`
	Params    *map[string]interface{} `json:"params,omitempty"`
	Hosts     []RunTaskHostEntry      `json:"hosts"`
}

// RunTasksReply — native body for GET /v1/incarnations/{name}/runs/{apply_id}/tasks
// (NIM-37): the run's task plan + per-host results joined from audit_log. tasks
// is non-nil (empty plan → `[]`).
type RunTasksReply struct {
	Tasks []RunTaskEntry `json:"tasks"`
}

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
			Name:      t.Name,
			Module:    t.Module,
			NoLog:     t.NoLog,
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
