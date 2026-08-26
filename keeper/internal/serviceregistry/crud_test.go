package serviceregistry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeDB is an ExecQueryRower stub for unit tests without a live PG. Returns
// a configured error from QueryRow.Scan / Exec to exercise error-mapping.
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

// TestValidateFields — field validation of a Service record.
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
		// ★ NIM-706. Well-formed kebab-case, so ValidName passes — the refusal has to
		// come from the reserved rule standing on its own, and it must be a DISTINCT
		// sentinel from ErrInvalidName: "invalid service name" would send an operator
		// looking for a typo in a name that has none.
		{"reserved-keeper", "keeper", "g", "r", nil, ErrReservedName},
		{"reserved-herald", "herald", "g", "r", nil, ErrReservedName},
		{"reserved-provider", "provider", "g", "r", nil, ErrReservedName},
		{"reserved-internal", "internal", "g", "r", nil, ErrReservedName},
		// Whole-word: a neighbouring name is a different Vault namespace.
		{"ok-name-resembling-reserved", "heralds", "g", "r", nil, nil},
		{"ok-name-prefixed-reserved", "keeper-notes", "g", "r", nil, nil},
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

// TestValidSettingKey — keeper_settings key format (snake_case).
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

// TestMapServiceWriteError — mapping of pgx write-path errors to sentinels.
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

// TestMapSettingWriteError — FK-violation on setting upsert → ErrOperatorNotFound.
func TestMapSettingWriteError(t *testing.T) {
	pgErr := &pgconn.PgError{Code: pgErrCodeForeignKeyViolation, ConstraintName: "keeper_settings_updated_by_aid_fkey"}
	got := mapSettingWriteError(pgErr)
	if !errors.Is(got, ErrOperatorNotFound) {
		t.Fatalf("err = %v, want errors.Is ErrOperatorNotFound", got)
	}
}

// TestService_CreateValidationBeforeDB — bad input is caught by validation
// BEFORE touching the DB. fakeDB.QueryRow would return ErrNoRows on any real
// call; we verify it never gets called (a validation sentinel is returned instead).
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

// ★ NIM-706. The whole point of refusing at REGISTRATION is that the name never enters
// the cluster: once the row exists the name is the primary key and immutable, so every
// secret that service ever derives is already inside `secret/herald/…`. fakeDB.QueryRow
// would return ErrNoRows on any real call — reaching it at all would mean the check runs
// after the write rather than before it.
func TestService_CreateRefusesReservedName(t *testing.T) {
	svc, err := NewService(ServiceDeps{Pool: &fakeDB{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	for _, name := range []string{"keeper", "herald", "provider", "internal"} {
		_, gotErr := svc.CreateService(t.Context(), CreateServiceInput{Name: name, Git: "g", Ref: "r"})
		if !errors.Is(gotErr, ErrReservedName) {
			t.Fatalf("CreateService(%q) = %v, want ErrReservedName", name, gotErr)
		}
		if !strings.Contains(gotErr.Error(), name) {
			t.Errorf("CreateService(%q) error %q does not name the offending word — an operator cannot tell what to change", name, gotErr)
		}
	}
}

// The same rule on the update path. Name is immutable on update, so this guards the
// other half of the choke point rather than a second way in: a route that validated only
// on create would admit the name through any future path that reuses UpdateService.
func TestService_UpdateRefusesReservedName(t *testing.T) {
	svc, err := NewService(ServiceDeps{Pool: &fakeDB{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, gotErr := svc.UpdateService(t.Context(), UpdateServiceInput{Name: "herald", Git: "g", Ref: "r"})
	if !errors.Is(gotErr, ErrReservedName) {
		t.Fatalf("UpdateService = %v, want ErrReservedName", gotErr)
	}
}

// TestService_GetSettingValidatesKey — GetSetting/SetSetting reject a bad key
// before the round-trip.
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

// TestNewService_NilPool — the constructor rejects a nil pool.
func TestNewService_NilPool(t *testing.T) {
	if _, err := NewService(ServiceDeps{}); err == nil {
		t.Fatal("NewService(nil pool) = nil error, want error")
	}
}
