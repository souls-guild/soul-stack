package herald

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// --- fake ExecQueryRower (unit, без PG) -------------------------------

type fakeDB struct {
	execTag pgconn.CommandTag
	execErr error

	rowErr error // ошибка, отдаваемая QueryRow.Scan
}

func (f *fakeDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return f.execTag, f.execErr
}

func (f *fakeDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return errRow{err: f.rowErr}
}

func (f *fakeDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("herald_test: Query not stubbed")
}

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error {
	if r.err == nil {
		return pgx.ErrNoRows
	}
	return r.err
}

func cmdTag(s string) pgconn.CommandTag { return pgconn.NewCommandTag(s) }

func strptr(s string) *string { return &s }

// --- ValidName --------------------------------------------------------

func TestValidName(t *testing.T) {
	cases := map[string]bool{
		"ops-webhook": true,
		"a":           true,
		"slack-1":     true,
		"":            false,
		"Bad-Case":    false,
		"_under":      false,
		"trailing-":   true, // дефис разрешён где угодно (как omens)
	}
	for name, want := range cases {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

// --- ValidHeraldType --------------------------------------------------

func TestValidHeraldType(t *testing.T) {
	if !ValidHeraldType(HeraldWebhook) {
		t.Error("webhook must be valid")
	}
	if ValidHeraldType(HeraldType("slack")) {
		t.Error("slack must be invalid in MVP")
	}
}

// --- ValidateConfig (webhook + SSRF-контур) ---------------------------

func TestValidateConfig_Webhook(t *testing.T) {
	cases := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{"https ok", map[string]any{"url": "https://hooks.example.com/x"}, false},
		{"missing url", map[string]any{}, true},
		{"empty url", map[string]any{"url": ""}, true},
		{"url not string", map[string]any{"url": 42}, true},
		{"http denied by default", map[string]any{"url": "http://hooks.example.com/x"}, true},
		{"http allowed opt-out", map[string]any{"url": "http://hooks.example.com/x", "http_allowed": true}, false},
		{"literal private ip denied", map[string]any{"url": "https://10.0.0.5/x"}, true},
		{"literal private ip allowed opt-out", map[string]any{"url": "https://10.0.0.5/x", "allow_private": true}, false},
		{"loopback denied", map[string]any{"url": "https://127.0.0.1/x"}, true},
		{"http_allowed not bool ignored", map[string]any{"url": "http://hooks.example.com/x", "http_allowed": "yes"}, true},
		{"unsupported scheme even with opt-out", map[string]any{"url": "ftp://x/y", "http_allowed": true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfig(HeraldWebhook, tc.config)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateConfig(%v) err = %v, wantErr = %v", tc.config, err, tc.wantErr)
			}
		})
	}
}

func TestValidateConfig_UnknownType(t *testing.T) {
	if err := ValidateConfig(HeraldType("slack"), map[string]any{"url": "https://x/y"}); err == nil {
		t.Error("unknown type must error")
	}
}

// --- ValidateSecretRef ------------------------------------------------

func TestValidateSecretRef(t *testing.T) {
	if err := ValidateSecretRef(nil); err != nil {
		t.Errorf("nil secret_ref must be ok (optional), got %v", err)
	}
	if err := ValidateSecretRef(strptr("vault:secret/keeper/herald/sign")); err != nil {
		t.Errorf("valid vault-ref err = %v", err)
	}
	if err := ValidateSecretRef(strptr("plain-token")); err == nil {
		t.Error("non-vault-ref must error")
	}
	if err := ValidateSecretRef(strptr("vault:secret")); err == nil {
		t.Error("vault-ref without <mount>/<path> must error")
	}
}

// --- ValidateEventTypes -----------------------------------------------

