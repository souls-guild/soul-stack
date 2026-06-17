package rbac

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// nopPool — заглушка [ServicePool] для unit-тестов invalidate-хука: NewService
// требует non-nil Pool, но эти тесты не дёргают БД (только invalidate/
// SetInvalidator). Любой реальный вызов → ошибка (тест бы упал, если бы случайно
// дошёл до БД-пути).
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

// countingInvalidator — тестовый [Invalidator], считающий вызовы Invalidate.
type countingInvalidator struct {
	calls atomic.Int64
}

func (c *countingInvalidator) Invalidate(_ context.Context) { c.calls.Add(1) }

// TestService_Invalidate_NilSafe — без подключённого invalidator-а
// Service.invalidate — no-op (single-Keeper/dev без Redis), не паникует.
func TestService_Invalidate_NilSafe(t *testing.T) {
	s, err := NewService(ServiceDeps{Pool: nopPool{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// invalidate без SetInvalidator — тихо ничего не делает.
	s.invalidate(context.Background())

	// SetInvalidator(nil) тоже не должен ломать последующий invalidate.
	s.SetInvalidator(nil)
	s.invalidate(context.Background())
}

// TestService_Invalidate_CallsInvalidator — подключённый invalidator
// дёргается на каждый invalidate (хук, который 5 мутаций зовут после commit-а).
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

	// Снятие invalidator-а возвращает к no-op (счётчик не растёт).
	s.SetInvalidator(nil)
	s.invalidate(context.Background())
	if got := inv.calls.Load(); got != 2 {
		t.Fatalf("after SetInvalidator(nil): calls = %d, want 2", got)
	}
}
