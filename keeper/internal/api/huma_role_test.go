package api

// Guard-тесты ПЕРВОГО ТИРАЖ-БАТЧА разворота ROLE-домена ЦЕЛИКОМ на huma full-typed
// (ADR-054 §Audit). Все role-роуты на huma: READ (list — pilot-1 без audit) + WRITE
// (create/delete/update-permissions/grant/revoke-operator — pilot-2 + huma-audit-
// middleware вариант B). Доказывают, что huma-роуты поверх chi сохраняют инварианты
// кластера:
//
//   - wire/golden: create 201 пустое тело; list 200 RoleView[] байт-в-байт
//     (Description всегда, DefaultScope nil→пропуск, []-vs-null); write-204 пустое;
//   - unknown-field → 400 problem+json (huma additionalProperties:false ЧЕСТНЫЙ);
//   - missing-required → 422 problem+json (huma `required:"true"`);
//   - RBAC-deny → 403 (навеска группы наследуется huma);
//   - S6-GUARD на КАЖДЫЙ write-роут (КРИТ, рецидив урока S6): write через ПОЛНУЮ
//     huma-навеску (RequirePermission + humaAuditMiddleware + huma-handler) пишет
//     audit-event с НЕПУСТЫМ payload на 2xx и НЕ пишет на 4xx — huma САМ пишет ответ
//     (StatusRecorder неприменим), audit держит hctx.Status() + carrier-payload.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// strictRoleBody — валидное тело role.create (пустой набор permissions: subset-
// check не читает БД, один INSERT в tx → 201 без реального Postgres).
const strictRoleBody = `{"name":"ops","permissions":[]}`

// roleSuccessPool — узкий мок [rbac.ServicePool] для success-path всех role-write-
// роутов huma-теста (delete/update/grant/revoke): lockRole=found+не-builtin,
// rolePermissions=пусто (роль не даёт `*` → self-lockout-проба не дёргается),
// lockRoleOperator=found, caller-perms=`*`. Покрывает ТОЛЬКО 2xx-путь (S6-guard:
// audit пишется на успехе) — error-классификацию валидируют handler-unit-тесты
// (handlers/role_test.go на rbacFakePool). Tx проксирует Exec/Query на pool.
type roleSuccessPool struct{}

func (roleSuccessPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("OK 1"), nil
}
func (roleSuccessPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return roleErrRow{err: pgx.ErrNoRows}
}
func (roleSuccessPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "SELECT builtin FROM rbac_roles"):
		return &roleBoolRows{values: []bool{false}}, nil // роль есть, не builtin
	case strings.Contains(sql, "SELECT permission FROM rbac_role_permissions"):
		return &roleStrRows{}, nil // роль не даёт `*` → self-lockout-проба не нужна
	case strings.Contains(sql, "SELECT rp.permission"):
		return &roleStrRows{values: []string{"*"}}, nil // caller=cluster-admin
	case strings.Contains(sql, "SELECT default_scope FROM rbac_roles"):
		return &roleNullStrRows{}, nil // NULL scope
	case strings.Contains(sql, "SELECT 1 FROM rbac_role_operators"):
		return &roleIntRows{values: []int{1}}, nil // membership есть (revoke)
	}
	return nil, errStrictUnexpectedSQL
}
func (roleSuccessPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return roleSuccessTx{}, nil
}

type roleErrRow struct{ err error }

func (r roleErrRow) Scan(...any) error { return r.err }

// roleSuccessTx — pgx.Tx, проксирующий Exec/Query обратно на success-pool;
// Commit/Rollback no-op. Встраивание nil-pgx.Tx даёт остальные (не-вызываемые) методы.
type roleSuccessTx struct{ pgx.Tx }

func (roleSuccessTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return roleSuccessPool{}.Exec(ctx, sql, args...)
}
func (roleSuccessTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return roleSuccessPool{}.Query(ctx, sql, args...)
}
func (roleSuccessTx) Commit(context.Context) error   { return nil }
func (roleSuccessTx) Rollback(context.Context) error { return nil }

// --- минимальные pgx.Rows-обёртки (api-пакет; handlers-версии package-private) ---

type roleBoolRows struct {
	values []bool
	idx    int
}

