// Incarnation domain bodies: the runtime instance, its runs, its state history
// and the mutations over them.
//
// Status is IncarnationStatus, one of the three enums the UI references by $ref
// (see enums.go). state / status_details / input are `*map[string]interface{}`:
// the distinction between an absent key and a present null is contract here, not
// an accident of typing.
//
// The list/history/runs envelopes (IncarnationListReply, IncarnationHistoryReply,
// IncarnationRunsReply) are named structs with EXACTLY four int32 fields and no
// cursor fields, deliberately narrower than the generic
// shared/api.PagedResponse: keyset pagination belongs to the soul domain, not to
// this one. keeper aliases the generic instantiation onto them so the schema
// carries the contract name.

package wire

import (
	"time"
)

// IncarnationCreateReply — native 202 body for POST /v1/incarnations. apply_id is optional
// (lifecycle.auto_create:false → incarnation goes ready without a run, apply_id omitted).
type IncarnationCreateReply struct {
	ApplyID     *string `json:"apply_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	Incarnation string  `json:"incarnation" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"`       // ← incarnation.IDPattern
}

// IncarnationRunReply — native 202 body for POST .../scenarios/{scenario} (apply_id + echo).
// A run deferred to the maintenance window (ADR-0093 §3) has no run yet: apply_id is
// omitted and voyage_id + schedule_at name the scheduled Voyage that will start it.
type IncarnationRunReply struct {
	ApplyID     string     `json:"apply_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" doc:"the started run; omitted when the run was deferred to the maintenance window"` // ULID (audit.NewULID)
	Incarnation string     `json:"incarnation" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"`                                                                                          // ← incarnation.IDPattern
	Scenario    string     `json:"scenario"`
	VoyageID    string     `json:"voyage_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" doc:"the scheduled Voyage of a run deferred to the maintenance window; cancel it to drop the run"`
	ScheduleAt  *time.Time `json:"schedule_at,omitempty" doc:"when the deferred run is due to start; asked again then, and moved if a freeze was added or a window changed"`
}

// IncarnationUnlockReply — native 200 body for POST .../unlock. status/previous_status —
// native enum IncarnationStatus (exposed via SchemaProvider, wire form is a string). unlocked_at —
// nanosecond time-wire (handler gives .UTC()).
type IncarnationUnlockReply struct {
	ID             string            `json:"id"`
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
	Incarnation string `json:"incarnation" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.IDPattern
	Scenario    string `json:"scenario" pattern:"^[a-z][a-z0-9_]*$"`            // name of the restarted scenario (the last one that failed)
}

// IncarnationDestroyReply — native 202 body for DELETE /v1/incarnations/{id} (apply_id).
type IncarnationDestroyReply struct {
	ApplyID string `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	// Unreleased — present ONLY when teardown was skipped (allow_destroy=true, or a
	// service with lifecycle.auto_destroy disabled): the record is gone but nothing
	// was released. Absent on the regular path, where teardown runs asynchronously.
	Unreleased *UnreleasedResourcesReply `json:"unreleased,omitempty"`
}

// UnreleasedResourcesReply — infrastructure that outlived a force-destroyed
// incarnation (NIM-395). Removing the record and releasing the resource are
// different operations, and force does only the first: the `destroy` scenario
// never runs, so the cloud VMs keep running — and keep billing — while the
// membership relation that named their hosts is deleted with the record.
//
// The same set is written to `incarnation_archive.status_details.unreleased` and
// to the `incarnation.destroy_completed` audit event, which is where to look for
// it after the fact. The whole object is OMITTED when force ran with nothing to
// abandon — an empty `{}` would leave the operator deciding whether it meant
// "checked, clean" or "could not tell".
//
// Each field is a dimension of its own, not a restatement of the others: a
// create that failed before the driver committed its ids leaves registered hosts
// and no `vm_ids`, and a service that provisions no cloud leaves hosts and no
// `provider`. Read them together, not as one number.
type UnreleasedResourcesReply struct {
	Provider string   `json:"provider,omitempty" doc:"cloud Provider name in the registry that owns the VMs below; empty when the incarnation recorded no provisioned cloud — either it provisions none, or the create failed before the driver committed one"`
	VMIDs    []string `json:"vm_ids,omitempty" doc:"provider VM ids NOT destroyed — these machines are still running at the provider and must be reclaimed by hand. Empty does NOT mean no machines exist: the ids are read from incarnation.state, which a failed create never committed"`
	SIDs     []string `json:"sids,omitempty" doc:"member hosts (incarnation_membership) whose souls, seeds and bootstrap tokens were NOT revoked — the membership rows themselves are gone with the record"`
}

