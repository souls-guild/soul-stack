package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

// svcFakePool — узкий мок [serviceregistry.ServicePool] для unit-тестов
// ServiceHandler-а. Классифицирует SQL по подстроке и отдаёт заданный тестом
// исход. Покрывает ДОМЕННЫЙ слой (*Typed: маппинг sentinel→problem / статусы /
// audit-payload); консистентность SQL-логики Service-а валидируют
// serviceregistry/integration_test.go (testcontainers PG). bind/decode/400-кейсы
// покрывает huma-integration (handler-native T5d).
type svcFakePool struct {
	// insertErr — ошибка RETURNING-scan-а INSERT (Register): pgErr unique → 409,
	// pgErr FK → 404; nil → 201.
	insertErr error
	// updateErr — ошибка RETURNING-scan-а UPDATE (Update): pgx.ErrNoRows → 404.
	updateErr error
	// deleteRows — RowsAffected DELETE (Deregister): 0 → ErrNotFound (404).
	deleteRows int64
	// getRow — исход SELECT … WHERE name (Get): nil → ErrNotFound; иначе строка.
	getValues []any
	getErr    error
	// listValues — строки SELECT … ORDER BY name (List); каждая — []any на 8 колонок.
	listValues [][]any
}

func (p *svcFakePool) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if contains(sql, "DELETE FROM service_registry") {
		return pgconn.NewCommandTag("DELETE " + itoa(p.deleteRows)), nil
	}
	return pgconn.CommandTag{}, errSvcUnexpected(sql)
}

func (p *svcFakePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case contains(sql, "INSERT INTO service_registry"):
		if p.insertErr != nil {
			return errRow{err: p.insertErr}
		}
		now := time.Now()
		return staticRow{values: []any{now, now}} // RETURNING created_at, updated_at
	case contains(sql, "UPDATE service_registry"):
		if p.updateErr != nil {
			return errRow{err: p.updateErr}
		}
		now := time.Now()
		return staticRow{values: []any{now, now}}
	case contains(sql, "FROM service_registry"):
		if p.getErr != nil {
			return errRow{err: p.getErr}
		}
		if p.getValues == nil {
			return errRow{err: pgx.ErrNoRows}
		}
		return staticRow{values: p.getValues}
	}
	return errRow{err: errSvcUnexpected(sql)}
}

func (p *svcFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if contains(sql, "FROM service_registry") && contains(sql, "ORDER BY name") {
		return &svcRows{rows: p.listValues}, nil
	}
	return nil, errSvcUnexpected(sql)
}

func errSvcUnexpected(sql string) error { return &svcErr{"svcFakePool: unexpected SQL: " + sql} }

type svcErr struct{ s string }

func (e *svcErr) Error() string { return e.s }

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	return "1" // достаточно для RowsAffected-классификации (0 vs >0)
}

// svcRows — pgx.Rows-обёртка над списком []any-строк (8 колонок ServiceEntry).
type svcRows struct {
	rows [][]any
	idx  int
}

func (r *svcRows) Next() bool { r.idx++; return r.idx <= len(r.rows) }
func (r *svcRows) Scan(dest ...any) error {
	return staticRow{values: r.rows[r.idx-1]}.Scan(dest...)
}
func (r *svcRows) Err() error                                   { return nil }
func (r *svcRows) Close()                                       {}
func (r *svcRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *svcRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *svcRows) Values() ([]any, error)                       { return nil, nil }
func (r *svcRows) RawValues() [][]byte                          { return nil }
func (r *svcRows) Conn() *pgx.Conn                              { return nil }

// newServiceHandler собирает ServiceHandler поверх serviceregistry.Service на
// fake-pool (без lister-ов).
func newServiceHandler(t *testing.T, pool *svcFakePool) *ServiceHandler {
	t.Helper()
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("serviceregistry.NewService: %v", err)
	}
	return NewServiceHandler(svc, nil, nil, nil, nil, nil)
}

// newServiceHandlerWith собирает ServiceHandler поверх pool + произвольных lister-ов.
func newServiceHandlerWith(t *testing.T, pool *svcFakePool, refs ServiceRefsLister, scen ServiceScenarioLister, ss ServiceStateSchemaLister, deps ServiceDependenciesLister) *ServiceHandler {
	t.Helper()
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("serviceregistry.NewService: %v", err)
	}
	return NewServiceHandler(svc, refs, scen, ss, deps, nil)
}

func serviceRow(name, git, ref string) []any {
	now := time.Now()
	return []any{name, git, ref, nil, nil, nil, now, now}
}

