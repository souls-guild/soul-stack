package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	selectIncarnationForUpdateSQL = `
SELECT name, service, service_version, state_schema_version,
       spec, state, status, status_details, created_by_aid,
       created_at, updated_at
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	updateIncarnationStatusSQL = `
UPDATE incarnation
SET status = $2, updated_at = NOW()
WHERE name = $1
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
WHERE name = $1
`
)

// selectForUpdate reads incarnation under FOR UPDATE (guards against concurrent
// runs: while the row is locked by lockRun's transaction, a concurrent Start
// blocks on this SELECT until COMMIT).
func selectForUpdate(ctx context.Context, tx pgx.Tx, name string) (*incarnation.Incarnation, error) {
	row := tx.QueryRow(ctx, selectIncarnationForUpdateSQL, name)
	return scanForUpdate(row)
}

// scanForUpdate parses an incarnation row (same columns as
// incarnation.SelectByName, but via a locking SELECT inside the runner's
// transaction — incarnation.scanIncarnation isn't exported, so we duplicate the
// minimum).
func scanForUpdate(row pgx.Row) (*incarnation.Incarnation, error) {
	var (
		inc                incarnation.Incarnation
		statusStr          string
		specBytes          []byte
		stateBytes         []byte
		statusDetailsBytes []byte
		createdByAID       *string
	)
	err := row.Scan(
		&inc.Name, &inc.Service, &inc.ServiceVersion, &inc.StateSchemaVersion,
		&specBytes, &stateBytes, &statusStr, &statusDetailsBytes, &createdByAID,
		&inc.CreatedAt, &inc.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, incarnation.ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("scenario: scan incarnation: %w", err)
	}
	inc.Status = incarnation.Status(statusStr)
	inc.CreatedByAID = createdByAID
	if inc.Spec, err = unmarshalJSONB(specBytes); err != nil {
		return nil, fmt.Errorf("scenario: unmarshal spec: %w", err)
	}
	if inc.State, err = unmarshalJSONB(stateBytes); err != nil {
		return nil, fmt.Errorf("scenario: unmarshal state: %w", err)
	}
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

// essenceInput builds [essence.ResolveInput] for a representative host:
// OS-family from soulprint, host Coven labels, override from incarnation.spec.essence.
func essenceInput(serviceDir string, inc *incarnation.Incarnation, host *topology.HostFacts) essence.ResolveInput {
	return essence.ResolveInput{
		ServiceDir:      serviceDir,
		OSFamily:        osFamilyOf(host),
		Covens:          host.Coven,
		IncarnationSpec: specEssence(inc),
	}
}

// keeperEssenceInput builds [essence.ResolveInput] for the keeper context — empty
// roster, provision-from-zero (ADR-0061 §context): no representative host. The
// OS-family overlay is skipped (OSFamily empty — no per-host soulprint), the
// Coven overlay is the incarnation's root Coven label (inc.Name, ADR-008: every
// host in the roster carries it, so it applies even without a host), and the
// spec.essence override applies as usual. Symmetric to renderKeeperTask, which
// renders keeper tasks in keeper context without per-host soulprint.
func keeperEssenceInput(serviceDir string, inc *incarnation.Incarnation) essence.ResolveInput {
	return essence.ResolveInput{
		ServiceDir:      serviceDir,
		Covens:          []string{inc.Name},
		IncarnationSpec: specEssence(inc),
	}
}

// osFamilyOf extracts `soulprint.self.os.family` from the host's last-reported
// facts. Missing facts / field → "" (essence skips the os layer).
func osFamilyOf(host *topology.HostFacts) string {
	os, ok := host.Soulprint["os"].(map[string]any)
	if !ok {
		return ""
	}
	family, _ := os["family"].(string)
	return family
}

// specEssence returns incarnation.spec.essence (operator override) or nil.
func specEssence(inc *incarnation.Incarnation) map[string]any {
	if inc.Spec == nil {
		return nil
	}
	e, _ := inc.Spec["essence"].(map[string]any)
	return e
}

// loadRegisterByHost reads accumulated register data for the run from
// `apply_task_register` (migration 022) and builds a per-host register map
// (sid → register-name → payload) for rendering `state_changes.sets` (slice 2).
//
// task_idx → register-name resolution happens here (not on the handler side):
// scenario-runner holds []RenderedTask with the Register field, but the handler
// doesn't know the name at TaskEvent time (proto carries only task_idx,
// ADR-012(d)). This instance is the one that initiated the run, so tasks are
// available locally, while the shared Postgres table survives cross-Keeper
// TaskEvent routing (ADR-002).
//
// Rows without a register name (task has no register:, or the register belongs
// to a task not in tasks) are skipped. Empty result → empty map.
func (r *Runner) loadRegisterByHost(ctx context.Context, applyID string, tasks []*render.RenderedTask) (map[string]map[string]any, error) {
	rows, err := applyrun.SelectTaskRegistersByApplyID(ctx, r.deps.DB, applyID)
	if err != nil {
		return nil, fmt.Errorf("scenario: load run register data: %w", err)
	}
	return buildRegisterByHost(rows, tasks), nil
}

// loadRegisterByHostUpToPassage reads register data for the run accumulated in
// Passages STRICTLY LESS than upToPassage (staged render, ADR-056 §v.1):
// rendering Passage N substitutes the register of all prior Passages per-host.
// upToPassage=0 (first Passage) → empty map (register not collected yet — same
// as up-front render).
//
// task_idx → register-name resolution is the same as in [loadRegisterByHost] (by
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
// tasks. Split out of loadRegisterByHost for unit testing without PG.
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
// no_log (variant B): a task with NoLog=true doesn't enter nameByIdx, so its
// register row isn't accumulated into the per-host map and never reaches the
// state graph (orchestration.md §7). A state_changes.sets referencing such a
// task's register gets no-such-key — a no_log task's sensitive value never lands
// in stored incarnation.state. This is source-side protection; masking on GET
// output is the second layer (defense-in-depth).
func buildRegisterByHost(rows []applyrun.TaskRegister, tasks []*render.RenderedTask) map[string]map[string]any {
	if len(rows) == 0 {
		return map[string]map[string]any{}
	}
	nameByIdx := make(map[int]string, len(tasks))
	for _, t := range tasks {
		if t.NoLog {
			continue
		}
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
// neighboring task (mismatched state_changes whitelist + audit).
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
// minimum idx, via keyOrder without sorting). A NoLog task is included in the
// fold: changed_tasks carries only counts + metadata (name/register/id/module),
// no register/params payload values — no secret leak from a no_log task here.
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

// mergeStateChanges applies the ordered list of rendered `state_changes`
// operations (render.RenderStateOps) on top of stateBefore and returns the new
// state (orchestration.md §7, the list-form grammar). ★ Logic identical to
// trial.mergeStateChanges (diff.go): a divergence would split Trial from prod —
// guarded by the Mirror test (state_test.go ↔ diff_test.go).
//
// deep-copy stateBefore (a commit snapshot doesn't hold a reference to the
// source map) → sequential application of operations to the intermediate state.
// matchEval is the CEL evaluator for add's list-dedup match predicate
// (render.Pipeline.EvalStateMatch); opEval is the CEL evaluator for modify/remove
// match+patch with the full scenario context (render.Pipeline.EvalStateOpExpr);
// schema is the service's state_schema (collection type for materializing a
// missing field). Empty/nil ops → state unchanged.
//
// Operation semantics:
//   - set:    out[field] = value (whole-field overwrite, last-wins);
//   - add:    materialize the collection (from the existing value / schema) →
//     identity check (map: by Key; list: Match predicate) → append/insert OR
//     no-op/replace/error per OnConflict (default skip — idempotent);
//   - modify: patch ALL collection elements matching Match (all-by-default).
//     map: match sees key/value, patches the entry; list: match sees elem,
//     patches the element. patch is a merge at a path-in-element (nested dotted
//     path), not a whole-record overwrite. expect → cardinality assert before
//     commit;
//   - remove: delete ALL elements matching Match. empty-match → no-op for both.
//
// foreach never reaches here — expanded in the render phase into N RenderedOp.
func mergeStateChanges(stateBefore map[string]any, ops []render.RenderedOp, schema map[string]any, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) (map[string]any, error) {
	out := deepCopyMap(stateBefore)
	for i := range ops {
		op := ops[i]
		switch op.Verb {
		case config.VerbSet:
			out[op.Field] = op.Value
		case config.VerbAdd:
			if err := applyAddOp(out, op, schema, matchEval, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] add %q: %w", i, op.Field, err)
			}
		case config.VerbModify:
			if err := applyModifyOp(out, op, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] modify %q: %w", i, op.Field, err)
			}
		case config.VerbRemove:
			if err := applyRemoveOp(out, op, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] remove %q: %w", i, op.Field, err)
			}
		default:
			return nil, fmt.Errorf("state_changes[%d]: verb %q not supported by the engine", i, op.Verb)
		}
	}
	return out, nil
}

// applyModifyOp patches ALL elements of collection op.Field matching op.Match
// (all-by-default). ★ Logic identical to trial.applyModifyOp. map: match sees
// key/value, the patch merges into the entry's value; list: match sees elem, the
// patch merges into the element. Matched cardinality is checked against
// op.Expect BEFORE mutation (expect failure → error, state not committed).
// Empty-match → no-op.
func applyModifyOp(out map[string]any, op render.RenderedOp, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	if !present {
		// Field absent — nothing to patch. empty-match no-op (field is
		// semantically an empty collection); not an error.
		return checkExpect(op, 0)
	}
	switch coll := existing.(type) {
	case map[string]any:
		matched := 0
		for k, v := range coll {
			binds := map[string]any{"key": k, "value": v}
			ok, err := evalOpBool(opEval, op.Match, op.Context, binds)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			matched++
			patched, err := applyPatch(v, op.Patch, op.Context, binds, opEval)
			if err != nil {
				return err
			}
			coll[k] = patched
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = coll
		return nil
	case []any:
		matched := 0
		for i := range coll {
			binds := map[string]any{"elem": coll[i]}
			ok, err := evalOpBool(opEval, op.Match, op.Context, binds)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			matched++
			patched, err := applyPatch(coll[i], op.Patch, op.Context, binds, opEval)
			if err != nil {
				return err
			}
			coll[i] = patched
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = coll
		return nil
	}
	return fmt.Errorf("field %q is not a collection (map/list)", op.Field)
}

// applyRemoveOp deletes ALL elements of collection op.Field matching op.Match.
// ★ Logic identical to trial.applyRemoveOp. Cardinality is checked against
// op.Expect BEFORE mutation. Empty-match → no-op. Field absent → no-op (nothing
// to delete).
func applyRemoveOp(out map[string]any, op render.RenderedOp, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	if !present {
		return checkExpect(op, 0)
	}
	switch coll := existing.(type) {
	case map[string]any:
		matched := 0
		drop := make([]string, 0, len(coll))
		for k, v := range coll {
			ok, err := evalOpBool(opEval, op.Match, op.Context, map[string]any{"key": k, "value": v})
			if err != nil {
				return err
			}
			if ok {
				matched++
				drop = append(drop, k)
			}
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		for _, k := range drop {
			delete(coll, k)
		}
		out[op.Field] = coll
		return nil
	case []any:
		kept := make([]any, 0, len(coll))
		matched := 0
		for i := range coll {
			ok, err := evalOpBool(opEval, op.Match, op.Context, map[string]any{"elem": coll[i]})
			if err != nil {
				return err
			}
			if ok {
				matched++
				continue
			}
			kept = append(kept, coll[i])
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = kept
		return nil
	}
	return fmt.Errorf("field %q is not a collection (map/list)", op.Field)
}

// evalOpBool evaluates the modify/remove match predicate via opEval (full
// scenario context + element bindings) and coerces it to bool. ★ Logic
// identical to trial.evalOpBool.
func evalOpBool(opEval render.StateOpEvalFunc, match string, ctx, binds map[string]any) (bool, error) {
	if match == "" {
		// Empty match never reaches here (the config validator warns; the engine
		// could treat it as "all"). Fail-safe: an empty predicate matches nothing.
		return false, nil
	}
	res, err := opEval(match, ctx, binds, true)
	if err != nil {
		return false, fmt.Errorf("match predicate %q: %w", match, err)
	}
	b, ok := res.(bool)
	if !ok {
		return false, fmt.Errorf("match predicate %q returned %T, want bool", match, res)
	}
	return b, nil
}

// applyPatch merges a patch map into a collection record/element (a dotted path
// is a nested merge, not a whole-record overwrite). Each patch value is a
// CEL/literal, evaluated via opEval (context + element bindings). The record is
// deep-copied before mutation (the source state element isn't touched until the
// chain succeeds). ★ Logic identical to trial.applyPatch.
func applyPatch(elem any, patch, ctx, binds map[string]any, opEval render.StateOpEvalFunc) (any, error) {
	target, ok := deepCopyValue(elem).(map[string]any)
	if !ok {
		// A scalar list element (list of scalars) can't be patched by dotted path.
		return nil, fmt.Errorf("patch applies only to a record object (element %T is not an object)", elem)
	}
	for path, rawVal := range patch {
		val, err := renderPatchValue(rawVal, ctx, binds, opEval)
		if err != nil {
			return nil, fmt.Errorf("patch %q: %w", path, err)
		}
		if err := setNestedPath(target, path, val); err != nil {
			return nil, fmt.Errorf("patch %q: %w", path, err)
		}
	}
	return target, nil
}

// renderPatchValue evaluates a single patch value: string → CEL/literal via
// opEval (interpolation, native type); everything else (number/bool from a YAML
// literal) passes through as-is. ★ Logic identical to trial.renderPatchValue.
func renderPatchValue(raw any, ctx, binds map[string]any, opEval render.StateOpEvalFunc) (any, error) {
	s, ok := raw.(string)
	if !ok {
		return raw, nil
	}
	return opEval(s, ctx, binds, false)
}

// setNestedPath places a value at a dotted path (`config.maxmemory`) into a map,
// materializing MISSING intermediate objects (ADR-057 §f). ★ Logic identical to
// trial.setNestedPath. A dotted path is a nested merge (sibling fields of the
// record stay intact); a flat path is top-level.
//
// An intermediate segment that ALREADY exists and is NOT a map (scalar/list) is
// an error (state_changes_apply_failed, not a silent clobber): pushing a nested
// path through a non-object would lose the node's prior value. Difference from
// §f: a missing intermediate node is materialized (ok); an existing non-map node
// is an explicit rejection (operator is patching an incompatible shape).
func setNestedPath(m map[string]any, path string, val any) error {
	parts := splitPath(path)
	cur := m
	for i := 0; i < len(parts)-1; i++ {
		seg := parts[i]
		existing, present := cur[seg]
		if !present {
			next := map[string]any{}
			cur[seg] = next
			cur = next
			continue
		}
		next, ok := existing.(map[string]any)
		if !ok {
			return fmt.Errorf("intermediate node %q already exists and is not an object (%T) - patch of nested path %q would clobber it", seg, existing, path)
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = val
	return nil
}

// splitPath splits a patch's dotted path into segments. ★ Logic identical to trial.splitPath.
func splitPath(path string) []string {
	return strings.Split(path, ".")
}

// checkExpect checks the actual match cardinality against op.Expect (ADR-057
// §c). ""/any → no assert. one → exactly 1; at_most_one → 0 or 1. Violation →
// error (run.go → error_locked, state not committed). ★ Logic identical to
// trial.checkExpect.
func checkExpect(op render.RenderedOp, matched int) error {
	switch op.Expect {
	case "", config.ExpectAny:
		return nil
	case config.ExpectOne:
		if matched != 1 {
			return fmt.Errorf("expect: one - match hit %d elements (expected exactly one)", matched)
		}
	case config.ExpectAtMostOne:
		if matched > 1 {
			return fmt.Errorf("expect: at_most_one - match hit %d elements (expected <=1)", matched)
		}
	}
	return nil
}

// applyAddOp applies a single add operation to the intermediate state out
// (mutates out in place — out is already a deep copy of the source state). The
// out[field] collection is materialized when absent (type from schema), then
// the element is added idempotently per the OnConflict policy. ★ Logic identical
// to trial.applyAddOp.
func applyAddOp(out map[string]any, op render.RenderedOp, schema map[string]any, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	kind := collectionKind(existing, present, schema, op.Field)

	switch kind {
	case collKindMap:
		if op.Key == "" {
			return fmt.Errorf("add into a map collection requires key:")
		}
		coll, _ := existing.(map[string]any)
		if coll == nil {
			coll = map[string]any{}
		}
		if _, exists := coll[op.Key]; exists {
			switch op.OnConflict {
			case config.OnConflictError:
				// WITHOUT the resolved op.Key in reason: a map key could be
				// `${ vault(...) }` (a resolved secret), and reason travels into
				// incarnation.status_details.error unmasked (audit.MaskSecrets
				// catches `vault:` refs, not plaintext values). Print only the
				// collection-field name (BUG-3, security).
				return fmt.Errorf("add %q: key already exists (on_conflict: error)", op.Field)
			case config.OnConflictReplace:
				coll[op.Key] = op.Value
			default: // skip (default) — idempotent no-op
			}
		} else {
			coll[op.Key] = op.Value
		}
		out[op.Field] = coll
		return nil

	case collKindList:
		coll, _ := existing.([]any)
		idx, err := findListMatch(coll, op, matchEval, opEval)
		if err != nil {
			return err
		}
		if idx >= 0 {
			switch op.OnConflict {
			case config.OnConflictError:
				// Without the resolved op.Value/elem in reason (BUG-3, security): value
				// could be `${ vault(...) }`. Print only the collection-field name.
				return fmt.Errorf("add %q: an element with this identity already exists (on_conflict: error)", op.Field)
			case config.OnConflictReplace:
				coll[idx] = op.Value
			default: // skip (default) — idempotent no-op
			}
		} else {
			coll = append(coll, op.Value)
		}
		out[op.Field] = coll
		return nil
	}
	return fmt.Errorf("field %q is not a collection (map/list) and the type can't be inferred from schema", op.Field)
}

// findListMatch finds the index of an existing element identical to the one
// being added (op.Value): via the Match predicate if set, else deep-equal.
// Returns -1 if none is identical. ★ Logic identical to trial.findListMatch.
//
// If op.Context != nil (add inside a foreach — match refers to the `as` name,
// e.g. `elem == sid`), match is evaluated by the context-aware opEval evaluator
// (elem/value bindings + the foreach binding from Context). Otherwise it's a
// pure add-match matchEval (elem/value only, ADR-057: identity = a function of
// elem+value).
func findListMatch(coll []any, op render.RenderedOp, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) (int, error) {
	for i := range coll {
		if op.Match != "" {
			var ok bool
			var err error
			if op.Context != nil {
				ok, err = evalOpBool(opEval, op.Match, op.Context, map[string]any{"elem": coll[i], "value": op.Value})
			} else {
				ok, err = matchEval(op.Match, coll[i], op.Value)
			}
			if err != nil {
				return -1, fmt.Errorf("match predicate %q: %w", op.Match, err)
			}
			if ok {
				return i, nil
			}
			continue
		}
		if reflect.DeepEqual(coll[i], op.Value) {
			return i, nil
		}
	}
	return -1, nil
}

// collKind is the collection kind under a state field (for add materialization).
// ★ Logic identical to trial.collKind*.
type collKind int

const (
	collKindUnknown collKind = iota
	collKindList
	collKindMap
)

// collectionKind determines a field's collection kind: first from the existing
// state value (authoritative — actual shape), and if absent, from state_schema
// (`properties.<field>.type`: array→list, object→map). Unknown → collKindUnknown
// (applyAddOp returns an error). ★ Logic identical to trial.collectionKind.
func collectionKind(existing any, present bool, schema map[string]any, field string) collKind {
	if present {
		switch existing.(type) {
		case []any:
			return collKindList
		case map[string]any:
			return collKindMap
		}
		return collKindUnknown
	}
	switch schemaFieldType(schema, field) {
	case "array":
		return collKindList
	case "object":
		return collKindMap
	}
	return collKindUnknown
}

// schemaFieldType extracts `state_schema.properties.<field>.type` from the
// service's flat state_schema map (service.yml shape:
// {type:object, properties:{...}}). "" if schema isn't declared or the field
// isn't described. ★ Logic identical to trial.schemaFieldType.
func schemaFieldType(schema map[string]any, field string) string {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return ""
	}
	fieldSchema, ok := props[field].(map[string]any)
	if !ok {
		return ""
	}
	t, _ := fieldSchema["type"].(string)
	return t
}

// deepCopyMap deep-copies a map[string]any via a JSON round-trip (values are
// YAML/PG data: maps/slices/scalars, JSON-safe). nil → empty map
// (incarnation.state is never nil in a commit snapshot).
func deepCopyMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		// state is JSON-safe (read from JSONB); marshal doesn't fail.
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// deepCopyValue deep-copies an arbitrary JSON-safe value (map/slice/scalar) via
// a JSON round-trip. Needed by applyPatch: a mutated collection element must not
// hold a reference to the source state until the chain succeeds. ★ Logic
// identical to trial.deepCopyValue. A marshal failure is impossible (state is
// JSON-safe from JSONB); on error we return the original (a mismatch would be
// caught by verification/tests).
func deepCopyValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
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