func TestValidateEventTypes(t *testing.T) {
	cases := []struct {
		name    string
		ets     []string
		wantErr bool
	}{
		{"empty list", nil, true},
		{"area glob scenario_run", []string{"scenario_run.*"}, false},
		{"area glob command_run", []string{"command_run.*"}, false},
		{"area glob voyage", []string{"voyage.*"}, false},
		{"area glob cadence", []string{"cadence.*"}, false},
		{"exact in scope", []string{"scenario_run.completed"}, false},
		{"point drift allowed", []string{"incarnation.drift_checked"}, false},
		{"point run_completed allowed", []string{"incarnation.run_completed"}, false},
		{"mixed valid", []string{"scenario_run.*", "command_run.failed", "incarnation.drift_checked", "incarnation.run_completed"}, false},
		{"bare wildcard", []string{"*"}, true},
		{"leading wildcard", []string{"*.created"}, true},
		{"unknown area glob", []string{"role.*"}, true},
		{"exact out of scope", []string{"role.created"}, true},
		{"incarnation glob not whole-scope", []string{"incarnation.*"}, true},
		{"incarnation point not drift", []string{"incarnation.created"}, true},
		{"no dot", []string{"scenariorun"}, true},
		{"empty element", []string{""}, true},
		{"mid wildcard", []string{"scenario_run.*x"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEventTypes(tc.ets)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateEventTypes(%v) err = %v, wantErr = %v", tc.ets, err, tc.wantErr)
			}
		})
	}
}

// TestPointEventsListInError — текст ошибки out-of-scope перечисляет ВСЕ
// разрешённые точечные типы из runScopePointEvents (map-driven, отсортированный),
// а не статичную строку. Guard против стейл-message: при добавлении point-типа
// (как incarnation.run_completed) перечень в ошибке обязан обновляться сам.
func TestPointEventsListInError(t *testing.T) {
	err := validateEventType("role.created") // out-of-scope → ошибка с перечнем point-типов
	if err == nil {
		t.Fatal("validateEventType(role.created) = nil, want out-of-scope error")
	}
	for et := range runScopePointEvents {
		if !strings.Contains(err.Error(), et) {
			t.Errorf("out-of-scope error %q does not list point type %q (стейл-message)", err.Error(), et)
		}
	}
}

// TestRunScopeGetters — экспортируемые геттеры каталога (`GET /v1/event-types`)
// отдают РОВНО внутренние scope-множества (единый источник правды): каталог-
// эндпоинт следует за scope без собственного списка. Также: оба геттера
// детерминированно отсортированы (стабильность каталога/UI/тестов).
func TestRunScopeGetters(t *testing.T) {
	gotAreas := RunScopeAreas()
	if len(gotAreas) != len(runScopeAreas) {
		t.Fatalf("RunScopeAreas() len=%d, runScopeAreas len=%d", len(gotAreas), len(runScopeAreas))
	}
	for _, a := range gotAreas {
		if _, ok := runScopeAreas[a]; !ok {
			t.Errorf("RunScopeAreas() вернул %q вне runScopeAreas", a)
		}
	}
	assertStringsSorted(t, "RunScopeAreas", gotAreas)

	gotPoints := RunScopePointEvents()
	if len(gotPoints) != len(runScopePointEvents) {
		t.Fatalf("RunScopePointEvents() len=%d, runScopePointEvents len=%d", len(gotPoints), len(runScopePointEvents))
	}
	for _, p := range gotPoints {
		if _, ok := runScopePointEvents[p]; !ok {
			t.Errorf("RunScopePointEvents() вернул %q вне runScopePointEvents", p)
		}
	}
	assertStringsSorted(t, "RunScopePointEvents", gotPoints)
}

func assertStringsSorted(t *testing.T, label string, xs []string) {
	t.Helper()
	for i := 1; i < len(xs); i++ {
		if xs[i-1] >= xs[i] {
			t.Errorf("%s не отсортирован или дубль: %q >= %q", label, xs[i-1], xs[i])
		}
	}
}

// --- ValidateProjection (ADR-052(h) N1) -------------------------------