func claimsService() *jwt.Claims { return claimsFor("archon-alice") }

// --- Register ---

func TestServiceHandler_Register_201(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{})
	reply, err := h.RegisterTyped(context.Background(), claimsService(),
		ServiceRegisterInput{Name: "web", Git: "https://git/web.git", Ref: "v1.0.0"})
	if err != nil {
		t.Fatalf("RegisterTyped: %v", err)
	}
	if reply.Body.Name != "web" || reply.Body.Ref != "v1.0.0" {
		t.Errorf("reply body = %+v", reply.Body)
	}
}

func TestServiceHandler_Register_EmptyName_422(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{})
	_, err := h.RegisterTyped(context.Background(), claimsService(),
		ServiceRegisterInput{Name: "", Git: "g", Ref: "v1"})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestServiceHandler_Register_EmptyGit_422(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{})
	_, err := h.RegisterTyped(context.Background(), claimsService(),
		ServiceRegisterInput{Name: "web", Git: "", Ref: "v1"})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestServiceHandler_Register_BadRefresh_422(t *testing.T) {
	bad := "nonsense"
	h := newServiceHandler(t, &svcFakePool{})
	_, err := h.RegisterTyped(context.Background(), claimsService(),
		ServiceRegisterInput{Name: "web", Git: "g", Ref: "v1", Refresh: &bad})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestServiceHandler_Register_Duplicate_409(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{
		insertErr: &pgconn.PgError{Code: "23505", ConstraintName: "service_registry_pkey"},
	})
	_, err := h.RegisterTyped(context.Background(), claimsService(),
		ServiceRegisterInput{Name: "web", Git: "g", Ref: "v1"})
	wantProblem(t, err, problem.TypeServiceExists)
}

func TestServiceHandler_Register_FKViolation_404(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{
		insertErr: &pgconn.PgError{Code: "23503", ConstraintName: "service_registry_created_by_aid_fkey"},
	})
	_, err := h.RegisterTyped(context.Background(), claimsFor("archon-ghost"),
		ServiceRegisterInput{Name: "web", Git: "g", Ref: "v1"})
	wantProblem(t, err, problem.TypeNotFound)
}

// --- List / Get ---

func TestServiceHandler_List_200(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{
		listValues: [][]any{serviceRow("api", "g1", "v1"), serviceRow("web", "g2", "v2")},
	})
	page, err := h.ListTyped(context.Background())
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].Name != "api" || page.Items[1].Name != "web" {
		t.Errorf("page = %+v", page.Items)
	}
}

func TestServiceHandler_List_Empty_NonNil(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{listValues: nil})
	page, err := h.ListTyped(context.Background())
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	// non-nil пустой срез (native-проекция в api отдаёт items:[]).
	if page.Items == nil {
		t.Errorf("empty list items is nil, want non-nil empty slice")
	}
}

func TestServiceHandler_Get_200(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{getValues: serviceRow("web", "g", "v1")})
	view, err := h.GetTyped(context.Background(), "web")
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	if view.Name != "web" {
		t.Errorf("view = %+v", view)
	}
}

func TestServiceHandler_Get_NotFound_404(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{getValues: nil})
	_, err := h.GetTyped(context.Background(), "ghost")
	wantProblem(t, err, problem.TypeNotFound)
}

// --- Update ---

func TestServiceHandler_Update_200(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{})
	reply, err := h.UpdateTyped(context.Background(), claimsService(), "web",
		ServiceUpdateInput{Git: "https://git/web.git", Ref: "v2.0.0"})
	if err != nil {
		t.Fatalf("UpdateTyped: %v", err)
	}
	if reply.Body.Ref != "v2.0.0" {
		t.Errorf("reply body = %+v", reply.Body)
	}
}

func TestServiceHandler_Update_EmptyRef_422(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{})
	_, err := h.UpdateTyped(context.Background(), claimsService(), "web",
		ServiceUpdateInput{Git: "g", Ref: ""})
	wantProblem(t, err, problem.TypeValidationFailed)
}

func TestServiceHandler_Update_NotFound_404(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{updateErr: pgx.ErrNoRows})
	_, err := h.UpdateTyped(context.Background(), claimsService(), "ghost",
		ServiceUpdateInput{Git: "g", Ref: "v1"})
	wantProblem(t, err, problem.TypeNotFound)
}

// --- Deregister ---

func TestServiceHandler_Deregister_204(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{deleteRows: 1})
	if _, err := h.DeregisterTyped(context.Background(), "web"); err != nil {
		t.Fatalf("DeregisterTyped: %v", err)
	}
}