func (r *roleBoolRows) Next() bool                                   { r.idx++; return r.idx <= len(r.values) }
func (r *roleBoolRows) Scan(dest ...any) error                       { *dest[0].(*bool) = r.values[r.idx-1]; return nil }
func (r *roleBoolRows) Err() error                                   { return nil }
func (r *roleBoolRows) Close()                                       {}
func (r *roleBoolRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *roleBoolRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *roleBoolRows) Values() ([]any, error)                       { return nil, nil }
func (r *roleBoolRows) RawValues() [][]byte                          { return nil }
func (r *roleBoolRows) Conn() *pgx.Conn                              { return nil }

type roleIntRows struct {
	values []int
	idx    int
}

func (r *roleIntRows) Next() bool                                   { r.idx++; return r.idx <= len(r.values) }
func (r *roleIntRows) Scan(dest ...any) error                       { *dest[0].(*int) = r.values[r.idx-1]; return nil }
func (r *roleIntRows) Err() error                                   { return nil }
func (r *roleIntRows) Close()                                       {}
func (r *roleIntRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *roleIntRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *roleIntRows) Values() ([]any, error)                       { return nil, nil }
func (r *roleIntRows) RawValues() [][]byte                          { return nil }
func (r *roleIntRows) Conn() *pgx.Conn                              { return nil }

type roleStrRows struct {
	values []string
	idx    int
}

func (r *roleStrRows) Next() bool                                   { r.idx++; return r.idx <= len(r.values) }
func (r *roleStrRows) Scan(dest ...any) error                       { *dest[0].(*string) = r.values[r.idx-1]; return nil }
func (r *roleStrRows) Err() error                                   { return nil }
func (r *roleStrRows) Close()                                       {}
func (r *roleStrRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *roleStrRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *roleStrRows) Values() ([]any, error)                       { return nil, nil }
func (r *roleStrRows) RawValues() [][]byte                          { return nil }
func (r *roleStrRows) Conn() *pgx.Conn                              { return nil }

type roleNullStrRows struct {
	value *string
	done  bool
}

func (r *roleNullStrRows) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}
func (r *roleNullStrRows) Scan(dest ...any) error                       { *dest[0].(**string) = r.value; return nil }
func (r *roleNullStrRows) Err() error                                   { return nil }
func (r *roleNullStrRows) Close()                                       {}
func (r *roleNullStrRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *roleNullStrRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *roleNullStrRows) Values() ([]any, error)                       { return nil, nil }
func (r *roleNullStrRows) RawValues() [][]byte                          { return nil }
func (r *roleNullStrRows) Conn() *pgx.Conn                              { return nil }

// humaRoleRouter собирает chi-роутер со ВСЕМИ role-роутами через huma —
// продакшен-навеска буквально из router.go: RequirePermission(role.<action>) на
// каждой группе + (для write) huma-audit-middleware варианта B + huma-операция.
// installHumaErrorOverride вызывается явно. injectClaims заменяет RequireJWT.
// pool параметризован: auditRolePool — create-путь, roleSuccessPool — все write.
func humaRoleRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, pool rbac.ServicePool) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("rbac.NewService: %v", err)
	}
	roleH := handlers.NewRoleHandler(svc, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/roles", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "role", "create", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaRole(newHumaRoleAPI(r, auditW, audit.EventRoleCreated, nil), roleH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "role", "list", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaRoleList(newHumaCadenceAPI(r), roleH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "role", "delete", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaRoleDelete(newHumaRoleAPI(r, auditW, audit.EventRoleDeleted, nil), roleH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "role", "update", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaRoleUpdatePermissions(newHumaRoleAPI(r, auditW, audit.EventRolePermissionsUpdated, nil), roleH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "role", "grant-operator", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaRoleGrantOperator(newHumaRoleAPI(r, auditW, audit.EventRoleOperatorGranted, nil), roleH)
			})
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "role", "revoke-operator", apimiddleware.NoSelector)).Group(func(r chi.Router) {
				registerHumaRoleRevokeOperator(newHumaRoleAPI(r, auditW, audit.EventRoleOperatorRevoked, nil), roleH)
			})
		})
	})
	return r
}

// === CREATE (pilot-2, без изменений — sanity сосуществования всего домена) ===

