package runtime

import (
	"context"
	"sync"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/shared/config"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// registerIndex is the run's register accumulator, shared by the main flow and
// every async flow (ADR-0075). Two views of the same data, exactly as the
// sequential runner kept them: byIdx feeds requisite gating (`onchanges:`/
// `onfail:`, which Keeper resolved into indices), byName feeds the flow-control
// predicates that read `register.<name>` (ADR-012(d)). Concurrency is the only
// reason they live behind a mutex — on a plan without `async:` nothing but the
// main flow ever touches them.
type registerIndex struct {
	mu     sync.Mutex
	byIdx  map[int32]*structpb.Struct
	byName map[string]any

	// concurrent flips once the first async flow is launched. Until then the
	// by-name view can be handed to cel-go as-is (nobody writes mid-task, as
	// before this ADR); after, every activation gets a copy.
	concurrent bool
}

func newRegisterIndex(hint int) *registerIndex {
	return &registerIndex{
		byIdx:  make(map[int32]*structpb.Struct, hint),
		byName: make(map[string]any, hint),
	}
}

// record accumulates a finished/skipped task's register payload into both
// views. An empty name skips byName — a task without `register:` is addressable
// only by its index.
func (x *registerIndex) record(idx int32, name string, data *structpb.Struct) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.byIdx[idx] = data
	if name != "" {
		x.byName[name] = data.AsMap()
	}
}

// get returns a task's register payload by index; nil when it has not run.
func (x *registerIndex) get(idx int32) *structpb.Struct {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.byIdx[idx]
}

// snapshot returns the by-name view for ONE CEL activation. cel-go reads the
// map throughout an evaluation, so once async flows can write it the caller
// gets a copy; before that the live map is returned and the sequential path
// costs what it always did.
func (x *registerIndex) snapshot() map[string]any {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.concurrent {
		return x.byName
	}
	out := make(map[string]any, len(x.byName))
	for k, v := range x.byName {
		out[k] = v
	}
	return out
}

func (x *registerIndex) markConcurrent() {
	x.mu.Lock()
	x.concurrent = true
	x.mu.Unlock()
}

// asyncFlow is one launched `async: true` task (ADR-0075(a)). done is closed
// once the flow is finalized — its register is in the shared [registerIndex]
// and its TaskEvent is on the wire — which is what makes a barrier a receive on
// done plus a plain read of failed (written before the close, read only after).
type asyncFlow struct {
	idx    int32
	name   string
	done   chan struct{}
	failed bool
}

// asyncFlows tracks one run's async flows: what has been launched, the
// host-side concurrency ceiling, and the first sink error a flow hit.
//
// Barrier targets are always resolved IN THE MAIN FLOW at the awaiting task's
// plan position ([asyncFlows.barriers]) and only then handed to whoever waits.
// A flow launched later than the awaiting task is therefore never in the set,
// which is both what `require: all` means ("every async task started earlier in
// this run", destiny/tasks.md §8) and what turns a forward `require:` into a
// no-op instead of a deadlock — rejecting that authoring mistake statically is
// NIM-152's.
type asyncFlows struct {
	mu    sync.Mutex
	byIdx map[int32]*asyncFlow
	// byName holds EVERY flow registered under a name, not the newest one
	// (NIM-246). A `loop:` fans out into N tasks sharing one `register:`, so an
	// implicit barrier — a `register.<name>` reference from a Soul-side key —
	// has to collect all of them; keeping one meant the reader waited for
	// whichever iteration happened to be launched last and read the register
	// while its siblings were still writing it.
	byName map[string][]*asyncFlow
	order  []*asyncFlow

	wg sync.WaitGroup

	// sem is the host-side ceiling (ADR-0075(e), `async.max_concurrent` in
	// soul.yml). nil = unlimited. A flow queues for a slot AFTER its barrier, so
	// a waiting flow never occupies one.
	sem chan struct{}

	// sendErr is the first sink I/O failure a flow hit. Run returns it after the
	// final barrier, the same way the main flow returns its own.
	sendErr error
}

func newAsyncFlows(limit int) *asyncFlows {
	f := &asyncFlows{
		byIdx:  make(map[int32]*asyncFlow),
		byName: make(map[string][]*asyncFlow),
	}
	if limit > 0 {
		f.sem = make(chan struct{}, limit)
	}
	return f
}

// add registers a flow before its goroutine starts, so that a later task's
// barrier resolution sees it.
func (f *asyncFlows) add(fl *asyncFlow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byIdx[fl.idx] = fl
	if fl.name != "" {
		f.byName[fl.name] = append(f.byName[fl.name], fl)
	}
	f.order = append(f.order, fl)
}

// acquire takes a concurrency slot, returning its release. A cancelled run
// stops queueing: the flow proceeds and finalizes as CANCELLED without touching
// the host (ADR-0075(h) — CancelApply reaches in-flight flows, not just the
// main loop).
func (f *asyncFlows) acquire(ctx context.Context) func() {
	if f.sem == nil {
		return func() {}
	}
	select {
	case f.sem <- struct{}{}:
		return func() { <-f.sem }
	case <-ctx.Done():
		return func() {}
	}
}

