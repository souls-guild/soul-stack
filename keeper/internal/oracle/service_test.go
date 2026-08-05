package oracle

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/subject"
)

// covenSel — the coven spelling of a subject, by far the most used in this file.
func covenSel(c ...string) subject.Selector { return subject.Selector{Covens: c} }

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
		t.Error("NewService without Where must fail")
	}
}

func TestService_CreateVigil_OK(t *testing.T) {
	svc := newTestService(t, &fakeDB{})
	v, err := svc.CreateVigil(context.Background(), CreateVigilInput{
		Name:     "web-conf",
		Subject:  covenSel("web"),
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
	// insertErr is set, but validation must reject before the round-trip —
	// the error is ErrValidation, not infra.
	db := &fakeDB{insertErr: errors.New("must be unreachable")}
	svc := newTestService(t, db)
	cases := []struct {
		name string
		in   CreateVigilInput
	}{
		{"bad name", CreateVigilInput{Name: "BAD", Subject: covenSel("web"), Interval: "30s", Check: "core.beacon.file_changed"}},
		{"bad interval", CreateVigilInput{Name: "x", Subject: covenSel("web"), Interval: "nope", Check: "core.beacon.file_changed"}},
		{"unknown check", CreateVigilInput{Name: "x", Subject: covenSel("web"), Interval: "30s", Check: "core.beacon.bogus"}},
		{"subject two dimensions", CreateVigilInput{Name: "x", Subject: subject.Selector{Covens: []string{"web"}, SIDs: []string{"h1"}}, Interval: "30s", Check: "core.beacon.file_changed"}},
		{"subject none", CreateVigilInput{Name: "x", Interval: "30s", Check: "core.beacon.file_changed"}},
		{"subject half-written incarnation", CreateVigilInput{Name: "x", Subject: subject.Selector{Incarnation: "redis-prod"}, Interval: "30s", Check: "core.beacon.file_changed"}},
		{"subject half-written trait", CreateVigilInput{Name: "x", Subject: subject.Selector{TraitKey: "tier"}, Interval: "30s", Check: "core.beacon.file_changed"}},
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
		t.Errorf("Exec must not be called on validation failure, got %q", db.execSQL)
	}
}

func TestService_CreateVigil_Duplicate(t *testing.T) {
	db := &fakeDB{insertErr: &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "vigils_pkey"}}
	svc := newTestService(t, db)
	_, err := svc.CreateVigil(context.Background(), CreateVigilInput{
		Name: "web-conf", Subject: covenSel("web"), Interval: "30s", Check: "core.beacon.file_changed",
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
		Subject:         covenSel("db"),
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
	db := &fakeDB{insertErr: errors.New("must be unreachable")}
	svc := newTestService(t, db)
	bad := "event.data.x =="
	_, err := svc.CreateDecree(context.Background(), CreateDecreeInput{
		Name:            "x",
		OnBeacon:        "db-svc",
		WhereCEL:        &bad,
		Subject:         covenSel("db"),
		IncarnationName: "prod-db",
		ActionScenario:  "restart_service",
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation (compile-check where-CEL)", err)
	}
	if db.execSQL != "" {
		t.Error("Exec must not be called with broken where-CEL")
	}
}

func TestService_CreateDecree_ValidationBeforeDB(t *testing.T) {
	db := &fakeDB{insertErr: errors.New("must be unreachable")}
	svc := newTestService(t, db)
	cases := []struct {
		name string
		in   CreateDecreeInput
	}{
		{"bad incarnation", CreateDecreeInput{Name: "x", OnBeacon: "db-svc", Subject: covenSel("db"), IncarnationName: "BAD..NAME", ActionScenario: "restart_service"}},
		{"bad scenario", CreateDecreeInput{Name: "x", OnBeacon: "db-svc", Subject: covenSel("db"), IncarnationName: "prod-db", ActionScenario: "Bad-Scenario"}},
		{"subject neither", CreateDecreeInput{Name: "x", OnBeacon: "db-svc", IncarnationName: "prod-db", ActionScenario: "restart_service"}},
		{"bad cooldown", CreateDecreeInput{Name: "x", OnBeacon: "db-svc", Subject: covenSel("db"), IncarnationName: "prod-db", ActionScenario: "restart_service", Cooldown: "nope"}},
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
