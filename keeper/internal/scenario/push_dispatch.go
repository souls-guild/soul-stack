package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/applysink"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The dispatcher's push branch (NIM-880). A scenario run reaches an
// agent host over the gRPC EventStream (ADR-012) and a `transport: ssh` host by
// opening an SSH session and exec'ing the `soul` binary the Keeper delivers
// (ADR-032). Both branches mint the same `apply_runs` row, feed the same
// [applysink.Sink] and are waited on by the same barrier — which is what makes
// `register:` and a `require:` over it behave identically on either transport.
//
// Reason codes. ⚠ They surface in TWO different places, and the difference
// matters to whoever is reading a failed run:
//
//   - the pre-dispatch refusals (transport/provider/push wiring) prefix the error
//     that aborts the run. Only `push_not_configured` aborts with its own reason;
//     the rest travel inside `incarnation.status_details.error` under the generic
//     `reason: dispatch_failed`, because they are raised from dispatch;
//   - `push_transport_failed` and `soul_passage_unsupported` are also written
//     into a host's `apply_runs.error_summary`, reaching `status_details` through
//     the barrier's failure reason. ⚠ `soul_passage_unsupported` does BOTH: the
//     streamed staged gate in run.go aborts with it directly, so the same string
//     can arrive either as the run's own `reason` or inside a host's summary.
const (
	// reasonTransportDisagreement — two tasks of one Passage target one host
	// and name different transports. One host gets ONE ApplyRequest per Passage
	// (composite PK apply_id,sid,passage), so there is no split to make: picking
	// either would dispatch a task down a line its own file does not name.
	reasonTransportDisagreement = "transport_disagreement"
	// reasonTransportMismatch — the task named a transport the host's registry
	// row does not carry. The key overrides the SshProvider, the account and the
	// port ([push.TransportOverride]); it does not retype the host, and
	// [push.SshDispatcher] refuses a non-ssh row anyway — refused here so the
	// message names the contradiction instead of the symptom.
	reasonTransportMismatch = "transport_mismatch"
	// reasonPushNotConfigured — the run needs the push branch and this Keeper
	// has none (empty `plugins.ssh_providers[]`, no discovered SshProvider, or
	// no `push.host_ca_ref`). A pull-only installation is a normal
	// configuration; targeting a push host from it is not.
	reasonPushNotConfigured = "push_not_configured"
	// reasonProviderNotRouted — no level of the ADR-032 router produced an
	// SshProvider for this host and the task named none either.
	reasonProviderNotRouted = "provider_not_routed"
	// reasonPushTransportFailed — the SSH session broke before a RunResult. A
	// CONSTANT, never the error text: `apply_runs.error_summary` is read back
	// unmasked, and a transport error can carry the request that produced it.
	reasonPushTransportFailed = "push_transport_failed"
	// reasonSoulPassageUnsupported — a host cannot carry a staged run's Passage
	// field (ADR-056 §S5). ONE constant for both branches, because it is one
	// defect caught two ways: on the stream by the capability a Soul announced,
	// on push by the Passage the delivered binary echoes back.
	reasonSoulPassageUnsupported = "soul_passage_unsupported"
)

// PushApplyDispatcher is the narrow push surface the scenario dispatcher needs:
// run one ApplyRequest on one host over SSH and hand back its RunResult. An
// interface (not *push.SshDispatcher) so the runner stays unit-testable without
// an SSH provider plugin, a host CA and a live sshd.
//
// Unlike [ApplyDispatcher] this call is SYNCHRONOUS — it returns when the run
// on the host has finished — so the caller runs it in its own goroutine and
// lets the barrier observe the terminal row, exactly as it observes a RunResult
// arriving on a stream.
//
// ★ CONTRACT: onEvent is called on THIS goroutine, for every event, and every
// call returns before SendApply does. [Runner.dispatchPushHost] reads a flag the
// handler sets as soon as SendApply returns, so an implementation that fanned
// events to a goroutine would let wrong-Passage rows land silently. The shipped
// [push.SshDispatcher] satisfies this by construction — it drives the handler
// from `ParseStream` over an already-collected stdout — but the requirement is
// the interface's, not that implementation's accident.
//
// Implemented by [push.SshDispatcher].
type PushApplyDispatcher interface {
	SendApply(ctx context.Context, sid string, route push.Route, req *keeperv1.ApplyRequest, onEvent push.EventHandler) (*keeperv1.RunResult, error)
}

