package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	keepergrpc "github.com/souls-guild/soul-stack/keeper/internal/grpc"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// enqFakeDB — fake [oracleEnqueuerDB] с маршрутизацией по SQL:
//   - QueryRow "FROM incarnation"     → incarnation-строка (или ErrNoRows);
//   - QueryRow "INSERT INTO apply_runs" → started_at + захват аргументов.
type enqFakeDB struct {
	inc            *incarnation.Incarnation // nil → SelectByName даёт ErrNoRows
	insertArgs     []any                    // захваченные аргументы InsertPlanned
	insertedRecipe *applyrun.Recipe         // распарсенный recipe из аргументов
}

func (f *enqFakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "FROM incarnation"):
		if f.inc == nil {
			return enqErrRow{err: pgx.ErrNoRows}
		}
		return enqIncRow{inc: f.inc}
	case strings.Contains(sql, "INSERT INTO apply_runs"):
		f.insertArgs = args
		// Аргумент 6 (index 5) — recipeJSON ([]byte) по insertPlannedSQL.
		if len(args) >= 6 {
			if b, ok := args[5].([]byte); ok && len(b) > 0 {
				var r applyrun.Recipe
				_ = json.Unmarshal(b, &r)
				f.insertedRecipe = &r
			}
		}
		return enqStartedAtRow{}
	}
	return enqErrRow{err: pgx.ErrNoRows}
}

func (f *enqFakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *enqFakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}

type enqErrRow struct{ err error }

func (r enqErrRow) Scan(...any) error { return r.err }

// enqIncRow эмулирует строку incarnation в порядке scanIncarnation:
// name, service, service_version, state_schema_version, spec, state, status,
// status_details, created_by_aid, created_at, updated_at, covens, traits,
// last_drift_check_at, last_drift_summary, created_scenario.
type enqIncRow struct{ inc *incarnation.Incarnation }

func (r enqIncRow) Scan(dest ...any) error {
	if len(dest) != 16 {
		return errors.New("enqIncRow: len mismatch")
	}
	*dest[0].(*string) = r.inc.Name
	*dest[1].(*string) = r.inc.Service
	*dest[2].(*string) = r.inc.ServiceVersion
	*dest[3].(*int) = r.inc.StateSchemaVersion
	*dest[4].(*[]byte) = []byte("{}")
	*dest[5].(*[]byte) = []byte("{}")
	*dest[6].(*string) = string(r.inc.Status)
	*dest[7].(*[]byte) = nil
	*dest[8].(**string) = nil
	*dest[9].(*time.Time) = time.Now()
	*dest[10].(*time.Time) = time.Now()
	*dest[11].(*[]string) = r.inc.Covens
	*dest[12].(*[]byte) = []byte("{}") // traits (ADR-060 amend R1)
	*dest[13].(**time.Time) = nil
	*dest[14].(*[]byte) = nil
	*dest[15].(*string) = "create" // created_scenario (миграция 089, NOT NULL DEFAULT)
	return nil
}

// enqStartedAtRow возвращает started_at для InsertPlanned RETURNING.
type enqStartedAtRow struct{}

func (r enqStartedAtRow) Scan(dest ...any) error {
	if len(dest) == 1 {
		if t, ok := dest[0].(*time.Time); ok {
			*t = time.Now()
		}
	}
	return nil
}

// fakeResolver — fake [incarnation.ServiceResolver].
type fakeResolver struct {
	ref artifact.ServiceRef
	ok  bool
}

func (r fakeResolver) Resolve(string) (artifact.ServiceRef, bool) { return r.ref, r.ok }

