package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/console"
	"github.com/souls-guild/soul-stack/keeper/internal/console/consoletest"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// Guard tests for keeper.soul.run-command (NIM-147, ADR-0074 amendment).
//
// The gate half runs through the real tool: run-command executes whatever the
// caller typed, so the checks that matter are the ones that say NO — no right,
// the weaker right, the right on another host. The RBAC check deliberately sits
// BEFORE the errand-stack nil-guard, which is what lets these run without a
// live dispatcher: a caller who passes the gate gets internal-error
// ("not configured"), a caller who doesn't gets forbidden. The two are never
// confusable, and that difference IS the assertion.
//
// The dispatch half (masking, audit, the pinned module) runs through
// [runCommand] with a fake, since HandlerDeps.ErrandDispatcher is a concrete
// *errand.Dispatcher over a pgxpool.

const (
	runCommandSID   = "web-01.example.com"
	runCommandOther = "db-01.example.com"
)

func runCommandArgsJSON(sid string) string {
	return `{"sid":"` + sid + `","command":"uptime"}`
}

// consoleCfg — archon-alice holds soul.console scoped to one host.
func consoleCfg(perm string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "console", Operators: []string{"archon-alice"}, Permissions: []string{perm}},
		},
	}
}

// TestRunCommand_DeniedWithoutConsoleRight — an operator with no soul.console
// is refused, and the refusal names the right it is missing.
func TestRunCommand_DeniedWithoutConsoleRight(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, consoleCfg("soul.list"))

	resp := callTool(t, h, "archon-alice", "keeper.soul.run-command", runCommandArgsJSON(runCommandSID))
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeForbidden {
		t.Fatalf("code = %q, want forbidden", data.Code)
	}
	if !contains(resp.Error.Message, "soul.console") {
		t.Errorf("message = %q, want it to name soul.console", resp.Error.Message)
	}
}

// TestRunCommand_ErrandRunIsNotEnough — the central guard of ADR-0074: an
// arbitrary command line is a console, so errand.run does NOT open this tool.
// If this ever passes, the most privileged action in the system has silently
// inherited the gate written for the least.
func TestRunCommand_ErrandRunIsNotEnough(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, consoleCfg("errand.run"))

	resp := callTool(t, h, "archon-alice", "keeper.soul.run-command", runCommandArgsJSON(runCommandSID))
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Fatalf("code = %q, want forbidden (errand.run must not imply soul.console)", data.Code)
	}
}

// TestRunCommand_HostScope — soul.console on host=web-01 runs on web-01 and
// nowhere else. The allowed case stops at the nil-guard, which is proof the
// gate let it through.
func TestRunCommand_HostScope(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, consoleCfg("soul.console on host="+runCommandSID))

	cases := []struct {
		name string
		sid  string
		want string
	}{
		{"own host passes the gate", runCommandSID, mcpCodeInternalError},
		{"another host is refused", runCommandOther, mcpCodeForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", "keeper.soul.run-command", runCommandArgsJSON(tc.sid))
			if resp.Error == nil {
				t.Fatal("expected error response")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != tc.want {
				t.Errorf("code = %q, want %q", data.Code, tc.want)
			}
		})
	}
}

// TestRunCommand_SoulWildcardCovers — ADR-0074(c), recorded on purpose: `soul.*`
// covers soul.console, so a role holding the resource wildcard reaches this tool.
// A role that must not hand out shells enumerates actions instead.
func TestRunCommand_SoulWildcardCovers(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, consoleCfg("soul.*"))

	resp := callTool(t, h, "archon-alice", "keeper.soul.run-command", runCommandArgsJSON(runCommandSID))
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
		t.Errorf("code = %q, want internal-error (gate passed, errand stack absent)", data.Code)
	}
}

// TestRunCommand_Validation — argument shape. A caller must not be able to smuggle
// a module past the pinned one, so an unknown field is a malformed request.
func TestRunCommand_Validation(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, consoleCfg("*"))

	cases := []struct {
		name string
		args string
		want string
	}{
		{"missing sid", `{"command":"uptime"}`, mcpCodeValidationFailed},
		{"malformed sid", `{"sid":"WEB 01","command":"uptime"}`, mcpCodeValidationFailed},
		{"missing command", `{"sid":"` + runCommandSID + `"}`, mcpCodeValidationFailed},
		{"blank command", `{"sid":"` + runCommandSID + `","command":"   "}`, mcpCodeValidationFailed},
		{"module is not an argument", `{"sid":"` + runCommandSID + `","command":"id","module":"core.exec.run"}`, mcpCodeMalformedRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", "keeper.soul.run-command", tc.args)
			if resp.Error == nil {
				t.Fatal("expected error response")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != tc.want {
				t.Errorf("code = %q, want %q", data.Code, tc.want)
			}
		})
	}
}

