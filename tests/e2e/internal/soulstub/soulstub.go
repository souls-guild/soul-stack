//go:build e2e

// Package soulstub is a fake-Soul helper for L3a E2E (ADR-039(2)).
//
// Opens gRPC bidi stream to Keeper over mTLS (exactly like real Soul), replies to
// ApplyRequest with pre-recorded RunResult from YAML scripts. Does NOT run real
// apply, mutate filesystem, or parse destiny: L3a contract test checks keeper-side
// lifecycle of apply_runs / RBAC / audit / metrics, not apply realism (that is L3b).
//
// NOT a binary (ADR-004): test fixture without operator lifecycle.
package soulstub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/structpb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ErrAlreadyOpen is returned when Open is called second time without Close.
var ErrAlreadyOpen = errors.New("soulstub: stream already open")

// Stub is one fake Soul holding a long-lived EventStream to Keeper.
type Stub struct {
	SID            string
	KeeperGRPCAddr string

	// endpoints is fallback list of keeper gRPC addresses (multi-keeper, mirror
	// of soul.yml::keeper.endpoints). Filled by [SetEndpoints]; used by
	// reconnectIfBroken to pick a live keeper when stream breaks.
	// KeeperGRPCAddr (initial Open) is NOT reused as first reconnect candidate:
	// endpoints list is authoritative. Empty -> reconnect disabled (single-shot
	// stub, as before WardRoster extension).
	endpoints []string

	// reconnectEnabled toggles auto-reconnect+WardRoster on stream break
	// (WardRoster dispatched-orphan e2e). Mirrors real soul/cmd/soul
	// reconnectLoop->handleSession: on (re)connect, stub sends Hello, then
	// IMMEDIATELY WardRoster(activeWard). Default false: stub does not reconnect
	// (recvLoop simply exits on error, previous L3a behavior). Enabled by
	// [EnableReconnect].
	reconnectEnabled bool

	// holdApply mode "holds ApplyRequest": on ApplyRequest, stub does NOT send
	// RunResult (apply_runs row stays `dispatched`), only registers apply_id in
	// activeWard. Emulates Soul that physically accepted task and has not
	// completed Run yet. For dispatched-orphan e2e: after SIGKILL of keeper holder,
	// row hangs dispatched, reconnect+WardRoster reconciles it. Default false.
	// Enabled by [SetHoldApply].
	holdApply bool

	// activeWard is set of apply_id values declared by stub as active in
	// WardRoster on reconnect (mirror of runtime.ApplyRunner.ActiveSet).
	// holdApply adds each held ApplyRequest; [ClearActiveWard] resets it
	// (emulates Soul process restart: no in-flight work physically exists).
	// Empty -> WardRoster sends empty set, keeper terminalizes ALL dispatched
	// rows of SID (OrphanDispatched).
	activeWard map[string]int32

	// TLS material for mTLS handshake. cert/key are stub client cert (Vault-issued
	// leaf for SID), caBundle is root CA of Keeper server cert.
	cert     []byte
	key      []byte
	caBundle []byte

	// scripted is map scenario-name -> list of ScriptEntry, filled from
	// fixtures/stub-responses.yaml. Matching by task_name (task order in
	// ApplyRequest is not guaranteed - ADR-027).
	scripted map[string][]ScriptEntry

	// errandStatus is status returned by stub for ErrandRequest (ADR-033/041).
	// Default SUCCESS (see New). recvLoop sends ErrandResult with this status +
	// echoed errand_id on ErrandRequest. Lets ErrandRun e2e test drive
	// dispatch->terminal chain without real exec (stub does not run shell - L3a contract).
	errandStatus keeperv1.ErrandStatus

	// applyStatusBySID / errandStatusBySID are per-SID overrides over global
	// default (applyDefaultSuccess / errandStatus). Needed for partial-failure
	// tests (Tide/ErrandRun abort/continue): one host in wave returns FAILED,
	// others use global default. Routing is by connection (one Stub = one SID),
	// but recvLoop explicitly checks req-SID (echo in ErrandRequest.sid) / s.SID
	// for readability and for multiple Stubs in one test. Empty -> default.
	applyStatusBySID  map[string]bool
	errandStatusBySID map[string]keeperv1.ErrandStatus

	// applyDefaultSuccess mode "success for any ApplyRequest": task not covered
	// by scripted table counts as SUCCESS (not FAILED). Useful for apply-e2e
	// where apply_runs lifecycle matters (planned->...->success), not realism of
	// per-task RunResult (L3a contract). Default false (strict mode: unscripted
	// task = FAILED, explicit signal of fixture gap). Enabled by SetApplyDefaultSuccess.
	applyDefaultSuccess bool

	// taskRegisterByName is scripted per-task register (staged-render, ADR-056):
	// task_name -> per-SID register-payload (sid -> register_data). On ApplyRequest
	// stub emits TaskEvent with RegisterData BEFORE aggregate RunResult (like real
	// Soul on probe task), echoing passage from request. Keeper-side
	// accumulateRegister stores register per-(apply_id, sid, passage), from where
	// next Passage render resolves where: register.*. Without scripted-register
	// (default), stub emits no register - normal apply (L3a contract). Enabled by
	// [SetTaskRegister].
	taskRegisterByName map[string]map[string]map[string]any

	// dryRunPlanSet toggles Plan reply on dry_run ApplyRequest (Scry, ADR-031).
	// When true, for ApplyRequest{dry_run:true} stub sends one TaskEvent per task
	// before RunResult with status=CHANGED|OK and register_data{changed:dryRunChanged};
	// keeper-side accumulateRegister stores them in apply_task_register, from
	// which CheckDrift builds per-task changed (drifted/clean). Default false:
	// without explicit enabling, dry_run run behaves as normal (only RunResult),
	// drift-report is built with host=clean (no register rows). Emulates
	// SoulModule.Plan (mod.Apply is not called on dry_run - read-only guarantee
	// ADR-031), does NOT execute real Plan core module (L3a contract, like whole stub).
	dryRunPlanSet bool
	dryRunChanged bool

	mu       sync.Mutex
	conn     *grpc.ClientConn
	stream   keeperv1.Keeper_EventStreamClient
	recorded []Message
	cancel   context.CancelFunc
	done     chan struct{}
	// closed means stub is closed (Close called). reconnectIfBroken does not
	// open a new stream after that (otherwise reconnect loop would outlive cleanup Close).
	closed bool
}

