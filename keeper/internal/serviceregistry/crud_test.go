package serviceregistry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB — ExecQueryRower-stub для unit-тестов без подъёма PG. Возвращает
// заданную ошибку из QueryRow.Scan / Exec, чтобы проверить error-mapping.
type fakeDB struct {
	queryRowErr error
	execTag     pgconn.CommandTag
	execErr     error
}

func (f *fakeDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return f.execTag, f.execErr
}

func (f *fakeDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return errRow{err: f.queryRowErr}
}

func (f *fakeDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeDB: Query not configured")
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error {
	if r.err == nil {
		return pgx.ErrNoRows
	}
	return r.err
}

func ptr[T any](v T) *T { return &v }

// TestValidateFields — прикладная валидация полей Service-записи.
func TestValidateFields(t *testing.T) {
	cases := []struct {
		name    string
		svcName string
		git     string
		ref     string
		refresh *string
		wantErr error
	}{
		{"ok-no-refresh", "web", "git@x:web.git", "v1.0.0", nil, nil},
		{"ok-refresh", "web", "git@x:web.git", "main", ptr("5m"), nil},
		{"ok-refresh-days", "web", "git@x:web.git", "main", ptr("30d"), nil},
		{"bad-name-upper", "Web", "g", "r", nil, ErrInvalidName},
		{"bad-name-underscore", "web_svc", "g", "r", nil, ErrInvalidName},
		{"bad-name-leading-digit", "1web", "g", "r", nil, ErrInvalidName},
		{"empty-git", "web", "", "r", nil, ErrInvalidGit},
		{"empty-ref", "web", "g", "", nil, ErrInvalidRef},
		{"bad-refresh", "web", "g", "r", ptr("notaduration"), ErrInvalidRefresh},
		{"bad-refresh-composite", "web", "g", "r", ptr("1d2h"), ErrInvalidRefresh},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateFields(c.svcName, c.git, c.ref, c.refresh)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("validateFields = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("validateFields = %v, want errors.Is %v", err, c.wantErr)
			}
		})
	}
}

// TestValidSettingKey — формат ключа keeper_settings (snake_case).
func TestValidSettingKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"default_destiny_source", true},
		{"x", true},
		{"a1_b2", true},
		{"Default", false},
		{"with-dash", false},
		{"1lead", false},
		{"", false},
	}
	for _, c := range cases {
		if got := ValidSettingKey(c.key); got != c.want {
			t.Errorf("ValidSettingKey(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

// TestMapServiceWriteError — маппинг pgx-ошибок write-пути в sentinel-ы.
func TestMapServiceWriteError(t *testing.T) {
	t.Run("unique→ErrAlreadyExists", func(t *testing.T) {
		pgErr := &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "service_registry_pkey"}
		got := mapServiceWriteError(pgErr)
		if !errors.Is(got, ErrAlreadyExists) {
			t.Fatalf("err = %v, want errors.Is ErrAlreadyExists", got)
		}
		if !errors.Is(got, pgErr) {
			t.Errorf("original PgError lost in wrap: %v", got)
		}
	})

	t.Run("fk→ErrOperatorNotFound", func(t *testing.T) {
		pgErr := &pgconn.PgError{Code: pgErrCodeForeignKeyViolation, ConstraintName: "service_registry_created_by_fk"}
		got := mapServiceWriteError(pgErr)
		if !errors.Is(got, ErrOperatorNotFound) {
			t.Fatalf("err = %v, want errors.Is ErrOperatorNotFound", got)
		}
		if want := "service_registry_created_by_fk"; !strings.Contains(got.Error(), want) {
			t.Errorf("err = %q, want substring %q", got.Error(), want)
		}
	})

	t.Run("check→generic wrap", func(t *testing.T) {
		pgErr := &pgconn.PgError{Code: pgErrCodeCheckViolation, ConstraintName: "service_registry_git_nonempty"}
		got := mapServiceWriteError(pgErr)
		if errors.Is(got, ErrAlreadyExists) || errors.Is(got, ErrOperatorNotFound) {
			t.Errorf("CHECK violation wrongly mapped to sentinel: %v", got)
		}
		if want := "CHECK violation"; !strings.Contains(got.Error(), want) {
			t.Errorf("err = %q, want substring %q", got.Error(), want)
		}
	})

	t.Run("other→generic wrap", func(t *testing.T) {
		base := errors.New("connection reset")
		got := mapServiceWriteError(base)
		if errors.Is(got, ErrAlreadyExists) || errors.Is(got, ErrOperatorNotFound) {
			t.Errorf("generic error wrongly mapped to sentinel: %v", got)
		}
		if !errors.Is(got, base) {
			t.Errorf("base error lost in wrap: %v", got)
		}
	})
}

// TestMapSettingWriteError — FK-violation upsert-а настройки → ErrOperatorNotFound.
func TestMapSettingWriteError(t *testing.T) {
	pgErr := &pgconn.PgError{Code: pgErrCodeForeignKeyViolation, ConstraintName: "keeper_settings_updated_by_aid_fkey"}
	got := mapSettingWriteError(pgErr)
	if !errors.Is(got, ErrOperatorNotFound) {
		t.Fatalf("err = %v, want errors.Is ErrOperatorNotFound", got)
	}
}

// TestService_CreateValidationBeforeDB — битый ввод ловится валидацией ДО
// обращения к БД. fakeDB.QueryRow вернул бы ErrNoRows на любой реальный вызов;
// проверяем, что до него дело не доходит (возвращается validation-sentinel).
func TestService_CreateValidationBeforeDB(t *testing.T) {
	svc, err := NewService(ServiceDeps{Pool: &fakeDB{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, gotErr := svc.CreateService(t.Context(), CreateServiceInput{
		Name: "Bad_Name", Git: "g", Ref: "r",
	})
	if !errors.Is(gotErr, ErrInvalidName) {
		t.Fatalf("CreateService = %v, want ErrInvalidName", gotErr)
	}
}

// TestService_GetSettingValidatesKey — GetSetting/SetSetting отбивают битый ключ
// до round-trip-а.
func TestService_GetSettingValidatesKey(t *testing.T) {
	svc, err := NewService(ServiceDeps{Pool: &fakeDB{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.GetSetting(t.Context(), "Bad-Key"); !errors.Is(err, ErrInvalidSettingKey) {
		t.Fatalf("GetSetting = %v, want ErrInvalidSettingKey", err)
	}
	if _, err := svc.SetSetting(t.Context(), SetSettingInput{Key: "Bad-Key", Value: "v"}); !errors.Is(err, ErrInvalidSettingKey) {
		t.Fatalf("SetSetting = %v, want ErrInvalidSettingKey", err)
	}
}

// TestNewService_NilPool — конструктор отвергает nil-pool.
func TestNewService_NilPool(t *testing.T) {
	if _, err := NewService(ServiceDeps{}); err == nil {
		t.Fatal("NewService(nil pool) = nil error, want error")
	}
}