func TestServiceHandler_Deregister_NotFound_404(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{deleteRows: 0})
	_, err := h.DeregisterTyped(context.Background(), "ghost")
	wantProblem(t, err, problem.TypeNotFound)
}

// --- ListRefs ---

// fakeRefsLister — простой ServiceRefsLister: возвращает заданный набор refs
// либо ошибку. Запоминает имя сервиса/git-URL, с которыми его дёрнули.
type fakeRefsLister struct {
	refs       []artifact.GitRef
	err        error
	gotName    string
	gotGitURL  string
	called     int
	invalidate []string
}

func (f *fakeRefsLister) ListRefs(_ context.Context, name, gitURL string) ([]artifact.GitRef, error) {
	f.called++
	f.gotName = name
	f.gotGitURL = gitURL
	if f.err != nil {
		return nil, f.err
	}
	return f.refs, nil
}

// Invalidate реализует интерфейс, который handler проверяет через type-assertion
// в invalidateRefs (для синхронизации кеша после Update/Deregister).
func (f *fakeRefsLister) Invalidate(name string) {
	f.invalidate = append(f.invalidate, name)
}

func TestServiceHandler_ListRefs_200(t *testing.T) {
	lister := &fakeRefsLister{refs: []artifact.GitRef{
		{Name: "v2.0.0", Type: artifact.GitRefTypeTag, Commit: "abc"},
		{Name: "main", Type: artifact.GitRefTypeBranch, Commit: "def", IsDefault: true},
	}}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1.0.0")}, lister, nil, nil, nil)

	reply, err := h.ListRefsTyped(context.Background(), "web")
	if err != nil {
		t.Fatalf("ListRefsTyped: %v", err)
	}
	if reply.Service != "web" || len(reply.Refs) != 2 {
		t.Errorf("reply = %+v", reply)
	}
	if reply.Refs[1].Name != "main" || !reply.Refs[1].IsDefault {
		t.Errorf("default ref = %+v", reply.Refs[1])
	}
	if lister.gotName != "web" || lister.gotGitURL != "https://git/web.git" {
		t.Errorf("lister dispatch = (%q, %q)", lister.gotName, lister.gotGitURL)
	}
}

func TestServiceHandler_ListRefs_ServiceNotFound_404(t *testing.T) {
	h := newServiceHandlerWith(t, &svcFakePool{getValues: nil}, &fakeRefsLister{}, nil, nil, nil)
	_, err := h.ListRefsTyped(context.Background(), "ghost")
	wantProblem(t, err, problem.TypeNotFound)
}

func TestServiceHandler_ListRefs_LsRemoteFails_502(t *testing.T) {
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1")},
		&fakeRefsLister{err: errSvcUnexpected("upstream down")}, nil, nil, nil)
	_, err := h.ListRefsTyped(context.Background(), "web")
	wantProblem(t, err, problem.TypeBadGateway)
}

func TestServiceHandler_ListRefs_NoLister_500(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{})
	_, err := h.ListRefsTyped(context.Background(), "web")
	wantProblem(t, err, problem.TypeInternalError)
}

func TestServiceHandler_ListRefs_EmptyRefs_200(t *testing.T) {
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1")},
		&fakeRefsLister{refs: nil}, nil, nil, nil)
	reply, err := h.ListRefsTyped(context.Background(), "web")
	if err != nil {
		t.Fatalf("ListRefsTyped: %v", err)
	}
	// non-nil пустой срез (native-проекция отдаёт refs:[]).
	if reply.Refs == nil {
		t.Errorf("empty refs is nil, want non-nil empty slice")
	}
}

// TestServiceHandler_Update_InvalidatesRefs — после успешного UpdateTyped handler
// инвалидирует refs-кеш по имени (чтобы следующий /refs увидел новый git-URL).
func TestServiceHandler_Update_InvalidatesRefs(t *testing.T) {
	lister := &fakeRefsLister{refs: []artifact.GitRef{{Name: "v1", Type: artifact.GitRefTypeTag, Commit: "a"}}}
	h := newServiceHandlerWith(t, &svcFakePool{}, lister, nil, nil, nil)
	if _, err := h.UpdateTyped(context.Background(), claimsService(), "web",
		ServiceUpdateInput{Git: "https://git/web-new.git", Ref: "v2.0.0"}); err != nil {
		t.Fatalf("UpdateTyped: %v", err)
	}
	if len(lister.invalidate) != 1 || lister.invalidate[0] != "web" {
		t.Errorf("ожидался Invalidate(\"web\"), got %v", lister.invalidate)
	}
}

