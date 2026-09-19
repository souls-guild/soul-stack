// Package applysink persists and publishes the events one host reports for an
// apply run: `TaskEvent` and the final `RunResult`.
//
// It exists because a run now arrives over two transports and must leave the
// SAME trace behind either way (NIM-880). A Soul on the gRPC EventStream
// (ADR-012) and a Soul exec'd over SSH by push (ADR-032) emit byte-identical
// protobuf; before this package the stream's copy was handled in
// keeper/internal/grpc and push had no copy at all, so `register:` did not fill
// on a push run and a barrier waiting on it would never release. Routing both
// through one object is what makes "the register fills the same way on both
// transports" a fact about the code rather than a promise about two
// implementations.
//
// What it does NOT own: the apply_runs terminal transition. The stream path has
// an epoch (`attempt`) to check against a re-claim (ADR-027(g)) and an
// incarnation to resolve, the push path has neither — so each caller writes its
// own terminal and calls [Sink.RunResult] only for the observable half.
package applysink

import (
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// Deps are the four channels a host event lands in. Every one is optional and
// nil-degrades to a no-op on that channel alone — a unit build without PG, a
// single-Keeper dev without SSE and an installation with audit off each lose
// their own channel and nothing else.
type Deps struct {
	// DB is the `apply_runs` / `apply_task_register` registry. nil → no
	// register accumulation, no failure reason, no notices.
	DB applyrun.ExecQueryRower
	// Audit receives `task.executed` / `run.completed`. nil → not written.
	//
	// ★ It is not only an audit trail: the changed-task rollup and the
	// cross-passage onchanges/onfail gating (ADR-056 R3) read their facts back
	// out of these events (auditpg.SelectChangedTaskKeys). A transport that
	// skipped them would report no changed task and release no onchanges.
	Audit audit.Writer
	// Bus is the operator SSE channel. nil → no publish.
	Bus *applybus.EventBus
	// Source is the audit `source` recorded on both event types. The stream
	// path passes [audit.SourceSoulGRPC]; push passes
	// [audit.SourceKeeperInternal], because a push run is the Keeper's own act
	// — it holds the SSH session and parses the stream itself, with no
	// Soul-initiated connection to forward from. Same reason the keeper-side
	// `on: keeper` events carry it.
	Source audit.Source
	// Logger receives the swallowed errors of every best-effort write. nil →
	// discard.
	Logger *slog.Logger
}

// Sink records one host's apply events into [Deps]'s channels.
type Sink struct {
	deps Deps
}

// New returns a Sink over deps. A zero Deps is usable: every channel degrades
// to a no-op, which is what a unit test without PG/audit/SSE wants.
func New(deps Deps) *Sink {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Source == "" {
		deps.Source = audit.SourceKeeperInternal
	}
	return &Sink{deps: deps}
}