// PushProviderRouter is the ADR-032 3-tier SshProvider resolver (per-SID →
// per-coven → cluster), narrowed to its one method. The task's own
// `transport: { ssh: { ssh_provider: … } }` stands above all three as Level 0
// (NIM-870) and short-circuits it.
//
// Implemented by [push.PGRouter]. nil beside a non-nil dispatcher is SUPPORTED,
// not a wire-up error: every push host must then carry
// `transport: { ssh: { ssh_provider: … } }`, and one that does not is refused
// per-host as `provider_not_routed` rather than at startup.
type PushProviderRouter interface {
	RouteFor(ctx context.Context, sid string) (providerName string, source push.RouteSource, err error)
}

// PushRouteObserver counts routing decisions
// (`keeper_push_provider_routed_total`), the same counter the bare push API
// feeds. nil → no-op.
type PushRouteObserver interface {
	ObserveProviderRouted(providerName, decisionSource string)
}

// hostDispatch is one host's resolved transport decision for one Passage: which
// branch carries it, what the TASK named (empty = it wrote no key) and what the
// task overrode. The three are kept apart because `transport: ssh` names a
// transport and overrides nothing — a record that showed only the override
// would make such a run indistinguishable from one that never wrote the key.
type hostDispatch struct {
	// transport is the EFFECTIVE transport: [config.TransportAgent] or
	// [config.TransportSSH]. It always equals the host's `souls.transport`; see
	// [resolveHostDispatch] for why the task cannot move it.
	transport string
	// named is the transport the task wrote, "" when it wrote none.
	named    string
	override push.TransportOverride
}

func (h hostDispatch) isPush() bool { return h.transport == config.TransportSSH }

// resolveHostDispatch derives the per-host transport decision for one Passage's
// plans, over the roster the run resolved.
//
// ★ The effective transport is the HOST's (`souls.transport`), and a task's
// `transport:` may not contradict it. That is not a weakening of "the task beats
// the registry" (NIM-870) — the three things that key overrides are the
// SshProvider, the account and the port, and all three still beat
// `souls.ssh_target` and `keeper.yml::push.*`. What it may not do is retype the
// host: the address, the credentials and the very existence of a push target
// come from the registry row, `push.SshDispatcher` refuses a row whose transport
// is not `ssh`, and an agent host has no `ssh_target` to dial. A key that
// claimed otherwise would be a key that bootstraps a bare VM, and it does not
// (see [config.TransportSpecOf]).
//
// So the key does two things here: it states the transport out loud, where
// `soul-lint` can judge it offline and a reader of the file can see it, and it
// carries the Level 0 overrides. Naming a transport the host does not have is a
// refusal, not a no-op — a decorative key is the one outcome NIM-870 exists to
// prevent.
//
// A host in perHost with no roster entry cannot happen (both come from the same
// resolve) and is refused rather than defaulted: guessing `agent` would dispatch
// over a stream nobody confirmed.
func resolveHostDispatch(perHost map[string][]*render.RenderedTask, plans []render.DispatchPlan, hosts []*topology.HostFacts) (map[string]hostDispatch, map[string]refusal) {
	registry := make(map[string]string, len(hosts))
	for _, h := range hosts {
		if h == nil {
			continue
		}
		registry[h.SID] = h.Transport
	}

	named, refusals := namedTransportsByHost(perHost, plans)
	out := make(map[string]hostDispatch, len(perHost))
	for sid := range perHost {
		if _, bad := refusals[sid]; bad {
			continue
		}
		effective, ok := registry[sid]
		if !ok {
			refusals[sid] = refusal{
				reason: reasonTransportMismatch,
				err:    fmt.Errorf("host %s is not in the run's roster, so its transport is unknown", sid),
			}
			continue
		}
		if effective == "" {
			// `souls.transport` is NOT NULL DEFAULT 'agent' (migration 007); an
			// empty value here means a projection that stopped reading the column,
			// not a host without one.
			effective = config.TransportAgent
		}
		spec := named[sid]
		if spec.named != "" && spec.named != effective {
			refusals[sid] = refusal{
				reason: reasonTransportMismatch,
				err: fmt.Errorf("task names transport %q for host %s, whose registry row carries transport %q - the key overrides the ssh provider, user and port, it does not retype the host",
					spec.named, sid, effective),
			}
			continue
		}
		out[sid] = hostDispatch{transport: effective, named: spec.named, override: spec.override}
	}
	return out, refusals
}

// refusal is a per-host pre-dispatch rejection: a reason code for
// `apply_runs.error_summary` and the sentence behind it.
type refusal struct {
	reason string
	err    error
}