func TestHumaRole_Create_GoldenEmptyBody(t *testing.T) {
	r := humaRoleRouter(t, strictAllowAll{}, nil, auditRolePool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/roles", strings.NewReader(strictRoleBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	const golden = "" // легаси POST /v1/roles 201 — без тела
	if got := rec.Body.String(); got != golden {
		t.Errorf("GOLDEN wire-дрейф role.create 201-тела: got=%q want=%q", got, golden)
	}
}

func TestHumaRole_Create_UnknownField_400(t *testing.T) {
	r := humaRoleRouter(t, strictAllowAll{}, nil, auditRolePool{})

	body := `{"name":"ops","permissions":[],"bogus_field":1}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/roles", strings.NewReader(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

func TestHumaRole_Create_MissingName_422(t *testing.T) {
	r := humaRoleRouter(t, strictAllowAll{}, nil, auditRolePool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/roles", strings.NewReader(`{"permissions":[]}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

func TestHumaRole_Create_RBACDeny_403(t *testing.T) {
	r := humaRoleRouter(t, strictDenyAll{}, nil, auditRolePool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/roles", strings.NewReader(strictRoleBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_RoleCreate_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaRoleRouter(t, strictAllowAll{}, auditCap, auditRolePool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/roles", strings.NewReader(strictRoleBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventRoleCreated, map[string]any{"name": "ops", "created_by_aid": "archon-alice"})
}

// === LIST (READ pilot-1, БЕЗ audit) ===

// listOnePool — минимальный pool для GET /v1/roles: одна роль `ops` с пустыми
// permissions/operators, builtin=false, NULL default_scope. Доказывает wire-форму
// toRoleView (Description="" присутствует, DefaultScope опущен, []-vs-null).
type listOnePool struct{}

func (listOnePool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errStrictUnexpectedSQL
}
func (listOnePool) QueryRow(context.Context, string, ...any) pgx.Row {
	return roleErrRow{err: pgx.ErrNoRows}
}
func (listOnePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "SELECT name, description, builtin, default_scope FROM rbac_roles"):
		return &roleViewRows{}, nil
	case strings.Contains(sql, "permission"):
		return &roleStrRows{}, nil // у роли нет permissions
	case strings.Contains(sql, "operator"):
		return &roleStrRows{}, nil // у роли нет operators
	}
	return nil, errStrictUnexpectedSQL
}
func (listOnePool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errStrictUnexpectedSQL
}

// roleViewRows — один row LoadRoleViews-каталога: name=ops, description="",
// builtin=false, default_scope=NULL.
type roleViewRows struct{ done bool }

func (r *roleViewRows) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}
func (r *roleViewRows) Scan(dest ...any) error {
	*dest[0].(*string) = "ops" // name
	*dest[1].(*string) = ""    // description
	*dest[2].(*bool) = false   // builtin
	*dest[3].(**string) = nil  // default_scope NULL
	return nil
}
func (r *roleViewRows) Err() error                                   { return nil }
func (r *roleViewRows) Close()                                       {}
func (r *roleViewRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *roleViewRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *roleViewRows) Values() ([]any, error)                       { return nil, nil }
func (r *roleViewRows) RawValues() [][]byte                          { return nil }
func (r *roleViewRows) Conn() *pgx.Conn                              { return nil }

// TestHumaRole_List_GoldenWire — GOLDEN wire-guard READ-роута: 200-тело
// байт-в-байт. Фиксирует toRoleView-семантику: Description присутствует даже как
// "" (без omitempty), DefaultScope опущен при NULL, permissions/operators —
// []-not-null (emptyIfNil), items — массив. Дрейф (huma вмешает $schema / поле
// потеряет []-форму) ломает байты.
func TestHumaRole_List_GoldenWire(t *testing.T) {
	r := humaRoleRouter(t, strictAllowAll{}, nil, listOnePool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/roles", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Ремаршал через map → детерминированный порядок ключей; golden фиксирует
	// набор/форму, не порядок.
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[{"builtin":false,"description":"","name":"ops","operators":[],"permissions":[]}]}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф role.list:\n got  = %s\n want = %s", got, golden)
	}
}

// TestHumaRole_List_NoAudit — READ-роут не пишет audit (нет middleware). Прогон с
// capture-writer: 0 событий на 200.
func TestHumaRole_List_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaRoleRouter(t, strictAllowAll{}, auditCap, listOnePool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/roles", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("READ-роут role.list записал audit (%d событий) — у него нет audit-middleware", len(auditCap.Events()))
	}
}