// --- dispatch half ---

// fakeDispatch records the request and replays a canned result.
type fakeDispatch struct {
	got  errand.DispatchRequest
	res  errand.DispatchResult
	err  error
	call int
}

func (f *fakeDispatch) Dispatch(_ context.Context, req errand.DispatchRequest) (errand.DispatchResult, error) {
	f.call++
	f.got = req
	return f.res, f.err
}

// auditRecorder captures what runCommand wrote.
type auditRecorder struct {
	events []auditRecord
}

type auditRecord struct {
	eventType     audit.EventType
	aid           string
	correlationID string
	payload       map[string]any
}

func (r *auditRecorder) write(eventType audit.EventType, aid, correlationID string, payload map[string]any) {
	r.events = append(r.events, auditRecord{eventType, aid, correlationID, payload})
}

func newRunCommandFakes(res errand.DispatchResult, err error) (*fakeDispatch, *auditRecorder, runCommandDeps) {
	d, a, deps, _ := newRunCommandFakesWithStore(res, err)
	return d, a, deps
}

// newRunCommandFakesWithStore also hands back the recording store, for the
// guard tests that assert on what was recorded (ADR-0074(g), NIM-145). The
// recorder itself is the real one — only the storage is faked.
func newRunCommandFakesWithStore(res errand.DispatchResult, err error) (*fakeDispatch, *auditRecorder, runCommandDeps, *consoletest.Store) {
	d := &fakeDispatch{res: res, err: err}
	a := &auditRecorder{}
	store := consoletest.NewStore()
	recorder, rerr := console.NewRecorder(store, console.RecorderConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if rerr != nil {
		panic("newRunCommandFakes: " + rerr.Error())
	}
	return d, a, runCommandDeps{Dispatch: d.Dispatch, Audit: a.write, Recorder: recorder}, store
}

// TestRunCommand_PinsShellModule — the module is ours, not the caller's, and the
// command travels as the `cmd` param of core.cmd.shell (docs/module/core/cmd).
func TestRunCommand_PinsShellModule(t *testing.T) {
	d, _, deps := newRunCommandFakes(errand.DispatchResult{ErrandID: "01HF7Z", Status: errand.StatusSuccess}, nil)

	_, err := runCommand(context.Background(), deps, "archon-alice", runCommandArgs{
		SID:            runCommandSID,
		Command:        "systemctl status redis",
		Cwd:            "/tmp",
		Env:            map[string]string{"LANG": "C"},
		TimeoutSeconds: 45,
	})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if d.got.Module != runCommandModule {
		t.Errorf("module = %q, want %q", d.got.Module, runCommandModule)
	}
	if d.got.SID != runCommandSID {
		t.Errorf("sid = %q, want %q", d.got.SID, runCommandSID)
	}
	if d.got.StartedByAID != "archon-alice" {
		t.Errorf("started_by_aid = %q, want archon-alice", d.got.StartedByAID)
	}
	if d.got.TimeoutSec != 45 {
		t.Errorf("timeout = %d, want 45", d.got.TimeoutSec)
	}
	if got := d.got.Input["cmd"]; got != "systemctl status redis" {
		t.Errorf("input[cmd] = %v, want the command line", got)
	}
	if got := d.got.Input["cwd"]; got != "/tmp" {
		t.Errorf("input[cwd] = %v, want /tmp", got)
	}
	env, ok := d.got.Input["env"].(map[string]any)
	if !ok || env["LANG"] != "C" {
		t.Errorf("input[env] = %v, want {LANG: C}", d.got.Input["env"])
	}
	// dry_run belongs to read-safe modules; a shell line has no Plan.
	if d.got.DryRun {
		t.Error("dry_run must never be set for a shell command")
	}
}

// TestRunCommand_MasksSecretsInOutput — a vault reference that surfaces in the
// command's output does not reach the agent, exactly as it does not reach a log
// line (ADR-010 §7.4, the same `MaskSecrets`-over-a-wrapped-string the taskevent
// log path uses). Asserted on all three channels because each is a separate
// projection and each has leaked in some system.
//
// The bound is honest and worth stating: on a free-form byte stream only the
// content layer (vault provenance) can fire — the key-name layer has no key to
// look at, and a credential the command prints in plaintext is indistinguishable
// from any other text. Recognizing it would mean knowing what the command was
// going to do, which is the very thing ADR-0074 says a console cannot promise.
// A custom KV mount is covered too (security audit K5), so the fixture uses one.
func TestRunCommand_MasksSecretsInOutput(t *testing.T) {
	_, _, deps := newRunCommandFakes(errand.DispatchResult{
		ErrandID:     "01HF7Z",
		Status:       errand.StatusFailed,
		Stdout:       "db_url=vault:kv/db/creds#password",
		Stderr:       "cannot read vault:secret/db/creds#password",
		ErrorMessage: "render failed on vault:secret/app/token",
	}, nil)

	out, err := runCommand(context.Background(), deps, "archon-alice", runCommandArgs{
		SID: runCommandSID, Command: "cat /etc/app.conf",
	})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	for _, f := range []struct {
		name, got string
	}{
		{"stdout", out.Stdout},
		{"stderr", out.Stderr},
		{"error_message", out.ErrorMessage},
	} {
		if contains(f.got, "vault:") {
			t.Errorf("%s = %q, leaks a vault reference", f.name, f.got)
		}
		if !contains(f.got, "***MASKED***") {
			t.Errorf("%s = %q, want it masked", f.name, f.got)
		}
	}
}

// TestRunCommand_Audited — every execution lands in the audit log as a fact,
// with the same detail a console session gets: who, where, correlated to the run.
// The command line is not in the payload, the same way console.opened holds no
// keystrokes.
func TestRunCommand_Audited(t *testing.T) {
	_, rec, deps := newRunCommandFakes(errand.DispatchResult{
		ErrandID: "01HF7Z5G8Q5KQ8X7Y2N3R4M5P6",
		Status:   errand.StatusSuccess,
		Stdout:   "up 3 days",
	}, nil)

	if _, err := runCommand(context.Background(), deps, "archon-alice", runCommandArgs{
		SID: runCommandSID, Command: "mysql -p'hunter2' -e 'select 1'",
	}); err != nil {
		t.Fatalf("runCommand: %v", err)
	}

	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.eventType != audit.EventConsoleCommand {
		t.Errorf("event_type = %q, want %q", ev.eventType, audit.EventConsoleCommand)
	}
	if ev.aid != "archon-alice" {
		t.Errorf("archon_aid = %q, want archon-alice", ev.aid)
	}
	if ev.correlationID != "01HF7Z5G8Q5KQ8X7Y2N3R4M5P6" {
		t.Errorf("correlation_id = %q, want the errand id", ev.correlationID)
	}
	if ev.payload["sid"] != runCommandSID {
		t.Errorf("payload[sid] = %v, want %q", ev.payload["sid"], runCommandSID)
	}
	if ev.payload["status"] != string(errand.StatusSuccess) {
		t.Errorf("payload[status] = %v, want success", ev.payload["status"])
	}
	if _, ok := ev.payload["command"]; ok {
		t.Error("payload carries the command line; a console audits the fact, not the keystrokes")
	}
}

// TestRunCommand_DispatchErrorIsNotAudited — nothing ran, so nothing is recorded
// as having run. The transport's own trail covers a half-dispatched errand.
func TestRunCommand_DispatchErrorIsNotAudited(t *testing.T) {
	_, rec, deps := newRunCommandFakes(errand.DispatchResult{}, errand.ErrSoulNotConnected)

	_, err := runCommand(context.Background(), deps, "archon-alice", runCommandArgs{
		SID: runCommandSID, Command: "uptime",
	})
	if !errors.Is(err, errand.ErrSoulNotConnected) {
		t.Fatalf("err = %v, want ErrSoulNotConnected", err)
	}
	if len(rec.events) != 0 {
		t.Errorf("audit events = %d, want 0", len(rec.events))
	}
}

// --- mandatory recording (ADR-0074(g), NIM-145) -------------------------------

// The one-shot half of the console plane leaves the same artifact the
// interactive half does. Same right reaching the same shell, same record — the
// tty is the only thing that differs, and it is not what recording is for.
func TestRunCommand_IsRecorded(t *testing.T) {
	_, _, deps, store := newRunCommandFakesWithStore(errand.DispatchResult{
		ErrandID: "01HF7Z", Status: errand.StatusSuccess,
		Stdout: "uid=0(root)\n", Stderr: "a warning\n",
	}, nil)

	out, err := runCommand(context.Background(), deps, "archon-alice", runCommandArgs{
		SID: runCommandSID, Command: "id",
	})
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}

	rec := store.Only()
	if rec == nil {
		t.Fatalf("recordings = %d, want exactly 1", store.Count())
	}
	if rec.Meta.Kind != console.RecordingCommand {
		t.Errorf("recording kind = %q, want %q", rec.Meta.Kind, console.RecordingCommand)
	}
	if rec.Meta.SID != runCommandSID || rec.Meta.AID != "archon-alice" {
		t.Errorf("recording meta = %+v, want the target host and the caller", rec.Meta)
	}
	if !rec.Closed {
		t.Error("the recording was never finished")
	}

	body := rec.Body()
	// The command line is recorded as operator input — it is what a person
	// would have typed at the prompt this tool replaces.
	if !strings.Contains(body, `"i"`) || !strings.Contains(body, "id\\n") {
		t.Errorf("the command line is not in the recording: %q", body)
	}
	for _, want := range []string{"uid=0(root)", "a warning"} {
		if !strings.Contains(body, want) {
			t.Errorf("output %q is not in the recording: %q", want, body)
		}
	}
	_ = out
}

