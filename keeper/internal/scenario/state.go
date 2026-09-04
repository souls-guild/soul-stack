package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
)

const (
	// covens and traits are in the list because `vars/_stack.yaml` reads BOTH
	// (servicevars.IncarnationContext): every other path to an incarnation goes
	// through incarnation.SelectByID, which has always selected them, so
	// leaving either out here would make the RUN resolve a different set of
	// service vars than the pre-flight gate and the drift check on the same row —
	// and worse, differently from the mid-run re-render of the same run, which
	// goes through RenderForHost -> SelectByName.
	//
	// The two failure modes are not symmetrical, which is why this is a hard rule
	// rather than a preference: `incarnation.traits.env == 'prd'` on a step aborts
	// the run outright (no such key), while `has(incarnation.traits)` quietly
	// resolves one layer fewer and returns success.
	selectIncarnationForUpdateSQL = `
SELECT id, service, service_version, state_schema_version,
       state, status, status_details, created_by_aid,
       created_at, updated_at, covens, traits
FROM incarnation
WHERE id = $1
FOR UPDATE
`
	updateIncarnationStatusSQL = `
UPDATE incarnation
SET status = $2, updated_at = NOW()
WHERE id = $1
`
	// lockApplyingWithEpochSQL moves incarnation to applying and in the SAME UPDATE
	// writes the applying-flag epoch (ADR-027 amend (m-S1)): apply_id / attempt /
	// owner KID / lock-acquisition time. Atomicity is critical: epoch and status
	// change in one row in one tx, so there's no "applying without epoch" window
	// (which reconcile_orphan_applying would mistake for legacy-NULL and skip
	// reclaiming, hanging forever if the lock owner crashes).
	lockApplyingWithEpochSQL = `
UPDATE incarnation
SET status            = 'applying',
    applying_apply_id = $2,
    applying_attempt  = $3,
    applying_by_kid   = $4,
    applying_since    = NOW(),
    updated_at        = NOW()
WHERE id = $1
`
)

// selectForUpdate reads incarnation under FOR UPDATE (guards against concurrent
// runs: while the row is locked by lockRun's transaction, a concurrent Start
// blocks on this SELECT until COMMIT).
func selectForUpdate(ctx context.Context, tx pgx.Tx, name string) (*incarnation.Incarnation, error) {
	row := tx.QueryRow(ctx, selectIncarnationForUpdateSQL, name)
	return scanForUpdate(row)
}

// scanForUpdate parses an incarnation row (a SUBSET of incarnation.SelectByID's
// columns — every field a service-vars resolve reads must be in it, see
// selectIncarnationForUpdateSQL — via a locking SELECT inside the runner's
// transaction — incarnation.scanIncarnation isn't exported, so we duplicate the
// minimum).
func scanForUpdate(row pgx.Row) (*incarnation.Incarnation, error) {
	var (
		inc                incarnation.Incarnation
		statusStr          string
		stateBytes         []byte
		traitsBytes        []byte
		statusDetailsBytes []byte
		createdByAID       *string
	)
	err := row.Scan(
		&inc.ID, &inc.Service, &inc.ServiceVersion, &inc.StateSchemaVersion,
		&stateBytes, &statusStr, &statusDetailsBytes, &createdByAID,
		&inc.CreatedAt, &inc.UpdatedAt, &inc.Covens, &traitsBytes,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, incarnation.ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("scenario: scan incarnation: %w", err)
	}
	inc.Status = incarnation.Status(statusStr)
	inc.CreatedByAID = createdByAID
	if inc.State, err = unmarshalJSONB(stateBytes); err != nil {
		return nil, fmt.Errorf("scenario: unmarshal state: %w", err)
	}
	if inc.Traits, err = unmarshalJSONB(traitsBytes); err != nil {
		return nil, fmt.Errorf("scenario: unmarshal traits: %w", err)
	}
	inc.TraitsRaw = traitsBytes // scope reads the raw jsonb, never the map (NIM-521)
	if len(statusDetailsBytes) > 0 {
		if err := json.Unmarshal(statusDetailsBytes, &inc.StatusDetails); err != nil {
			return nil, fmt.Errorf("scenario: unmarshal status_details: %w", err)
		}
	}
	return &inc, nil
}