// ScriptEntry is one scripted stub reply.
//
// TaskName is task name that stub answers with RunResult.
// Status is RunStatus enum value.
// StateChanges is arbitrary jsonb payload packed into RunResult.state_changes.
type ScriptEntry struct {
	TaskName     string
	Status       keeperv1.RunStatus
	StateChanges map[string]any
}

// Message is payload from Keeper recorded by stub.
type Message struct {
	Kind  string
	Frame *keeperv1.FromKeeper
}

// New constructs Stub. cert/key are mTLS client cert, caBundle is root CA of
// Keeper server cert (for server-cert verification on handshake).
func New(sid, keeperGRPCAddr string, cert, key, caBundle []byte) *Stub {
	return &Stub{
		SID:                sid,
		KeeperGRPCAddr:     keeperGRPCAddr,
		cert:               cert,
		key:                key,
		caBundle:           caBundle,
		scripted:           make(map[string][]ScriptEntry),
		errandStatus:       keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS,
		applyStatusBySID:   make(map[string]bool),
		errandStatusBySID:  make(map[string]keeperv1.ErrandStatus),
		taskRegisterByName: make(map[string]map[string]map[string]any),
		activeWard:         make(map[string]int32),
		done:               make(chan struct{}),
	}
}

// SetEndpoints sets fallback list of keeper gRPC addresses for reconnect
// (multi-keeper, mirror of soul.yml::keeper.endpoints). Reconnect iterates in order,
// choosing first reachable live keeper. Without this call reconnect has no
// candidates and ends (stub remains single-shot).
func (s *Stub) SetEndpoints(addrs []string) {
	s.mu.Lock()
	s.endpoints = append([]string(nil), addrs...)
	s.mu.Unlock()
}

