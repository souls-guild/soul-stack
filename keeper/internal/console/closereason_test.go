package console

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// A Keeper-side close must reach keeper_console_sessions_total, and must reach
// it under a reason that says WHICH kill-on-disconnect path ran (NIM-253).
//
// Two failures live here. The counter only ever moved on a Soul-reported exit
// and on an orphan reap, so every session Keeper itself closed left the gauge
// and the totals unable to reconcile — the metric's own contract. And the
// reason a socket died was a sentence handed to CloseAllFor by its caller, so
// "the operator closed their tab" and "the socket fell behind and took every
// pty with it" were indistinguishable after the fact: the first is routine, the
// second is the operator's whole wall dying, and an operator asking why has
// nothing else to go on (the socket is gone, so no close frame can carry it).
func TestHub_CloseCountsTerminalUnderItsOwnReason(t *testing.T) {
	reg := obs.NewRegistry()
	h, _ := newTestHub(t, HubDeps{Metrics: RegisterMetrics(reg)})

	detached := mustOpen(t, h, "pane-1", "host-a", "archon-a", &captureSink{})
	stalled := mustOpen(t, h, "pane-2", "host-a", "archon-a", &captureSink{})

	h.Close(context.Background(), detached, CloseOperatorDetached)
	h.CloseAllFor(context.Background(), []*Session{stalled}, CloseSocketWriteFailed)

	body := obstest.Scrape(t, reg.Gatherer())
	for _, want := range []string{
		`keeper_console_sessions_total{reason="operator_detached"} 1`,
		`keeper_console_sessions_total{reason="operator_socket_write_failed"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s; got=\n%s", want, body)
		}
	}
}

// The label set must stay closed. CloseAllFor takes the reason from its caller,
// and a caller that interpolated a session id — the obvious way to make a log
// line more useful — would mint a new time series per console (ADR-024 §2.2).
func TestCloseReason_UnknownCollapsesToUnknownLabel(t *testing.T) {
	rogue := CloseReason("socket 01J0F2R6NX closed")
	if got := rogue.Label(); got != "unknown" {
		t.Errorf("Label() = %q, want %q — an unbounded label reaches Prometheus", got, "unknown")
	}
	// Prose has no cardinality budget, and swallowing it would lose the only
	// clue to where the rogue value came from.
	if got := rogue.Text(); got != string(rogue) {
		t.Errorf("Text() = %q, want it passed through verbatim", got)
	}
}

// The sentences are persisted in console_recordings.close_reason and served by
// the recordings API, so a recording written before this type existed and one
// written after must read the same. Changing a value here rewrites what old
// records appear to say.
func TestCloseReason_TextsAreTheOnesAlreadyOnDisk(t *testing.T) {
	for reason, want := range map[CloseReason]string{
		CloseOperatorDetached:     "operator detached the pane",
		CloseSocketClosed:         "operator socket closed",
		CloseIdleTimeout:          "idle timeout",
		CloseRecordingUnavailable: "recording unavailable",
	} {
		if got := reason.Text(); got != want {
			t.Errorf("%s.Text() = %q, want %q", reason, got, want)
		}
	}
}
