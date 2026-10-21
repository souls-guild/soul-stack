package render

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Failure is a render error carrying the address an operator needs to find it
// (NIM-1000): the task chain it happened in, the host it was evaluated for, the
// params cell, the CEL expression and the innermost reason. Its Error() is the
// error chain's own text — render's messages are unchanged by it — except that a
// withheld cause is replaced there too, so every channel that prints the error
// (status_details, state_history, the log, the span) prints the same safe text.
//
// It is attached where a task is rendered and gains an outer task on its way up —
// a block above its child, an applier above the destiny task it expanded into.
// Read it with [FailureOf].
type Failure struct {
	// Tasks is the chain of tasks the failure happened in, outermost first. Empty
	// for a failure outside any task (`compute:`, the namespace fence).
	Tasks []FailureTask
	// Host is the SID the failing evaluation ran for, "" when it was not
	// host-scoped.
	Host string
	// Cell is the params cell path (`args[0]`, `acl[1].name`), `vars.<name>` for an
	// error inside that var's definition, "" outside params.
	Cell string
	// Expr is the CEL expression as the author wrote it — the failing definition's
	// for an error inside one — "" when the error is not a CEL one.
	Expr string
	// Cause is the innermost reason. When Withheld it is a fixed sentence instead.
	Cause string
	// Withheld — the failing expression reads a secret source, so the evaluator's
	// own message is not shown: CEL quotes a missing map key by its value
	// (`no such key: <value>`), and that value can be the secret.
	Withheld bool

	err      error
	rawCause string
	// inDestiny — created inside a destiny pass, where an expression reads the
	// destiny's scope only; the scenario's names above it say nothing about it.
	inDestiny bool
}

// FailureTask is one task of [Failure.Tasks]. Index is the task's plan index
// (RenderedTask.Index) — a loop iteration's own — and -1 for a task that has
// none: a block or an applier, which expand into the tasks that do, an assert,
// which renders into none, and a loop whose failing iteration is not known.
// Destiny is what an applier applies.
type FailureTask struct {
	Index        int
	Name         string
	Module       string
	Destiny      string
	Source       config.SourcePos
	IncludedFrom []config.SourcePos
}

// frameIndex is idx for a task rendered into one RenderedTask of its own, and
// -1 for any other: idx there is the next task's.
func frameIndex(task config.Task, idx int) int {
	if task.Block != nil || task.Apply != nil || task.Loop != nil || task.Assert != nil {
		return -1
	}
	return idx
}

// withheldCause is [Failure.Cause] for a withheld cause — the wording of
// [whenFailure], which withholds the same thing on a `when:`.
const withheldCause = "evaluation failed; the error is withheld because the expression reads a secret"

func (f *Failure) Error() string {
	text := f.err.Error()
	if !f.Withheld {
		return text
	}
	if f.rawCause != "" && strings.HasSuffix(text, f.rawCause) {
		return strings.TrimSuffix(text, f.rawCause) + withheldCause
	}
	// The cause is not where the chain's own wrappers put it, so nothing of the
	// chain is known to be safe.
	return fmt.Sprintf("render: CEL eval %q: %s", f.Expr, withheldCause)
}

func (f *Failure) Unwrap() error { return f.err }

// FailureOf returns the [Failure] in err's chain, or nil.
func FailureOf(err error) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	return nil
}

// taskFailure attaches task to err's [Failure], creating one if the chain has
// none. The same task met again on the way up (a loop iteration, then its loop)
// is not repeated; the inner record keeps its index.
//
// Withholding is decided at every level with that level's seal sources and kept
// once set: a destiny pass knows its own secret inputs, the scenario task above
// it knows its `vars:`.
func (p *Pipeline) taskFailure(in RenderInput, task config.Task, idx int, err error) error {
	if err == nil {
		return nil
	}
	f, err := failureOf(err, in.destinyIsolated)
	frame := FailureTask{Index: idx, Name: task.Name, Source: task.Source, IncludedFrom: task.IncludedFrom}
	if task.Module != nil {
		frame.Module = task.Module.Module
	}
	if task.Apply != nil {
		frame.Destiny = task.Apply.Destiny
	}
	if len(f.Tasks) == 0 || !sameTask(f.Tasks[0], frame) {
		f.Tasks = append([]FailureTask{frame}, f.Tasks...)
	}
	p.withhold(f, in, p.taskSealSources(in, task))
	return err
}