// EnableReconnect enables auto reconnect plus WardRoster on stream break
// (WardRoster dispatched-orphan e2e). On (re)connect, the stub sends Hello,
// then immediately sends WardRoster(activeWard), mirroring real soul/cmd/soul
// handleSession. Requires configured [SetEndpoints]. Disabled by default.
func (s *Stub) EnableReconnect(v bool) {
	s.mu.Lock()
	s.reconnectEnabled = v
	s.mu.Unlock()
}

// SetHoldApply enables hold mode for ApplyRequest: the stub does NOT send
// RunResult (apply_runs row remains `dispatched`), and instead registers apply_id
// in activeWard. Emulates a Soul that accepted a task and did not finish the Run.
// Used by dispatched-orphan e2e (row stays dispatched until reconnect+WardRoster).
func (s *Stub) SetHoldApply(v bool) {
	s.mu.Lock()
	s.holdApply = v
	s.mu.Unlock()
}

// ClearActiveWard resets the set of warded apply_id values. It emulates a Soul
// process restart: after restart there are physically no in-flight runs, and
// WardRoster on reconnect declares an empty set, so keeper terminates ALL
// dispatched rows for this SID (OrphanDispatched). Without this call, reconnect
// declares held apply_id values as warded and keeper does NOT orphan them
// (epoch-fenced guard: Soul declares that the run is still in progress).
func (s *Stub) ClearActiveWard() {
	s.mu.Lock()
	s.activeWard = make(map[string]int32)
	s.mu.Unlock()
}