// namedTransportsByHost folds the `transport:` each plan carries onto the hosts
// that plan targets, and refuses a host whose tasks disagree.
//
// Disagreement is an error rather than first-wins for the same reason
// `pushorch` refuses it: one host receives ONE ApplyRequest per Passage, so
// there is no request to split, and a run that silently picked one of two
// decisions is a run nobody can explain afterwards. A task that named none
// agrees with everything — the block-level key already merged into it during
// render (mergeBlockInheritance), so "none" here means the whole enclosing
// chain named none.
//
// ★ The comparison is the WHOLE decision, not the transport's name.
// `transport: ssh` beside `transport: { ssh: { user: deploy } }` names one
// transport and two different connections, and letting the later plan win would
// drop a Level 0 override with no diagnostic — the exact outcome the name check
// is justified by, one field over.
//
// Only the tasks that survived the cross-passage gate for a host vote on that
// host's transport. `plans` is the Passage's whole list, but `perHost` has
// already been narrowed by [crossPassageGate.applyGate]: a task the gate dropped
// runs nowhere, so letting it disagree would abort a run over a line that is not
// being dispatched.
func namedTransportsByHost(perHost map[string][]*render.RenderedTask, plans []render.DispatchPlan) (map[string]hostDispatch, map[string]refusal) {
	live := make(map[string]map[int]bool, len(perHost))
	for sid, tasks := range perHost {
		set := make(map[int]bool, len(tasks))
		for _, t := range tasks {
			if t != nil {
				set[t.Index] = true
			}
		}
		live[sid] = set
	}

	out := make(map[string]hostDispatch, len(perHost))
	refusals := make(map[string]refusal)
	for _, p := range plans {
		if p.Keeper || p.TransportName == "" {
			continue
		}
		override, usable, err := push.TransportOverrideFrom(p.TransportName, p.TransportParams)
		// `usable` is false for a transport push cannot carry (`agent`), which is
		// not a failure here: it is the ordinary pull path, and the empty override
		// is the right answer for it.
		if !usable {
			override = push.TransportOverride{}
		}
		want := hostDispatch{named: p.TransportName, override: override}

		for _, sid := range p.TargetSIDs {
			if !live[sid][p.TaskIndex] {
				continue
			}
			if err != nil {
				// The key decoded to a transport whose params do not. Fails the
				// hosts it targets rather than falling back to the registry the key
				// exists to beat — the same refusal render.stampTransport makes
				// offline.
				refusals[sid] = refusal{reason: reasonTransportMismatch, err: err}
				continue
			}
			if prev, seen := out[sid]; seen && prev != want {
				refusals[sid] = refusal{
					reason: reasonTransportDisagreement,
					err:    transportDisagreementError(sid, prev, want),
				}
				continue
			}
			out[sid] = want
		}
	}
	return out, refusals
}

// transportDisagreementError words the two shapes of disagreement apart,
// because their fixes are opposite: one task is on the wrong transport, or two
// tasks describe two different connections over the same one.
func transportDisagreementError(sid string, prev, want hostDispatch) error {
	if prev.named != want.named {
		return fmt.Errorf("tasks targeting host %s name two transports in one Passage (%q and %q) - a host receives one ApplyRequest per Passage, so there is nothing to split",
			sid, prev.named, want.named)
	}
	return fmt.Errorf("tasks targeting host %s name transport %q with two different parameter sets in one Passage (%s and %s) - a host receives one ApplyRequest per Passage, so one of them would be silently dropped",
		sid, prev.named, describeOverride(prev.override), describeOverride(want.override))
}

// describeOverride renders the three fields a task may override, for a
// diagnostic. An unset field is printed as `-` rather than omitted: the whole
// point of the message is which of the two lines set what.
func describeOverride(o push.TransportOverride) string {
	provider, user, port := o.Provider, o.User, strconv.Itoa(o.Port)
	if provider == "" {
		provider = "-"
	}
	if user == "" {
		user = "-"
	}
	if o.Port == 0 {
		// 0 is how an unset port is spelled in the struct; printing it would read
		// as "port zero was written", in the one message whose job is saying which
		// of the two lines set what.
		port = "-"
	}
	return fmt.Sprintf("{ssh_provider: %s, user: %s, port: %s}", provider, user, port)
}

