package api

// Guard-тесты PILOT-1 разворота POST /v1/cadences на huma, FULL-TYPED форма
// (code-first, ADR-054 Amendment 2026-06-12). Доказывают, что huma-роут поверх chi
// сохраняет инварианты кластера, сосуществуя с oapi-strict-роутами, и что FULL-TYPED
// граница (typed Body + конверт + извлечённая CreateTyped) корректна:
//
//   - wire: 201 + Location + cadence_id (CreateTyped → typed output);
//   - unknown-field → 400 application/problem+json (теперь huma-нативно: huma
//     ловит additionalProperties:false, error-override классифицирует
//     "unexpected property" → 400, НЕ доменный DisallowUnknownFields);
//   - malformed-body → 400 application/problem+json (huma JSON-parse);
//   - missing-required (target) → 422 problem+json (huma `required:"true"`);
//   - RBAC-deny на cadence.create → 403 (навеска группы, huma её наследует);
//   - AUDIT (КРИТ, урок S6): cadence.create через huma пишет audit-event с непустым
//     payload (cadence self-audit ВНУТРИ CreateTyped → huma не задевает);
//   - no-audit-on-reject: 422 не пишет audit;
//   - OpenAPI-фрагмент: huma генерит 3.1-спеку POST /v1/cadences из FULL-TYPED
//     Go-типов;
//   - GOLDEN-JSON: 201-reply байт-в-байт == зафиксированный эталон (wire-регресс-
//     guard тиража: omitempty/nullable NextRunAt, []-vs-null).

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
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// humaCadenceRouter собирает chi-роутер с POST /v1/cadences через huma —
// продакшен-навеска буквально из router.go: RequirePermission(cadence.create) на
// группе + huma-операция внутри. installHumaErrorOverride вызывается явно (как в
// buildRouter — единая точка). injectClaims заменяет RequireJWT (валидный JWT не
// нужен — предмет проверки huma-обвязка, не auth). enforcer/auditW параметризованы.
func humaCadenceRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	store := &strictFakeCadenceStore{}
	cadenceH := handlers.NewCadenceHandler(store, nil, nil, enforcer, auditW, nil, 0, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/cadences", func(r chi.Router) {
			r.With(
				injectClaims,
				apimiddleware.RequirePermission(enforcer, "cadence", "create", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaCadence(newHumaCadenceAPI(r), cadenceH)
			})
		})
	})
	return r
}

