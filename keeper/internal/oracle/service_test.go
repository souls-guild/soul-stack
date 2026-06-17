package oracle

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func newTestService(t *testing.T, db ExecQueryRower) *Service {
	t.Helper()
	where, err := NewWhereEvaluator()
	if err != nil {
		t.Fatalf("NewWhereEvaluator: %v", err)
	}
	svc, err := NewService(ServiceDeps{Pool: db, Where: where})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestNewService_NilWhere(t *testing.T) {
	if _, err := NewService(ServiceDeps{Pool: &fakeDB{}}); err == nil {
		t.Error("NewService без Where должен падать")
	}
}

func TestService_CreateVigil_OK(t *testing.T) {
	svc := newTestService(t, &fakeDB{})
	v, err := svc.CreateVigil(context.Background(), CreateVigilInput{
		Name:     "web-conf",
		Coven:    []string{"web"},
		Interval: "30s",
		Check:    "core.beacon.file_changed",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateVigil: %v", err)
	}
	if v.Name != "web-conf" || v.CheckAddr != "core.beacon.file_changed" {
		t.Errorf("vigil = %+v", v)
	}
}

func TestService_CreateVigil_ValidationBeforeDB(t *testing.T) {
	// insertErr выставлен, но валидация должна отбить до round-trip-а — ошибка =
	// ErrValidation, не инфра.
	db := &fakeDB{insertErr: errors.New("должен быть недостижим")}
	svc := newTestService(t, db)
	cases := []struct {
		name string
		in   CreateVigilInput
	}{
		{"bad name", CreateVigilInput{Name: "BAD", Coven: []string{"web"}, Interval: "30s", Check: "core.beacon.file_changed"}},
		{"bad interval", CreateVigilInput{Name: "x", Coven: []string{"web"}, Interval: "nope", Check: "core.beacon.file_changed"}},
		{"unknown check", CreateVigilInput{Name: "x", Coven: []string{"web"}, Interval: "30s", Check: "core.beacon.bogus"}},
		{"subject both", CreateVigilInput{Name: "x", Coven: []string{"web"}, SID: strptr("h1"), Interval: "30s", Check: "core.beacon.file_changed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.CreateVigil(context.Background(), c.in)
			if !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
	if db.execSQL != "" {
		t.Errorf("Exec не должен вызываться при провале валидации, got %q", db.execSQL)
	}
}

func TestService_CreateVigil_Duplicate(t *testing.T) {
	db := &fakeDB{insertErr: &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "vigils_pkey"}}
	svc := newTestService(t, db)
	_, err := svc.CreateVigil(context.Background(), CreateVigilInput{
		Name: "web-conf", Coven: []string{"web"}, Interval: "30s", Check: "core.beacon.file_changed",
	})
	if !errors.Is(err, ErrVigilAlreadyExists) {
		t.Errorf("err = %v, want ErrVigilAlreadyExists", err)
	}
}

func TestService_CreateDecree_OK(t *testing.T) {
	svc := newTestService(t, &fakeDB{})
	where := "event.data.severity == \"critical\""
	d, err := svc.CreateDecree(context.Background(), CreateDecreeInput{
		Name:            "restart-on-down",
		OnBeacon:        "db-svc",
		WhereCEL:        &where,
		Coven:           []string{"db"},
		IncarnationName: "prod-db",
		ActionScenario:  "restart_service",
	})
	if err != nil {
		t.Fatalf("CreateDecree: %v", err)
	}
	if d.Name != "restart-on-down" {
		t.Errorf("decree = %+v", d)
	}
}

func TestService_CreateDecree_BadWhereCEL(t *testing.T) {
	db := &fakeDB{insertErr: errors.New("должен быть недостижим")}
	svc := newTestService(t, db)
	bad := "event.data.x =="
	_, err := svc.CreateDecree(context.Background(), CreateDecreeInput{
		Name:            "x",
		OnBeacon:        "db-svc",
		WhereCEL:        &bad,
		Coven:           []string{"db"},
		IncarnationName: "prod-db",
		ActionScenario:  "restart_service",
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation (compile-check where-CEL)", err)
	}
	if db.execSQL != "" {
		t.Error("Exec не должен вызываться при битом where-CEL")
	}
}

func TestService_CreateDecree_ValidationBeforeDB(t *testing.T) {
	db := &fakeDB{insertErr: errors.New("должен быть недостижим")}
	svc := newTestService(t, db)
	cases := []struct {
		name string
		in   CreateDecreeInput
	}{
		{"bad incarnation", CreateDecreeInput{Name: "x", OnBeacon: "db-svc", Coven: []string{"db"}, IncarnationName: "BAD..NAME", ActionScenario: "restart_service"}},
		{"bad scenario", CreateDecreeInput{Name: "x", OnBeacon: "db-svc", Coven: []string{"db"}, IncarnationName: "prod-db", ActionScenario: "Bad-Scenario"}},
		{"subject neither", CreateDecreeInput{Name: "x", OnBeacon: "db-svc", IncarnationName: "prod-db", ActionScenario: "restart_service"}},
		{"bad cooldown", CreateDecreeInput{Name: "x", OnBeacon: "db-svc", Coven: []string{"db"}, IncarnationName: "prod-db", ActionScenario: "restart_service", Cooldown: "nope"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.CreateDecree(context.Background(), c.in)
			if !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}