// guardPushConfigured refuses a run whose STARTING roster holds a push host on a
// Keeper that has no push dispatcher.
//
// ★ Once, before the first Passage and before any keeper-side task, because this
// is a property of the whole run rather than of a Passage: `PushApply` is nil
// for the entire life of a pull-only Keeper. Left to the per-Passage pass it
// would abort a staged run only when its push Passage came up — with the earlier
// Passages fully applied and, worse, with the cloud VMs `core.cloud.provisioned`
// had already created still standing.
//
// It does NOT cover a host that joins at a refresh boundary; that is what the
// same check inside [Runner.resolvePushRoutes] is still there for.
func (r *Runner) guardPushConfigured(incarnationName, scenarioName string, hosts []*topology.HostFacts) error {
	if r.deps.PushApply != nil {
		return nil
	}
	pushSIDs := pushHostSIDs(hosts)
	if len(pushSIDs) == 0 {
		return nil
	}
	return fmt.Errorf("scenario %s/%s: hosts %v have transport=ssh but this Keeper has no push dispatcher configured (plugins.ssh_providers / push.host_ca_ref) - a pull-only installation cannot reach them",
		incarnationName, scenarioName, pushSIDs)
}

// pushHostSIDs lists the roster's `transport: ssh` hosts, sorted. Used by the
// paths that cannot carry one, so their refusal can name them.
func pushHostSIDs(hosts []*topology.HostFacts) []string {
	out := make([]string, 0)
	for _, h := range hosts {
		if h != nil && h.Transport == config.TransportSSH {
			out = append(out, h.SID)
		}
	}
	sort.Strings(out)
	return out
}

// refusedDispatchError folds the per-host pre-dispatch refusals into the one
// error that aborts the run. All of them are reported, not just the first: a
// scenario that contradicts itself usually does so on every host it targets,
// and an operator who fixes them one run at a time learns that the slow way.
//
// Hosts are listed in SID order so the message is byte-identical for identical
// input.
func refusedDispatchError(refusals map[string]refusal) error {
	sids := make([]string, 0, len(refusals))
	for sid := range refusals {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	parts := make([]string, 0, len(sids))
	for _, sid := range sids {
		parts = append(parts, fmt.Sprintf("%s: %v", refusals[sid].reason, refusals[sid].err))
	}
	return fmt.Errorf("scenario: dispatch refused - %s", strings.Join(parts, "; "))
}

// pushRoute is one host's resolved SshProvider decision, kept beside the route
// so the audit event can name the level that answered.
type pushRoute struct {
	route  push.Route
	source push.RouteSource
}

// resolvePushRoutes resolves every push host's SshProvider BEFORE the first
// wave, and refuses the run if any of them cannot be resolved.
//
// ★ Before the Passage's first wave, not at each host's turn: otherwise the
// hosts sorted ahead of the failing one would already have executed their
// ApplyRequests, and another push host would already have opened, run and closed
// an SSH session, before the run aborted over a routing miss. `pushorch`
// resolves its providers in the same order and for the same reason.
//
// ⚠ The `push_not_configured` arm here is the SECOND line of defence. A Keeper
// with no push wiring is a property of the whole run, so [Runner.run] refuses it
// once against the starting roster, before the first Passage and before any
// keeper-side task provisions anything. This arm still stands because the roster
// can GROW at a refresh boundary (ADR-0061 §S3) and bring a push host into a
// later Passage that the up-front check never saw.
//
// `provider_not_routed` cannot be hoisted the same way and is deliberately left
// per-Passage: the task's `transport: { ssh: { ssh_provider: … } }` is Level 0
// (ADR-0088), so which provider a host gets is a property of the Passage's
// rendered plans, and for a staged run Passage N's plans are placeholders until
// Passage N-1 has run.
func (r *Runner) resolvePushRoutes(ctx context.Context, dispatchByHost map[string]hostDispatch) (map[string]pushRoute, map[string]refusal) {
	routes := make(map[string]pushRoute)
	refusals := make(map[string]refusal)
	for _, sid := range sortedDispatchSIDs(dispatchByHost) {
		hd := dispatchByHost[sid]
		if !hd.isPush() {
			continue
		}
		if r.deps.PushApply == nil {
			refusals[sid] = refusal{
				reason: reasonPushNotConfigured,
				err:    fmt.Errorf("host %s has transport=ssh but this Keeper has no push dispatcher configured (plugins.ssh_providers / push.host_ca_ref)", sid),
			}
			continue
		}
		route, source, err := r.resolvePushRoute(ctx, sid, hd)
		if err != nil {
			refusals[sid] = refusal{reason: reasonProviderNotRouted, err: err}
			continue
		}
		routes[sid] = pushRoute{route: route, source: source}
	}
	return routes, refusals
}

// sortedDispatchSIDs keeps the routing pass deterministic: the router's levels
// read PG per SID, and a refusal message assembled in map order would differ
// between two runs of the same plan.
func sortedDispatchSIDs(dispatchByHost map[string]hostDispatch) []string {
	out := make([]string, 0, len(dispatchByHost))
	for sid := range dispatchByHost {
		out = append(out, sid)
	}
	sort.Strings(out)
	return out
}

// startPushHost records the transport decision and hands the run to its own
// goroutine. The route was resolved before the first wave
// ([Runner.resolvePushRoutes]), so nothing here can refuse — a missing entry is
// an invariant violation, and it fails the host rather than dialling with a
// zero route.
func (r *Runner) startPushHost(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, sid string, hd hostDispatch, pr pushRoute, req *keeperv1.ApplyRequest, taskCount int) error {
	if pr.route.Provider == "" || r.deps.PushApply == nil {
		err := fmt.Errorf("scenario: host %s reached push dispatch with no resolved route - resolvePushRoutes should have refused the run first", sid)
		r.terminatePushHost(ctx, spec, log, passage, sid, applyrun.StatusFailed, reasonProviderNotRouted)
		return err
	}

	// Counted HERE and not where the route was resolved, because resolution is
	// not dispatch: a fail-stop in an earlier wave, or the invariant return
	// below, leaves a host resolved and never dialled, and observing at resolve
	// time reported a routing decision for a session that never opened. (This is
	// still once per host per PASSAGE — a staged run legitimately routes each of
	// its Passages, and each of them does open a session.)
	r.observePushRoute(pr.route.Provider, pr.source)
	r.auditPushDispatched(ctx, spec.ApplyID, sid, taskCount, hd, pr.route, pr.source, log)
	log.Info("scenario: ApplyRequest dispatched over push",
		slog.String("sid", sid),
		slog.Int("tasks", taskCount),
		slog.String("transport", hd.transport),
		slog.String("ssh_provider", pr.route.Provider),
		slog.String("route_source", pr.source.String()))

	// Tracked on the Runner's WaitGroup so Shutdown waits for a host that is
	// mid-session. Safe to Add here: the run-goroutine holds a count of its own,
	// so the counter is never zero at this point.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.dispatchPushHost(ctx, spec, log, passage, sid, hd, pr.route, req)
	}()
	return nil
}