func TestHumaRole_List_RBACDeny_403(t *testing.T) {
	r := humaRoleRouter(t, strictDenyAll{}, nil, listOnePool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/roles", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// === DELETE (WRITE+AUDIT role.deleted) ===

func TestHumaRole_Delete_204(t *testing.T) {
	r := humaRoleRouter(t, strictAllowAll{}, nil, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/roles/ops", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Errorf("204-тело role.delete должно быть ПУСТЫМ, got %q", body)
	}
}

// TestHumaAudit_RoleDelete_RecordsOnSuccess — S6-GUARD role.deleted: 204 пишет
// audit с payload {name}.
func TestHumaAudit_RoleDelete_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaRoleRouter(t, strictAllowAll{}, auditCap, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/roles/ops", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventRoleDeleted, map[string]any{"name": "ops"})
}

// TestHumaAudit_RoleDelete_NoAudit_OnRBACDeny — negative S6: 403 не пишет audit.
func TestHumaAudit_RoleDelete_NoAudit_OnRBACDeny(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaRoleRouter(t, strictDenyAll{}, auditCap, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/roles/ops", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на RBAC-deny role.delete (%d событий)", len(auditCap.Events()))
	}
}

// === UPDATE PERMISSIONS (WRITE+AUDIT role.permissions-updated) ===

func TestHumaRole_UpdatePermissions_204(t *testing.T) {
	r := humaRoleRouter(t, strictAllowAll{}, nil, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/roles/ops/permissions", strings.NewReader(`{"permissions":["incarnation.run"]}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_RoleUpdatePermissions_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaRoleRouter(t, strictAllowAll{}, auditCap, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/roles/ops/permissions", strings.NewReader(`{"permissions":["incarnation.run"]}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventRolePermissionsUpdated, map[string]any{"name": "ops"})
}

// TestHumaRole_UpdatePermissions_MissingPermissions_422 — required permissions
// (huma `required:"true"`) → 422, audit не пишется.
func TestHumaAudit_RoleUpdatePermissions_NoAudit_OnReject(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaRoleRouter(t, strictAllowAll{}, auditCap, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/roles/ops/permissions", strings.NewReader(`{}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на rejected PATCH (%d событий)", len(auditCap.Events()))
	}
}

// scopeCapturePool — success-pool PATCH /v1/roles/{name}/permissions, перехватывающий
// эффект presence-конверта default_scope на доменном дне. UpdateRoleDefaultScope
// (UPDATE rbac_roles SET default_scope = $2 WHERE name = $1) вызывается rbac.Service
// ТОЛЬКО при SetDefaultScope=true (presence ключа в теле), с arg=NULL при сбросе (null)
// или RAW-строкой при установке. Перехват args[1] этого Exec доказывает, что
// (SetDefaultScope, DefaultScope) долетели до домена корректно из Optional[string].
type scopeCapturePool struct {
	scopeUpdateCalled bool // был ли Exec UPDATE default_scope (== SetDefaultScope)
	scopeArg          any  // args[1] этого Exec: nil (NULL/сброс) либо строка (установка)
}

func (p *scopeCapturePool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "UPDATE rbac_roles SET default_scope") {
		p.scopeUpdateCalled = true
		if len(args) >= 2 {
			p.scopeArg = args[1]
		}
	}
	return pgconn.NewCommandTag("OK 1"), nil
}
func (p *scopeCapturePool) QueryRow(context.Context, string, ...any) pgx.Row {
	return roleErrRow{err: pgx.ErrNoRows}
}
func (p *scopeCapturePool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return roleSuccessPool{}.Query(ctx, sql, args...)
}
func (p *scopeCapturePool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return &scopeCaptureTx{pool: p}, nil
}

// scopeCaptureTx проксирует Exec/Query обратно на capture-pool (чтобы UPDATE
// default_scope, идущий внутри tx, был перехвачен).
type scopeCaptureTx struct {
	pgx.Tx
	pool *scopeCapturePool
}

func (t *scopeCaptureTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.pool.Exec(ctx, sql, args...)
}
func (t *scopeCaptureTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.pool.Query(ctx, sql, args...)
}
func (t *scopeCaptureTx) Commit(context.Context) error   { return nil }
func (t *scopeCaptureTx) Rollback(context.Context) error { return nil }

// TestHumaRole_UpdatePermissions_ScopePresence — SECURITY-relevant guard (ADR-054
// §Pattern третий tier; scope-narrowing RBAC, ADR-047): три ветки presence
// default_scope долетают через РЕАЛЬНЫЙ PATCH-роут (huma input → Optional[string] →
// optionalToPtr/Set → UpdatePermissionsInput → rbac.Service) в корректный доменный
// (SetDefaultScope, DefaultScope):
//
//   - тело БЕЗ default_scope (omitted) → SetDefaultScope=false → UpdateRoleDefaultScope
//     НЕ вызывается (scope роли не трогается);
//   - {"default_scope":null} (explicit null) → SetDefaultScope=true, DefaultScope=nil →
//     UpdateRoleDefaultScope с NULL-arg (scope СБРАСЫВАЕТСЯ);
//   - {"default_scope":"coven=prod"} (value) → SetDefaultScope=true → UpdateRoleDefaultScope
//     с arg "coven=prod" (scope УСТАНАВЛИВАЕТСЯ).
//
// Регресс presence-конверта = молчаливая RBAC scope-эскалация/деградация — потому guard.
func TestHumaRole_UpdatePermissions_ScopePresence(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		wantScopeWrite bool // ожидаем ли UPDATE default_scope (== SetDefaultScope)
		wantScopeArg   any  // ожидаемый args[1] при write (nil=NULL/сброс)
	}{
		{"omitted_не_трогать", `{"permissions":["incarnation.run"]}`, false, nil},
		{"null_сброс", `{"permissions":["incarnation.run"],"default_scope":null}`, true, nil},
		{"value_установка", `{"permissions":["incarnation.run"],"default_scope":"coven=prod"}`, true, "coven=prod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := &scopeCapturePool{}
			r := humaRoleRouter(t, strictAllowAll{}, nil, pool)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPatch, "/v1/roles/ops/permissions", strings.NewReader(tc.body))
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
			}
			if pool.scopeUpdateCalled != tc.wantScopeWrite {
				t.Fatalf("UPDATE default_scope вызван=%v, want %v (presence-конверт SetDefaultScope сломан)", pool.scopeUpdateCalled, tc.wantScopeWrite)
			}
			if tc.wantScopeWrite && pool.scopeArg != tc.wantScopeArg {
				t.Errorf("default_scope arg = %v, want %v (presence-конверт DefaultScope сломан)", pool.scopeArg, tc.wantScopeArg)
			}
		})
	}
}

