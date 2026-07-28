package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/console"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// `keeper.soul.run-command` — the non-interactive half of the console plane
// (ADR-0074 amendment, NIM-147): an agent runs one command line on a host and
// gets a machine-readable result instead of an ANSI byte stream.
//
// It is a CONSOLE, not an Errand, and the right says so. An Errand is one named
// module with declared params, so it can be checked against a module allow-list
// before it runs; a command line cannot be checked against anything, and it
// executes as the Soul daemon's user (typically root). Dropping the tty does not
// make that weaker — it only removes the echo — so the gate is `soul.console`
// with the existing `host=<sid>` selector, exactly the check the WebSocket runs
// per `open` frame (`api/console_ws.go`). No lighter right is minted for it.
//
// The TRANSPORT is the Errand stack, because request/response is what makes an
// answer machine-readable: split stdout/stderr, an integer exit code, the 64 KiB
// cap and its truncation flags, and the async escalation for a slow command —
// none of which a pty can express (it merges the channels onto one fd and
// reports only the shell's own exit). The module is pinned to `core.cmd.shell`
// here and is NOT an argument: this tool executes command lines, and a caller
// who could name the module would be holding `keeper.soul.errand.run` under the
// wrong right.

// runCommandModule is the verb module every run-command call is carried on.
// Pinned, never taken from arguments (docs/module/core/cmd).
const runCommandModule = "core.cmd.shell"

// runCommandArgs — arguments for keeper.soul.run-command (schemaRunCommandInput).
// The shape is `core.cmd.shell` minus its scenario-only idempotency guards
// (`creates`/`unless`/`onlyif`): a one-shot agent call has no desired state to
// converge on.
type runCommandArgs struct {
	SID            string            `json:"sid"`
	Command        string            `json:"command"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

// runCommandOutput — structured output of keeper.soul.run-command.
//
// The command line is deliberately NOT echoed back: the caller already has it,
// and a request field mirrored into a response is one more channel for a
// credential typed into an argument to travel through.
type runCommandOutput struct {
	ErrandID        string `json:"errand_id"`
	SID             string `json:"sid"`
	Status          string `json:"status"`
	Async           bool   `json:"async"`
	ExitCode        *int32 `json:"exit_code,omitempty"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	DurationMs      *int64 `json:"duration_ms,omitempty"`
	ErrorMessage    string `json:"error_message,omitempty"`
}

// runCommandDeps is the surface [runCommand] needs, narrowed to funcs so the
// guard tests can drive masking, recording and the audit trail without a live
// errand stack (HandlerDeps.ErrandDispatcher is a concrete *errand.Dispatcher
// over a pgxpool, and its nil-check stays on the concrete type — no typed-nil
// here).
type runCommandDeps struct {
	Dispatch func(context.Context, errand.DispatchRequest) (errand.DispatchResult, error)
	Audit    func(eventType audit.EventType, aid, correlationID string, payload map[string]any)
	Recorder console.Recorder
}

// callSoulRunCommand — mutating tool keeper.soul.run-command.
//
// RBAC — soul.console with selector `host=<sid>` (rbac.md §Console), the same
// scope-aware check the WebSocket runs per `open` frame. It is checked BEFORE
// the wiring nil-guard on purpose: whether this Keeper has an errand stack is
// not something a caller without the right gets to learn.
func (h *Handler) callSoulRunCommand(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.soul.run-command"

	var a runCommandArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest,
				"invalid arguments: "+err.Error())
		}
	}
	if a.SID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'sid' is required")
	}
	if !soul.ValidSID(a.SID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
			"field 'sid' must match "+soul.SIDPattern)
	}
	if strings.TrimSpace(a.Command) == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'command' is required")
	}

	// RBAC: soul.console, selector host=<sid> (ADR-0074 amendment, rbac.md
	// §Console). NOT errand.run — that right gates named modules, and this tool
	// runs whatever the caller typed.
	if err := h.deps.RBAC.Check(claims.Subject, "soul", "console", map[string]string{"host": a.SID}); err != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission soul.console")
	}

	if h.deps.ErrandDispatcher == nil {
		return h.toolError(req.ID, toolName, mcpCodeInternalError, errandNotConfigured)
	}
	if h.deps.ConsoleRecorder == nil {
		// Not a wiring detail to shrug at: `soul.console` authorizes an
		// arbitrary command as root, and ADR-0074(g) says what makes that
		// grantable is the record it leaves. With nowhere to record, the answer
		// is no.
		return h.toolError(req.ID, toolName, mcpCodeInternalError, recordingNotConfigured)
	}

	out, err := runCommand(ctx, runCommandDeps{
		Dispatch: h.deps.ErrandDispatcher.Dispatch,
		Audit:    h.writeAuditCorrelated,
		Recorder: h.deps.ConsoleRecorder,
	}, claims.Subject, a)
	if err != nil {
		if errors.Is(err, console.ErrRecordingUnavailable) {
			h.deps.Logger.Error("mcp: run-command refused — session recording unavailable",
				slog.Any("error", err))
			return h.toolError(req.ID, toolName, mcpCodeInternalError, recordingNotConfigured)
		}
		return h.mapErrandDispatchError(req.ID, toolName, err)
	}
	return h.toolResult(req.ID, out)
}