// updateStatus moves incarnation to a new status within a transaction (no
// state_history write — this is an "intermediate" transition to applying, not
// a run-result commit).
func updateStatus(ctx context.Context, tx pgx.Tx, name string, status incarnation.Status) error {
	tag, err := tx.Exec(ctx, updateIncarnationStatusSQL, name, string(status))
	if err != nil {
		return fmt.Errorf("scenario: update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return incarnation.ErrIncarnationNotFound
	}
	return nil
}

// lockApplyingWithEpoch moves incarnation to applying and in the SAME UPDATE/tx
// writes the applying-flag epoch (ADR-027 amend (m-S1)): applying_apply_id /
// applying_attempt / applying_by_kid / applying_since. This turns a bare
// applying-bool into an inline epoch that the Reaper's reconcile_orphan_applying
// rule uses to distinguish "run genuinely in progress" (owner alive in Conclave)
// from "owner dead, lock orphaned". CRITICAL: a single Exec — a crash between
// writing status and writing epoch is impossible (one UPDATE is atomic), so
// there's no applying-without-epoch window.
//
// attempt echoes the current apply_runs.attempt; at lockRun time the apply_runs
// row doesn't exist yet (dispatch inserts it later), so this is the run's
// starting attempt. The column is written for parity with apply_runs.attempt
// (groundwork for post-MVP epoch-check on RunResult ingestion); standalone lock
// release doesn't read it — death is proven by presence, FENCING-1 fences by
// apply_id.
func lockApplyingWithEpoch(ctx context.Context, tx pgx.Tx, name, applyID, kid string, attempt int) error {
	tag, err := tx.Exec(ctx, lockApplyingWithEpochSQL, name, applyID, attempt, kid)
	if err != nil {
		return fmt.Errorf("scenario: lock applying with epoch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return incarnation.ErrIncarnationNotFound
	}
	return nil
}

// serviceVarsInput builds [servicevars.ResolveInput] for one incarnation.
//
// It takes no host: a service's vars are host-INVARIANT (ADR-0082). The
// per-host layers this used to select on — the old `os/<family>.yaml` and
// `coven/<label>.yaml` overlays — are gone; a service that needs a host-dependent
// value declares the step in `vars/_stack.yaml`. This is also why the keeper
// context (provision-from-zero, no representative host — ADR-0061 §context) no
// longer needs a resolve input of its own: there is nothing left for it to
// differ in.
func serviceVarsInput(serviceDir string, inc *incarnation.Incarnation) servicevars.ResolveInput {
	return servicevars.ResolveInput{
		ServiceDir:  serviceDir,
		Incarnation: incarnationStackVars(inc),
	}
}

// incarnationStackVars builds the `incarnation.*` step context a
// `vars/_stack.yaml` evaluates against. Deliberately the ROW's own fields —
// `covens` in particular, which is how a step reaches the labels an operator put
// on the incarnation now that the hard-wired `coven/<label>.yaml` overlay is gone
// (ADR-0082). These are the INCARNATION's labels selecting overlays of its own
// vars; they are not, and never become, labels on its member hosts (NIM-281).
func incarnationStackVars(inc *incarnation.Incarnation) servicevars.IncarnationContext {
	if inc == nil {
		return servicevars.IncarnationContext{}
	}
	return servicevars.IncarnationContext{
		// [servicevars.IncarnationContext] is the CEL activation's shape and its
		// field names mirror the CEL keys, which since NIM-730 agree with the
		// column: both are `id` ([ADR-0085]). The retired `incarnation.name`
		// spelling is still readable, but as an alias added at the activation
		// (shared/cel.Vars.incarnationRoot), never as a second field here.
		ID:             inc.ID,
		Service:        inc.Service,
		ServiceVersion: inc.ServiceVersion,
		Covens:         inc.Covens,
		Traits:         inc.Traits,
	}
}

// loadRegisterByHostUpToPassage reads register data for the run accumulated in
// Passages STRICTLY LESS than upToPassage (staged render, ADR-056 §v.1):
// rendering Passage N substitutes the register of all prior Passages per-host.
// upToPassage=0 (first Passage) → empty map (register not collected yet — same
// as up-front render).
//
// task_idx → register-name resolution is the same as in [buildRegisterByHost] (by
// []RenderedTask of the current run). run.go's stage-loop calls this before
// rendering each Passage and passes the result into RenderInput.RegisterByHost.
func (r *Runner) loadRegisterByHostUpToPassage(ctx context.Context, applyID string, upToPassage int, tasks []*render.RenderedTask) (map[string]map[string]any, error) {
	rows, err := applyrun.SelectTaskRegistersByApplyIDUpToPassage(ctx, r.deps.DB, applyID, upToPassage)
	if err != nil {
		return nil, fmt.Errorf("scenario: load run register data (passage < %d): %w", upToPassage, err)
	}
	return buildRegisterByHost(rows, tasks), nil
}

// buildRegisterByHost is a pure fold of a run's register rows into a per-host
// map (sid → register-name → payload) via plan_index→register-name mapping from
// tasks. Split out of the loader for unit testing without PG.
//
// Correlation is by GLOBAL plan_index (ADR-056 §S1 fix Variant B): nameByIdx is
// built from RenderedTask.Index (the global end-to-end index across the whole
// plan), and a register row carries TaskRegister.PlanIndex (echo of
// TaskEvent.plan_index, the same global index). It used to map nameByIdx[t.Index]
// (global) against rows.TaskIdx (LOCAL position within a Passage's ApplyRequest)
// — names desynced on passage>0 (latent bug). Local task_idx doesn't work for
// correlation: it's not unique across Passages or across hosts of the same
// Passage (different where:).
//
// If several tasks on one host share a register name (possible programmatically,
// but the scenario validator rejects it) — the row with the larger plan_index
// wins (SelectTaskRegistersByApplyID sorts by plan_index ASC, later overwrites
// earlier).
//
// Every task with a register: enters nameByIdx. The `no_log:` exclusion that used
// to drop such a task's row wholesale is gone with the key itself ([ADR-0083] §8),
// and it has no per-field successor here on purpose: this same fold feeds the NEXT
// Passage's render (loadRegisterByHostUpToPassage), so dropping a field would break
// the register chain rather than protect it.
//
// What stands in its place is narrower and earlier. A declared secret does not
// travel through a register as plaintext at all — §1 keeps it out of
// incarnation.state, and §6 turns the value a keeper task writes into a `vault:`
// ref, sealing the register name that carried it so any state cell reading it is
// masked on the way out ([ADR-010] §7.4). The protection moved from "drop the row
// at the source" to "the row never holds the plaintext".
func buildRegisterByHost(rows []applyrun.TaskRegister, tasks []*render.RenderedTask) map[string]map[string]any {
	if len(rows) == 0 {
		return map[string]map[string]any{}
	}
	nameByIdx := make(map[int]string, len(tasks))
	for _, t := range tasks {
		if t.Register != "" {
			nameByIdx[t.Index] = t.Register
		}
	}

	out := make(map[string]map[string]any)
	for i := range rows {
		name := nameByIdx[rows[i].PlanIndex]
		if name == "" {
			continue
		}
		hostReg := out[rows[i].SID]
		if hostReg == nil {
			hostReg = make(map[string]any)
			out[rows[i].SID] = hostReg
		}
		hostReg[name] = rows[i].RegisterData
	}
	return out
}

// ChangedTask is the per-task "what changed" outcome of a scenario run, the
// record shape of the terminal `incarnation.run_completed` event (T3, ADR-052 §k).
//
// Task address (Register ∪ ID) is a stable identifier for a Tiding subscription
// to "task X changed" (T4): Register if the task captures it, else ID (DSL core
// `id:`, T1; a task can't have both — the config validator T2 forbids it). An
// unaddressable task (neither register nor id) still enters the array with an
// empty address — "how much and where changed" stays complete (see
// buildChangedTasks).
//
// ChangedHosts/TotalHosts are counts of UNIQUE sid (union across all idx of the
// address), not per-idx sums: loop expands one source task into N RenderedTask
// with sequential idx, but they all share ONE address — summing would inflate
// the denominator (M hosts × K iterations). Metadata (Name/Module/Register/ID)
// comes from the in-memory []RenderedTask, NOT from journal payload (secret
// hygiene, T3).
type ChangedTask struct {
	// Idx is the address's representative task_idx: the minimum idx among that
	// address's iterations. For a loop-collapsed address (several idx), the
	// smallest one; point addressing goes by Register/ID, not Idx.
	Idx          int
	Name         string
	Register     string
	ID           string
	Module       string
	ChangedHosts int
	TotalHosts   int
}

// taskAddress returns the task's address (Register ∪ ID) and an addressability
// flag. Register outranks ID (capturing a result beats a label); T1/T2 guarantee
// both aren't set at once, so the priority is just a guard against a programming
// error.
func taskAddress(t *render.RenderedTask) (addr string, addressable bool) {
	if t.Register != "" {
		return t.Register, true
	}
	if t.ID != "" {
		return t.ID, true
	}
	return "", false
}

// buildChangedTasks is a pure fold of per-task "what changed" by address
// (Register ∪ ID). Modeled on [buildRegisterByHost] (idx→register resolution
// from tasks). No PG/audit reads: changedKeys is the already-read set of
// (sid, plan_index) for CHANGED tasks (auditpg.SelectChangedTaskKeys), plans are
// the run's DispatchPlans (TargetSIDs after on:/where:).
//
// Correlating the CHANGED fact with the plan goes by GLOBAL RenderedTask.Index
// (= ChangedTaskKey.PlanIndex, ADR-056 §S1 fix Variant B, T3): under staged/
// per-host-where, local task_idx != global, so keying on it would point at a
// neighboring task (the wrong task counted as changed, in audit and in secret
// hygiene alike).
// SelectChangedTaskKeys already returns the global plan_index from the payload
// (fallback to task_idx for N=1).
//
// Grouping:
//   - addressable task (register/id) → key = address; loop iterations sharing
//     an address collapse into one ChangedTask (different idx, same address).
//   - unaddressable task (no register, no id) → key = its Index; each stays a
//     separate record (doesn't collapse with other unaddressable tasks; a loop
//     over an unaddressable task yields several records — no address to fold on).
//
// Counters are UNIQUE sid (union, not a per-idx sum):
//   - TotalHosts = |union of TargetSIDs across all idx of the address| (after
//     on:/where:/run_once: — NOT the whole roster);
//   - ChangedHosts = |union of sid from changedKeys across all idx of the address|.
//
// Only addresses with ChangedHosts>0 make it into the result (a task unchanged
// on every host is absent). Order is first appearance of the address (= idx
// order for non-loop tasks; for a loop-collapsed address, the position of its
// minimum idx, via keyOrder without sorting). Every task is included in the fold:
// changed_tasks carries only counts + metadata (name/register/id/module), no
// register/params payload values — nothing secret can travel this way.
func buildChangedTasks(
	tasks []*render.RenderedTask,
	plans []render.DispatchPlan,
	changedKeys map[auditpg.ChangedTaskKey]struct{},
) []ChangedTask {
	if len(tasks) == 0 {
		return nil
	}

	// targetsByIdx: idx → TargetSIDs (after on:/where:). DispatchPlan.TaskIndex
	// refers to RenderedTask.Index.
	targetsByIdx := make(map[int][]string, len(plans))
	for i := range plans {
		targetsByIdx[plans[i].TaskIndex] = plans[i].TargetSIDs
	}

	// Aggregate accumulator keyed by grouping key (address, or synthetic key for
	// unaddressable tasks).
	type acc struct {
		repIdx      int // representative (minimum) idx
		name        string
		register    string
		id          string
		module      string
		totalSIDs   map[string]struct{}
		changedSIDs map[string]struct{}
	}
	// keyOrder preserves first-appearance order of the key (determinism before sorting).
	groups := make(map[string]*acc)
	var keyOrder []string

	for _, t := range tasks {
		addr, addressable := taskAddress(t)
		// Grouping key: for addressable — "a:"+address (collapses loop iterations);
		// for unaddressable — "i:"+idx (each its own record). The prefix separates
		// namespaces so id "5" and idx 5 don't collide.
		var key string
		if addressable {
			key = "a:" + addr
		} else {
			key = "i:" + fmt.Sprint(t.Index)
		}

		a := groups[key]
		if a == nil {
			a = &acc{
				repIdx:      t.Index,
				name:        t.Name,
				register:    t.Register,
				id:          t.ID,
				module:      t.Module,
				totalSIDs:   make(map[string]struct{}),
				changedSIDs: make(map[string]struct{}),
			}
			groups[key] = a
			keyOrder = append(keyOrder, key)
		} else if t.Index < a.repIdx {
			a.repIdx = t.Index
		}

		// union this idx's TargetSIDs into total.
		for _, sid := range targetsByIdx[t.Index] {
			a.totalSIDs[sid] = struct{}{}
		}
		// union this idx's CHANGED sid into changed. Checked against the address's
		// TargetSIDs: changedKeys is a set of (sid, plan_index); we walk the idx's
		// target sids and keep those marked CHANGED. t.Index is the GLOBAL
		// RenderedTask.Index, matching the PlanIndex key (T3); local task_idx isn't
		// used here.
		for _, sid := range targetsByIdx[t.Index] {
			if _, ok := changedKeys[auditpg.ChangedTaskKey{SID: sid, PlanIndex: t.Index}]; ok {
				a.changedSIDs[sid] = struct{}{}
			}
		}
	}

	out := make([]ChangedTask, 0, len(keyOrder))
	for _, key := range keyOrder {
		a := groups[key]
		if len(a.changedSIDs) == 0 {
			continue // task unchanged on every host — excluded from the array
		}
		out = append(out, ChangedTask{
			Idx:          a.repIdx,
			Name:         a.name,
			Register:     a.register,
			ID:           a.id,
			Module:       a.module,
			ChangedHosts: len(a.changedSIDs),
			TotalHosts:   len(a.totalSIDs),
		})
	}
	return out
}

// unmarshalJSONB parses JSONB bytes into a map (symmetric with the incarnation
// layer). Empty bytes / `null` → nil map.
func unmarshalJSONB(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
