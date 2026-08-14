package util

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// SendFinal — sends the final ApplyEvent with changed/failed/output on the
// stream. Output can be nil (the field is then omitted). This helper exists
// so each core-module doesn't repeat the *pluginv1.ApplyEvent assembly
// boilerplate and the stream.Send call.
//
// pluginv1.ApplyEvent contract convention: the final event is the one with
// changed or failed set; MVP-core doesn't yet send intermediate diagnostic
// messages (without changed/failed).
func SendFinal(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, output map[string]any) error {
	ev := &pluginv1.ApplyEvent{Changed: changed}
	if output != nil {
		s, err := structpb.NewStruct(output)
		if err != nil {
			return err
		}
		ev.Output = s
	}
	return stream.Send(ev)
}

// SendFailed — final event with failed=true and the error text in message.
// Carries no output: this is the failure of a module that never got far enough
// to produce any — a bad param, an unknown state, a process that would not
// start. When the module DID produce output and still fails, the output has to
// travel with it: use [SendFailedWithOutput].
func SendFailed(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], message string) error {
	return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: message})
}

// SendFailedWithOutput — failed=true, and the output survives.
//
// Needed since NIM-687 gave the verb-shell modules an `exit_codes` set: a
// command that ran and returned a code outside it fails the task, but its
// stdout/stderr/exit_code are exactly what the operator needs, and dropping
// them would break two things at once. Diagnostics lose the stderr the failure
// was declared for; and `failed_when:` loses its escape hatch, because a
// predicate over `register.self.exit_code` cannot be evaluated when the field
// is not there. Both work because runtime.selfRegisterData copies
// ApplyEvent.Output into register.self.* whether or not failed is set — the
// event just has to carry it. `exit_code` is a number, so it is always
// encodable and the escape hatch always has something to read; see the
// per-field degradation below for the fields that are not.
//
// No `changed` counterpart, deliberately: runtime.runTask derives the base
// outcome from the FIRST matching case of failed/changed, so `changed` on a
// failed event is never read. A parameter for it would look like it kept the
// change signal through a `failed_when: false` waiver, and it would not — such
// a task ends OK, not CHANGED. Wiring that through is a change to ADR-012(d)
// semantics for every module, not a detail of this one.
//
// Unserializable output degrades per field, it does not abort and it does not
// drop the rest. A command's stdout is arbitrary bytes and structpb rejects
// invalid UTF-8, so `printf '\xff'; exit 3` cannot become a Struct. Failing the
// whole conversion would take the verdict down with it; failing the whole map
// would take `exit_code` and `stderr` down with `stdout`, and those are the two
// fields the escape hatch above is built on. So only the offending key is left
// out, and it is named in the message. Same boundary as http/probe.go, one step
// finer.
func SendFailedWithOutput(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], message string, output map[string]any) error {
	ev := &pluginv1.ApplyEvent{Failed: true, Message: message}
	if output != nil {
		s, dropped := structKeepingWhatEncodes(output)
		if len(s.GetFields()) > 0 {
			ev.Output = s
		}
		if len(dropped) > 0 {
			ev.Message += fmt.Sprintf(" (output serialization failed for %s)", strings.Join(dropped, ", "))
		}
	}
	return stream.Send(ev)
}

// structKeepingWhatEncodes converts what it can and reports the rest by key
// NAME, never by value. structpb's own error embeds a %q of the value that
// would not encode (structpb.NewValue → "invalid UTF-8 in string: %q"), and
// OSRunner buffers stdout without a ceiling — so pasting that error into the
// message turns a command that wrote 8 MB of binary into a message several
// times the size of the output it is standing in for, and applyrunner copies
// the message into TaskError verbatim. The key name says the same thing in a
// bounded number of bytes.
func structKeepingWhatEncodes(output map[string]any) (*structpb.Struct, []string) {
	if s, err := structpb.NewStruct(output); err == nil {
		return s, nil
	}
	fields := make(map[string]*structpb.Value, len(output))
	var dropped []string
	for k, v := range output {
		val, err := structpb.NewValue(v)
		if err != nil {
			dropped = append(dropped, k)
			continue
		}
		fields[k] = val
	}
	sort.Strings(dropped) // map order is random; the message must not be
	return &structpb.Struct{Fields: fields}, dropped
}

// SendPlanFinal — final dry-run PlanEvent (ADR-031 Scry) with the machine
// `changed` (drift). SendFinal's counterpart for Apply: core-modules don't
// repeat the *pluginv1.PlanEvent assembly boilerplate. Plan output isn't
// passed in MVP — dry-run only reports the fact of drift.
func SendPlanFinal(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], changed bool) error {
	return stream.Send(&pluginv1.PlanEvent{Changed: changed})
}

// PlanFailed — a dry-run error: the module couldn't determine drift (invalid
// param, unsupported backend/state). Returned from Plan as a Go error — the
// host (runtime.planTask) maps a non-nil error to FAILED (plan.error), NOT to
// clean (ADR-031: never a false-clean). PlanEvent has no failed field
// (only-add changed, symmetry with ApplyEvent falls short here), so a plan
// failure travels as Plan's returned error, not as an event — the host must
// check error before changed. SendFailed's counterpart for Apply (which sends
// a failed event, since ApplyEvent.failed exists; here it doesn't — hence the
// difference in shape).
func PlanFailed(message string) error {
	return errors.New(message)
}

// StringsToAny converts []string to []any for a list-valued output field:
// structpb.NewStruct only accepts []any (not []string) as a list.
// A single spot for core-modules that put a string list into output
// (warnings and the like), so they don't repeat the boilerplate loop at
// every call site.
func StringsToAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
