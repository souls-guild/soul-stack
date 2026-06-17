package api

// Guard-набор ЧЕТВЁРТОГО tier ADR-054 (read-with-typed-query) для GET /v1/audit на
// huma full-typed. Доказывает КОНТРАКТ-инвариант 400/422 (решение A 2026-06-13,
// продолжение ADR-051 Amendment), который раньше держала strict bind-фаза:
//
//   - bad date-time (started_after) → 400 problem+json (huma parseInto → error-
//     override hasQueryParseError);
//   - bad int (offset/limit) → 400 (тот же parse-детект);
//   - КЛЮЧЕВОЙ: bad source-enum → 422 (schema-validate enum-mismatch — другой Message,
//     в parse-набор НЕ попадает → дефолтная 422-ветка). Доказывает, что детект
//     отличает parse-error от enum-mismatch на одинаковом query.-Location;
//   - valid date-time/enum/pagination → 200 + golden envelope {items,offset,limit,total}
//     байт-в-байт (PagedResponse-эквивалентность сохранена);
//   - OpenAPI-фрагмент: typed query-params (date-time/int/enum), нет requestBody у GET.
//
// Роутер собирается продакшен-навеской буквально из router.go: RequirePermission(
// audit.read) на группе + huma-операция, БЕЗ huma-audit-middleware (READ). pool —
// auditpg.Reader поверх q400ListPool (COUNT→0, SELECT→пусто). cfg.CreateHooks=nil
// держит newHumaCadenceAPI (без $schema-украшения тела).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// humaAuditRouter собирает chi-роутер с GET /v1/audit через huma — навеска из
// router.go: RequirePermission(audit.read) на группе + huma-операция (READ, без
// audit-middleware). installHumaErrorOverride явно. injectClaims заменяет RequireJWT.
func humaAuditRouter(t *testing.T, enforcer apimiddleware.PermissionChecker) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	auditH := handlers.NewAuditHandler(auditpg.NewReader(q400ListPool{}), nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "audit", "read", apimiddleware.NoSelector)).Group(func(r chi.Router) {
			registerHumaAuditList(newHumaCadenceAPI(r), auditH)
		})
	})
	return r
}

func auditGet(t *testing.T, r *chi.Mux, rawurl string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, rawurl, http.NoBody)
	r.ServeHTTP(rec, req)
	return rec
}