// === GRANT OPERATOR (WRITE+AUDIT role.operator-granted) ===

func TestHumaRole_GrantOperator_204(t *testing.T) {
	r := humaRoleRouter(t, strictAllowAll{}, nil, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/roles/ops/operators", strings.NewReader(`{"aid":"archon-bob"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_RoleGrantOperator_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaRoleRouter(t, strictAllowAll{}, auditCap, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/roles/ops/operators", strings.NewReader(`{"aid":"archon-bob"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventRoleOperatorGranted, map[string]any{
		"name": "ops", "aid": "archon-bob", "granted_by_aid": "archon-alice",
	})
}

// TestHumaAudit_RoleGrantOperator_NoAudit_OnInvalidAID — пустой AID → 422
// (доменная валидация в GrantOperatorTyped), audit не пишется.
func TestHumaAudit_RoleGrantOperator_NoAudit_OnInvalidAID(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaRoleRouter(t, strictAllowAll{}, auditCap, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/roles/ops/operators", strings.NewReader(`{"aid":"bad aid!"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (битый AID); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на invalid-AID grant (%d событий)", len(auditCap.Events()))
	}
}

// === REVOKE OPERATOR (WRITE+AUDIT role.operator-revoked) ===

func TestHumaRole_RevokeOperator_204(t *testing.T) {
	r := humaRoleRouter(t, strictAllowAll{}, nil, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/roles/ops/operators/archon-bob", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHumaAudit_RoleRevokeOperator_RecordsOnSuccess(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaRoleRouter(t, strictAllowAll{}, auditCap, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/roles/ops/operators/archon-bob", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertAuditWritten(t, auditCap, audit.EventRoleOperatorRevoked, map[string]any{"name": "ops", "aid": "archon-bob"})
}

// TestHumaAudit_RoleRevokeOperator_NoAudit_OnInvalidAID — битый path-AID → 422,
// audit не пишется.
func TestHumaAudit_RoleRevokeOperator_NoAudit_OnInvalidAID(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaRoleRouter(t, strictAllowAll{}, auditCap, roleSuccessPool{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/roles/ops/operators/INVALID", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (битый path-AID); body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на invalid-AID revoke (%d событий)", len(auditCap.Events()))
	}
}

// === OpenAPI-фрагмент: ВСЕ role-операции из FULL-TYPED Go-типов ===

func TestHumaRole_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaRoleSpecYAML()
	if err != nil {
		t.Fatalf("HumaRoleSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma-фрагмент не несёт `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"createRole", "listRoles", "deleteRole", "updateRolePermissions",
		"grantRoleOperator", "revokeRoleOperator", "permissions", "default_scope",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI-фрагмент не содержит %q:\n%s", want, frag)
		}
	}
	// GOLDEN tier-инвариант (ADR-054 §Pattern третий tier): presence default_scope
	// несёт Optional[string], а НЕ RawBody []byte — поэтому ни один role-роут НЕ
	// тащит application/octet-stream в requestBody. Рецидив RawBody-моста сломал бы
	// web-codegen при мерже спеки (явный ассерт отсутствия артефакта).
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI-фрагмент несёт application/octet-stream (рецидив RawBody []byte-моста, ADR-054 §Pattern третий tier):\n%s", frag)
	}
}

// TestHumaRole_PatchPermissions_RequestBody_JSONOnly — GOLDEN requestBody-guard
// PATCH /v1/roles/{name}/permissions: тело — ТОЛЬКО application/json (presence
// default_scope в типе Optional[string]), БЕЗ application/octet-stream. Локализован
// на PATCH-роуте (а не на всём фрагменте), чтобы дрейф именно tier-эталона ловился
// адресно. default_scope — nullable string (3.1 `type: [string, null]`).
func TestHumaRole_PatchPermissions_RequestBody_JSONOnly(t *testing.T) {
	frag, err := HumaRoleSpecYAML()
	if err != nil {
		t.Fatalf("HumaRoleSpecYAML: %v", err)
	}
	// Тело PATCH-операции описано схемой RolePermissionsUpdateRequest (контрактное имя
	// рукописи после выравнивания N1); именно её requestBody-MIME сторожим. octet-stream
	// в ЛЮБОМ месте фрагмента — провал tier-а.
	if strings.Contains(frag, "octet-stream") {
		t.Fatalf("PATCH-permissions requestBody несёт application/octet-stream (Optional[string] обязан давать чистый application/json):\n%s", frag)
	}
	if !strings.Contains(frag, "application/json") {
		t.Errorf("PATCH-permissions requestBody не несёт application/json:\n%s", frag)
	}
	// default_scope — nullable string (presence-сброс через null), НЕ required.
	const bodySchemaName = "RolePermissionsUpdateRequest:"
	bodyIdx := strings.Index(frag, bodySchemaName)
	if bodyIdx < 0 {
		t.Fatalf("фрагмент не содержит схему %s\n%s", bodySchemaName, frag)
	}
	bodySection := frag[bodyIdx:]
	if !strings.Contains(bodySection, "- string") || !strings.Contains(bodySection, `- "null"`) {
		t.Errorf("default_scope в %s не nullable-string (ожидался `type: [string, null]`):\n%s", bodySchemaName, bodySection[:min(len(bodySection), 600)])
	}
}

// assertAuditWritten — общий ассерт S6-guard: ровно одно событие нужного типа от
// archon-alice (Source API) с НЕПУСТЫМ payload, содержащим заданные пары.
func assertAuditWritten(t *testing.T, cap *auditCaptureWriter, evt audit.EventType, wantPayload map[string]any) {
	t.Helper()
	evs := cap.Events()
	if len(evs) == 0 {
		t.Fatalf("audit НЕ записан на успешном write-роуте (S6-рецидив: huma-audit-middleware не довёл write-путь до audit, event=%s)", evt)
	}
	ev := evs[0]
	if ev.EventType != evt {
		t.Errorf("event_type = %q, want %q", ev.EventType, evt)
	}
	if ev.Source != audit.SourceAPI {
		t.Errorf("source = %q, want %q", ev.Source, audit.SourceAPI)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("archon_aid = %q, want archon-alice", ev.ArchonAID)
	}
	if len(ev.Payload) == 0 {
		t.Fatalf("audit payload пуст — huma-audit-middleware потерял доменный payload (carrier не пробросился), event=%s", evt)
	}
	for k, want := range wantPayload {
		if ev.Payload[k] != want {
			t.Errorf("audit payload[%q] = %v, want %v (event=%s, payload=%+v)", k, ev.Payload[k], want, evt, ev.Payload)
		}
	}
}