func TestValidateProjection(t *testing.T) {
	cases := []struct {
		name    string
		paths   []string
		wantErr bool
	}{
		{"empty list ok (full form)", nil, false},
		{"single segment", []string{"event_type"}, false},
		{"dotted path", []string{"summary.succeeded"}, false},
		{"with underscores/digits", []string{"a_1.b2_c"}, false},
		{"multiple paths", []string{"event_type", "payload.summary.total"}, false},
		{"empty path", []string{""}, true},
		{"leading dot", []string{".x"}, true},
		{"trailing dot", []string{"x."}, true},
		{"double dot", []string{"a..b"}, true},
		{"literal dotdot", []string{".."}, true},
		{"uppercase segment", []string{"Summary"}, true},
		{"dash in segment", []string{"a-b"}, true},
		{"one bad among good", []string{"ok.path", "BAD"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateProjection(tc.paths); (err != nil) != tc.wantErr {
				t.Errorf("ValidateProjection(%v) err = %v, wantErr = %v", tc.paths, err, tc.wantErr)
			}
		})
	}
}

// --- ValidateAnnotationsJSON (ADR-052(h)/(i) N1) ----------------------

func TestValidateAnnotationsJSON(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"empty ok (no static fields)", "", false},
		{"null ok", "null", false},
		{"object ok", `{"team":"ops","severity":"high"}`, false},
		{"empty object ok", `{}`, false},
		{"nested object ok", `{"a":{"b":1}}`, false},
		{"array rejected", `["a","b"]`, true},
		{"string scalar rejected", `"plain"`, true},
		{"number scalar rejected", `42`, true},
		{"bool scalar rejected", `true`, true},
		{"invalid json rejected", `{bad`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAnnotationsJSON([]byte(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateAnnotationsJSON(%q) err = %v, wantErr = %v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

// TestValidateTiding_EphemeralVoyageInvariant — инвариант ephemeral⟺voyage_id
// (ADR-052(g)) на domain-уровне через InsertTiding (DB не задействуется —
// нарушение режется до похода в БД).
func TestValidateTiding_EphemeralVoyageInvariant(t *testing.T) {
	db := &fakeDB{rowErr: errors.New("db must not be hit on validation reject")}
	cases := []struct {
		name      string
		ephemeral bool
		voyageID  *string
		wantErr   bool
	}{
		{"persistent without voyage ok", false, nil, false},
		{"persistent with empty voyage ok", false, strptr(""), false},
		{"ephemeral with voyage ok", true, strptr("vy_1"), false},
		{"ephemeral without voyage rejected", true, nil, true},
		{"ephemeral with empty voyage rejected", true, strptr(""), true},
		{"persistent with voyage rejected", false, strptr("vy_1"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := &Tiding{
				Name:       "t1",
				Herald:     "ops",
				EventTypes: []string{"scenario_run.*"},
				Ephemeral:  tc.ephemeral,
				VoyageID:   tc.voyageID,
			}
			err := InsertTiding(context.Background(), db, tg)
			if tc.wantErr {
				if !errors.Is(err, ErrEphemeralRequiresVoyage) {
					t.Errorf("err = %v, want ErrEphemeralRequiresVoyage", err)
				}
				if !IsValidationError(err) {
					t.Errorf("err = %v, want IsValidationError true (→422)", err)
				}
			} else if err != nil {
				// Валидный вход не должен зарезаться валидацией — но db-stub вернёт
				// rowErr на Scan. Главное: ошибка НЕ про ephemeral-инвариант.
				if errors.Is(err, ErrEphemeralRequiresVoyage) {
					t.Errorf("valid input rejected by ephemeral invariant: %v", err)
				}
			}
		})
	}
}

// TestValidateTiding_NormalizesEmptyVoyageToNil — пустая строка VoyageID
// нормализуется к nil в validateTiding, чтобы SQL-arg (optStrArg) писал NULL, а
// не пустую строку (иначе non-ephemeral+&"" падал бы на CHECK 500-ой).
func TestValidateTiding_NormalizesEmptyVoyageToNil(t *testing.T) {
	tg := &Tiding{
		Name:       "t1",
		Herald:     "ops",
		EventTypes: []string{"scenario_run.*"},
		Ephemeral:  false,
		VoyageID:   strptr(""),
	}
	if err := validateTiding(tg); err != nil {
		t.Fatalf("non-ephemeral with empty voyage rejected: %v", err)
	}
	if tg.VoyageID != nil {
		t.Errorf("VoyageID = %v, want nil (empty string normalized → optStrArg writes NULL)", tg.VoyageID)
	}
}

// TestValidateTiding_NormalizesEmptyTaskToNil — пустая строка task-селектора
// нормализуется к nil в validateTiding (ADR-052 §l): nil = «без фильтра», а
// пустой адрес changed_tasks им матчиться не должен. Без нормализации optStrArg
// записал бы `”` — мёртвый селектор.
func TestValidateTiding_NormalizesEmptyTaskToNil(t *testing.T) {
	tg := &Tiding{
		Name:       "t1",
		Herald:     "ops",
		EventTypes: []string{"incarnation.run_completed"},
		Task:       strptr(""),
	}
	if err := validateTiding(tg); err != nil {
		t.Fatalf("validateTiding rejected empty task: %v", err)
	}
	if tg.Task != nil {
		t.Errorf("Task = %v, want nil (empty string normalized → optStrArg writes NULL)", tg.Task)
	}
}

// TestInsertTiding_RejectsBadProjection — битый projection-путь режется до БД.
func TestInsertTiding_RejectsBadProjection(t *testing.T) {
	db := &fakeDB{rowErr: errors.New("db must not be hit")}
	tg := &Tiding{
		Name:       "t1",
		Herald:     "ops",
		EventTypes: []string{"scenario_run.*"},
		Projection: []string{"summary..succeeded"},
	}
	if err := InsertTiding(context.Background(), db, tg); !IsValidationError(err) {
		t.Errorf("err = %v, want validation error on bad projection", err)
	}
}

// --- Insert pre-condition rejections (no DB hit) ----------------------

func TestInsertHerald_RejectsBeforeDB(t *testing.T) {
	cases := []struct {
		name string
		h    *Herald
	}{
		{"nil", nil},
		{"bad name", &Herald{Name: "Bad", Type: HeraldWebhook, Config: map[string]any{"url": "https://x/y"}}},
		{"bad type", &Herald{Name: "ok", Type: HeraldType("slack"), Config: map[string]any{"url": "https://x/y"}}},
		{"bad config", &Herald{Name: "ok", Type: HeraldWebhook, Config: map[string]any{}}},
		{"bad secret_ref", &Herald{Name: "ok", Type: HeraldWebhook, Config: map[string]any{"url": "https://x/y"}, SecretRef: strptr("plain")}},
	}
	db := &fakeDB{rowErr: errors.New("db must not be hit")}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := InsertHerald(context.Background(), db, tc.h); err == nil {
				t.Error("expected validation error before DB")
			}
		})
	}
}