// ActiveWardIDs returns a sorted snapshot of apply_id values declared as warded
// (for test assertions). Pure read projection; it does not change state.
func (s *Stub) ActiveWardIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.activeWard))
	for id := range s.activeWard {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// SetTaskRegister configures a scripted per-task register (staged-render,
// ADR-056): taskName on host s.SID emits TaskEvent with RegisterData=data BEFORE
// aggregated RunResult (like a real Soul on a probe task, echoing passage from
// ApplyRequest). Keeper accumulates register per-(apply_id, sid, passage), and
// rendering of the next Passage resolves `where: register.<name>.*` per host
// using this fact. For 2-passage e2e probe->where: one host returns
// role='master', another returns 'slave'.
func (s *Stub) SetTaskRegister(taskName string, data map[string]any) {
	s.mu.Lock()
	bySID := s.taskRegisterByName[taskName]
	if bySID == nil {
		bySID = make(map[string]map[string]any)
		s.taskRegisterByName[taskName] = bySID
	}
	bySID[s.SID] = data
	s.mu.Unlock()
}

// SetErrandStatus sets the status the stub returns for ErrandRequest
// (SUCCESS by default). Used by tests for failed/timeout ErrandRun branches.
func (s *Stub) SetErrandStatus(st keeperv1.ErrandStatus) {
	s.mu.Lock()
	s.errandStatus = st
	s.mu.Unlock()
}

// SetApplyDefaultSuccess enables "success for any ApplyRequest" mode: a task not
// covered by scripted table returns SUCCESS instead of FAILED. Used by apply-e2e,
// where apply_runs lifecycle matters more than per-task RunResult realism.
func (s *Stub) SetApplyDefaultSuccess(v bool) {
	s.mu.Lock()
	s.applyDefaultSuccess = v
	s.mu.Unlock()
}

// SetApplyStatusForSID configures a per-SID RunResult status override for
// ApplyRequest: success=true -> RUN_STATUS_SUCCESS, success=false ->
// RUN_STATUS_FAILED, regardless of scripted table and applyDefaultSuccess. Applies
// when sid matches s.SID (one Stub = one SID, routed by connection). Used by Tide
// partial-failure tests (one wave with a failed host -> on_failure=abort/continue).
// Without this call behavior stays the same (global default).
func (s *Stub) SetApplyStatusForSID(sid string, success bool) {
	s.mu.Lock()
	s.applyStatusBySID[sid] = success
	s.mu.Unlock()
}

// SetErrandStatusForSID configures a per-SID ErrandResult status override for
// ErrandRequest over the global errandStatus. Applies when sid matches s.SID.
// Used by ErrandRun partial-failure tests (one host FAILED ->
// on_failure=abort/continue). Without this call behavior stays the same (global
// SetErrandStatus/default).
func (s *Stub) SetErrandStatusForSID(sid string, status keeperv1.ErrandStatus) {
	s.mu.Lock()
	s.errandStatusBySID[sid] = status
	s.mu.Unlock()
}

// SetDryRunPlan enables a Plan response to dry_run ApplyRequest (Scry, ADR-031):
// before RunResult, the stub emits TaskEvent for each task with
// register_data{changed}=changed. changed=true -> drift (per-task CHANGED),
// false -> clean (per-task OK). Without this call, dry_run behaves like a normal
// run (RunResult only) and the drift report is built as host=clean without
// per-task register rows.
func (s *Stub) SetDryRunPlan(changed bool) {
	s.mu.Lock()
	s.dryRunPlanSet = true
	s.dryRunChanged = changed
	s.mu.Unlock()
}

// LoadScript fills the scripted map from a prepared parse result of
// `stub-responses.yaml`. RunResult structure validation is the caller's
// responsibility (harness).
func (s *Stub) LoadScript(perScenario map[string][]ScriptEntry) {
	for k, v := range perScenario {
		s.scripted[k] = v
	}
}

// Open connects to Keeper over mTLS, opens EventStream, sends Hello, and starts
// recv-loop in a background goroutine. There is no blocking call; the caller then
// makes assertions and calls Close.
func (s *Stub) Open(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stream != nil {
		return ErrAlreadyOpen
	}

	if err := s.dialAndHandshake(ctx, s.KeeperGRPCAddr, false); err != nil {
		return err
	}

	go s.recvLoop()
	return nil
}

// dialAndHandshake opens a gRPC stream to addr, sends Hello and (on reconnect)
// WardRoster, and sets s.conn/s.stream/s.cancel. Called under s.mu. Shared code
// for initial Open ([Open]) and reconnect (reconnectIfBroken). When
// sendWardRoster=true it sends WardRoster(activeWard) immediately after Hello,
// mirroring real soul/cmd/soul handleSession (the FIRST app message after
// handshake).
func (s *Stub) dialAndHandshake(ctx context.Context, addr string, sendWardRoster bool) error {
	clientCert, err := tls.X509KeyPair(s.cert, s.key)
	if err != nil {
		return fmt.Errorf("soulstub(%s): X509KeyPair: %w", s.SID, err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(s.caBundle) {
		return fmt.Errorf("soulstub(%s): failed to add CA to pool", s.SID)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS13,
	}

	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
	if err != nil {
		return fmt.Errorf("soulstub(%s): grpc.NewClient: %w", s.SID, err)
	}

	client := keeperv1.NewKeeperClient(conn)
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := client.EventStream(streamCtx)
	if err != nil {
		cancel()
		_ = conn.Close()
		return fmt.Errorf("soulstub(%s): EventStream: %w", s.SID, err)
	}

	if err := stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_Hello{
			Hello: &keeperv1.Hello{
				SidEcho:     s.SID,
				SoulVersion: "soulstub-l3a",
				// Protocol feature announcement (ADR-056 S5): same canonical list
				// sent by real Soul (soul/internal/grpc/client.go). Without
				// "passage", keeper rejects the stub under staged scenario
				// (N>1 Passage) fail-closed (soul_passage_unsupported, run.go),
				// although respondToApply echoes passage in TaskEvent/RunResult (S3);
				// capability must match behavior.
				Capabilities: config.SoulCapabilities(),
			},
		},
	}); err != nil {
		cancel()
		_ = conn.Close()
		return fmt.Errorf("soulstub(%s): send Hello: %w", s.SID, err)
	}

	// WardRoster (Soul-reconcile, ADR-027(g)): the FIRST app message after
	// handshake on reconnect is a snapshot of warded apply_id values (ReplaceAll).
	// Mirrors soul/cmd/soul handleSession (sent immediately after Hello, before
	// anything else). Keeper uses it to terminate orphaned dispatched rows for
	// the SID. It is not sent on initial Open (nothing is being warded yet; real
	// Soul also sends an empty set, but we keep previous initial behavior for a
	// clean L3a contract of older tests).
	if sendWardRoster {
		if err := stream.Send(&keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_WardRoster{
				WardRoster: &keeperv1.WardRoster{Active: s.wardRosterActiveLocked()},
			},
		}); err != nil {
			cancel()
			_ = conn.Close()
			return fmt.Errorf("soulstub(%s): send WardRoster: %w", s.SID, err)
		}
	}

	s.conn = conn
	s.stream = stream
	s.cancel = cancel
	return nil
}