// TestHumaCadence_Create_WireEquivalent — FULL-TYPED конверт даёт 201 + Location +
// cadence_id (CreateTyped → typed output), как прямой вызов handler-а.
func TestHumaCadence_Create_WireEquivalent(t *testing.T) {
	r := humaCadenceRouter(t, strictAllowAll{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(strictCadenceBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/v1/cadences/") {
		t.Errorf("Location = %q, want /v1/cadences/<id>", loc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var reply struct {
		CadenceID string `json:"cadence_id"`
		Name      string `json:"name"`
		Location  string `json:"location"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal reply: %v; body=%s", err, rec.Body.String())
	}
	if reply.CadenceID == "" || reply.Name != "hourly" || reply.Location == "" {
		t.Errorf("reply неполон: %+v", reply)
	}
}

// TestHumaCadence_Create_UnknownField_400 — КЛЮЧЕВОЙ инвариант FULL-TYPED: huma
// валидирует typed Body (additionalProperties:false ЧЕСТНЫЙ), unknown-поле ловит
// huma → "unexpected property" → error-override классифицирует 400
// application/problem+json (контракт кластера unknown→400, теперь huma-нативно).
func TestHumaCadence_Create_UnknownField_400(t *testing.T) {
	r := humaCadenceRouter(t, strictAllowAll{}, nil)

	body := `{"name":"x","schedule_kind":"cron","cron_expr":"0 * * * *","overlap_policy":"queue","kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]},"bogus_field":1}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown-field через huma additionalProperties:false); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaCadence_Create_MalformedBody_400 — битый JSON → 400 problem+json
// (huma JSON-parse, error-override 400 TypeMalformedRequest).
func TestHumaCadence_Create_MalformedBody_400(t *testing.T) {
	r := humaCadenceRouter(t, strictAllowAll{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(`{broken`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeMalformedRequest)
}

// TestHumaCadence_Create_MissingTarget_422 — required-валидация (target обязателен)
// теперь делает huma (`required:"true"`) → 422 problem+json. Доказывает, что
// missing-required отбивается huma-нативно (НЕ доменным кодом), статус совпадает с
// прежней доменной классификацией 422.
func TestHumaCadence_Create_MissingTarget_422(t *testing.T) {
	r := humaCadenceRouter(t, strictAllowAll{}, nil)

	body := `{"name":"x","schedule_kind":"cron","cron_expr":"0 * * * *","overlap_policy":"queue","kind":"command","module":"core.cmd.shell"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (missing target, huma required); body=%s", rec.Code, rec.Body.String())
	}
	assertHumaProblem(t, rec, problem.TypeValidationFailed)
}

// TestHumaCadence_Create_RBACDeny_403 — RequirePermission(cadence.create) на группе
// отбивает запрос ДО huma-handler-а (deny-enforcer) → 403. Доказывает, что навеска
// группы наследуется huma-роутом.
func TestHumaCadence_Create_RBACDeny_403(t *testing.T) {
	r := humaCadenceRouter(t, strictDenyAll{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(strictCadenceBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (RBAC-deny на cadence.create); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaCadence_Create_AuditRecorded — КРИТ guard (урок S6): cadence.create через
// huma пишет audit-event с непустым payload на успешном write-пути. Cadence self-
// audit (emitWrite ВНУТРИ CreateTyped) — FULL-TYPED извлечение его сохраняет; guard
// ловит регресс «huma проглотил/не довёл write-путь до audit».
func TestHumaCadence_Create_AuditRecorded(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRouter(t, strictAllowAll{}, auditCap)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(strictCadenceBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	evs := auditCap.Events()
	if len(evs) == 0 {
		t.Fatalf("audit НЕ записан на успешном 201 huma-роуте (huma сломал write-путь cadence.create)")
	}
	ev := evs[0]
	if ev.EventType != audit.EventCadenceCreated {
		t.Errorf("event_type = %q, want %q", ev.EventType, audit.EventCadenceCreated)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("archon_aid = %q, want archon-alice", ev.ArchonAID)
	}
	if len(ev.Payload) == 0 {
		t.Error("audit payload пуст — FULL-TYPED извлечение потеряло доменный payload")
	}
	if ev.Payload["cadence_id"] == nil || ev.Payload["name"] == nil {
		t.Errorf("audit payload без cadence_id/name: %+v", ev.Payload)
	}
}

// TestHumaCadence_NoAudit_OnReject — negative-guard: при huma-reject (missing target
// → 422) audit НЕ пишется (CreateTyped не доходит до emitWrite).
func TestHumaCadence_NoAudit_OnReject(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRouter(t, strictAllowAll{}, auditCap)

	body := `{"name":"x","schedule_kind":"cron","cron_expr":"0 * * * *","overlap_policy":"queue","kind":"command","module":"core.cmd.shell"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на rejected-запросе (%d событий) — write-путь не должен писать на 422", len(auditCap.Events()))
	}
}

// TestHumaCadence_OpenAPIFragment_3_1 — huma генерит OpenAPI-фрагмент POST
// /v1/cadences из FULL-TYPED Go-типов (code-first), версия 3.1.0 (huma-дефолт).
// Фрагмент несёт форму тела (required name/schedule_kind, enum kind) И форму ответа.
func TestHumaCadence_OpenAPIFragment_3_1(t *testing.T) {
	frag, err := HumaCadenceSpecYAML()
	if err != nil {
		t.Fatalf("HumaCadenceSpecYAML: %v", err)
	}
	if !strings.Contains(frag, "openapi: 3.1.0") {
		t.Errorf("huma-фрагмент не несёт `openapi: 3.1.0`:\n%s", frag)
	}
	for _, want := range []string{"createCadence", "getCadence", "listCadenceRuns", "patchCadence", "deleteCadence", "enableCadence", "disableCadence", "schedule_kind", "overlap_policy", "cadence_id"} {
		if !strings.Contains(frag, want) {
			t.Errorf("OpenAPI-фрагмент не содержит %q (форма из Go-типов потеряна):\n%s", want, frag)
		}
	}
}

// TestHumaCadence_Create_GoldenWire — GOLDEN-JSON snapshot (wire-регресс-guard
// тиража, КРИТ): 201-reply huma-output, проведённый через map→sorted-marshal, ==
// зафиксированный эталон. Guard фиксирует НАБОР ключей, omitempty/nullable
// (NextRunAt отсутствует при nil — strictFakeCadenceStore не возвращает next_run_at)
// и отсутствие лишних полей ($schema). Порядок ключей для JSON не семантичен и
// нормализуется ремаршалом через map. cadence_id нормализуется (ULID
// недетерминирован) перед сравнением.
func TestHumaCadence_Create_GoldenWire(t *testing.T) {
	r := humaCadenceRouter(t, strictAllowAll{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences", strings.NewReader(strictCadenceBody))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	// Нормализация недетерминированного ULID (cadence_id + хвост location) и
	// timestamp next_run_at на плейсхолдеры — иначе golden недостижим. Присутствие/
	// отсутствие ключей (omitempty) и форма сохраняются: тело cron-расписания несёт
	// next_run_at (resolved next-hour) — golden фиксирует ЕГО ПРИСУТСТВИЕ как
	// nullable-ключ (если CadenceCreateReply потеряет omitempty/тип — дрейф).
	got := normalizeCadenceWire(t, rec.Body.Bytes())

	const golden = `{"cadence_id":"_ULID_","enabled":true,"location":"/v1/cadences/_ULID_","name":"hourly","next_run_at":"_TS_"}`
	if got != golden {
		t.Errorf("GOLDEN wire-дрейф FULL-TYPED reply:\n got  = %s\n want = %s\n(набор ключей/omitempty/nullable/наличие $schema изменился — проверь CadenceCreateReply и newHumaCadenceAPI)", got, golden)
	}
}

// normalizeCadenceWire перекладывает reply через map → канонический marshal (ключи
// сортируются) и заменяет недетерминированные значения (cadence_id + хвост location
// → "<ULID>"; next_run_at-timestamp → "<TS>") на плейсхолдеры. Сохраняет
// присутствие/отсутствие ключей (omitempty) и любые лишние поля (например, $schema —
// сразу всплывёт в diff). Так golden фиксирует ФОРМУ, не значения.
func normalizeCadenceWire(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; raw=%s", err, raw)
	}
	if id, ok := m["cadence_id"].(string); ok && id != "" {
		m["cadence_id"] = "_ULID_"
		if loc, ok := m["location"].(string); ok {
			m["location"] = strings.Replace(loc, id, "_ULID_", 1)
		}
	}
	if _, ok := m["next_run_at"]; ok {
		m["next_run_at"] = "_TS_" // фиксируем присутствие nullable-ключа, не значение
	}
	out, err := json.Marshal(m) // json.Marshal map → ключи отсортированы (детерминизм)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}

// assertHumaProblem проверяет Content-Type=application/problem+json и type-URN.
func assertHumaProblem(t *testing.T, rec *httptest.ResponseRecorder, wantType string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q; body=%s", ct, problem.ContentType, rec.Body.String())
	}
	var p problem.Details
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("тело не problem+json: %v; body=%s", err, rec.Body.String())
	}
	if p.Type != wantType {
		t.Errorf("problem type = %q, want %q", p.Type, wantType)
	}
}

// strictDenyAll — deny-all PermissionChecker (RBAC-deny guard). Любой не-nil error
// от Check → RequirePermission отдаёт 403 (см. rbac.go).
type strictDenyAll struct{}

func (strictDenyAll) Check(string, string, string, map[string]string) error {
	return rbac.ErrPermissionDenied
}