// barriers computes what a task waits for at its plan position, split by WHERE
// the wait happens.
//
// gate — the flows whose register the task's GATING reads: `onchanges:`/
// `onfail:`/`aggregate_of` (indices resolved by Keeper) and `register.<name>`
// inside `when:`. Awaited by the main flow even when the task itself is async,
// because gating is evaluated at the plan position so that WHETHER a task runs
// never depends on timing (ADR-0075(h)).
//
// start — the explicit barrier (`require:` / `require: all`) plus the
// `register.<name>` of the post-Apply predicates `changed_when:`/`failed_when:`/
// `until:`. Awaited immediately before the module runs, which for an async task
// is inside its own flow: `require:` orders the TASK, it does not hold up the
// plan (ADR-0075(b.1)). Folding the post-Apply predicates in here waits a little
// earlier than the reference site, which can only over-wait — never miss a
// register the predicate then reads.
//
// A -1 entry (a source filtered out by `where:` on this host, or living in an
// earlier Passage) resolves to nothing: for a barrier the sentinel reads
// "nothing to wait for", not "contributes false to a gate".
//
// A flow that has ALREADY finished stays resolvable — waiting on it costs
// nothing and its verdict is what the barrier is for. That settles the point
// tasks.md §12 left open for `require: all` ("those still in flight"): scoping
// it to what is currently running would make whether a failure is noticed here
// or only at the final barrier depend on timing, and gating must not.
func (f *asyncFlows) barriers(task *keeperv1.RenderedTask) (gate, start []*asyncFlow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Nothing has been launched yet, so no reference can name an async task. A
	// plan without `async:` never pays for the resolution.
	if len(f.order) == 0 {
		return nil, nil
	}

	var g, s flowSet
	g.addIdx(f, task.GetOnchangesIdx())
	g.addIdx(f, task.GetOnfailIdx())
	g.addIdx(f, task.GetAggregateOf())
	g.addNames(f, task.GetWhen())

	if task.GetRequireAll() {
		s.addAll(f)
	} else {
		s.addIdx(f, task.GetRequireIdx())
	}
	s.addNames(f, task.GetChangedWhen())
	s.addNames(f, task.GetFailedWhen())
	s.addNames(f, task.GetUntil())

	return g.out, s.out
}

// wait blocks until every launched flow is finalized and reports whether any of
// them failed. The final barrier (ADR-0075(b.3)).
func (f *asyncFlows) wait() (failed bool) {
	f.wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fl := range f.order {
		if fl.failed {
			return true
		}
	}
	return false
}

func (f *asyncFlows) recordSendErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr == nil {
		f.sendErr = err
	}
}

func (f *asyncFlows) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sendErr
}

// waitFlows blocks until every flow in the set is finalized. It deliberately
// does NOT watch a context: a cancelled flow ends on its own (its module sees
// the dead ctx), and returning early would let Run finish while a flow is still
// writing to the sink.
func waitFlows(flows []*asyncFlow) {
	for _, fl := range flows {
		<-fl.done
	}
}

// anyFailed reports whether an awaited flow ended FAILED/TIMED_OUT. Valid only
// after [waitFlows] over the same set.
func anyFailed(flows []*asyncFlow) bool {
	for _, fl := range flows {
		if fl.failed {
			return true
		}
	}
	return false
}

// flowSet accumulates barrier targets without duplicates (the same flow is
// easily named by both a requisite and a predicate).
type flowSet struct {
	seen map[int32]struct{}
	out  []*asyncFlow
}

func (s *flowSet) add(fl *asyncFlow) {
	if fl == nil {
		return
	}
	if s.seen == nil {
		s.seen = make(map[int32]struct{}, 4)
	}
	if _, dup := s.seen[fl.idx]; dup {
		return
	}
	s.seen[fl.idx] = struct{}{}
	s.out = append(s.out, fl)
}

// addIdx resolves task indices; callers hold f.mu.
func (s *flowSet) addIdx(f *asyncFlows, idx []int32) {
	for _, i := range idx {
		s.add(f.byIdx[i])
	}
}

// addNames resolves the `register.<name>` references of one CEL predicate
// through the canonical parser the cross-ref validator and stage-render
// stratification already share; callers hold f.mu.
func (s *flowSet) addNames(f *asyncFlows, expr string) {
	if expr == "" {
		return
	}
	for _, name := range config.ExtractRegisterRefs(expr) {
		for _, fl := range f.byName[name] {
			s.add(fl)
		}
	}
}

func (s *flowSet) addAll(f *asyncFlows) {
	for _, fl := range f.order {
		s.add(fl)
	}
}

// lockedSink serializes writes to the run's sink. With async flows several
// goroutines finalize at once, and an EventSink is not required to be safe for
// concurrent use on its own — [NDJSONSink] writes lines into a shared
// io.Writer. Cheap enough to apply unconditionally rather than branching on
// whether the plan uses `async:`.
type lockedSink struct {
	mu   sync.Mutex
	sink EventSink
}

func (s *lockedSink) SendTaskEvent(ev *keeperv1.TaskEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sink.SendTaskEvent(ev)
}

func (s *lockedSink) SendRunResult(rr *keeperv1.RunResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sink.SendRunResult(rr)
}

// failedStatus reports whether a terminal task status counts as a failure — for
// fail-stop, for a barrier's verdict on an async flow, and for the run outcome.
// TIMED_OUT is a failure that also feeds `onfail:` (buildRegisterData writes
// failed=true for it).
func failedStatus(s keeperv1.TaskStatus) bool {
	return s == keeperv1.TaskStatus_TASK_STATUS_FAILED ||
		s == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT
}
