package consolerunner

import (
	"errors"
	"strings"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TestSession_UndeliverableExitIsReportedLoudly — when the only terminal event a
// session has cannot be sent, the runner says so at WARN.
//
// This is a guard on audibility, not on delivery, and the distinction is the
// finding (NIM-397). Delivery cannot be guaranteed: escalate step 4 exists
// precisely to cancel the RPC of a sender blocked on a Keeper that stopped
// reading, and after that cut its ConsoleExit has nowhere to go. What CAN be
// guaranteed is that the loss is stated.
//
// It was not. The line sat at Debug, next to the cosmetic "send chunk failed",
// and `TestLiveGRPC_ConsoleRoundTripOverItsOwnStream` failed in CI with `timed out
// after 10s waiting for ConsoleExit` while its explanation went to a logger the
// tests had pointed at io.Discard. Four hypotheses were measured and rejected
// before anyone looked at the level of a log line.
func TestSession_UndeliverableExitIsReportedLoudly(t *testing.T) {
	shell := requireShell(t)
	logger, captured := testLoggerWithCapture(t)

	// A sink that fails every Send is the smallest honest model of the state
	// escalate step 4 leaves behind: the session is alive, its transport is not.
	sink := &recordingSink{err: errors.New("stream broken")}
	r := New(sink, Limits{}, logger, nil)

	const id = "01UNDELIVERABLEEXIT0000000"
	r.Open(&keeperv1.ConsoleOpen{SessionId: id, Shell: shell, Cols: 80, Rows: 24})
	waitFor(t, 10*time.Second, "the session to register", func() bool { return r.ActiveCount() == 1 })

	r.Close(&keeperv1.ConsoleClose{SessionId: id, Reason: "terminal-audibility guard"})
	waitFor(t, 20*time.Second, "the session to be reaped", func() bool { return r.ActiveCount() == 0 })

	log := captured.String()
	if !strings.Contains(log, "ConsoleExit could not be delivered") {
		t.Fatalf("the terminal event was lost and the runner never said so.\n"+
			"A session whose ConsoleExit does not reach anyone must announce it —\n"+
			"otherwise the next timeout waiting for it is unexplainable.\nlog:\n%s", log)
	}
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "ConsoleExit could not be delivered") {
			if !strings.Contains(line, "level=WARN") {
				t.Fatalf("the lost terminal event is reported below WARN, so it is "+
					"invisible at any normal log level:\n%s", line)
			}
			return
		}
	}
}
