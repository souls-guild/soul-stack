package runtime

import (
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// NDJSONSink — [EventSink] implementation for push mode (`soul apply`,
// ADR-004). Writes each TaskEvent and the final RunResult as one protojson
// line (NDJSON / JSON-lines), which Keeper reads from the SSH session's stdout.
//
// Same proto messages as pull mode (EventStream) — one shared contract
// (ADR-012). The transport differs: line-oriented stdout instead of a gRPC stream.
type NDJSONSink struct {
	w   io.Writer
	enc protojson.MarshalOptions
	// lastStatus is the status of the last written RunResult. The caller
	// (`soul apply`) reads it after Run for the exit code: ApplyRunner always
	// sends exactly one RunResult at the end of a run.
	lastStatus keeperv1.RunStatus
}

// NewNDJSONSink builds a sink over w (typically os.Stdout). Each message is
// serialized to one line (no indentation) and terminated with '\n'.
func NewNDJSONSink(w io.Writer) *NDJSONSink {
	return &NDJSONSink{
		w: w,
		// Multiline:false guarantees "one message = one line" (NDJSON).
		// EmitUnpopulated:false keeps output compact; the Keeper-side reader
		// uses the same proto defaults on Unmarshal.
		enc: protojson.MarshalOptions{Multiline: false},
	}
}

func (s *NDJSONSink) SendTaskEvent(ev *keeperv1.TaskEvent) error {
	return s.writeLine(ev)
}

func (s *NDJSONSink) SendRunResult(rr *keeperv1.RunResult) error {
	s.lastStatus = rr.GetStatus()
	return s.writeLine(rr)
}

// LastStatus returns the status of the last written RunResult.
// RUN_STATUS_UNSPECIFIED until the first SendRunResult.
func (s *NDJSONSink) LastStatus() keeperv1.RunStatus {
	return s.lastStatus
}

func (s *NDJSONSink) writeLine(m proto.Message) error {
	b, err := s.enc.Marshal(m)
	if err != nil {
		return fmt.Errorf("ndjson: marshal %T: %w", m, err)
	}
	b = append(b, '\n')
	if _, err := s.w.Write(b); err != nil {
		return fmt.Errorf("ndjson: write %T: %w", m, err)
	}
	return nil
}