func TestServiceHandler_Deregister_InvalidatesRefs(t *testing.T) {
	lister := &fakeRefsLister{}
	h := newServiceHandlerWith(t, &svcFakePool{deleteRows: 1}, lister, nil, nil, nil)
	if _, err := h.DeregisterTyped(context.Background(), "web"); err != nil {
		t.Fatalf("DeregisterTyped: %v", err)
	}
	if len(lister.invalidate) != 1 || lister.invalidate[0] != "web" {
		t.Errorf("ожидался Invalidate(\"web\"), got %v", lister.invalidate)
	}
}

// --- ListScenarios ---

// fakeScenariosLister — простой ServiceScenarioLister: возвращает заданный
// набор scenario либо ошибку; запоминает аргументы вызова + поддерживает
// Invalidate (handler полагается на type-assertion в invalidateScenarios).
type fakeScenariosLister struct {
	scenarios  []artifact.Scenario
	err        error
	gotName    string
	gotGitURL  string
	gotRef     string
	called     int
	invalidate []string
}

func (f *fakeScenariosLister) ListScenarios(_ context.Context, name, gitURL, ref string) ([]artifact.Scenario, error) {
	f.called++
	f.gotName = name
	f.gotGitURL = gitURL
	f.gotRef = ref
	if f.err != nil {
		return nil, f.err
	}
	return f.scenarios, nil
}

func (f *fakeScenariosLister) Invalidate(name string) {
	f.invalidate = append(f.invalidate, name)
}

func TestServiceHandler_ListScenarios_200(t *testing.T) {
	lister := &fakeScenariosLister{scenarios: []artifact.Scenario{
		{Name: "add_replicas", Path: "scenario/add_replicas/main.yml", Description: "extra"},
		{Name: "create", Path: "scenario/create/main.yml", Description: "make"},
	}}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1.0.0")}, nil, lister, nil, nil)

	got, err := h.ListScenariosTyped(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("ListScenariosTyped: %v", err)
	}
	if got.Service != "web" || got.Ref != "v1.0.0" {
		t.Errorf("got = %+v", got)
	}
	if lister.gotName != "web" || lister.gotGitURL != "https://git/web.git" || lister.gotRef != "v1.0.0" {
		t.Errorf("lister dispatch = (%q, %q, %q)", lister.gotName, lister.gotGitURL, lister.gotRef)
	}
	// kind размечается handler-ом по канону LifecycleScenarioNames: create →
	// lifecycle, add_replicas → operational. UI читает каталог, не хардкодит.
	kinds := map[string]string{}
	for _, s := range got.Scenarios {
		kinds[s.Name] = s.Kind
	}
	if kinds["create"] != artifact.ScenarioKindLifecycle {
		t.Errorf("create kind = %q, want lifecycle", kinds["create"])
	}
	if kinds["add_replicas"] != artifact.ScenarioKindOperational {
		t.Errorf("add_replicas kind = %q, want operational", kinds["add_replicas"])
	}
}

// Имена из LifecycleScenarioNames (create/destroy) размечаются как lifecycle,
// любое другое — operational. `converge` выведен из набора (amend ADR-031,
// 2026-06-10): это operational drift-target, не lifecycle. Тест охраняет
// разметку каталога на handler-уровне от рассинхрона канона и DTO-разметки.
func TestServiceHandler_ListScenarios_KindByLifecycleCanon(t *testing.T) {
	lister := &fakeScenariosLister{scenarios: []artifact.Scenario{
		{Name: "converge", Path: "scenario/converge/main.yml"},
		{Name: "create", Path: "scenario/create/main.yml"},
		{Name: "destroy", Path: "scenario/destroy/main.yml"},
		{Name: "rotate_certs", Path: "scenario/rotate_certs/main.yml"},
	}}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1.0.0")}, nil, lister, nil, nil)

	got, err := h.ListScenariosTyped(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("ListScenariosTyped: %v", err)
	}
	want := map[string]string{
		"converge":     artifact.ScenarioKindOperational,
		"create":       artifact.ScenarioKindLifecycle,
		"destroy":      artifact.ScenarioKindLifecycle,
		"rotate_certs": artifact.ScenarioKindOperational,
	}
	for _, s := range got.Scenarios {
		if want[s.Name] != s.Kind {
			t.Errorf("%s kind = %q, want %q", s.Name, s.Kind, want[s.Name])
		}
	}
}

