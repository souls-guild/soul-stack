//go:build e2e

package harness

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/internal/soulstub"
)

// ConnectSoulStubReconnect opens a live EventStream for the i-th pre-auth
// Soul to the primary keeper (like ConnectSoulStub), but additionally arms
// the stub with a fallback list of ALL cluster keeper gRPC addresses and
// enables auto-reconnect + WardRoster (mirroring the real
// soul/cmd/soul reconnectLoop -> handleSession). After the stream-holder
// keeper dies, the stub reconnects itself to a live keeper and announces its
// set of watched apply_ids via WardRoster.
//
// holdApply=true additionally puts the stub into a "hold ApplyRequest" mode:
// on an ApplyRequest it does NOT send a RunResult (the apply_runs row stays
// `dispatched`), and registers the apply_id in activeWard. This reproduces a
// dispatched-orphan: a task handed out, no RunResult received, the
// keeper-holder killed.
//
// Returns the stub (with a HelloReply already confirmed on the primary).
func (s *Stack) ConnectSoulStubReconnect(t *testing.T, soulIndex int, holdApply bool) *soulstub.Stub {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.souls) {
		t.Fatalf("ConnectSoulStubReconnect(%d): out of range (created %d souls)", soulIndex, len(s.souls))
	}
	id := s.souls[soulIndex]

	stub := soulstub.New(id.SID, s.KeeperGRPCAddr, id.Cert, id.Key, s.caBundle)
	stub.SetEndpoints(s.AllKeeperGRPCAddrs())
	stub.EnableReconnect(true)
	stub.SetHoldApply(holdApply)

	ctx, cancel := context.WithCancel(context.Background())
	if err := stub.Open(ctx); err != nil {
		cancel()
		t.Fatalf("ConnectSoulStubReconnect(%s): open stream: %v", id.SID, err)
	}
	s.cleanups = append(s.cleanups, func() {
		_ = stub.Close()
		cancel()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range stub.Messages() {
			if m.Kind == "HelloReply" {
				return stub
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("ConnectSoulStubReconnect(%s): HelloReply not received within 10s (lease/handshake did not complete)", id.SID)
	return nil
}

// ApplyRunStatusForSID reads the status of the apply_runs row (applyID, sid)
// from PG. Empty string if the row doesn't exist yet (TaskEvent/Insert hasn't
// landed). A narrow read helper for polling the dispatched->orphaned
// transition.
func (s *Stack) ApplyRunStatusForSID(t *testing.T, applyID, sid string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status string
	err := s.db.QueryRow(ctx,
		`SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2 ORDER BY passage ASC LIMIT 1`,
		applyID, sid).Scan(&status)
	if err != nil {
		return ""
	}
	return status
}

// WaitApplyRunStatusForSID polls the status of the apply_runs row (applyID,
// sid) until it enters one of the want statuses, or a timeout. Returns the
// reached status. Fatals on timeout, including the last observed status.
func (s *Stack) WaitApplyRunStatusForSID(t *testing.T, applyID, sid string, want []string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = s.ApplyRunStatusForSID(t, applyID, sid)
		for _, w := range want {
			if last == w {
				return last
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("WaitApplyRunStatusForSID(%s,%s): status %v not reached within %s (last=%q)",
		applyID, sid, want, timeout, last)
	return ""
}

// StreamHolderKID returns the KID of the keeper the soul-stub's EventStream
// is connected to (= the keeper at the primary address s.KeeperGRPCAddr that
// Open dialed directly). This is the stream-holder keeper: SendApply is
// routed to it via the SID lease, and its death leaves a dispatched row
// orphaned. Deterministic without reading Redis: the stub always opens its
// initial stream to the primary.
func (s *Stack) StreamHolderKID(t *testing.T) string {
	t.Helper()
	kid := s.KeeperKIDForGRPCAddr(s.KeeperGRPCAddr)
	if kid == "" {
		t.Fatalf("StreamHolderKID: could not map primary address %q to any KID", s.KeeperGRPCAddr)
	}
	return kid
}