// dispatchPushHost runs one host's ApplyRequest over SSH and records its
// terminal, in its own goroutine. The `apply_runs` row is already inserted
// (running) by the caller, so the barrier is already waiting on this host.
//
// Two properties make the barrier behave the same as on the stream branch:
//
//   - the TaskEvents go through the SAME [applysink.Sink] the EventStream
//     handler uses, so `register:`, the failure reason, the notices, the audit
//     events and the SSE frames land in the same places;
//   - every exit writes a terminal status. A transport failure (no provider, a
//     refused Authorize, a cut-off stream) never produces a RunResult, and
//     without a written terminal the barrier would wait for this host until the
//     run timeout.
//
// The terminal write deliberately runs on a context detached from the run's:
// cancelling a run tears down the SSH session, and a terminal that then failed
// to write would turn a cancel into a hang.
func (r *Runner) dispatchPushHost(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, sid string, hd hostDispatch, route push.Route, req *keeperv1.ApplyRequest) {
	sink := applysink.New(applysink.Deps{
		DB:    r.applyDB,
		Audit: r.deps.Audit,
		Bus:   r.deps.ApplyBus,
		// A push run is the Keeper's own act: it holds the SSH session and parses
		// the stream itself, with no Soul-initiated connection to forward from.
		// Same source the keeper-side `on: keeper` events carry.
		Source: audit.SourceKeeperInternal,
		Logger: log,
	})

	// ★ The Passage echo is CORRECTED and reported, not trusted and not thrown
	// away. This is what stands in for the ADR-056 §S5 gate on a push host
	// (NIM-880): that gate asks a presence source whether a Soul announced
	// `passage` support, and a push host announces nothing.
	//
	// The hazard is real here and worse. A binary that does not know the field
	// echoes `passage: 0` on every event, [applysink] keys the register and the
	// failure reason on the ECHO, and [Runner.terminatePushHost] keys the terminal
	// on the Keeper's own number — so a Passage-1 failure would write its reason
	// into the Passage-0 row while Passage 1 went terminal with a bare `failed`.
	// Wrong data, no diagnostic.
	//
	// The Keeper put `passage` in the ApplyRequest, so a disagreeing echo is a
	// protocol violation rather than a fact: the AUTHORITATIVE number is stamped
	// back on before the event is persisted. Dropping the events instead — which
	// an earlier revision of this branch did — closes the corruption and opens a
	// worse hole: the request still runs to completion on the host, so a Passage
	// that installed packages and wrote files would leave the machine changed and
	// the run record holding no trace of what was applied.
	//
	// The run still FAILS: a binary that cannot carry the field cannot be trusted
	// with a staged plan. But it fails with its work recorded.
	//
	// A non-staged run sends passage 0 and every binary echoes 0, so the common
	// case is untouched.
	//
	// ⚠ The flag is read after SendApply returns, which is only sound because
	// [PushApplyDispatcher] requires onEvent to be called synchronously before it
	// does — see that interface's doc.
	var (
		passageEchoMismatch atomic.Bool
		sawTaskFailure      atomic.Bool
	)
	rr, err := r.deps.PushApply.SendApply(ctx, sid, route, req, func(ev *keeperv1.TaskEvent) {
		if int(ev.GetPassage()) != passage {
			passageEchoMismatch.Store(true)
			ev = stampPassage(ev, passage)
		}
		if isFailedTaskStatus(ev) {
			sawTaskFailure.Store(true)
		}
		sink.TaskEvent(ctx, sid, ev)
	})

	// A transport failure is LOGGED even when the echo was also wrong — they are
	// different breakages with different fixes, and an earlier revision let the
	// echo branch return before this line, so the thing that actually ended the
	// session went unrecorded anywhere. The operator-visible `error_summary`
	// still carries only one of them: the echo code wins, because a binary that
	// cannot carry the Passage explains the broken session more often than the
	// other way round.
	if err != nil {
		log.Warn("scenario: push run failed before RunResult",
			slog.String("sid", sid),
			slog.String("ssh_provider", route.Provider),
			slog.Bool("passage_echo_mismatch", passageEchoMismatch.Load()),
			slog.Any("error", err))
	}
	if passageEchoMismatch.Load() {
		log.Error("scenario: push host echoed the wrong Passage - its soul binary cannot carry a staged plan",
			slog.String("sid", sid), slog.Int("passage", passage))
		// Same suppression as the transport branch below, for the same reason:
		// UpdateStatus COALESCEs, so a non-nil summary WINS. A task that failed
		// has already had its own reason recorded — under the CORRECTED Passage,
		// because the stamp happened first — and that reason is what an operator
		// acts on. The Passage problem is infrastructure: it is in the log line
		// above, and the run fails either way.
		r.terminatePushHost(ctx, spec, log, passage, sid, applyrun.StatusFailed,
			passageMismatchSummary(sid, passage, sawTaskFailure.Load()))
		return
	}
	if err != nil {
		// ★ A SAFE constant, never err.Error(). `error_summary` is read through
		// GET /v1/incarnations/<name> WITHOUT masking, and a SendApply error text
		// can carry the request — the same reason the stream branch writes the
		// constant `send_apply_failed` instead of its error.
		//
		// ⚠ The full error goes to the log, and the KEEPER LOG IS NOT MASKED
		// either (`shared/log` builds a bare slog handler). A malformed NDJSON
		// line, for instance, is wrapped with 200 bytes of the host's raw output.
		// That is pre-existing parity with `pushorch` rather than something this
		// branch introduced, but it is not a reason to widen it to a column an
		// operator reads over HTTP.
		//
		// And only when no task failure was recorded: UpdateStatus COALESCEs, so a
		// non-nil argument WINS, and writing here unconditionally would replace the
		// per-task reason the sink already stored with a transport sentence that
		// says less.
		summary := ""
		if !sawTaskFailure.Load() {
			summary = reasonPushTransportFailed
		}
		r.terminatePushHost(ctx, spec, log, passage, sid, applyrun.StatusFailed, summary)
		return
	}

	// The RunResult's own Passage gets the SAME treatment as the events: it is
	// corrected, recorded and then fails the host. `run.completed` and the
	// apply.completed/failed SSE frame are stamped from this message, so
	// returning early here — which an earlier revision did — would leave a host
	// that ran with no terminal record at all, which is the same "the work
	// happened and nothing says so" hole the events already taught us.
	//
	// Reachable independently of the event check: a Passage whose tasks were all
	// skipped emits no TaskEvent, so this is the only guard that fires.
	runResultEchoMismatch := int(rr.GetPassage()) != passage
	if runResultEchoMismatch {
		log.Error("scenario: push host echoed the wrong Passage on its RunResult",
			slog.String("sid", sid), slog.Int("passage", passage))
		rr = stampRunResultPassage(rr, passage)
	}

	sink.RunResult(ctx, sid, rr)
	if runResultEchoMismatch {
		r.terminatePushHost(ctx, spec, log, passage, sid, applyrun.StatusFailed,
			passageMismatchSummary(sid, passage, sawTaskFailure.Load()))
		return
	}
	status := pushRunStatus(rr.GetStatus())
	log.Info("scenario: push run finished",
		slog.String("sid", sid),
		slog.String("transport", hd.transport),
		slog.String("ssh_provider", route.Provider),
		slog.String("status", rr.GetStatus().String()))
	// error_summary is left nil on the failure path too: the per-task reason is
	// already recorded by the sink above (applysink.recordTaskFailure), and
	// UpdateStatus COALESCEs — passing a generic sentence here would overwrite it
	// and say less.
	r.terminatePushHost(ctx, spec, log, passage, sid, status, "")
}