// runnable размечается handler-ом по канону scenario-пакета (ADR-042 «тупой
// фронт»): create=true (bootstrap-run), destroy=false (удаление — спец-флоу
// DELETE, не run), converge/operational=true. UI фильтрует Run-форму по этому
// признаку, не по хардкоду имён. Тест охраняет от рассинхрона канона и
// DTO-разметки (S4 lifecycle-rework).
func TestServiceHandler_ListScenarios_RunnableByCanon(t *testing.T) {
	lister := &fakeScenariosLister{scenarios: []artifact.Scenario{
		{Name: "converge", Path: "scenario/converge/main.yml"},
		{Name: "create", Path: "scenario/create/main.yml"},
		{Name: "destroy", Path: "scenario/destroy/main.yml"},
		{Name: "rotate_certs", Path: "scenario/rotate_certs/main.yml"},
	}}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1.0.0")}, nil, lister, nil, nil)

	got, err := h.ListScenariosTyped(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("ListScenariosTyped: %v", err)
	}
	want := map[string]bool{
		"converge":     true,
		"create":       true,
		"destroy":      false,
		"rotate_certs": true,
	}
	seen := map[string]bool{}
	for _, s := range got.Scenarios {
		seen[s.Name] = true
		if want[s.Name] != s.Runnable {
			t.Errorf("%s runnable = %v, want %v", s.Name, s.Runnable, want[s.Name])
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("scenario %q отсутствует в ответе", name)
		}
	}
}

// `?ref=...` переопределяет ref из реестра — UI смотрит scenario-листинг
// конкретного tag-а до Upgrade.
func TestServiceHandler_ListScenarios_RefQueryOverride(t *testing.T) {
	lister := &fakeScenariosLister{scenarios: []artifact.Scenario{{Name: "create"}}}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1.0.0")}, nil, lister, nil, nil)

	got, err := h.ListScenariosTyped(context.Background(), "web", "v2.0.0")
	if err != nil {
		t.Fatalf("ListScenariosTyped: %v", err)
	}
	if lister.gotRef != "v2.0.0" {
		t.Errorf("ref override не сработал: lister.gotRef = %q, want v2.0.0", lister.gotRef)
	}
	if got.Ref != "v2.0.0" {
		t.Errorf("ответ должен отражать выбранный ref: %q", got.Ref)
	}
}

func TestServiceHandler_ListScenarios_ServiceNotFound_404(t *testing.T) {
	h := newServiceHandlerWith(t, &svcFakePool{getValues: nil}, nil, &fakeScenariosLister{}, nil, nil)
	_, err := h.ListScenariosTyped(context.Background(), "ghost", "")
	wantProblem(t, err, problem.TypeNotFound)
}

func TestServiceHandler_ListScenarios_LoaderFails_502(t *testing.T) {
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1")},
		nil, &fakeScenariosLister{err: errSvcUnexpected("git unreachable")}, nil, nil)
	_, err := h.ListScenariosTyped(context.Background(), "web", "")
	wantProblem(t, err, problem.TypeBadGateway)
}

func TestServiceHandler_ListScenarios_NoLister_500(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{})
	_, err := h.ListScenariosTyped(context.Background(), "web", "")
	wantProblem(t, err, problem.TypeInternalError)
}

func TestServiceHandler_ListScenarios_EmptyScenarios_200(t *testing.T) {
	lister := &fakeScenariosLister{scenarios: nil}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1")}, nil, lister, nil, nil)
	got, err := h.ListScenariosTyped(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("ListScenariosTyped: %v", err)
	}
	if got.Scenarios == nil {
		t.Errorf("пустой scenarios is nil, want non-nil empty slice")
	}
}

func TestServiceHandler_Update_InvalidatesScenarios(t *testing.T) {
	lister := &fakeScenariosLister{scenarios: []artifact.Scenario{{Name: "create"}}}
	h := newServiceHandlerWith(t, &svcFakePool{}, nil, lister, nil, nil)
	if _, err := h.UpdateTyped(context.Background(), claimsService(), "web",
		ServiceUpdateInput{Git: "https://git/web-new.git", Ref: "v2.0.0"}); err != nil {
		t.Fatalf("UpdateTyped: %v", err)
	}
	if len(lister.invalidate) != 1 || lister.invalidate[0] != "web" {
		t.Errorf("ожидался Invalidate(\"web\") у scenarios-кеша, got %v", lister.invalidate)
	}
}