// passFailure is [Pipeline.taskFailure] for an error a pass returns: it adds no
// task, and withholds by the pass's own seal sources.
//
// No `vars.*` taint, because no var error reaches here unjudged: one raised in a
// task was withheld by taskFailure with that task's view, a superset of any view
// here, and the one evaluation outside a task that reads `vars`, `compute:`, reads
// the service view alone, whose definitions can name no secret source.
func (p *Pipeline) passFailure(in RenderInput, err error) error {
	if err == nil {
		return nil
	}
	f, err := failureOf(err, in.destinyIsolated)
	p.withhold(f, in, scenarioSealSources(in))
	return err
}

// withhold decides whether f's cause can quote a secret. A `vault()` error names
// the path it read, so it is kept unless something secret built that path.
func (p *Pipeline) withhold(f *Failure, in RenderInput, sources cel.SealSources) {
	if f.Withheld || f.rawCause == "" || f.Expr == "" || (f.inDestiny && !in.destinyIsolated) {
		return
	}
	reads := p.cel.PredicateReadsSecret(f.Expr, sources)
	if cel.IsVaultError(f.rawCause) {
		reads = p.cel.ReadsSecretField(f.Expr, sources)
	}
	if reads {
		f.Withheld = true
		f.Cause = withheldCause
	}
}

// failureOf returns the chain's [Failure], wrapping err in a new one when it
// has none.
func failureOf(err error, inDestiny bool) (*Failure, error) {
	if f := FailureOf(err); f != nil {
		return f, err
	}
	f := &Failure{err: err, inDestiny: inDestiny}
	var (
		host *hostError
		at   *hostTag
	)
	switch {
	case errors.As(err, &host):
		f.Host = host.sid
	case errors.As(err, &at):
		f.Host = at.sid
	}
	var cell *cellError
	if errors.As(err, &cell) {
		f.Cell = cell.path
	}
	// An error raised inside a definition is THAT definition's (ADR-0082 amendment
	// 2026-10-15, point 11): its own expression, and `vars.<name>` as where it failed,
	// not the cell that asked for it. So withhold judges the definition's text,
	// however many reads away the asking cell is.
	cause := err
	if ve := innermostVarError(err); ve != nil {
		f.Cell = "vars." + ve.Name
		cause = ve.Err
	}
	var (
		evalErr     *cel.ErrEval
		ruleErr     *config.ValidateRuleError
		compileErr  *cel.ErrCompile
		unsupported *cel.ErrUnsupported
	)
	switch {
	case errors.As(cause, &evalErr):
		// The only CEL errors that carry a value of the run, this one and a
		// `validate:` rule's: compile errors and unsupported constructs speak of
		// the author's text.
		f.Expr, f.rawCause = evalErr.Expr, evalErr.Err.Error()
		f.Cause = f.rawCause
	case errors.As(cause, &ruleErr):
		f.Expr, f.rawCause = ruleErr.That, ruleErr.Err.Error()
		f.Cause = f.rawCause
	case errors.As(cause, &compileErr):
		f.Expr, f.Cause = compileErr.Expr, compileErr.Err.Error()
	case errors.As(cause, &unsupported):
		f.Expr, f.Cause = unsupported.Expr, "uses an unsupported construct: "+unsupported.Feature
	default:
		f.Cause = rootCause(cause).Error()
	}
	return f, f
}

// innermostVarError is the deepest [cel.VarError] of err's chain, or nil.
func innermostVarError(err error) *cel.VarError {
	var inner, ve *cel.VarError
	for errors.As(err, &ve) {
		inner, err = ve, ve.Err
	}
	return inner
}

// rootCause is the deepest error of a single-wrap chain.
func rootCause(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}

func sameTask(a, b FailureTask) bool {
	return a.Name == b.Name && a.Source == b.Source && slices.Equal(a.IncludedFrom, b.IncludedFrom)
}

// hostError names the host a task failed to render for. Its text is the one the
// call sites always printed.
type hostError struct {
	task, sid string
	err       error
}

func (e *hostError) Error() string {
	return fmt.Sprintf("render: task %q (host %s): %v", e.task, e.sid, e.err)
}

func (e *hostError) Unwrap() error { return e.err }

func hostErr(task, sid string, err error) error {
	return &hostError{task: task, sid: sid, err: err}
}

// hostTag records the host an error was evaluated for where the message already
// names it in its own words; the text is left as it is.
type hostTag struct {
	sid string
	err error
}

func (e *hostTag) Error() string { return e.err.Error() }

func (e *hostTag) Unwrap() error { return e.err }

func atHost(sid string, err error) error {
	return &hostTag{sid: sid, err: err}
}

// cellError names the params cell whose value failed to render.
type cellError struct {
	path string
	err  error
}

func (e *cellError) Error() string {
	return fmt.Sprintf("render: cell %q: %v", e.path, e.err)
}

func (e *cellError) Unwrap() error { return e.err }