func TestInsertHerald_MapsUniqueViolation(t *testing.T) {
	db := &fakeDB{rowErr: &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "heralds_pkey"}}
	h := &Herald{Name: "ops", Type: HeraldWebhook, Config: map[string]any{"url": "https://x/y"}}
	if err := InsertHerald(context.Background(), db, h); !errors.Is(err, ErrHeraldExists) {
		t.Errorf("err = %v, want ErrHeraldExists", err)
	}
}

func TestInsertTiding_RejectsBeforeDB(t *testing.T) {
	cases := []struct {
		name string
		t    *Tiding
	}{
		{"nil", nil},
		{"bad name", &Tiding{Name: "Bad", Herald: "ops", EventTypes: []string{"scenario_run.*"}}},
		{"empty herald", &Tiding{Name: "ok", Herald: "", EventTypes: []string{"scenario_run.*"}}},
		{"bad event_types", &Tiding{Name: "ok", Herald: "ops", EventTypes: []string{"role.*"}}},
		{"empty event_types", &Tiding{Name: "ok", Herald: "ops", EventTypes: nil}},
	}
	db := &fakeDB{rowErr: errors.New("db must not be hit")}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := InsertTiding(context.Background(), db, tc.t); err == nil {
				t.Error("expected validation error before DB")
			}
		})
	}
}