// IncarnationGetReply — native body for GET /v1/incarnations/{id} (and PATCH .../hosts, list element).
// Form is 1:1 with the former IncarnationGetReply: covens is always an array (WITHOUT omitempty, never nil);
// created_by_aid/spec/state/status_details — `*map`/`*string` WITHOUT omitempty (nil → `null`);
// created_scenario/traits — WITH omitempty (nil/empty →
// key omitted; traits is a bare map, NOT `*map`, so an empty `{}` gets omitted). created_at/
// updated_at — nanosecond time-wire (handler gives .UTC() without Truncate).
type IncarnationGetReply struct {
	// ApplyingApplyID — apply_id of the in-progress run (ADR-068 §A1); omitempty: nil (no run
	// in progress / terminal) → key omitted. UI opens live-SSE using this apply_id.
	ApplyingApplyID *string   `json:"applying_apply_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	Covens          []string  `json:"covens" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"`                 // ← soul.CovenPattern (per-element)
	CreatedAt       time.Time `json:"created_at"`
	CreatedByAID    *string   `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CreatedScenario string    `json:"created_scenario,omitempty"`                             // starting scenario (multiple-create mechanism); empty → omitted
	// Label — the display caption ([ADR-0085]), free text and mutable via
	// PUT /v1/incarnations/{id}/label. Absent means the row carries none and
	// the consumer shows `name`. NOT the Vault path segment, NOT the RBAC
	// `incarnation=` scope value and NOT the CEL root — `name` is all three.
	Label              *string                 `json:"label,omitempty"`
	ID                 string                  `json:"id" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.IDPattern
	Service            string                  `json:"service"`
	ServiceVersion     string                  `json:"service_version"`
	State              *map[string]interface{} `json:"state"`
	StateSchemaVersion int32                   `json:"state_schema_version"`
	Status             IncarnationStatus       `json:"status"`
	StatusDetails      *map[string]interface{} `json:"status_details"`
	Traits             map[string]interface{}  `json:"traits,omitempty"` // operator-set labels (ADR-060); empty map → omitted
	UpdatedAt          time.Time               `json:"updated_at"`
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

// RunSummaryEntry — native element of runs.items (GET /v1/incarnations/{id}/runs).
// status — aggregate run status (applying/success/failed/partial_failed/cancelled).
// finished_at / started_by_aid — omitempty (nil → key omitted: run still applying /
// initiator removed). Form is symmetric with StateHistoryEntry.
type RunSummaryEntry struct {
	ApplyID      string           `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Scenario     string           `json:"scenario"`
	Status       string           `json:"status" enum:"applying,success,failed,partial_failed,cancelled"`
	StartedAt    time.Time        `json:"started_at"`
	FinishedAt   *time.Time       `json:"finished_at,omitempty"`
	StartedByAID *string          `json:"started_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"`
	Failure      *RunFailureEntry `json:"failure,omitempty"`
}

// RunFailureEntry — why and where a run that did not succeed stopped (NIM-1000),
// read from the run's terminal history row. summary is the one-line reason that
// row records; the other fields are that reason taken apart and are present only
// on a run recorded since they exist. Absent on a successful run and on one whose
// terminal was never recorded. Masked on the write path: a cause the failing
// expression could have quoted a secret into, as the seal detector sees it, is
// withheld (cause_withheld) rather than stored.
type RunFailureEntry struct {
	Summary       string                `json:"summary,omitempty" doc:"the reason as one masked line - the error chain of the failure"`
	Code          string                `json:"code,omitempty" doc:"the run's abort reason, e.g. render_failed, dispatch_failed, no_hosts"`
	Step          *int                  `json:"step,omitempty" doc:"0-based step the run stopped at (hosts[].passage); absent when it stopped outside a step - before its first or after its last"`
	Tasks         []RunFailureTaskEntry `json:"tasks,omitempty" doc:"task chain of a render failure, outermost first: a block and its child, an applier and the destiny task it expanded into"`
	Host          string                `json:"host,omitempty" doc:"SID the failing evaluation ran for"`
	Cell          string                `json:"cell,omitempty" doc:"where the failing evaluation was: the params cell path that failed to render, e.g. args[0], or vars.<name> when the error was raised inside the definition of var <name> - the innermost failing definition, whichever cell or predicate read it; absent when neither"`
	Expression    string                `json:"expression,omitempty" doc:"the CEL expression as the author wrote it"`
	Cause         string                `json:"cause,omitempty" doc:"innermost reason; a fixed sentence when cause_withheld"`
	CauseWithheld bool                  `json:"cause_withheld,omitempty" doc:"the failing expression reads a secret, so the evaluator's own message - which can quote a value - was not recorded"`
}

// RunFailureTaskEntry — one task of RunFailureEntry.tasks.
type RunFailureTaskEntry struct {
	Index        *int                    `json:"index,omitempty" doc:"plan_index of the task (tasks[].plan_index of GET .../runs/{apply_id}/tasks), a loop iteration's own; absent for a task that expands (a block, an applier), for an assert, which renders into no task, and for a loop whose failing iteration is not known"`
	Name         string                  `json:"name,omitempty"`
	Module       string                  `json:"module,omitempty"`
	Destiny      string                  `json:"destiny,omitempty" doc:"the destiny an applier task applies"`
	File         string                  `json:"file,omitempty" doc:"file the task is written in, relative to its artifact: the service for a scenario task (scenario/<name>/main.yml), the destiny the applier above it names for a destiny task"`
	Line         int                     `json:"line,omitempty" doc:"1-based line of the task in file"`
	IncludedFrom []RunFailureSourceEntry `json:"included_from,omitempty" doc:"the include: tasks that spliced this task in, outermost first"`
}

// RunFailureSourceEntry — a line of a file of an artifact.
type RunFailureSourceEntry struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// RunHostStatusEntry — native element of runs/{apply_id}.hosts[]: one row of one host
// in the run, a host having one row per step that reached it, sent or settled by the
// Keeper (per Passage under the deprecated staged model). failed_task_idx (local
// index of the failed task within the row's ApplyRequest) / failed_plan_index (global
// cross-cutting plan_index of the same task) / error_summary are filled on a failed
// or handled row (omitempty: nil → key omitted on any other success row and on a
// running one). status — host-level
// status (planned/claimed/running/dispatched/success/failed/cancelled/orphaned/no_match).
type RunHostStatusEntry struct {
	SID             string  `json:"sid" pattern:"^(keeper|__run__|[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*)$" doc:"host FQDN OR synthetic run sid (keeper=on:keeper, __run__=run-sentinel abort to dispatch), not addressing a Soul (NIM-36)"`
	Status          string  `json:"status"`
	Passage         int     `json:"passage" doc:"0-based index of the step this row was dispatched for, a host having one row per step that reached it, sent or settled by the Keeper without a send (the Passage index for a run under the deprecated staged model). Values can have holes - nothing about them is contiguous"`
	FailedTaskIdx   *int    `json:"failed_task_idx,omitempty"`
	FailedPlanIndex *int    `json:"failed_plan_index,omitempty" doc:"plan_index of the first task that failed on this row (a later failure on the same row does not overwrite it); correlates with tasks[].plan_index"`
	ErrorSummary    *string `json:"error_summary,omitempty"`
	Attempt         int32   `json:"attempt" doc:"fencing epoch of the last work-queue claim of this row; 0 if the row was never claimed (sent inline, keeper-side, or still planned). It grows across a host's rows of one run, so a later step of a host can start above 1 without ever having been re-claimed"`
	CancelRequested bool    `json:"cancel_requested"`
	Dropped         bool    `json:"dropped,omitempty" doc:"true on the failed or orphaned row of a host the run dropped under on_failure: host and finished without; absent on every other row, a failed row of a run that ended early included"`
	Handled         bool    `json:"handled,omitempty" doc:"true on a row whose failures a rescue handled: a failed or orphaned row that is no failure of the run, and a success row the Soul finished after handling a failure inside its message. Such a row keeps failed_task_idx, failed_plan_index and error_summary as its first failure; absent on every other row"`
	// Notices — advisory findings this host reported during the run (NIM-237),
	// deduplicated by (code, module, param). Unlike error_summary, INDEPENDENT of
	// status: the task ran and succeeded, and something about how it was asked is
	// on its way out. omitempty — a run with nothing to say looks exactly as it
	// did before the field existed.
	Notices []RunNoticeEntry `json:"notices,omitempty"`
}

// RunNoticeEntry — one advisory finding on a run (NIM-237): today only
// `deprecated_param`, reported by the Soul that holds the manifest its params
// were checked against. message carries the deadline and the replacement,
// rendered by that same side so every surface says it identically.
type RunNoticeEntry struct {
	Code    string `json:"code"`
	Module  string `json:"module"`
	Param   string `json:"param,omitempty"`
	Message string `json:"message"`
}

// RunDetailReply — native body for GET /v1/incarnations/{id}/runs/{apply_id}: run
// header (apply_id/scenario/status/time/initiator) + a slice of hosts. hosts is non-nil
// (an empty run with no host rows is impossible — SelectRunDetail would return not-found).
// input omitempty — the masked snapshot of the operator input for the run (secret
// masking on the write path, ***MASKED*** for secrets); nil for old runs / input-less
// paths.
type RunDetailReply struct {
	ApplyID      string                  `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Scenario     string                  `json:"scenario"`
	Status       string                  `json:"status" enum:"applying,success,failed,partial_failed,cancelled"`
	StartedAt    time.Time               `json:"started_at"`
	FinishedAt   *time.Time              `json:"finished_at,omitempty"`
	StartedByAID *string                 `json:"started_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"`
	Hosts        []RunHostStatusEntry    `json:"hosts"`
	Input        *map[string]interface{} `json:"input,omitempty"`
	Failure      *RunFailureEntry        `json:"failure,omitempty"`
}

// RunTaskErrorEntry — native error part of a per-host task outcome (FAILED/TIMED_OUT).
// message omitempty: a module may fail without one.
type RunTaskErrorEntry struct {
	Code    string `json:"code"`
	Module  string `json:"module"`
	Message string `json:"message,omitempty"`
}

// RunTaskHostEntry — native element of tasks[].hosts[]: per-host task outcome. output —
// register_data (omitempty: nil for tasks without register:), with the module's
// declared-secret output fields already masked on the write path ([ADR-0083] §8).
// error — only on the failed host (omitempty). status — TASK_STATUS_*
// (keeperv1.TaskStatus).
type RunTaskHostEntry struct {
	SID    string                  `json:"sid" pattern:"^(keeper|__run__|[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*)$" doc:"host FQDN OR synthetic run sid (keeper=on:keeper)"`
	Status string                  `json:"status" enum:"TASK_STATUS_UNSPECIFIED,TASK_STATUS_OK,TASK_STATUS_CHANGED,TASK_STATUS_SKIPPED,TASK_STATUS_FAILED,TASK_STATUS_TIMED_OUT,TASK_STATUS_CANCELLED"`
	Output *map[string]interface{} `json:"output,omitempty"`
	Error  *RunTaskErrorEntry      `json:"error,omitempty"`
}

// RunTaskEntry — native element of tasks[]: plan of one task (host-invariant
// name/module/passage) + per-host results. params omitempty — masked
// operator input parameters of the task (NIM-37 S1b, seal-aware masking on the
// write path); nil for tasks without params. hosts — only hosts with a result in
// audit (pending hosts not included).
type RunTaskEntry struct {
	PlanIndex    int                     `json:"plan_index"`
	Passage      int                     `json:"passage" doc:"0-based index of the step this task was dispatched in, the passage of the run-detail rows it ran on (the Passage index for a run under the deprecated staged model)"`
	ID           string                  `json:"id"`
	Module       string                  `json:"module"`
	Params       *map[string]interface{} `json:"params,omitempty" doc:"masked input of the task; a top-level key whose value differs between hosts reads <per-host>, and params_by_host carries its values"`
	ParamsByHost *RunTaskParamsByHost    `json:"params_by_host,omitempty" doc:"present only when the task's masked input differs between hosts: every leaf of the input with the values it took and the hosts that took each"`
	Waves        [][]string              `json:"waves,omitempty" doc:"present only for a host task of a serial: step sent in more than one wave: the step's hosts per wave in dispatch order, the same on every host task of the step. It lists the hosts of a wave the run ended before; a host the Keeper settled before the first wave without sending it anything is in no wave"`
	Hosts        []RunTaskHostEntry      `json:"hosts"`
}

// RunTaskParamsByHost — a task's input across its hosts (NIM-985). Built from
// masked data only: a sealed cell is one masked value on every host.
type RunTaskParamsByHost struct {
	Hosts int                `json:"hosts" doc:"number of hosts the task was rendered for"`
	Cells []RunTaskParamCell `json:"cells" doc:"leaves in path order; the hunks of one multi-line value follow each other in line order under its path"`
	Texts []RunTaskParamText `json:"texts,omitempty" doc:"base text of every leaf split into hunks (a string on every host, multi-line on at least one, within the diff bounds); replacing each hunk's line range by a host's variant gives that host's text"`
}

// RunTaskParamCell — one leaf of a task's input, or one hunk of a multi-line leaf.
type RunTaskParamCell struct {
	Path      string                `json:"path" doc:"leaf chain, e.g. config.replication.role or acl.users[0].name; a key holding a dot, a bracket or a quote reads [\"a.b\"]; past 32 segments (keys and list indices) the rest of a value is one cell whose value is that subtree"`
	Line      int                   `json:"line,omitempty" doc:"1-based base-text line a hunk starts at; absent for a cell that is not a hunk"`
	LineCount int                   `json:"line_count,omitempty" doc:"base-text lines the hunk replaces; absent or 0 on a hunk is an insertion before line"`
	Same      bool                  `json:"same" doc:"the leaf is equal on every host; values then holds one entry without sids"`
	Values    []RunTaskParamVariant `json:"values" doc:"ordered by host count, largest first, then by first SID"`
}

// RunTaskParamVariant — one value of a cell and the hosts that received it. A
// variant with neither absent nor lines whose value is missing is JSON null.
type RunTaskParamVariant struct {
	Value  any      `json:"value,omitempty"`
	Absent bool     `json:"absent,omitempty" doc:"the hosts have no such key, or no lines at this hunk"`
	Lines  []string `json:"lines,omitempty" doc:"a hunk variant's lines"`
	SIDs   []string `json:"sids,omitempty"`
}

// RunTaskParamText — the base text a path's hunks are line ranges of.
type RunTaskParamText struct {
	Path  string   `json:"path"`
	Lines []string `json:"lines"`
}

// RunTasksReply — native body for GET /v1/incarnations/{id}/runs/{apply_id}/tasks
// (NIM-37): the run's task plan + per-host results joined from audit_log. tasks
// is non-nil (empty plan → `[]`).
type RunTasksReply struct {
	Tasks []RunTaskEntry `json:"tasks"`
}

// IncarnationCreateRequest — Go form of the POST /v1/incarnations body. service
// required; name/covens/input optional. Format of name/service/coven — domain validation
// (422 in CreateTyped). additionalProperties:false (huma default) → unknown field → 400.
// Struct name = contract schema name in OpenAPI (huma DefaultSchemaNamer takes
// reflect.Type.Name() directly) — aligned to the committed hand-written spec (T4b pilot).
//
// `id` lost `required:"true"` with ADR-0079: a create scenario declaring
// `id.template` composes it server-side from input components, and whether it does
// is only known once the service snapshot resolves — past the schema layer.
// The domain still rejects an omitted id when nothing composes one (422
// "field 'id' is required"), so the contract did not loosen, it moved one layer in.
type IncarnationCreateRequest struct {
	ID string `json:"id,omitempty" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"new instance id (kebab-case, immutable); omit when the create scenario declares id.template (ADR-0079) — then it is composed server-side from input components."`
	// label is the optional display caption (ADR-0085): free text, changed later
	// by PUT /v1/incarnations/{id}/label. Unlike `id` it is never composed by
	// an id.template — a template composes an identifier, and a caption is not one.
	Label   *string        `json:"label,omitempty" doc:"Display caption: free text, may carry capitals and spaces (ADR-0085). Omitted means consumers show the name instead. Never used to derive a Vault path, an RBAC scope, a snapshot directory or a CEL root - in particular incarnation.label does not resolve in CEL"`
	Service string         `json:"service" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"service name from registry (ADR-029)"`
	Covens  []string       `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"declared environment tags (ADR-008 amendment a)"`
	Input   map[string]any `json:"input,omitempty" doc:"input for selected create scenario"`
	// Traits — operator-set trait labels of the incarnation (ADR-060 amend R1): map key →
	// scalar | list of scalars. Stored in incarnation.traits (source of truth) and
	// materialized into souls.traits of member hosts. Format/value is validated by the domain
	// (nested object/array → 422). Operational replacement — PUT .../traits.
	Traits map[string]any `json:"traits,omitempty" doc:"operator-set trait labels (key → scalar|list of scalars), ADR-060"`
	// CreateScenario — choice of the start scenario (mechanism of multiple create scenarios).
	// Optional. Empty-choice contract (Phase 2, union removed): a service offering create
	// scenarios (scenario with `create: true`) + empty → 422 create_scenario_required; a
	// service without them + empty → bare incarnation (ready without a run, created_scenario=
	// NULL). Auto-create by the default `create` is gone. A non-empty name must belong to the
	// service's create set, otherwise 422; the choice is saved in incarnation.created_scenario
	// (rerun-last uses it on the create path).
	CreateScenario string `json:"create_scenario,omitempty" pattern:"^[a-z][a-z0-9_]*$" doc:"name of start scenario (mechanism for multiple creates, scenario with create:true). Empty: service offers create scenarios → 422 create_scenario_required; service without them → bare incarnation (ready without run)"`
}

// IncarnationRunRequest — Go form of the POST .../scenarios/{scenario} body. name/scenario
// echoed from the path are ignored (the path is authoritative). input is optional.
// additionalProperties:false → unknown field → 400. The name = the contract schema name (T4b).
type IncarnationRunRequest struct {
	ID       *string        `json:"id,omitempty" doc:"echo path-id (ignored)"`
	Scenario *string        `json:"scenario,omitempty" doc:"echo path-scenario (ignored)"`
	Input    map[string]any `json:"input,omitempty" doc:"scenario input"`
	// ADR-0093 §4, §5.
	InMaintenanceWindow bool `json:"in_maintenance_window,omitempty" doc:"start inside the incarnation's maintenance windows and outside every freeze: when closed now, the run becomes a scheduled Voyage (reply carries voyage_id + schedule_at)"`
	ConfirmFreeze       bool `json:"confirm_freeze,omitempty" doc:"consent to start during an active freeze; without it such a run is refused with 409 freeze-active. Changes nothing outside a freeze"`
}

// IncarnationUnlockRequest — Go form of the POST .../unlock body. reason required; name echo
// is ignored. additionalProperties:false → unknown field → 400. The name = the contract
// schema name (T4b).
type IncarnationUnlockRequest struct {
	ID     *string `json:"id,omitempty" doc:"echo path-id (ignored)"`
	Reason string  `json:"reason" required:"true" minLength:"1" maxLength:"500" doc:"free text confirmation"`
}

// IncarnationUpgradeRequest — Go form of the POST .../upgrade body. to_version required; name
// echo is ignored. additionalProperties:false → unknown field → 400. The name = the contract
// schema name (T4b).
type IncarnationUpgradeRequest struct {
	ID            *string `json:"id,omitempty" doc:"echo path-id (ignored)"`
	ToVersion     string  `json:"to_version" required:"true" doc:"target service version (git-ref)"`
	ConfirmFreeze bool    `json:"confirm_freeze,omitempty" doc:"consent to start the upgrade scenario during an active freeze; without it 409 freeze-active, nothing committed"`
}

// IncarnationRerunLastRequest — Go form of the POST .../rerun-last body. reason
// required; input optional.
type IncarnationRerunLastRequest struct {
	Reason string `json:"reason" required:"true" minLength:"1" maxLength:"500" doc:"free text confirmation"`
	// Input — the operator's input for the restart, used ONLY when the failed
	// attempt's history row carries no replayable snapshot (NIM-408). It is a
	// recovery path, not an override: with a snapshot present, sending input is
	// refused rather than silently ignored, because "rerun that" and "run this
	// instead" are different requests and the second one has its own endpoint.
	Input         map[string]any `json:"input,omitempty" doc:"operator input, accepted only when the attempt cannot be replayed from history"`
	ConfirmFreeze bool           `json:"confirm_freeze,omitempty" doc:"consent to rerun during an active freeze; without it 409 freeze-active and the incarnation stays error_locked"`
}

// IncarnationSetTraitsRequest — Go form of the PUT .../traits body. traits — a full
// replacement of operator-set trait labels (key → scalar|list of scalars); empty/absent
// = clear. The value format (nested forbidden) is validated by the domain → 422.
// additionalProperties:false → unknown field → 400. The name = the contract schema name.
type IncarnationSetTraitsRequest struct {
	Traits map[string]any `json:"traits,omitempty" doc:"full set of trait-tags (key -> scalar|list of scalars); empty/omitted = clear (ADR-060)"`
}

// IncarnationFormPrefillReply — the native 200 body of POST .../form-prefill. Values —
// a map `field → current-value` (only prefill-declared non-secret fields with a
// covered state path; the rest are omitted). The struct name = the contract schema name.
type IncarnationFormPrefillReply struct {
	Values map[string]any `json:"values" doc:"field → current value from incarnation.state (prefill-hint)"`
}

// IncarnationRevealSecretRequest — the body of POST .../secrets/reveal.
type IncarnationRevealSecretRequest struct {
	SecretID string `json:"secret_id" doc:"id of the declared secret: the state_schema field, or <field>.<property> for a collection"`
	Key      string `json:"key,omitempty" doc:"element key of the current-state collection; empty for a scalar secret"`
}

// IncarnationRevealSecretReply — the native 200 body of POST .../secrets/reveal.
// Value — plaintext (a sanctioned reveal: NOT run through MaskSecrets).
type IncarnationRevealSecretReply struct {
	Value string `json:"value" doc:"plaintext value of the secret"`
}

// IncarnationRevealableSecretItem — one item of the discovery response.
type IncarnationRevealableSecretItem struct {
	SecretID   string   `json:"secret_id" doc:"id of the secret (passed as secret_id on reveal)"`
	Label      string   `json:"label" doc:"label for UI"`
	StatePath  string   `json:"state_path" doc:"top-level state_schema field holding the secret (e.g. redis_users)"`
	Collection bool     `json:"collection" doc:"true when reveal needs a key; false for one secret per incarnation"`
	Keys       []string `json:"keys" doc:"allowed keys of the current state (empty for a scalar secret)"`
}

// IncarnationRevealableSecretsReply — the native 200 body of GET .../secrets/revealable.
type IncarnationRevealableSecretsReply struct {
	Items []IncarnationRevealableSecretItem `json:"items" doc:"revealable secrets of the incarnation"`
}

// IncarnationResolveIDRequest — the request body of POST /v1/incarnations/
// resolve-id. Field names mirror IncarnationCreateRequest deliberately: the same
// body describes the same intended create, and gate (a) reads `service`/`covens`
// out of it with the SAME selector, so the two cannot drift on what a request means.
//
// There is no `id`: it is the answer, not a parameter. Covens does not affect the
// composition — it is here so a coven-scoped operator's permission matches on the
// preview exactly as it matches on the create.
type IncarnationResolveIDRequest struct {
	Service        string         `json:"service" required:"true" pattern:"^[a-z0-9][a-z0-9-]{0,62}$" doc:"service name from registry (ADR-029)"`
	CreateScenario string         `json:"create_scenario,omitempty" pattern:"^[a-z][a-z0-9_]*$" doc:"chosen create scenario, whose id.template composes the id"`
	Input          map[string]any `json:"input,omitempty" doc:"the create input so far — partial is expected, this is a live preview"`
	Covens         []string       `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$" maxLength:"63" doc:"declared environment tags of the intended create — scope parity with POST /v1/incarnations, not part of the composition"`
}

// IncarnationResolveIDReply — the native 200 body of the resolve. The struct name
// = the contract schema name.
//
// `composes: false` says the chosen scenario declares no id.template: the
// operator types the id and the form keeps its id field. Every other field is
// then zero.
//
// `valid: false` is the ordinary answer while the operator is still typing, and it
// is a 200, not a 422 — a form that errors on every keystroke has no live preview.
// `invalid_reason` always says why, so the preview is never a silently empty box;
// `composed_id` still carries the offending value when there is one, so the
// character count has something to count.
//
// The template TEXT is not in this reply on purpose: the operator is shown the
// id, not the formula (NIM-340).
//
// `available` and `taken_by_service` are meaningful only when `valid`.
// `taken_by_service` is omitted for a caller who cannot see the occupying
// incarnation — they still learn the name is taken, they just do not get a report
// on someone else's estate.
type IncarnationResolveIDReply struct {
	Composes       bool   `json:"composes" doc:"the chosen create scenario composes the id from id.template (ADR-0079); false → the operator names the incarnation"`
	ComposedID     string `json:"composed_id" doc:"the id a create with this input would produce; carries the offending value when invalid"`
	Length         int    `json:"length" doc:"character count of composed_id"`
	MaxLength      int    `json:"max_length" doc:"the incarnation id ceiling — server-sourced so the form does not restate it"`
	Valid          bool   `json:"valid" doc:"composed_id is a legal incarnation id"`
	InvalidReason  string `json:"invalid_reason,omitempty" doc:"why the id could not be composed or was rejected, in operator terms"`
	Available      bool   `json:"available" doc:"no incarnation holds this id (meaningful only when valid)"`
	TakenByService string `json:"taken_by_service,omitempty" doc:"service of the incarnation holding the id — only when the caller may see it"`
}

// IncarnationMemberBindRequest — Go form of the POST .../members body. bound_by_aid is
// NOT taken from the body (it comes from the JWT). SID format, the caller's soul scope and
// host status are domain validation. Struct name = contract schema name in OpenAPI.
type IncarnationMemberBindRequest struct {
	SIDs []string `json:"sids" required:"true" minItems:"1" maxItems:"200" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$" doc:"SIDs (FQDN) of already-onboarded, connected hosts to bind to this incarnation"`
}

// IncarnationMemberBindReply — the native 200 envelope of POST .../members. The bind is
// idempotent, so the reply splits the outcome: `bound` are the SIDs written by this call,
// `already_member` the ones that were members before it. Both sorted, both always
// present (`[]`, never null).
type IncarnationMemberBindReply struct {
	Incarnation   string   `json:"incarnation"`
	Bound         []string `json:"bound"`
	AlreadyMember []string `json:"already_member"`
}

// IncarnationMember — the native wire form of one roster entry. `status` is the HOST's
// lifecycle status at read time (membership itself carries no status); bound_at/
// bound_by_aid are the membership audit columns (migration 099).
type IncarnationMember struct {
	SID        string    `json:"sid"`
	Status     string    `json:"status"`
	BoundAt    time.Time `json:"bound_at"`
	BoundByAID *string   `json:"bound_by_aid,omitempty"`
}

// IncarnationMemberListReply — the native 200 envelope of GET .../members. Narrowed to
// the hosts within the caller's soul scope, so `total` is what THIS operator may see, not
// the size of the whole roster.
type IncarnationMemberListReply struct {
	Items  []IncarnationMember `json:"items"`
	Limit  int                 `json:"limit"`
	Offset int                 `json:"offset"`
	Total  int                 `json:"total"`
}

// IncarnationListReply — the alias target schema for the GET /v1/incarnations envelope. The shape is checked against
// the committed hand-written spec (docs/keeper/openapi.yaml → IncarnationListReply): EXACTLY 4
// int32 fields (items/offset/limit/total), all required, with no cursor fields (cursor belongs to the keyset
// domain soul, not incarnation). items.$ref to the contract native element IncarnationGetReply
// (T5a). The type name = the contract schema name (huma DefaultSchemaNamer capitalizes → "IncarnationListReply").
type IncarnationListReply struct {
	Items  []IncarnationGetReply `json:"items" doc:"page of incarnations"`
	Offset int32                 `json:"offset" doc:"offset from start of set"`
	Limit  int32                 `json:"limit" doc:"page size"`
	Total  int32                 `json:"total" doc:"total number of entries in set"`
}

// IncarnationHistoryReply — the alias target schema for the GET /v1/incarnations/{id}/history envelope.
// The shape is checked against the committed hand-written spec (docs/keeper/openapi.yaml → IncarnationHistoryReply):
// EXACTLY 4 int32 fields (items/offset/limit/total), all required, with no cursor fields. items.$ref
// to the contract native element StateHistoryEntry (T5a). The type name = the contract schema name.
type IncarnationHistoryReply struct {
	Items  []StateHistoryEntry `json:"items" doc:"page of state_history entries"`
	Offset int32               `json:"offset" doc:"offset from start of set"`
	Limit  int32               `json:"limit" doc:"page size"`
	Total  int32               `json:"total" doc:"total number of entries in set"`
}

// IncarnationRunsReply — the alias target schema for the GET /v1/incarnations/{id}/runs envelope.
// The same contract shape (4 int32 fields items/offset/limit/total, all required, with no
// cursor fields), items.$ref to the native element RunSummaryEntry. The type name = the contract
// schema name (huma DefaultSchemaNamer capitalizes → "IncarnationRunsReply").
type IncarnationRunsReply struct {
	Items  []RunSummaryEntry `json:"items" doc:"page of incarnation runs (apply_runs fold)"`
	Offset int32             `json:"offset" doc:"offset from start of set"`
	Limit  int32             `json:"limit" doc:"page size"`
	Total  int32             `json:"total" doc:"total number of incarnation runs"`
}
