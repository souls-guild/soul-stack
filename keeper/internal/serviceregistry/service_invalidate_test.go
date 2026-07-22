package serviceregistry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// hookPool — a controllable ExecQueryRower for invalidate-hook tests: QueryRow
// returns a successful/error row (Insert/Update/SetSetting), Exec — a preset tag
// + error (Delete). Verifies that invalidate is called ONLY after a successful
// commit of a specific mutation.
type hookPool struct {
	queryRowErr error // nil → row.Scan succeeds (Insert/Update/SetSetting went through)
	execTag     pgconn.CommandTag
	execErr     error
}

func (p *hookPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return p.execTag, p.execErr
}

func (p *hookPool) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return scanRow{err: p.queryRowErr}
}

func (p *hookPool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("hookPool: Query not configured")
}

// scanRow — on err==nil fills all passed *time.Time with the current
// time (RETURNING created_at/updated_at), otherwise returns err.
type scanRow struct{ err error }

func (r scanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	now := time.Now()
	for _, d := range dest {
		if tp, ok := d.(*time.Time); ok {
			*tp = now
		}
	}
	return nil
}

// countingInvalidator — a test [Invalidator] that counts Invalidate calls.
type countingInvalidator struct {
	calls atomic.Int64
}

func (c *countingInvalidator) Invalidate(_ context.Context) { c.calls.Add(1) }

// TestService_Invalidate_NilSafe — without a connected invalidator,
// Service.invalidate is a no-op (single-Keeper/dev without Redis), doesn't panic.
func TestService_Invalidate_NilSafe(t *testing.T) {
	s, err := NewService(ServiceDeps{Pool: &hookPool{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s.invalidate(context.Background())

	s.SetInvalidator(nil)
	s.invalidate(context.Background())
}

// TestService_Invalidate_SetInvalidatorTracksCalls — a connected invalidator
// fires on every invalidate; SetInvalidator(nil) reverts to no-op.
func TestService_Invalidate_SetInvalidatorTracksCalls(t *testing.T) {
	s, err := NewService(ServiceDeps{Pool: &hookPool{}})
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

	s.SetInvalidator(nil)
	s.invalidate(context.Background())
	if got := inv.calls.Load(); got != 2 {
		t.Fatalf("after SetInvalidator(nil): calls = %d, want 2", got)
	}
}

// TestService_Invalidate_OnlyAfterSuccessfulCommit — invalidate is called exactly
// on successful mutations (Create/Update/SetSetting → ok, Delete → ok) and is NOT
// called when the mutation fails (Delete on a nonexistent name).
func TestService_Invalidate_OnlyAfterSuccessfulCommit(t *testing.T) {
	t.Run("create-success-invalidates", func(t *testing.T) {
		inv := &countingInvalidator{}
		s := mustService(t, &hookPool{queryRowErr: nil})
		s.SetInvalidator(inv)
		if _, err := s.CreateService(context.Background(), CreateServiceInput{
			Name: "web", Git: "git@x:web.git", Ref: "main",
		}); err != nil {
			t.Fatalf("CreateService: %v", err)
		}
		if got := inv.calls.Load(); got != 1 {
			t.Fatalf("invalidate calls = %d after successful create, want 1", got)
		}
	})

	t.Run("create-failure-no-invalidate", func(t *testing.T) {
		inv := &countingInvalidator{}
		// UNIQUE-violation on Insert → CreateService returns an error, the hook stays silent.
		pgErr := &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "service_registry_pkey"}
		s := mustService(t, &hookPool{queryRowErr: pgErr})
		s.SetInvalidator(inv)
		if _, err := s.CreateService(context.Background(), CreateServiceInput{
			Name: "web", Git: "git@x:web.git", Ref: "main",
		}); !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("CreateService = %v, want ErrAlreadyExists", err)
		}
		if got := inv.calls.Load(); got != 0 {
			t.Fatalf("invalidate calls = %d after failed create, want 0", got)
		}
	})

	t.Run("update-success-invalidates", func(t *testing.T) {
		inv := &countingInvalidator{}
		s := mustService(t, &hookPool{queryRowErr: nil})
		s.SetInvalidator(inv)
		if _, err := s.UpdateService(context.Background(), UpdateServiceInput{
			Name: "web", Git: "git@x:web.git", Ref: "v2",
		}); err != nil {
			t.Fatalf("UpdateService: %v", err)
		}
		if got := inv.calls.Load(); got != 1 {
			t.Fatalf("invalidate calls = %d after successful update, want 1", got)
		}
	})

	t.Run("setsetting-success-invalidates", func(t *testing.T) {
		inv := &countingInvalidator{}
		s := mustService(t, &hookPool{queryRowErr: nil})
		s.SetInvalidator(inv)
		if _, err := s.SetSetting(context.Background(), SetSettingInput{
			Key: SettingDefaultDestinySource, Value: "git@x:destiny.git",
		}); err != nil {
			t.Fatalf("SetSetting: %v", err)
		}
		if got := inv.calls.Load(); got != 1 {
			t.Fatalf("invalidate calls = %d after successful set-setting, want 1", got)
		}
	})

	t.Run("delete-success-invalidates", func(t *testing.T) {
		inv := &countingInvalidator{}
		// CommandTag with RowsAffected>0 → DeleteService succeeds.
		s := mustService(t, &hookPool{execTag: pgconn.NewCommandTag("DELETE 1")})
		s.SetInvalidator(inv)
		if err := s.DeleteService(context.Background(), "web"); err != nil {
			t.Fatalf("DeleteService: %v", err)
		}
		if got := inv.calls.Load(); got != 1 {
			t.Fatalf("invalidate calls = %d after successful delete, want 1", got)
		}
	})

	t.Run("delete-notfound-no-invalidate", func(t *testing.T) {
		inv := &countingInvalidator{}
		// CommandTag with RowsAffected==0 → DeleteService → ErrNotFound, the hook stays silent.
		s := mustService(t, &hookPool{execTag: pgconn.CommandTag{}})
		s.SetInvalidator(inv)
		if err := s.DeleteService(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("DeleteService = %v, want ErrNotFound", err)
		}
		if got := inv.calls.Load(); got != 0 {
			t.Fatalf("invalidate calls = %d after not-found delete, want 0", got)
		}
	})
}

func mustService(t *testing.T, pool ServicePool) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}