// wardRosterActiveLocked builds the proto ActiveApply set from activeWard for
// WardRoster. Called under s.mu. Empty set -> nil (explicit declaration that
// nothing is being warded; reconcile will orphan all dispatched rows for the SID).
func (s *Stub) wardRosterActiveLocked() []*keeperv1.ActiveApply {
	if len(s.activeWard) == 0 {
		return nil
	}
	out := make([]*keeperv1.ActiveApply, 0, len(s.activeWard))
	for id, attempt := range s.activeWard {
		out = append(out, &keeperv1.ActiveApply{ApplyId: id, Attempt: attempt})
	}
	return out
}

// recvLoop reads messages from Keeper and responds to ApplyRequest with scripted
// RunResult. Any other message is recorded and ignored (L3a contract: the stub
// does not respond to CancelApply / SigilTrustAnchors beyond recording).
func (s *Stub) recvLoop() {
	defer close(s.done)
	for {
		s.mu.Lock()
		stream := s.stream
		s.mu.Unlock()
		if stream == nil {
			return
		}

		frame, err := stream.Recv()
		if err != nil {
			// Stream break (clean EOF / transport / keeper-holder death). If
			// reconnect is enabled, select a live keeper from fallback list and
			// send WardRoster (mirror of soul/cmd/soul reconnectLoop->handleSession).
			// Success -> continue recvLoop on the new stream; failure (or reconnect
			// disabled / Close) -> exit.
			if errors.Is(err, context.Canceled) {
				return // Close() canceled session ctx - normal exit.
			}
			if s.reconnectIfBroken(err) {
				continue
			}
			return
		}

		s.mu.Lock()
		s.recorded = append(s.recorded, Message{
			Kind:  payloadKind(frame),
			Frame: frame,
		})
		s.mu.Unlock()

		if req := frame.GetApplyRequest(); req != nil {
			s.respondToApply(req)
		}
		if er := frame.GetErrandRequest(); er != nil {
			s.respondToErrand(er)
		}
	}
}