func newEnqueuer(t *testing.T, db oracleEnqueuerDB, res incarnation.ServiceResolver) *oracleScenarioEnqueuer {
	t.Helper()
	return &oracleScenarioEnqueuer{
		db:       db,
		resolver: res,
		summons:  summonsPublisher{redis: nil}, // nil → Summons no-op
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestEnqueue_ResolvesServiceRefFromIncarnation(t *testing.T) {
	inc := &incarnation.Incarnation{
		Name: "web-app", Service: "web", ServiceVersion: "v2.0.0",
		Status: incarnation.StatusReady,
	}
	db := &enqFakeDB{inc: inc}
	// Резолвер вернёт git-координаты сервиса; ref ДОЛЖЕН быть переопределён
	// enqueuer-ом на inc.ServiceVersion (калька destroy_prepare.go:88).
	res := fakeResolver{ref: artifact.ServiceRef{Name: "web", Git: "https://git/web.git", Ref: "main"}, ok: true}
	e := newEnqueuer(t, db, res)

	applyID, err := e.EnqueueScenario(context.Background(), keepergrpc.EnqueueScenarioRequest{
		SubjectSID:      "host-a.example.com",
		IncarnationName: "web-app",
		ScenarioName:    "restart",
		ActionInput:     map[string]any{"service": "nginx"},
		DecreeName:      "restart-web",
	})
	if err != nil {
		t.Fatalf("EnqueueScenario: %v", err)
	}
	if applyID == "" {
		t.Fatal("ожидали non-empty apply_id (ULID)")
	}
	if db.insertedRecipe == nil {
		t.Fatal("InsertPlanned должен записать recipe")
	}
	// ServiceRef из incarnation: Git сервиса, но Ref = развёрнутая версия.
	if db.insertedRecipe.ServiceRef.Git != "https://git/web.git" {
		t.Errorf("recipe.ServiceRef.Git = %q, want git/web", db.insertedRecipe.ServiceRef.Git)
	}
	if db.insertedRecipe.ServiceRef.Ref != "v2.0.0" {
		t.Errorf("recipe.ServiceRef.Ref = %q, want v2.0.0 (inc.ServiceVersion override)", db.insertedRecipe.ServiceRef.Ref)
	}
	if db.insertedRecipe.ScenarioName != "restart" {
		t.Errorf("recipe.ScenarioName = %q, want restart", db.insertedRecipe.ScenarioName)
	}
	// Input — vault-ref КАК ЕСТЬ (инвариант A): проброшен дословно.
	if db.insertedRecipe.Input["service"] != "nginx" {
		t.Errorf("recipe.Input не пробросился: %+v", db.insertedRecipe.Input)
	}
	if db.insertedRecipe.StartedByAID != nil {
		t.Error("StartedByAID должен быть nil (Soul-инициированная реакция)")
	}
	// apply_runs row: planned-задание на subjectSID c корректной incarnation/scenario.
	// args порядок insertPlannedSQL: apply_id, sid, incarnation_name, scenario, started_by, recipe.
	if db.insertArgs[1] != "host-a.example.com" {
		t.Errorf("insert sid = %v, want host-a", db.insertArgs[1])
	}
	if db.insertArgs[2] != "web-app" {
		t.Errorf("insert incarnation_name = %v, want web-app", db.insertArgs[2])
	}
	if db.insertArgs[3] != "restart" {
		t.Errorf("insert scenario = %v, want restart", db.insertArgs[3])
	}
}

func TestEnqueue_IncarnationNotFound_FailClosed(t *testing.T) {
	db := &enqFakeDB{inc: nil} // SelectByName → ErrNoRows
	res := fakeResolver{ok: true}
	e := newEnqueuer(t, db, res)

	_, err := e.EnqueueScenario(context.Background(), keepergrpc.EnqueueScenarioRequest{
		SubjectSID:      "host-a.example.com",
		IncarnationName: "gone-app",
		ScenarioName:    "restart",
		DecreeName:      "restart-web",
	})
	if !errors.Is(err, ErrEnqueueIncarnationNotFound) {
		t.Fatalf("ожидали ErrEnqueueIncarnationNotFound, got %v", err)
	}
	// fail-closed: planned-задание НЕ записано.
	if db.insertArgs != nil {
		t.Error("incarnation not found: InsertPlanned НЕ должен вызываться")
	}
}

func TestEnqueue_ServiceNotRegistered(t *testing.T) {
	inc := &incarnation.Incarnation{Name: "web-app", Service: "web", ServiceVersion: "v1", Status: incarnation.StatusReady}
	db := &enqFakeDB{inc: inc}
	res := fakeResolver{ok: false} // сервис не в реестре
	e := newEnqueuer(t, db, res)

	_, err := e.EnqueueScenario(context.Background(), keepergrpc.EnqueueScenarioRequest{
		SubjectSID: "host-a.example.com", IncarnationName: "web-app", ScenarioName: "restart",
	})
	if err == nil {
		t.Fatal("ожидали ошибку при нерегистрированном сервисе")
	}
	if db.insertArgs != nil {
		t.Error("service not registered: InsertPlanned НЕ должен вызываться")
	}
}