// The audit event links the fact to the artifact, which is how a reader gets
// from "an arbitrary command ran here" to what it printed.
func TestRunCommand_AuditCarriesTheRecordingID(t *testing.T) {
	_, rec, deps, store := newRunCommandFakesWithStore(errand.DispatchResult{
		ErrandID: "01HF7Z", Status: errand.StatusSuccess,
	}, nil)

	if _, err := runCommand(context.Background(), deps, "archon-alice", runCommandArgs{
		SID: runCommandSID, Command: "uptime",
	}); err != nil {
		t.Fatalf("runCommand: %v", err)
	}

	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(rec.events))
	}
	got, _ := rec.events[0].payload["recording_id"].(string)
	if got == "" || got != store.Only().Meta.RecordingID {
		t.Fatalf("payload[recording_id] = %q, want the recording that was written", got)
	}
}

// Fail-closed, and for this path it can only act BEFORE the dispatch: a command
// that has already run cannot be un-run, so a store that is down means the
// command does not execute at all.
func TestRunCommand_RefusesToRunWhenItCannotBeRecorded(t *testing.T) {
	d, rec, deps, store := newRunCommandFakesWithStore(errand.DispatchResult{
		ErrandID: "01HF7Z", Status: errand.StatusSuccess,
	}, nil)
	store.FailBegin(true)

	_, err := runCommand(context.Background(), deps, "archon-alice", runCommandArgs{
		SID: runCommandSID, Command: "rm -rf /var/lib/soul-stack",
	})
	if !errors.Is(err, console.ErrRecordingUnavailable) {
		t.Fatalf("err = %v, want ErrRecordingUnavailable", err)
	}
	if d.call != 0 {
		t.Fatalf("the command was dispatched anyway: %+v", d.got)
	}
	if len(rec.events) != 0 {
		t.Errorf("audit events = %d, want 0 — nothing ran", len(rec.events))
	}
}

// A vault reference printed by the command is masked in the recording, and only
// the reference is: a blanked recording would be no record at all.
func TestRunCommand_MasksAVaultRefInTheRecording(t *testing.T) {
	_, _, deps, store := newRunCommandFakesWithStore(errand.DispatchResult{
		ErrandID: "01HF7Z", Status: errand.StatusSuccess,
		Stdout: "db url is vault:secret/db/dsn here\n",
	}, nil)

	if _, err := runCommand(context.Background(), deps, "archon-alice", runCommandArgs{
		SID: runCommandSID, Command: "cat /etc/app.conf",
	}); err != nil {
		t.Fatalf("runCommand: %v", err)
	}

	body := store.Only().Body()
	if strings.Contains(body, "vault:secret/db") {
		t.Fatalf("the vault reference is in the recording in plaintext: %q", body)
	}
	if !strings.Contains(body, audit.MaskedValue) {
		t.Fatalf("nothing was masked: %q", body)
	}
	if !strings.Contains(body, "db url is ") {
		t.Fatalf("masking swallowed the surrounding output: %q", body)
	}
}
