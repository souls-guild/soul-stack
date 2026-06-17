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

// hookPool — управляемый ExecQueryRower для тестов invalidate-хука: QueryRow
// отдаёт успешный/ошибочный row (Insert/Update/SetSetting), Exec — заданный tag
// + ошибку (Delete). Так проверяется, что invalidate зовётся ТОЛЬКО после
// успешного commit-а конкретной мутации.
type hookPool struct {
	queryRowErr error // nil → row.Scan успешен (Insert/Update/SetSetting прошли)
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

// scanRow — на err==nil успешно заполняет все переданные *time.Time текущим
// временем (RETURNING created_at/updated_at), иначе возвращает err.
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

// countingInvalidator — тестовый [Invalidator], считающий вызовы Invalidate.
type countingInvalidator struct {
	calls atomic.Int64
}

func (c *countingInvalidator) Invalidate(_ context.Context) { c.calls.Add(1) }

// TestService_Invalidate_NilSafe — без подключённого invalidator-а
// Service.invalidate — no-op (single-Keeper/dev без Redis), не паникует.
func TestService_Invalidate_NilSafe(t *testing.T) {
	s, err := NewService(ServiceDeps{Pool: &hookPool{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s.invalidate(context.Background())

	s.SetInvalidator(nil)
	s.invalidate(context.Background())
}

// TestService_Invalidate_SetInvalidatorTracksCalls — подключённый invalidator
// дёргается на каждый invalidate; SetInvalidator(nil) возвращает к no-op.
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

// TestService_Invalidate_OnlyAfterSuccessfulCommit — invalidate зовётся ровно
// на успешных мутациях (Create/Update/SetSetting → ok, Delete → ok) и НЕ
// зовётся, когда мутация провалилась (Delete по отсутствующему name).
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
		// UNIQUE-violation на Insert → CreateService возвращает ошибку, хук молчит.
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
		// CommandTag c RowsAffected>0 → DeleteService успешен.
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
		// CommandTag c RowsAffected==0 → DeleteService → ErrNotFound, хук молчит.
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
