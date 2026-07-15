package errandrunner

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// OutputCapBytes is the size ceiling for stdout/stderr on a single Errand
// boundary (ADR-033 §6 "Invariants", 64 KiB / channel). Mirrors the
// keeper/internal/errand.OutputCapBytes constant — defense-in-depth (Keeper
// applies the same cap+mask when receiving the result).
const OutputCapBytes = 64 * 1024

// outputCollector is a capture-only ApplyEvent server. Implements
// grpc.ServerStreamingServer[pluginv1.ApplyEvent]: the module sends events
// here, the collector appends them to a slice. The final is extracted via
// [extractFinal] — for shell/exec, stdout/stderr/exit_code live in
// ApplyEvent.Output fields; for read-safe modules the whole Output is
// structured.
//
// Not concurrent-safe: the module runs in one goroutine, ApplyEvents are
// sequential.
type outputCollector struct {
	grpc.ServerStream
	ctx    context.Context
	cap    int
	events []*pluginv1.ApplyEvent
}

func newOutputCollector(ctx context.Context, capBytes int) *outputCollector {
	return &outputCollector{ctx: ctx, cap: capBytes}
}

func (c *outputCollector) Context() context.Context     { return c.ctx }
func (c *outputCollector) SetHeader(metadata.MD) error  { return nil }
func (c *outputCollector) SendHeader(metadata.MD) error { return nil }
func (c *outputCollector) SetTrailer(metadata.MD)       {}
func (c *outputCollector) SendMsg(any) error            { return nil }
func (c *outputCollector) RecvMsg(any) error {
	return errors.New("errandrunner: RecvMsg not supported")
}

// Send accepts an ApplyEvent from the module. Capture-only — the event is
// stored whole, masking + cap are applied later in [extractFinal] (once on
// the final, not on every intermediate event — core modules MVP only send a
// final).
func (c *outputCollector) Send(ev *pluginv1.ApplyEvent) error {
	c.events = append(c.events, ev)
	return nil
}

// lastEvent returns the final ApplyEvent or nil. The final is the last
// event; nil if the module sent nothing (no-op or early return).
func (c *outputCollector) lastEvent() *pluginv1.ApplyEvent {
	if len(c.events) == 0 {
		return nil
	}
	return c.events[len(c.events)-1]
}

// extractFinal splits the final ApplyEvent.Output into Errand components:
//
//   - stdout / stderr — strings from output.fields for core.cmd / core.exec
//     (util.SendFinal contract: shell/exec put "stdout"/"stderr"/"exit_code"
//     directly into the map[string]any output).
//   - exit_code — int32 from the numeric field; absence → nil pointer in
//     proto (ErrandResult.exit_code = 0 ≠ "no exit_code", but the proto
//     default 0 is a valid interpretation for non-shell modules).
//   - structured — the remaining output WITHOUT stdout/stderr/exit_code (for
//     shell, this is an empty struct / nil; for read-safe modules, the
//     module's whole structured output, masked).
//
// A nil event (module sent no final) → empty stdout/stderr, exitCode=0,
// structured=nil. This is the "module no-op" terminal — status is set by the
// caller.
func (c *outputCollector) extractFinal() (stdout, stderr string, exitCode int32, structured *structpb.Struct) {
	last := c.lastEvent()
	if last == nil || last.GetOutput() == nil {
		return "", "", 0, nil
	}
	out := last.GetOutput()
	fields := out.GetFields()
	if fields == nil {
		return "", "", 0, nil
	}

	// Extract known fields. structpb types: stdout/stderr are string,
	// exit_code is number (core.cmd puts float64(res.ExitCode)).
	if v, ok := fields["stdout"]; ok {
		stdout = v.GetStringValue()
	}
	if v, ok := fields["stderr"]; ok {
		stderr = v.GetStringValue()
	}
	if v, ok := fields["exit_code"]; ok {
		exitCode = int32(v.GetNumberValue())
	}

	// Structured output is whatever's left without the shell channels. For
	// shell/exec the result is empty (everything was stdout/stderr/exit_code),
	// for read-safe modules (`core.http.probe`) — their whole output (status/
	// body/elapsed_ms/...). Masked through the same sensitive-keys dictionary
	// as stdout/stderr — a single secret policy.
	structured = maskOutputExceptShell(out)
	return stdout, stderr, exitCode, structured
}

// maskOutputExceptShell builds a masked *structpb.Struct from output WITHOUT
// the shell fields (stdout/stderr/exit_code). If the struct is empty after
// removing those fields, returns nil (the handler decides whether to write
// NULL to the DB on the keeper side).
func maskOutputExceptShell(out *structpb.Struct) *structpb.Struct {
	if out == nil {
		return nil
	}
	raw := out.AsMap()
	delete(raw, "stdout")
	delete(raw, "stderr")
	delete(raw, "exit_code")
	if len(raw) == 0 {
		return nil
	}
	masked := audit.MaskSecrets(raw)
	s, err := structpb.NewStruct(masked)
	if err != nil {
		// Impossible shape (chan/func) — audit-mask only returns
		// json-serializable forms, so defensive: return nil instead of a
		// potential panic.
		return nil
	}
	return s
}

// MaskAndCapBytes is the shared helper for masking + capping
// stdout/stderr/error_message. Mask first (a slice could cut a sensitive
// substring in half), then cap. Mirrors
// keeper/internal/errand.MaskAndCapBytes (defense-in-depth on both sides:
// same dictionary, same limit).
//
// Empty string → ("", false) with no allocation.
func MaskAndCapBytes(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	m := audit.MaskSecrets(map[string]any{"v": s})
	masked, _ := m["v"].(string)
	if masked == "" {
		masked = s
	}
	if len(masked) <= OutputCapBytes {
		return masked, false
	}
	return masked[:OutputCapBytes], true
}
