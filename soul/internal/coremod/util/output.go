package util

import (
	"errors"

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
// No output is passed — failure semantics make the output field meaningless.
func SendFailed(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], message string) error {
	return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: message})
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
