package console

// CloseReason names why Keeper ended a console session.
//
// It is a type rather than the free string it replaces because the reason is
// two things at once, with opposite requirements:
//
//   - a sentence, which reaches the Soul's log through ConsoleClose, the
//     recording's `close_reason` and the audit trail — read by people, so it
//     must stay stable once written;
//   - a Prometheus label on keeper_console_sessions_total, which ADR-024 §2.2
//     requires to come from a closed set. A label minted at a call site is
//     unbounded by construction: one caller interpolating a session id into its
//     reason would blow up the cardinality of the whole family.
//
// So the constants below ARE the label set, [CloseReason.Text] carries the
// prose, and [CloseReason.Label] refuses anything not in the table.
type CloseReason string

const (
	// CloseOperatorDetached — the operator closed one pane; the socket and its
	// other sessions live on.
	CloseOperatorDetached CloseReason = "operator_detached"

	// CloseSocketClosed — the operator's socket ended from the read side: the
	// tab was closed, the page navigated away, the network dropped. The
	// ordinary end of a console, and the only one of the socket reasons that is
	// not a failure.
	CloseSocketClosed CloseReason = "operator_socket_closed"

	// CloseSocketWriteFailed — a write to the operator's socket failed, so the
	// connection is unusable (gorilla cannot recover one) and every pty behind
	// it dies. It means the peer vanished or fell behind past the write budget,
	// which is the same budget the pong handler holds it to — the read side had
	// not yet noticed.
	CloseSocketWriteFailed CloseReason = "operator_socket_write_failed"

	// CloseSocketUnreachable — the keepalive ping could not be written. A slept
	// laptop keeps a half-open TCP connection for minutes, and this is what
	// notices before the read deadline does.
	CloseSocketUnreachable CloseReason = "operator_socket_unreachable"

	// CloseSocketCongested — a lifecycle frame found the outbound queue full.
	// Control frames are never dropped (losing an `opened` strands a pane on
	// "connecting", losing an `exit` leaves it live forever), so the socket goes
	// instead.
	CloseSocketCongested CloseReason = "operator_socket_congested"

	// CloseIdleTimeout — no operator input for longer than the idle limit.
	CloseIdleTimeout CloseReason = "idle_timeout"

	// CloseRecordingUnavailable — the session could no longer be recorded, and
	// a console that stops being recorded stops (ADR-0074(g)).
	CloseRecordingUnavailable CloseReason = "recording_unavailable"
)

// closeReasonText is the registry of known reasons. Membership here is what
// makes a value safe to use as a metric label, so a new reason is added here
// and nowhere else.
//
// The sentences are deliberately the ones these paths already wrote before the
// reason had a type: they are persisted in `console_recordings.close_reason`
// and served by the recordings API, so changing them would rewrite the meaning
// of records already on disk.
var closeReasonText = map[CloseReason]string{
	CloseOperatorDetached:     "operator detached the pane",
	CloseSocketClosed:         "operator socket closed",
	CloseSocketWriteFailed:    "operator socket write failed",
	CloseSocketUnreachable:    "operator socket unreachable",
	CloseSocketCongested:      "operator socket congested",
	CloseIdleTimeout:          "idle timeout",
	CloseRecordingUnavailable: "recording unavailable",
}

// Label is the metric label. Anything outside the table collapses to `unknown`
// rather than reaching Prometheus: the counter is worth less with one bucket
// missing than the family is with unbounded cardinality.
func (r CloseReason) Label() string {
	if _, ok := closeReasonText[r]; ok {
		return string(r)
	}
	return "unknown"
}

// Text is the sentence that reaches the Soul, the recording and the audit
// trail. An unknown reason passes through verbatim — prose has no cardinality
// budget, and swallowing it would lose the only clue to where it came from.
func (r CloseReason) Text() string {
	if text, ok := closeReasonText[r]; ok {
		return text
	}
	return string(r)
}
