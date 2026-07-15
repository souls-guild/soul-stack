package rbac

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// nopPool — a [ServicePool] stub for the invalidate-hook unit tests: NewService
// requires a non-nil Pool, but these tests never touch the DB (only invalidate/
// SetInvalidator). Any real call fails loudly (the test would break if it
// accidentally hit the DB path).
type nopPool struct{}

func (nopPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("nopPool: Exec not expected")
}
func (nopPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("nopPool: Query not expected")
}
func (nopPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("nopPool: BeginTx not expected")
}

// countingInvalidator is a test [Invalidator] that counts Invalidate calls.
type countingInvalidator struct {
	calls atomic.Int64
}

func (c *countingInvalidator) Invalidate(_ context.Context) { c.calls.Add(1) }

// TestService_Invalidate_NilSafe verifies that Service.invalidate is a no-op
// when no invalidator is attached (single-Keeper/dev without Redis) and
// doesn't panic.
func TestService_Invalidate_NilSafe(t *testing.T) {
	s, err := NewService(ServiceDeps{Pool: nopPool{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// invalidate without SetInvalidator silently does nothing.
	s.invalidate(context.Background())

	// SetInvalidator(nil) must not break a subsequent invalidate either.
	s.SetInvalidator(nil)
	s.invalidate(context.Background())
}

// TestService_Invalidate_CallsInvalidator verifies that an attached
// invalidator fires on every invalidate (the hook that all 5 mutations call
// after commit).
func TestService_Invalidate_CallsInvalidator(t *testing.T) {
	s, err := NewService(ServiceDeps{Pool: nopPool{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	inv := &countingInvalidator{}
	s.SetInvalidator(inv)

	s.invalidate(context.Background())
	s.invalidate(context.Background())
	if got := inv.calls.Load(); got != 2 {
		t.Fatalf("Invalidate calls = %d, want 2", got)
	}

	// Removing the invalidator falls back to no-op (counter stops growing).
	s.SetInvalidator(nil)
	s.invalidate(context.Background())
	if got := inv.calls.Load(); got != 2 {
		t.Fatalf("after SetInvalidator(nil): calls = %d, want 2", got)
	}
}