// passageMismatchSummary is the `error_summary` for a host whose binary echoed
// the wrong Passage: the reason code, unless a task already recorded its own.
//
// Empty when one did, because [applyrun.UpdateStatus] COALESCEs and a non-nil
// argument WINS — writing here would replace `task 3 core.pkg.installed: E:
// Version '7.2.4' not found` with a sentence about the protocol, and the failure
// an operator can act on would be unrecoverable from the API.
func passageMismatchSummary(sid string, passage int, sawTaskFailure bool) string {
	if sawTaskFailure {
		return ""
	}
	return fmt.Sprintf("%s: host %s echoed a Passage other than %d - the soul binary it ran cannot carry a staged plan (ADR-056 §S5)",
		reasonSoulPassageUnsupported, sid, passage)
}

// stampPassage returns ev carrying the Keeper's authoritative Passage, without
// mutating the caller's message.
//
// [proto.Clone] is a DEEP copy, so on a staged run against a passage-blind binary
// every event copies its `register_data` — up to the 1 MiB per line
// [push.ParseStream] admits. That is the price of keeping the run's record of
// what actually happened on the host: the alternative is either mutating a
// message the caller still owns, or discarding it.
//
// nil in, nil out. The [PushApplyDispatcher] contract does not forbid a nil
// event and [applysink.Sink.TaskEvent] tolerates one, so this must too — a
// typed-nil survives the type assertion and would be dereferenced.
func stampPassage(ev *keeperv1.TaskEvent, passage int) *keeperv1.TaskEvent {
	if ev == nil {
		return nil
	}
	out, ok := proto.Clone(ev).(*keeperv1.TaskEvent)
	if !ok || out == nil {
		return ev
	}
	out.Passage = int32(passage)
	return out
}