// recordingNotConfigured is the public detail for a refusal on the recording
// path. Deliberately says nothing about WHY the store is unreachable.
const recordingNotConfigured = "console session recording is unavailable — the command was not run"

// runCommand records the command, dispatches it and projects the result.
//
// Recording is opened BEFORE the dispatch, and its failure means the command
// does not run (ADR-0074(g)). The order is the entire guarantee here: unlike an
// interactive session, which can be closed the moment its recording breaks, a
// command that has already executed cannot be un-executed — so the only moment
// fail-closed can act is before it starts.
func runCommand(ctx context.Context, deps runCommandDeps, aid string, a runCommandArgs) (runCommandOutput, error) {
	input := map[string]any{"cmd": a.Command}
	if a.Cwd != "" {
		input["cwd"] = a.Cwd
	}
	if len(a.Env) > 0 {
		env := make(map[string]any, len(a.Env))
		for k, v := range a.Env {
			env[k] = v
		}
		input["env"] = env
	}

	rec, err := deps.Recorder.Open(ctx, console.RecordingSpec{
		// A one-shot command has no session, so the recording is its own
		// subject. The errand that carried it is reachable through the
		// `console.command` audit event, which carries both ids.
		SessionID: audit.NewULID(),
		Kind:      console.RecordingCommand,
		SID:       a.SID,
		AID:       aid,
	})
	if err != nil {
		return runCommandOutput{}, err
	}

	// The command line is recorded as operator input — it is what a person
	// would have typed at the prompt this tool exists to replace.
	if err := rec.Input([]byte(a.Command + "\n")); err != nil {
		rec.Close(ctx, "recording unavailable")
		return runCommandOutput{}, err
	}

	res, err := deps.Dispatch(ctx, errand.DispatchRequest{
		SID:          a.SID,
		Module:       runCommandModule,
		Input:        input,
		TimeoutSec:   a.TimeoutSeconds,
		StartedByAID: aid,
	})
	if err != nil {
		rec.Close(ctx, "dispatch failed")
		return runCommandOutput{}, err
	}

	// Output is recorded before it is returned, the same order the interactive
	// path uses. A caller must not learn what a root shell printed unless the
	// record of it survives — the command has already run either way, but
	// disclosing its output unrecorded is the leak this ADR is about.
	if err := recordCommandOutput(rec, res); err != nil {
		rec.Close(ctx, "recording unavailable")
		return runCommandOutput{}, err
	}
	rec.Close(ctx, string(res.Status))

	if deps.Audit != nil {
		deps.Audit(audit.EventConsoleCommand, aid, res.ErrandID, map[string]any{
			"sid":          a.SID,
			"status":       string(res.Status),
			"recording_id": rec.ID(),
		})
	}

	// Mask + cap once more on the way out. The Soul-side runner and the Keeper
	// receive path already do it (errand/mask.go), so this is the third pass and
	// idempotent — but it is the pass that belongs to THIS boundary: an agent
	// reply is an observable channel exactly like a log line (ADR-010 §7.4), and
	// masking must not depend on a transport this tool merely borrows.
	stdout, stdoutCut := errand.MaskAndCapBytes(res.Stdout)
	stderr, stderrCut := errand.MaskAndCapBytes(res.Stderr)
	errMsg, _ := errand.MaskAndCapBytes(res.ErrorMessage)

	return runCommandOutput{
		ErrandID:        res.ErrandID,
		SID:             a.SID,
		Status:          string(res.Status),
		Async:           res.Async,
		ExitCode:        res.ExitCode,
		Stdout:          stdout,
		Stderr:          stderr,
		StdoutTruncated: res.StdoutTruncated || stdoutCut,
		StderrTruncated: res.StderrTruncated || stderrCut,
		DurationMs:      res.DurationMs,
		ErrorMessage:    errMsg,
	}, nil
}

// recordCommandOutput writes the command's channels into the recording.
//
// Both land as pty output: a tty merges stdout and stderr before anyone sees
// them, and a recording of a command should replay as the terminal session it
// stands in for. The structured split the caller gets back is the Errand
// transport's contribution, and it stays in the `errands` row.
func recordCommandOutput(rec console.Recording, res errand.DispatchResult) error {
	for _, out := range []string{res.Stdout, res.Stderr} {
		if out == "" {
			continue
		}
		if err := rec.Output(keeperv1.ConsoleStream_CONSOLE_STREAM_STDOUT, []byte(out), 0); err != nil {
			return err
		}
	}
	return nil
}