// reconnectIfBroken selects a live keeper from fallback endpoints and recreates
// the stream (Hello + WardRoster), mirroring real soul/cmd/soul
// reconnectLoop->handleSession. Returns true when a new stream is up (recvLoop
// continues on it); false when reconnect is disabled / there are no endpoints /
// the stub is closed / all endpoints are dead.
//
// Auto-retry with short backoff: after SIGKILL of the keeper holder, live keepers
// are already up, but the TCP break against the killed one may still race; give
// several attempts within about 5s, like the real Soul backoff loop.
func (s *Stub) reconnectIfBroken(cause error) bool {
	s.mu.Lock()
	if s.closed || !s.reconnectEnabled || len(s.endpoints) == 0 {
		s.mu.Unlock()
		return false
	}
	// Close the old conn; it pointed to a dead/broken keeper.
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	s.stream = nil
	endpoints := append([]string(nil), s.endpoints...)
	s.mu.Unlock()

	deadline := time.Now().Add(15 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		for _, addr := range endpoints {
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return false
			}
			err := s.dialAndHandshake(context.Background(), addr, true)
			s.mu.Unlock()
			if err == nil {
				return true
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	_ = cause
	return false
}

// respondToErrand answers ErrandRequest (ADR-033/041) with ErrandResult using
// configured status (default SUCCESS). It does NOT run real shell/exec; the L3a
// contract checks keeper-side dispatch->applybus->Dispatcher-terminal->errands-row
// chain (including FK started_by_aid), not module execution realism. errand_id is
// echo-proxied from the request. exit_code=0 for SUCCESS.
func (s *Stub) respondToErrand(req *keeperv1.ErrandRequest) {
	s.mu.Lock()
	status := s.errandStatus
	if override, ok := s.errandStatusBySID[s.SID]; ok {
		status = override
	}
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return
	}
	var exitCode int32
	if status != keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS {
		exitCode = 1
	}
	_ = stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ErrandResult{
			ErrandResult: &keeperv1.ErrandResult{
				ErrandId:   req.GetErrandId(),
				Status:     status,
				ExitCode:   exitCode,
				Stdout:     "ok\n",
				DurationMs: 1,
			},
		},
	})
}

// respondToApply sends aggregated RunResult from the scripted table. If any task
// in ApplyRequest is not covered by scripted table, result status is FAILED
// (explicit signal to the test that the fixture has a gap).
func (s *Stub) respondToApply(req *keeperv1.ApplyRequest) {
	s.mu.Lock()
	defaultSuccess := s.applyDefaultSuccess
	dryRunPlanSet := s.dryRunPlanSet
	dryRunChanged := s.dryRunChanged
	sidOverride, hasSidOverride := s.applyStatusBySID[s.SID]
	holdApply := s.holdApply
	if holdApply {
		// Hold mode: the task is physically accepted (apply_id is warded), but
		// RunResult is not sent - apply_runs row remains `dispatched`. Register
		// apply_id in activeWard (its echoed attempt is used for epoch-fenced
		// WardRoster). Real Soul also keeps apply_id in ActiveSet until Run ends.
		s.activeWard[req.GetApplyId()] = req.GetAttempt()
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	// Per-SID override (Tide partial-failure tests): this host returns deterministic
	// RunResult{SUCCESS|FAILED} regardless of scripted table and applyDefaultSuccess
	// - one host in the wave fails while the others do not.
	if hasSidOverride {
		status := keeperv1.RunStatus_RUN_STATUS_SUCCESS
		if !sidOverride {
			status = keeperv1.RunStatus_RUN_STATUS_FAILED
		}
		_ = s.stream.Send(&keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_RunResult{
				RunResult: &keeperv1.RunResult{
					ApplyId: req.GetApplyId(),
					Status:  status,
					Attempt: req.GetAttempt(),
					Passage: req.GetPassage(),
				},
			},
		})
		return
	}

	// Scripted per-task register (staged-render, ADR-056): for a probe task (with
	// configured SetTaskRegister), emit TaskEvent with RegisterData BEFORE RunResult,
	// echoing passage from the request, like real Soul on a register task. Keeper
	// accumulates register per-(apply_id, sid, passage); rendering of the next
	// Passage resolves `where: register.<name>.*` using this fact. Without
	// scripted-register it is a no-op (normal apply).
	s.emitTaskRegisters(req)

	// dry_run + enabled Plan mode (Scry, ADR-031): emit per-task TaskEvent with
	// register_data{changed}, as Soul would do after mod.Plan. This fills
	// apply_task_register, where CheckDrift collects per-task drifted/clean.
	// RunResult below closes the host terminal (driftBarrier waits for it).
	if req.GetDryRun() && dryRunPlanSet {
		s.emitPlanTaskEvents(req, dryRunChanged)
	}

	worst := keeperv1.RunStatus_RUN_STATUS_SUCCESS
	merged := map[string]any{}

	for _, task := range req.GetTasks() {
		entries := s.findEntriesByTask(task.GetName())
		if len(entries) == 0 {
			if defaultSuccess {
				// Apply-default-success mode: unscripted task = SUCCESS (apply_runs
				// lifecycle matters more than per-task RunResult realism, L3a contract).
				continue
			}
			worst = keeperv1.RunStatus_RUN_STATUS_FAILED
			merged["_unscripted_task"] = task.GetName()
			continue
		}
		e := entries[0]
		if e.Status == keeperv1.RunStatus_RUN_STATUS_FAILED {
			worst = keeperv1.RunStatus_RUN_STATUS_FAILED
		}
		for k, v := range e.StateChanges {
			merged[k] = v
		}
	}

	stateStruct, err := structpb.NewStruct(merged)
	if err != nil {
		stateStruct = nil
	}

	_ = s.stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_RunResult{
			RunResult: &keeperv1.RunResult{
				ApplyId:      req.GetApplyId(),
				Status:       worst,
				StateChanges: stateStruct,
				Attempt:      req.GetAttempt(),
				// passage (ADR-056): echo from ApplyRequest; Keeper correlates terminal
				// per-(apply_id, sid, passage) and barriers the Passage slice.
				Passage: req.GetPassage(),
			},
		},
	})
}