// stampRunResultPassage is [stampPassage] for the run's final report — same
// reason, same nil handling.
func stampRunResultPassage(rr *keeperv1.RunResult, passage int) *keeperv1.RunResult {
	if rr == nil {
		return nil
	}
	out, ok := proto.Clone(rr).(*keeperv1.RunResult)
	if !ok || out == nil {
		return rr
	}
	out.Passage = int32(passage)
	return out
}

// isFailedTaskStatus reports whether an event made [applysink] record a per-task
// reason — the WHOLE of its condition, status AND index: recordTaskFailure also
// requires a non-negative task_idx, and reading only the status here would
// suppress the transport reason for an event that recorded nothing, leaving the
// barrier with a bare `failed`.
func isFailedTaskStatus(ev *keeperv1.TaskEvent) bool {
	if ev == nil || ev.GetTaskIdx() < 0 {
		return false
	}
	st := ev.GetStatus()
	return st == keeperv1.TaskStatus_TASK_STATUS_FAILED || st == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT
}

// pushRunStatus maps a [keeperv1.RunStatus] onto the `apply_runs` terminal, the
// same mapping the EventStream handler applies. ERROR_LOCKED and anything
// unrecognized fold into failed (fail-closed).
func pushRunStatus(rs keeperv1.RunStatus) applyrun.Status {
	switch rs {
	case keeperv1.RunStatus_RUN_STATUS_SUCCESS:
		return applyrun.StatusSuccess
	case keeperv1.RunStatus_RUN_STATUS_CANCELLED:
		return applyrun.StatusCancelled
	default:
		return applyrun.StatusFailed
	}
}

// pushTerminalWriteTimeout bounds the detached terminal write. Deliberately
// well under [Runner.Shutdown]'s 15s post-cancel grace: this write is the last
// thing a push goroutine does, so a budget equal to the grace would make a slow
// PG turn every forced shutdown into the "run goroutines did not exit within
// 15s - leak suspected" warning. The only thing waiting on it is the barrier,
// which otherwise waits out the whole run timeout.
const pushTerminalWriteTimeout = 5 * time.Second

