package consolerunner

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TestWireContract_ConsoleFieldNumbersAreFrozen enforces ADR-012(c) forward-compat
// only-add for the console sub-protocol.
//
// Field numbers are the wire contract between a Keeper and a Soul that may be
// running different builds. Renumbering or reusing one silently reinterprets a
// live fleet's messages — a `console_stdin` decoded as something else. The
// numbers below were allocated after the last occupied slot in each direction
// (FromKeeper ended at 12, FromSoul at 10) and must never move; a new console
// message takes the NEXT free number instead.
func TestWireContract_ConsoleFieldNumbersAreFrozen(t *testing.T) {
	fromKeeper := map[string]protoreflect.FieldNumber{
		"console_open":   13,
		"console_stdin":  14,
		"console_resize": 15,
		"console_close":  16,
	}
	fromSoul := map[string]protoreflect.FieldNumber{
		"console_opened": 11,
		"console_chunk":  12,
		"console_exit":   13,
	}

	assertFieldNumbers(t, (&keeperv1.FromKeeper{}).ProtoReflect().Descriptor(), fromKeeper)
	assertFieldNumbers(t, (&keeperv1.FromSoul{}).ProtoReflect().Descriptor(), fromSoul)
}

// TestWireContract_ConsoleIsInThePayloadOneof pins that the console messages ride
// the existing EventStream `oneof payload` rather than a new RPC — the whole
// reason this slice needed no transport work.
func TestWireContract_ConsoleIsInThePayloadOneof(t *testing.T) {
	for _, tc := range []struct {
		wrapper protoreflect.MessageDescriptor
		fields  []string
	}{
		{(&keeperv1.FromKeeper{}).ProtoReflect().Descriptor(), []string{"console_open", "console_stdin", "console_resize", "console_close"}},
		{(&keeperv1.FromSoul{}).ProtoReflect().Descriptor(), []string{"console_opened", "console_chunk", "console_exit"}},
	} {
		for _, name := range tc.fields {
			fd := tc.wrapper.Fields().ByName(protoreflect.Name(name))
			if fd == nil {
				t.Errorf("%s.%s is missing", tc.wrapper.Name(), name)
				continue
			}
			oneof := fd.ContainingOneof()
			if oneof == nil || oneof.Name() != "payload" {
				t.Errorf("%s.%s must live in `oneof payload`, got %v", tc.wrapper.Name(), name, oneof)
			}
		}
	}
}

// TestWireContract_ExitReasonsAreExhaustive keeps the metrics label mapping in
// step with the enum: a reason added to the proto without a label would silently
// land in the "unknown" bucket and hide a real terminal state.
func TestWireContract_ExitReasonsAreExhaustive(t *testing.T) {
	values := keeperv1.ConsoleExitReason(0).Descriptor().Values()
	for i := range values.Len() {
		reason := keeperv1.ConsoleExitReason(values.Get(i).Number())
		if reason == keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_UNSPECIFIED {
			continue
		}
		if got := reasonLabel(reason); got == labelReasonUnknown {
			t.Errorf("ConsoleExitReason %v has no metrics label — add it to reasonLabel", reason)
		}
	}
}

func assertFieldNumbers(t *testing.T, md protoreflect.MessageDescriptor, want map[string]protoreflect.FieldNumber) {
	t.Helper()
	for name, number := range want {
		fd := md.Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			t.Errorf("%s.%s is missing from the generated descriptor", md.Name(), name)
			continue
		}
		if fd.Number() != number {
			t.Errorf("%s.%s = field %d, want %d (only-add: numbers are frozen)", md.Name(), name, fd.Number(), number)
		}
	}
}