// emitTaskRegisters emits TaskEvent with RegisterData for ApplyRequest tasks that
// have scripted per-task register for this SID (SetTaskRegister). task_idx is the
// position of the task in req.Tasks[] (real Soul sets it the same way); passage is
// echo of ApplyRequest.passage. Keeper-side accumulateRegister stores
// register_data in apply_task_register by (apply_id, sid, task_idx) with this
// passage. no-op if scripted-register is not configured.
func (s *Stub) emitTaskRegisters(req *keeperv1.ApplyRequest) {
	s.mu.Lock()
	stream := s.stream
	byName := s.taskRegisterByName
	sid := s.SID
	s.mu.Unlock()
	if stream == nil || len(byName) == 0 {
		return
	}
	for idx, task := range req.GetTasks() {
		bySID, ok := byName[task.GetName()]
		if !ok {
			continue
		}
		data, ok := bySID[sid]
		if !ok {
			continue
		}
		reg, err := structpb.NewStruct(data)
		if err != nil {
			continue
		}
		_ = stream.Send(&keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_TaskEvent{
				TaskEvent: &keeperv1.TaskEvent{
					ApplyId:      req.GetApplyId(),
					TaskIdx:      int32(idx),
					Status:       keeperv1.TaskStatus_TASK_STATUS_OK,
					RegisterData: reg,
					Passage:      req.GetPassage(),
				},
			},
		})
	}
}