// terminatePushHost moves a push host's `apply_runs` row to its terminal.
//
// The context is detached from the run's (context.WithoutCancel) and given its
// own short deadline: the common way a push run ends early is the run being
// cancelled, and writing the terminal on the cancelled context would leave the
// row `running` — the barrier would then wait for a host that has already
// stopped.
func (r *Runner) terminatePushHost(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, sid string, status applyrun.Status, summary string) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTerminalWriteTimeout)
	defer cancel()

	var summaryPtr *string
	if summary != "" {
		summaryPtr = &summary
	}
	if r.applyDB == nil {
		// A unit build without PG. Nothing waits on a barrier there either.
		return
	}
	if err := applyrun.UpdateStatus(writeCtx, r.applyDB, spec.ApplyID, sid, passage, status, summaryPtr); err != nil {
		if errors.Is(err, applyrun.ErrApplyRunAlreadyTerminal) {
			// Another writer got there first (a re-dispatch, a recovery takeover).
			// The first committer wins, same as on the stream branch.
			log.Info("scenario: push host already terminal - no-op",
				slog.String("sid", sid), slog.String("status", string(status)))
			return
		}
		log.Error("scenario: writing the push host terminal failed - the barrier will wait out the run timeout",
			slog.String("sid", sid),
			slog.String("status", string(status)),
			slog.Any("error", err))
	}
}

// resolvePushRoute picks the SshProvider for one push host: the task's own
// `transport: { ssh: { ssh_provider: … } }` as Level 0 (NIM-870), otherwise the
// ADR-032 router's three levels. The task's user/port override rides the route
// whichever level answered — choosing a provider and overriding the connection
// fields are independent axes, and a task that named only a user must not lose
// it because the cluster default answered for the provider.
func (r *Runner) resolvePushRoute(ctx context.Context, sid string, hd hostDispatch) (push.Route, push.RouteSource, error) {
	if hd.override.Provider != "" {
		return push.Route{Provider: hd.override.Provider, Override: hd.override}, push.SourceTask, nil
	}
	if r.deps.PushRouter == nil {
		return push.Route{}, push.SourceUnknown, errors.New("no SshProvider router configured and the task named no ssh_provider")
	}
	name, source, err := r.deps.PushRouter.RouteFor(ctx, sid)
	if err != nil {
		return push.Route{}, push.SourceUnknown, err
	}
	return push.Route{Provider: name, Override: hd.override}, source, nil
}

func (r *Runner) observePushRoute(provider string, source push.RouteSource) {
	if r.deps.PushRoutes == nil {
		return
	}
	r.deps.PushRoutes.ObserveProviderRouted(provider, source.String())
}

// auditPushDispatched records the effective transport decision for one host on
// the run's audit trail (`apply.dispatched`, correlation_id = apply_id).
//
// ★ This is the visibility half of NIM-870 for the scenario path, and it is not
// optional bookkeeping. The task's `transport:` beats `souls.ssh_target` and the
// cluster config, which knowingly made a THIRD source of truth for
// provider/user/port; the price of that decision is that a run must say which
// source actually answered, or an incident review reads a registry the run never
// went to. The bare push API pays it in `push_runs.summary.hosts[]`; a scenario
// run has no such row, and this event is where it pays instead.
//
// The fields mirror that summary exactly:
//   - `transport` is present when the TASK named one — the scalar `transport:
//     ssh` included, which names a transport and overrides nothing;
//   - `route_source` is the level that picked the provider (task/soul/coven/
//     cluster);
//   - `ssh_user`/`ssh_port` appear only when the TASK set them. Their absence
//     means the resolver answered, and only the resolver knows whether that was
//     the `souls.ssh_target` row or its own default — a label here would have to
//     guess, and guessing in the field whose whole job is provenance is worse
//     than leaving it out.
//
// Audit=nil → no-op; a write error is logged and swallowed (the run is already
// dispatched, and losing the record must not undo it).
func (r *Runner) auditPushDispatched(ctx context.Context, applyID, sid string, tasks int, hd hostDispatch, route push.Route, source push.RouteSource, log *slog.Logger) {
	if r.deps.Audit == nil {
		return
	}
	payload := map[string]any{
		"sid":          sid,
		"apply_id":     applyID,
		"tasks_count":  tasks,
		"route_source": source.String(),
		"ssh_provider": route.Provider,
	}
	if hd.named != "" {
		payload["transport"] = hd.named
	}
	if hd.override.User != "" {
		payload["ssh_user"] = hd.override.User
	}
	if hd.override.Port != 0 {
		payload["ssh_port"] = hd.override.Port
	}
	if err := r.deps.Audit.Write(ctx, &audit.Event{
		EventType:     audit.EventApplyDispatched,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: applyID,
		Payload:       payload,
	}); err != nil {
		log.Warn("scenario: writing audit apply.dispatched for the push host failed",
			slog.String("sid", sid), slog.Any("error", err))
	}
}
