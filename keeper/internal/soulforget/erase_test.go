package soulforget

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The guard in this file is the one the whole ordering of [Erase] exists for:
// if the cluster cannot be told to close its streams, the host must still be
// registered afterwards. "Deleted the row anyway and told the operator it went
// fine" is the failure mode, and it can only be caught before the transaction
// is ever opened — which is why the fake pool below treats being called at all
// as the defect.

// refusingPool fails the test if anything tries to open a transaction. It is not
// a stub that returns an error: an error return would let [Erase] proceed down
// the "delete failed" branch and reach the same outward result for the wrong
// reason. Nothing must reach the database.
type refusingPool struct {
	t      *testing.T
	called bool
}

func (p *refusingPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	p.t.Helper()
	p.called = true
	p.t.Error("a transaction was opened even though the cluster-wide teardown notice failed — " +
		"the host would be erased while a stream on another Keeper instance stayed open, " +
		"with the operator shown a success")
	return nil, errors.New("refusingPool: BeginTx must not be called")
}

// scriptedTeardown drives each step of the teardown independently so a single
// unreleasable resource can be simulated without the others changing.
type scriptedTeardown struct {
	broadcastCalls int
	broadcastErr   []error // per call; short list reuses the last entry
	broadcastSent  bool

	closeLocal bool

	purged   int64
	purgeErr error
}

func (s *scriptedTeardown) CloseLocal(string) bool { return s.closeLocal }

func (s *scriptedTeardown) Broadcast(context.Context, string) (bool, error) {
	i := s.broadcastCalls
	s.broadcastCalls++
	if len(s.broadcastErr) == 0 {
		return s.broadcastSent, nil
	}
	if i >= len(s.broadcastErr) {
		i = len(s.broadcastErr) - 1
	}
	if err := s.broadcastErr[i]; err != nil {
		return false, err
	}
	return s.broadcastSent, nil
}

func (s *scriptedTeardown) PurgeCache(context.Context, string) (int64, error) {
	return s.purged, s.purgeErr
}

// TestErase_BroadcastFailure_ErasesNothing — the pre-flight. A publish that
// cannot even go out means the cluster cannot be told, so the operation stops
// with the registry untouched and the operator gets a retryable failure rather
// than a host that is gone from the database and still talking to a Keeper.
func TestErase_BroadcastFailure_ErasesNothing(t *testing.T) {
	pool := &refusingPool{t: t}
	td := &scriptedTeardown{broadcastErr: []error{errors.New("redis unreachable")}}

	res, err := Erase(context.Background(), pool, td, "host1.example.com", "forgotten by archon-alice")
	if !errors.Is(err, ErrTeardownUnavailable) {
		t.Fatalf("err = %v, want ErrTeardownUnavailable", err)
	}
	if pool.called {
		t.Error("BeginTx was reached") // detail already reported by refusingPool
	}
	if !reflect.DeepEqual(res, Result{}) {
		t.Errorf("result = %+v, want the zero Result — nothing happened, so nothing is reportable", res)
	}
	if td.broadcastCalls != 1 {
		t.Errorf("Broadcast calls = %d, want 1 (the pre-flight; the second notice is post-delete)", td.broadcastCalls)
	}
}

// TestErase_BroadcastFailure_KeepsTheUnderlyingCause — an operator retrying
// against an unreachable Redis needs the reason, not just the class. Wrapping
// that dropped the cause would answer "could not be sent" and nothing more.
func TestErase_BroadcastFailure_KeepsTheUnderlyingCause(t *testing.T) {
	cause := errors.New("dial tcp 127.0.0.1:6379: connection refused")
	td := &scriptedTeardown{broadcastErr: []error{cause}}

	_, err := Erase(context.Background(), &refusingPool{t: t}, td, "host1.example.com", "forgotten by archon-alice")
	if !errors.Is(err, cause) {
		t.Errorf("err = %v, want it to wrap the transport cause %v", err, cause)
	}
}

// TestErase_NilTeardown_StillErases — single-instance dev and unit tests wire no
// teardown at all. There is no cluster to notify and the stream dies with the
// process, so the absence must not be read as "the notice failed".
//
// It stops at the transaction (the fake pool has none), which is exactly the
// point: the pre-flight let it through instead of refusing.
func TestErase_NilTeardown_StillErases(t *testing.T) {
	sentinel := errors.New("reached the transaction")
	_, err := Erase(context.Background(), beginTxFunc(func() (pgx.Tx, error) {
		return nil, sentinel
	}), nil, "host1.example.com", "forgotten by archon-alice")

	if errors.Is(err, ErrTeardownUnavailable) {
		t.Fatalf("a nil Teardown was treated as a failed notice: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the operation to have reached the transaction", err)
	}
}

// beginTxFunc adapts a plain function to [TxBeginner].
type beginTxFunc func() (pgx.Tx, error)

func (f beginTxFunc) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return f() }