func TestInsertTiding_MapsHeraldFKViolation(t *testing.T) {
	db := &fakeDB{rowErr: &pgconn.PgError{Code: pgErrCodeForeignKeyViolation, ConstraintName: "tidings_herald_fk"}}
	tg := &Tiding{Name: "t1", Herald: "ghost", EventTypes: []string{"scenario_run.*"}}
	if err := InsertTiding(context.Background(), db, tg); !errors.Is(err, ErrHeraldNotFound) {
		t.Errorf("err = %v, want ErrHeraldNotFound", err)
	}
}

func TestInsertTiding_MapsUniqueViolation(t *testing.T) {
	db := &fakeDB{rowErr: &pgconn.PgError{Code: pgErrCodeUniqueViolation, ConstraintName: "tidings_pkey"}}
	tg := &Tiding{Name: "t1", Herald: "ops", EventTypes: []string{"scenario_run.*"}}
	if err := InsertTiding(context.Background(), db, tg); !errors.Is(err, ErrTidingExists) {
		t.Errorf("err = %v, want ErrTidingExists", err)
	}
}

// --- Delete/Update RowsAffected==0 → NotFound -------------------------

func TestDeleteHerald_NotFound(t *testing.T) {
	db := &fakeDB{execTag: cmdTag("DELETE 0")}
	if err := DeleteHerald(context.Background(), db, "missing"); !errors.Is(err, ErrHeraldNotFound) {
		t.Errorf("err = %v, want ErrHeraldNotFound", err)
	}
}

func TestDeleteTiding_NotFound(t *testing.T) {
	db := &fakeDB{execTag: cmdTag("DELETE 0")}
	if err := DeleteTiding(context.Background(), db, "missing"); !errors.Is(err, ErrTidingNotFound) {
		t.Errorf("err = %v, want ErrTidingNotFound", err)
	}
}

func TestUpdateHerald_NotFound(t *testing.T) {
	db := &fakeDB{execTag: cmdTag("UPDATE 0")}
	h := &Herald{Name: "ops", Type: HeraldWebhook, Config: map[string]any{"url": "https://x/y"}}
	if err := UpdateHerald(context.Background(), db, h); !errors.Is(err, ErrHeraldNotFound) {
		t.Errorf("err = %v, want ErrHeraldNotFound", err)
	}
}

func TestUpdateTiding_NotFound(t *testing.T) {
	db := &fakeDB{execTag: cmdTag("UPDATE 0")}
	tg := &Tiding{Name: "t1", Herald: "ops", EventTypes: []string{"scenario_run.*"}}
	if err := UpdateTiding(context.Background(), db, tg); !errors.Is(err, ErrTidingNotFound) {
		t.Errorf("err = %v, want ErrTidingNotFound", err)
	}
}

func TestUpdateTiding_MapsHeraldFKViolation(t *testing.T) {
	db := &fakeDB{execErr: &pgconn.PgError{Code: pgErrCodeForeignKeyViolation, ConstraintName: "tidings_herald_fk"}}
	tg := &Tiding{Name: "t1", Herald: "ghost", EventTypes: []string{"scenario_run.*"}}
	if err := UpdateTiding(context.Background(), db, tg); !errors.Is(err, ErrHeraldNotFound) {
		t.Errorf("err = %v, want ErrHeraldNotFound", err)
	}
}
