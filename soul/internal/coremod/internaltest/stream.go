// Package internaltest — shared test helpers for unit tests of Soul-side
// core modules (pkg/file/service/user/group/…). The package itself has no
// _test suffix, because test files of different packages (pkg_test,
// file_test, …) can't import each other's xxx_test packages.
//
// Contents are test infrastructure only, not used in production. The file
// ends up in the production build as dead code, but is never instantiated
// (no inits, no registry references).
package internaltest

import (
	"context"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// ApplyStream — fake grpc.ServerStreamingServer[ApplyEvent] for unit tests.
// Captures all Send events in Events; the final event is available as
// Last(). Ctx (optional) — run context for modules reading stream.Context()
// (augur client, core.module's FetchModule transport).
type ApplyStream struct {
	grpc.ServerStreamingServer[pluginv1.ApplyEvent]
	Events []*pluginv1.ApplyEvent
	Ctx    context.Context
}

func (s *ApplyStream) Send(e *pluginv1.ApplyEvent) error {
	s.Events = append(s.Events, e)
	return nil
}

func (s *ApplyStream) Context() context.Context {
	if s.Ctx != nil {
		return s.Ctx
	}
	return context.Background()
}

// Last — the most recently sent event; nil if none.
func (s *ApplyStream) Last() *pluginv1.ApplyEvent {
	if len(s.Events) == 0 {
		return nil
	}
	return s.Events[len(s.Events)-1]
}

// Runner — deterministic fake util.Runner. The key is built as name + " " +
// args (space-joined). Commands without explicit setup return Fallback.
//
// Each key can carry a response queue (via OnSeq) — successive calls to the
// same command get different results. Needed for scenarios like
// "pre-install: not installed; post-install: installed". Once the queue is
// exhausted, the last element sticks. Plain On is sugar for a single-element
// queue.
type Runner struct {
	Calls    []string
	Results  map[string][]util.Result
	Fallback util.Result
}

// NewRunner — constructor with empty Results and Fallback {ExitCode: 127}
// (mimics "command not found", a sane default for DetectPkgMgr /
// DetectInitSystem checks).
func NewRunner() *Runner {
	return &Runner{Results: map[string][]util.Result{}, Fallback: util.Result{ExitCode: 127}}
}

// On — fluent single-response setup for a command. Overwrites the current queue.
func (r *Runner) On(cmd string, res util.Result) *Runner {
	r.Results[cmd] = []util.Result{res}
	return r
}

// OnSeq — configures a response sequence for repeat calls of a command.
// The last element sticks once the queue is exhausted.
func (r *Runner) OnSeq(cmd string, results ...util.Result) *Runner {
	r.Results[cmd] = append([]util.Result(nil), results...)
	return r
}

func (r *Runner) Run(_ context.Context, name string, args ...string) util.Result {
	return r.dispatch(name, args)
}

// RunOpts — fake variant supporting cwd/env: the key is prefixed with
// `[cwd=<dir>] ` and/or `[env=KEY=VAL,…] ` so a test can verify the module
// actually passed the expected options. Sorted env keys keep this
// deterministic (Go map iteration isn't).
func (r *Runner) RunOpts(_ context.Context, opts util.RunOptions) util.Result {
	var prefix string
	if opts.Cwd != "" {
		prefix += "[cwd=" + opts.Cwd + "] "
	}
	if len(opts.Env) > 0 {
		env := append([]string(nil), opts.Env...)
		sort.Strings(env)
		prefix += "[env=" + strings.Join(env, ",") + "] "
	}
	key := prefix + opts.Name
	for _, a := range opts.Args {
		key += " " + a
	}
	r.Calls = append(r.Calls, key)
	seq, ok := r.Results[key]
	if !ok || len(seq) == 0 {
		return r.Fallback
	}
	res := seq[0]
	if len(seq) > 1 {
		r.Results[key] = seq[1:]
	}
	return res
}

func (r *Runner) dispatch(name string, args []string) util.Result {
	key := name
	for _, a := range args {
		key += " " + a
	}
	r.Calls = append(r.Calls, key)
	seq, ok := r.Results[key]
	if !ok || len(seq) == 0 {
		return r.Fallback
	}
	res := seq[0]
	if len(seq) > 1 {
		r.Results[key] = seq[1:]
	}
	// if seq.len==1 — keep the last one sticky
	return res
}