// emitPlanTaskEvents sends one TaskEvent for each task of dry_run ApplyRequest
// with status=CHANGED|OK and register_data{changed}. task_idx is the task
// position in req.Tasks[] (real Soul sets it the same way:
// applyrunner.go TaskIdx=int32(idx)), which matches RenderedTask.Index for a
// linear scenario such as converge. Keeper-side accumulateRegister stores
// register_data in apply_task_register by task_idx.
func (s *Stub) emitPlanTaskEvents(req *keeperv1.ApplyRequest, changed bool) {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return
	}
	status := keeperv1.TaskStatus_TASK_STATUS_OK
	if changed {
		status = keeperv1.TaskStatus_TASK_STATUS_CHANGED
	}
	for idx := range req.GetTasks() {
		reg, err := structpb.NewStruct(map[string]any{"changed": changed})
		if err != nil {
			continue
		}
		_ = stream.Send(&keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_TaskEvent{
				TaskEvent: &keeperv1.TaskEvent{
					ApplyId:      req.GetApplyId(),
					TaskIdx:      int32(idx),
					Status:       status,
					RegisterData: reg,
					Passage:      req.GetPassage(),
				},
			},
		})
	}
}

// SendPortent sends PortentEvent from the Soul stub into EventStream on behalf of
// the live Soul producer (V5-1 ADR-030 amendment 2026-05-26). Used by L3a tests
// for Oracle loop: stub emits typed/legacy Portent through real gRPC-mTLS, the
// Keeper handler accepts it and runs the full pipeline
// (match/where/cooldown/enqueue -> apply_runs). Returns send error (stream closed
// / transport).
//
// The stub does NOT emulate scheduler: caller builds PortentEvent with required
// beacon_name / payload (typed oneof or legacy Data). collected_at / sid are filled
// automatically from stub.SID and time.Now when caller does not set them.
func (s *Stub) SendPortent(ev *keeperv1.PortentEvent) error {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return errors.New("soulstub: stream is not open")
	}
	if ev.GetSid() == "" {
		ev.Sid = s.SID
	}
	return stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_PortentEvent{PortentEvent: ev},
	})
}

// findEntriesByTask looks up scripted entry by task_name across all scenarios.
// Stub does not know current scenario name (Keeper sends only task_name in
// RenderedTask), so it does a flat search across merged maps. Task name collision
// between scenarios is excluded in the test environment (one stub-responses.yaml
// per smoke case).
func (s *Stub) findEntriesByTask(taskName string) []ScriptEntry {
	var out []ScriptEntry
	for _, entries := range s.scripted {
		for _, e := range entries {
			if e.TaskName == taskName {
				out = append(out, e)
			}
		}
	}
	return out
}

// payloadKind returns the oneof variant name for Message.Kind.
func payloadKind(frame *keeperv1.FromKeeper) string {
	switch frame.GetPayload().(type) {
	case *keeperv1.FromKeeper_HelloReply:
		return "HelloReply"
	case *keeperv1.FromKeeper_ApplyRequest:
		return "ApplyRequest"
	case *keeperv1.FromKeeper_CancelApply:
		return "CancelApply"
	case *keeperv1.FromKeeper_SeedRotationReply:
		return "SeedRotationReply"
	case *keeperv1.FromKeeper_PluginSigil:
		return "PluginSigil"
	case *keeperv1.FromKeeper_AugurReply:
		return "AugurReply"
	case *keeperv1.FromKeeper_SigilSnapshot:
		return "SigilSnapshot"
	case *keeperv1.FromKeeper_SigilTrustAnchors:
		return "SigilTrustAnchors"
	case *keeperv1.FromKeeper_VigilSnapshot:
		return "VigilSnapshot"
	case *keeperv1.FromKeeper_ErrandRequest:
		return "ErrandRequest"
	default:
		return "unknown"
	}
}

// Close gracefully shuts down the stream. Safe to call more than once.
func (s *Stub) Close() error {
	s.mu.Lock()
	s.closed = true // reconnectIfBroken will not open a new stream after this.
	cancel := s.cancel
	conn := s.conn
	stream := s.stream
	s.stream = nil
	s.conn = nil
	s.cancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stream != nil {
		_ = stream.CloseSend()
	}
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// Messages returns a copy of messages received from Keeper during stream lifetime.
func (s *Stub) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.recorded))
	copy(out, s.recorded)
	return out
}