func TestServiceHandler_Deregister_InvalidatesScenarios(t *testing.T) {
	lister := &fakeScenariosLister{}
	h := newServiceHandlerWith(t, &svcFakePool{deleteRows: 1}, nil, lister, nil, nil)
	if _, err := h.DeregisterTyped(context.Background(), "web"); err != nil {
		t.Fatalf("DeregisterTyped: %v", err)
	}
	if len(lister.invalidate) != 1 || lister.invalidate[0] != "web" {
		t.Errorf("ожидался Invalidate(\"web\") у scenarios-кеша, got %v", lister.invalidate)
	}
}

// --- ListStateSchema ---

// fakeStateSchemaLister — простой ServiceStateSchemaLister: возвращает заданный
// info или ошибку; запоминает аргументы вызова + поддерживает Invalidate
// (handler полагается на type-assertion в invalidateStateSchema).
type fakeStateSchemaLister struct {
	info       *artifact.StateSchemaInfo
	err        error
	gotName    string
	gotGitURL  string
	gotRef     string
	called     int
	invalidate []string
}

func (f *fakeStateSchemaLister) ListStateSchema(_ context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error) {
	f.called++
	f.gotName = name
	f.gotGitURL = gitURL
	f.gotRef = ref
	if f.err != nil {
		return nil, f.err
	}
	return f.info, nil
}

func (f *fakeStateSchemaLister) Invalidate(name string) {
	f.invalidate = append(f.invalidate, name)
}

func sampleSchemaInfo() *artifact.StateSchemaInfo {
	return &artifact.StateSchemaInfo{
		Version: 2,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"master_host": map[string]any{"type": "string"},
			},
		},
		Migrations: []artifact.Migration{
			{From: 1, To: 2, Path: "migrations/001_to_002.yml"},
		},
	}
}

func TestServiceHandler_ListStateSchema_200(t *testing.T) {
	lister := &fakeStateSchemaLister{info: sampleSchemaInfo()}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1.0.0")}, nil, nil, lister, nil)

	got, err := h.ListStateSchemaTyped(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("ListStateSchemaTyped: %v", err)
	}
	if got.Service != "web" || got.Ref != "v1.0.0" || got.StateSchemaVersion != 2 {
		t.Errorf("got = %+v", got)
	}
	if len(got.Migrations) != 1 || got.Migrations[0].From != 1 || got.Migrations[0].To != 2 {
		t.Errorf("migrations = %+v", got.Migrations)
	}
	if got.Schema == nil {
		t.Errorf("schema is nil, want decl")
	}
	if lister.gotName != "web" || lister.gotGitURL != "https://git/web.git" || lister.gotRef != "v1.0.0" {
		t.Errorf("lister dispatch = (%q, %q, %q)", lister.gotName, lister.gotGitURL, lister.gotRef)
	}
}

func TestServiceHandler_ListStateSchema_RefQueryOverride(t *testing.T) {
	lister := &fakeStateSchemaLister{info: sampleSchemaInfo()}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1.0.0")}, nil, nil, lister, nil)

	got, err := h.ListStateSchemaTyped(context.Background(), "web", "v2.0.0")
	if err != nil {
		t.Fatalf("ListStateSchemaTyped: %v", err)
	}
	if lister.gotRef != "v2.0.0" {
		t.Errorf("ref override не сработал: lister.gotRef = %q, want v2.0.0", lister.gotRef)
	}
	if got.Ref != "v2.0.0" {
		t.Errorf("ответ должен отражать выбранный ref: %q", got.Ref)
	}
}

func TestServiceHandler_ListStateSchema_ServiceNotFound_404(t *testing.T) {
	h := newServiceHandlerWith(t, &svcFakePool{getValues: nil}, nil, nil, &fakeStateSchemaLister{}, nil)
	_, err := h.ListStateSchemaTyped(context.Background(), "ghost", "")
	wantProblem(t, err, problem.TypeNotFound)
}

func TestServiceHandler_ListStateSchema_LoaderFails_502(t *testing.T) {
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1")},
		nil, nil, &fakeStateSchemaLister{err: errSvcUnexpected("git unreachable")}, nil)
	_, err := h.ListStateSchemaTyped(context.Background(), "web", "")
	wantProblem(t, err, problem.TypeBadGateway)
}

func TestServiceHandler_ListStateSchema_NoLister_500(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{})
	_, err := h.ListStateSchemaTyped(context.Background(), "web", "")
	wantProblem(t, err, problem.TypeInternalError)
}

