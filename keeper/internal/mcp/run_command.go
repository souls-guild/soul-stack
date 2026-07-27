package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
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

// runCommandDeps is the surface [runCommand] needs, narrowed to two funcs so
// the guard tests can drive masking and the audit trail without a live errand
// stack (HandlerDeps.ErrandDispatcher is a concrete *errand.Dispatcher over a
// pgxpool, and its nil-check stays on the concrete type — no typed-nil here).
type runCommandDeps struct {
	Dispatch func(context.Context, errand.DispatchRequest) (errand.DispatchResult, error)
	Audit    func(eventType audit.EventType, aid, correlationID string, payload map[string]any)
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

	out, err := runCommand(ctx, runCommandDeps{
		Dispatch: h.deps.ErrandDispatcher.Dispatch,
		Audit:    h.writeAuditCorrelated,
	}, claims.Subject, a)
	if err != nil {
		return h.mapErrandDispatchError(req.ID, toolName, err)
	}
	return h.toolResult(req.ID, out)
}

// runCommand dispatches the command, records it and projects the result.
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

	res, err := deps.Dispatch(ctx, errand.DispatchRequest{
		SID:          a.SID,
		Module:       runCommandModule,
		Input:        input,
		TimeoutSec:   a.TimeoutSeconds,
		StartedByAID: aid,
	})
	if err != nil {
		return runCommandOutput{}, err
	}

	if deps.Audit != nil {
		deps.Audit(audit.EventConsoleCommand, aid, res.ErrandID, map[string]any{
			"sid":    a.SID,
			"status": string(res.Status),
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