// TestHumaAudit_BadStartedAfter_400 — bad date-time typed-query → 400 problem+json
// (huma parseInto "invalid date/time …" → hasQueryParseError).
func TestHumaAudit_BadStartedAfter_400(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?started_after=yesterday")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad started_after → parse-детект 400); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaAudit_BadOffset_400 — bad int offset → 400 (parseInto "invalid integer").
func TestHumaAudit_BadOffset_400(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?offset=notanint")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad offset → parse-детект 400); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaAudit_BadLimit_400 — bad int limit → 400 (parseInto "invalid integer").
func TestHumaAudit_BadLimit_400(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?limit=abc")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad limit → parse-детект 400); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaAudit_OutOfRangePagination_400 — КОНТРАКТ-инвариант границ (решение A):
// out-of-range offset/limit → 400 (api.CheckPageBounds в ListTyped), а НЕ 422
// (huma НЕ несёт schema-minimum/maximum). Должно совпадать с легаси/strict ParsePage
// (limit=0/1001/offset<0 → 400), иначе wire-change.
func TestHumaAudit_OutOfRangePagination_400(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	for _, c := range []string{
		"/v1/audit?limit=0",
		"/v1/audit?limit=1001",
		"/v1/audit?offset=-1",
	} {
		rec := auditGet(t, r, c)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (out-of-range → 400, parity ParsePage, НЕ huma-422); body=%s", c, rec.Code, rec.Body.String())
			continue
		}
		assertHumaProblem(t, rec, problem.TypeMalformedRequest)
	}
}

// TestHumaAudit_BadSource_422 — КЛЮЧЕВОЙ guard контракта: bad source-enum → 422
// (schema-validate enum-mismatch — Message "expected value to be one of …", НЕ в
// parse-наборе → дефолтная 422-ветка). Доказывает, что parse-детект НЕ ловит enum:
// одинаковый query.-Location, дискриминатор — Message-литерал.
func TestHumaAudit_BadSource_422(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?source=hax0r")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad source-enum → 422, parse-детект его НЕ ловит); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

// TestHumaAudit_ConfigBootstrapSource_200 — РЕГРЕССИЯ-guard MAJOR (адверс-верификация
// 2026-06-13): config_bootstrap — валидный source (audit.Source.Valid() его принимает,
// реально эмитится push/auto_import.go). Эталонный enum-тег его опускал → huma отбивал
// бы рабочий фильтр на 422 (wire-regression 200→422 vs легаси strict, который enum на
// bind НЕ валидировал). Фикс: enum-тег = ПОЛНЫЙ доменный valid-set. Guard ассертит 200
// (НЕ 422) — config_bootstrap проходит huma-enum И доменную Source.Valid().
func TestHumaAudit_ConfigBootstrapSource_200(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?source=config_bootstrap")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (config_bootstrap — валидный source, НЕ 422; enum-тег = домен-valid-set); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaAudit_ValidFilters_200 — valid date-time/enum/pagination проходят bind →
// ListTyped (пустой reader) → 200 + golden envelope {items,offset,limit,total}
// байт-в-байт. items=[] (non-nil), пагинация = переданные значения.
func TestHumaAudit_ValidFilters_200(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit?source=api&source=mcp&started_after=2026-05-25T00:00:00Z&offset=10&limit=20")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// Ремаршал через map → детерминированный порядок ключей; golden фиксирует
	// набор/форму envelope (items=[] non-nil, offset/limit echo, total=0).
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[],"limit":20,"offset":10,"total":0}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN wire-дрейф audit-envelope:\n got  = %s\n want = %s", got, golden)
	}
}

// TestHumaAudit_DefaultPagination_200 — опущенные offset/limit → default (0/50),
// совпадающие с shared/api.ParsePage (иначе wire-change). Подтверждает границы.
func TestHumaAudit_DefaultPagination_200(t *testing.T) {
	r := humaAuditRouter(t, strictAllowAll{})
	rec := auditGet(t, r, "/v1/audit")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; body=%s", err, rec.Body.String())
	}
	out, _ := json.Marshal(m)
	const golden = `{"items":[],"limit":50,"offset":0,"total":0}`
	if got := string(out); got != golden {
		t.Errorf("GOLDEN дрейф default-пагинации (должно совпадать с ParsePage offset=0 limit=50):\n got  = %s\n want = %s", got, golden)
	}
}

// TestHumaAudit_RBACDeny_403 — RBAC-deny на группе наследуется huma (403 ДО bind).
func TestHumaAudit_RBACDeny_403(t *testing.T) {
	r := humaAuditRouter(t, strictDenyAll{})
	rec := auditGet(t, r, "/v1/audit")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaAudit_OpenAPIFragment_3_1 — фрагмент из FULL-TYPED Go-типов: typed
// query-params (date-time/int/enum), нет requestBody у GET.
func TestHumaAudit_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaAuditSpecYAML()
	if err != nil {
		t.Fatalf("HumaAuditSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma-фрагмент не несёт `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{
		"listAuditEvents",
		"in: query",
		"started_after",
		"date-time",
		"keeper_internal",  // source-enum значение
		"config_bootstrap", // MAJOR-фикс: enum-тег = домен-valid-set (включает config_bootstrap)
		"offset",
		"limit",
		"explode: true", // MINOR-фикс: multi-value []string-query (type/source) несут explode:true
		"int32",         // MINOR-фикс: offset/limit — int32 (match committed OffsetQuery/LimitQuery), НЕ int64
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI-фрагмент не содержит %q:\n%s", want, frag)
		}
	}
	// MINOR-3 scope — ИМЕННО query-параметры offset/limit обязаны быть int32 (match
	// committed OffsetQuery/LimitQuery). Негатив на int64 — ТОЛЬКО по блоку
	// `parameters:` операции (между `parameters:` и `responses:`); body-envelope
	// AuditEventListReply (items/offset/limit/total) и shared HumaProblemError.status
	// несут int64 как Go-int response-схема — это НЕ предмет query-tier-фикса.
	_, afterParams, hasParams := strings.Cut(frag, "parameters:")
	paramsBlock, _, hasResponses := strings.Cut(afterParams, "responses:")
	if !hasParams || !hasResponses {
		t.Fatalf("во фрагменте нет блока `parameters:`…`responses:` — структура операции изменилась, негатив-проверка int64 невалидна:\n%s", frag)
	}
	if strings.Contains(paramsBlock, "int64") {
		t.Errorf("query-параметры операции несут int64 (offset/limit обязаны быть int32):\n%s", paramsBlock)
	}
	// GET без тела: requestBody не должен присутствовать в операции.
	if strings.Contains(frag, "requestBody") {
		t.Errorf("GET /v1/audit фрагмент несёт requestBody (у GET тела быть не должно):\n%s", frag)
	}
	// четвёртый tier не тащит RawBody octet-stream артефакт.
	if strings.Contains(frag, "octet-stream") {
		t.Errorf("OpenAPI-фрагмент несёт application/octet-stream:\n%s", frag)
	}
}