// Декларация структуры в service.yml — опциональна; native-проекция omitempty
// выбрасывает `schema` из JSON, а migrations превращается в []. Здесь проверяем
// доменную форму: Schema nil, Migrations non-nil [].
func TestServiceHandler_ListStateSchema_NoSchemaDecl(t *testing.T) {
	lister := &fakeStateSchemaLister{info: &artifact.StateSchemaInfo{Version: 1, Schema: nil, Migrations: nil}}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1")}, nil, nil, lister, nil)
	got, err := h.ListStateSchemaTyped(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("ListStateSchemaTyped: %v", err)
	}
	if got.StateSchemaVersion != 1 {
		t.Errorf("version = %d, want 1", got.StateSchemaVersion)
	}
	if got.Migrations == nil {
		t.Errorf("empty migrations is nil, want non-nil empty slice")
	}
	if got.Schema != nil {
		t.Errorf("nil schema decl должна оставаться nil, got %v", got.Schema)
	}
}

// Defensive: lister вернул (nil, nil) — handler отдаёт 502, не пустоту.
func TestServiceHandler_ListStateSchema_LoaderReturnsNilInfo_502(t *testing.T) {
	lister := &fakeStateSchemaLister{info: nil, err: nil}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1")}, nil, nil, lister, nil)
	_, err := h.ListStateSchemaTyped(context.Background(), "web", "")
	wantProblem(t, err, problem.TypeBadGateway)
}

func TestServiceHandler_Update_InvalidatesStateSchema(t *testing.T) {
	lister := &fakeStateSchemaLister{info: sampleSchemaInfo()}
	h := newServiceHandlerWith(t, &svcFakePool{}, nil, nil, lister, nil)
	if _, err := h.UpdateTyped(context.Background(), claimsService(), "web",
		ServiceUpdateInput{Git: "https://git/web-new.git", Ref: "v2.0.0"}); err != nil {
		t.Fatalf("UpdateTyped: %v", err)
	}
	if len(lister.invalidate) != 1 || lister.invalidate[0] != "web" {
		t.Errorf("ожидался Invalidate(\"web\") у state-schema-кеша, got %v", lister.invalidate)
	}
}

// --- ListDependencies ---

// fakeDependenciesLister — простой ServiceDependenciesLister: возвращает
// заданный deps или ошибку; запоминает аргументы вызова + поддерживает
// Invalidate (handler полагается на type-assertion в invalidateDependencies).
type fakeDependenciesLister struct {
	deps       *artifact.ServiceDependencies
	err        error
	gotName    string
	gotGitURL  string
	gotRef     string
	called     int
	invalidate []string
}

func (f *fakeDependenciesLister) ListDependencies(_ context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error) {
	f.called++
	f.gotName = name
	f.gotGitURL = gitURL
	f.gotRef = ref
	if f.err != nil {
		return nil, f.err
	}
	return f.deps, nil
}

func (f *fakeDependenciesLister) Invalidate(name string) {
	f.invalidate = append(f.invalidate, name)
}

func sampleDependencies() *artifact.ServiceDependencies {
	return &artifact.ServiceDependencies{
		Destiny: []artifact.Dependency{
			{Name: "redis", Ref: "v2.0.0"},
			{Name: "redis-replication-config", Ref: "v1.0.0"},
		},
		Modules: []artifact.Dependency{
			{Name: "wb.redis-failover", Ref: "v1.2.0"},
		},
	}
}

func TestServiceHandler_ListDependencies_200(t *testing.T) {
	lister := &fakeDependenciesLister{deps: sampleDependencies()}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1.0.0")}, nil, nil, nil, lister)

	got, err := h.ListDependenciesTyped(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("ListDependenciesTyped: %v", err)
	}
	if got.Service != "web" || got.Ref != "v1.0.0" {
		t.Errorf("got = %+v", got)
	}
	if len(got.Destiny) != 2 || got.Destiny[0].Name != "redis" || got.Destiny[0].Ref != "v2.0.0" {
		t.Errorf("destiny = %+v", got.Destiny)
	}
	if len(got.Modules) != 1 || got.Modules[0].Name != "wb.redis-failover" {
		t.Errorf("modules = %+v", got.Modules)
	}
	if lister.gotName != "web" || lister.gotGitURL != "https://git/web.git" || lister.gotRef != "v1.0.0" {
		t.Errorf("lister dispatch = (%q, %q, %q)", lister.gotName, lister.gotGitURL, lister.gotRef)
	}
}

func TestServiceHandler_ListDependencies_RefQueryOverride(t *testing.T) {
	lister := &fakeDependenciesLister{deps: sampleDependencies()}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1.0.0")}, nil, nil, nil, lister)

	got, err := h.ListDependenciesTyped(context.Background(), "web", "v2.0.0")
	if err != nil {
		t.Fatalf("ListDependenciesTyped: %v", err)
	}
	if lister.gotRef != "v2.0.0" {
		t.Errorf("ref override не сработал: lister.gotRef = %q, want v2.0.0", lister.gotRef)
	}
	if got.Ref != "v2.0.0" {
		t.Errorf("ответ должен отражать выбранный ref: %q", got.Ref)
	}
}

