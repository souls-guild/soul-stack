package push

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// ErrNoRunResult — the stdout stream ended (EOF) without a final RunResult.
// This marks an undelivered result: `soul apply` always sends exactly one
// RunResult at the end (ndjsonsink.go), so its absence means the process
// crashed or the session was cut before the run finished. The dispatcher must
// treat this as a failure (fail-closed), not as "success without a report".
var ErrNoRunResult = errors.New("push: NDJSON stream ended without RunResult")

// EventHandler — callback for each intermediate TaskEvent in the NDJSON
// stream. nil is fine: RunResult carries the run's outcome.
//
// ★ A push run's `register:` DOES NOT FILL, and this is the seam where that is
// decided (verified for NIM-869). `register_data` rides on TaskEvent and
// nothing else — RunResult has no field for it — so a push run's only copy
// passes through here, and [SshDispatcher.SendApply] gives this callback to a
// debug log. Persisting it is not a matter of holding onto the events either:
// `apply_task_register` carries a foreign key to `apply_runs(apply_id, sid)`,
// and a push run writes `push_runs` and no row there. So a scenario task
// executed over push would leave `register.<name>` unresolved and any barrier
// waiting on it unreleased. Nothing reaches that state today — the scenario
// dispatcher has no push branch ([scenario.ApplyDispatcher] is implemented
// only by [grpc.Outbound]) — but the ticket that adds one (NIM-870) has to
// decide whether a push run mints an `apply_runs` row, and that decision is an
// ADR, not an implementation detail of this callback.
type EventHandler func(*keeperv1.TaskEvent)

// ParseStream reads the line-delimited NDJSON stdout of `soul apply`
// (ndjsonsink.go): a sequence of protojson TaskEvent lines, followed by
// EXACTLY one final RunResult line. Each TaskEvent is passed to onEvent (if
// not nil), RunResult is returned as the outcome.
//
// TaskEvent vs RunResult discriminator: both are serialized as bare protojson
// WITHOUT a type tag (ndjsonsink writes them with a single writeLine), but
// their `status` enum field uses non-overlapping prefixes — `TASK_STATUS_*`
// for TaskEvent vs `RUN_STATUS_*` for RunResult. We discriminate on that
// prefix rather than "the last line is the RunResult": line-by-line reading
// doesn't know the end in advance, and relying on overlapping fields
// (protojson.Unmarshal is lenient) would cause a silent mis-parse.
//
// Returns:
//   - (*RunResult, nil) — the stream is well-formed, the final RunResult was received.
//   - (nil, ErrNoRunResult) — EOF without a RunResult (crash/cut-off before completion).
//   - (nil, error) — a malformed/unclassifiable line, or a read error.
//
// Lines after RunResult are a protocol error (RunResult is final): such a
// stream is rejected. Empty lines (double '\n') are skipped.
func ParseStream(r io.Reader, onEvent EventHandler) (*keeperv1.RunResult, error) {
	sc := bufio.NewScanner(r)
	// soul apply may return a large TaskEvent (register_data/output): raise the
	// line limit from the 64 KiB default to 1 MiB so long output doesn't break
	// the stream with a spurious ErrTooLong.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var result *keeperv1.RunResult
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if result != nil {
			return nil, fmt.Errorf("push: NDJSON line after final RunResult: %q", truncate(line))
		}

		kind, err := classifyLine(line)
		if err != nil {
			return nil, err
		}
		switch kind {
		case lineTaskEvent:
			ev := &keeperv1.TaskEvent{}
			if err := protojson.Unmarshal([]byte(line), ev); err != nil {
				return nil, fmt.Errorf("push: parsing TaskEvent: %w (line: %q)", err, truncate(line))
			}
			if onEvent != nil {
				onEvent(ev)
			}
		case lineRunResult:
			rr := &keeperv1.RunResult{}
			if err := protojson.Unmarshal([]byte(line), rr); err != nil {
				return nil, fmt.Errorf("push: parsing RunResult: %w (line: %q)", err, truncate(line))
			}
			result = rr
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("push: reading NDJSON stream: %w", err)
	}
	if result == nil {
		return nil, ErrNoRunResult
	}
	return result, nil
}

type lineKind int

const (
	lineTaskEvent lineKind = iota
	lineRunResult
)

const (
	taskStatusPrefix = "TASK_STATUS_"
	runStatusPrefix  = "RUN_STATUS_"
)

// classifyLine determines the NDJSON line's type from the enum prefix of its
// `status` field (see ParseStream). The `status` field is sent by both
// message types and their enum names don't overlap, so it's a reliable
// discriminator without a type tag.
//
// Edge cases (all → a clear error, fail-closed):
//   - non-JSON / partial line → parse error;
//   - JSON without `status`, or `status` not a string → unclassifiable;
//   - `status` with an unknown prefix → unclassifiable.
func classifyLine(line string) (lineKind, error) {
	var probe struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return 0, fmt.Errorf("push: invalid NDJSON line: %w (line: %q)", err, truncate(line))
	}
	switch {
	case strings.HasPrefix(probe.Status, taskStatusPrefix):
		return lineTaskEvent, nil
	case strings.HasPrefix(probe.Status, runStatusPrefix):
		return lineRunResult, nil
	default:
		return 0, fmt.Errorf("push: unclassifiable NDJSON line (status=%q): %q", probe.Status, truncate(line))
	}
}

// truncate caps a string for error messages (a malformed line can be long —
// avoid flooding the log with the whole thing).
func truncate(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