func TestServiceHandler_ListDependencies_ServiceNotFound_404(t *testing.T) {
	h := newServiceHandlerWith(t, &svcFakePool{getValues: nil}, nil, nil, nil, &fakeDependenciesLister{})
	_, err := h.ListDependenciesTyped(context.Background(), "ghost", "")
	wantProblem(t, err, problem.TypeNotFound)
}

func TestServiceHandler_ListDependencies_LoaderFails_502(t *testing.T) {
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1")},
		nil, nil, nil, &fakeDependenciesLister{err: errSvcUnexpected("git unreachable")})
	_, err := h.ListDependenciesTyped(context.Background(), "web", "")
	wantProblem(t, err, problem.TypeBadGateway)
}

func TestServiceHandler_ListDependencies_NoLister_500(t *testing.T) {
	h := newServiceHandler(t, &svcFakePool{})
	_, err := h.ListDependenciesTyped(context.Background(), "web", "")
	wantProblem(t, err, problem.TypeInternalError)
}

// Сервис без destiny/modules — оба блока non-nil [] (native-проекция отдаёт []).
func TestServiceHandler_ListDependencies_EmptyBlocks(t *testing.T) {
	lister := &fakeDependenciesLister{deps: &artifact.ServiceDependencies{
		Destiny: []artifact.Dependency{},
		Modules: []artifact.Dependency{},
	}}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1")}, nil, nil, nil, lister)
	got, err := h.ListDependenciesTyped(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("ListDependenciesTyped: %v", err)
	}
	if got.Destiny == nil || got.Modules == nil {
		t.Errorf("пустые блоки должны быть non-nil [], got destiny=%v modules=%v", got.Destiny, got.Modules)
	}
}

// Defensive: lister вернул (nil, nil) — handler отдаёт 502, не пустоту.
func TestServiceHandler_ListDependencies_LoaderReturnsNil_502(t *testing.T) {
	lister := &fakeDependenciesLister{deps: nil, err: nil}
	h := newServiceHandlerWith(t, &svcFakePool{getValues: serviceRow("web", "https://git/web.git", "v1")}, nil, nil, nil, lister)
	_, err := h.ListDependenciesTyped(context.Background(), "web", "")
	wantProblem(t, err, problem.TypeBadGateway)
}

func TestServiceHandler_Update_InvalidatesDependencies(t *testing.T) {
	lister := &fakeDependenciesLister{deps: sampleDependencies()}
	h := newServiceHandlerWith(t, &svcFakePool{}, nil, nil, nil, lister)
	if _, err := h.UpdateTyped(context.Background(), claimsService(), "web",
		ServiceUpdateInput{Git: "https://git/web-new.git", Ref: "v2.0.0"}); err != nil {
		t.Fatalf("UpdateTyped: %v", err)
	}
	if len(lister.invalidate) != 1 || lister.invalidate[0] != "web" {
		t.Errorf("ожидался Invalidate(\"web\") у dependencies-кеша, got %v", lister.invalidate)
	}
}

func TestServiceHandler_Deregister_InvalidatesDependencies(t *testing.T) {
	lister := &fakeDependenciesLister{}
	h := newServiceHandlerWith(t, &svcFakePool{deleteRows: 1}, nil, nil, nil, lister)
	if _, err := h.DeregisterTyped(context.Background(), "web"); err != nil {
		t.Fatalf("DeregisterTyped: %v", err)
	}
	if len(lister.invalidate) != 1 || lister.invalidate[0] != "web" {
		t.Errorf("ожидался Invalidate(\"web\") у dependencies-кеша, got %v", lister.invalidate)
	}
}

func TestServiceHandler_Deregister_InvalidatesStateSchema(t *testing.T) {
	lister := &fakeStateSchemaLister{}
	h := newServiceHandlerWith(t, &svcFakePool{deleteRows: 1}, nil, nil, lister, nil)
	if _, err := h.DeregisterTyped(context.Background(), "web"); err != nil {
		t.Fatalf("DeregisterTyped: %v", err)
	}
	if len(lister.invalidate) != 1 || lister.invalidate[0] != "web" {
		t.Errorf("ожидался Invalidate(\"web\") у state-schema-кеша, got %v", lister.invalidate)
	}
}
