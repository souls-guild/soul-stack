package api

// Guard-тесты cadence-rest (PATCH/DELETE/enable/disable) на huma, FULL-TYPED форма
// WRITE-SELF-AUDIT (батч-2f, ADR-054). Эти роуты пишут audit ВНУТРИ handler-а
// (PatchTyped→emitWrite / DeleteTyped→emitDeleted / SetEnabledTyped→emitEnabledToggle),
// БЕЗ audit-middleware — отличие от middleware-audit-доменов role/operator. Guard-ы
// доказывают: wire 200/204, S6-SELF-AUDIT (handler РЕАЛЬНО пишет event с непустым
// payload на 2xx — как cadence pilot create), NoAudit на 404/403, golden-JSON byte-
// exact, RBAC-deny→403.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestHumaCadence_RestReachable_ChiCoexistence — guard на ДОСТИЖИМОСТЬ
// cadence-rest huma-роутов (PATCH/DELETE + GET/{id}+/runs) на РЕАЛЬНОМ
// buildRouter с НЕ-nil cadenceH (и choirH заодно). Прогон через реальный
// chi-mux (НЕ изолированную тест-топологию humaCadenceRestRouter): chi ДОЛЖЕН
// диспатчить запрос узла /{id} в huma-handler по методу (PATCH/DELETE/GET),
// а не отдавать 405. До фикса блокера sibling-саброутер r.Route("/{id}") c
// GET / + GET /runs затенял весь /{id}-узел → chi отдавал ему PATCH/DELETE → 405
// (huma-op никогда не вызывался). Тест ловит регресс топологии: чужой chi.Route
// на bare-параметре-узле /{id} рядом с huma-op на том же узле.
//
// Проверка через chi.Walk (обход router-tree), НЕ Match/ServeHTTP: enforcer=nil +
// verifier=nil (как TestHumaSoul_Exec_ChiCoexistence) — request-путь упёрся бы в
// 401 (auth-chain без verifier) ДО RBAC/huma, а chi.Routes.Match даёт FALSE-TRUE на
// затенённом узле (узел /{id} существует от sibling-саброутера, Match рапортует
// его pattern, но фактический handler-узла отвечает только методам саброутера →
// в ServeHTTP это 405). Достоверный сигнал — РЕГИСТРАЦИЯ метода в дереве: chi.Walk
// перечисляет именно зарегистрированные method+pattern. Если PATCH/DELETE
// /v1/cadences/{id} не в обходе — huma-op не смонтирован → в проде 405. Тест
// требует все четыре /{id}-метода (GET/{id}, GET/{id}/runs, PATCH, DELETE) ровно
// по разу. До фикса блокера PATCH/DELETE отсутствуют в обходе (sibling chi.Route
// поглотил /{id}-узел) → тест КРАСНЫЙ.
func TestHumaCadence_RestReachable_ChiCoexistence(t *testing.T) {
	cadenceH := handlers.NewCadenceHandler(foundCadenceStore(), nil, nil, nil, nil, nil, 0, nil)
	h := buildRouter(
		nil, // verifier
		nil, // healthH
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		stubRoleHandler(t), stubSynodHandler(t), stubSigilHandler(t), stubSigilKeyHandler(t),
		stubServiceHandler(t), nil, stubAugurHandler(t), stubOracleHandler(t),
		nil,                                     // pushH
		nil,                                     // pushProviderH
		nil,                                     // errandH
		nil,                                     // voyageH
		cadenceH,                                // cadenceH non-nil → cadence /{id}-роуты монтируются
		nil,                                     // auditH
		handlers.NewChoirHandler(nil, nil, nil), // choirH non-nil заодно
		nil,                                     // heraldH
		handlers.NewModuleCatalogHandler(nil, nil),
		handlers.NewModuleFormPrepHandler(nil, nil),
		handlers.NewPermissionCatalogHandler(nil),
		handlers.NewEventTypeCatalogHandler(nil),
		handlers.NewMyPermissionsHandler(nil, nil),
		nil,   // enforcer (nil: RBAC не дёргается — проверка через router-tree)
		nil,   // auditWriter
		nil,   // metricsHTTP
		nil,   // tollDegraded
		nil,   // tempoLimiter
		nil,   // tempoMetrics
		nil,   // tempoVoyageCreateLimits
		nil,   // tempoVoyagePreviewLimits
		false, // webUIEnabled — /ui вне интереса cadence-роутинг-теста
		nil,   // ldapAuth (LDAP не сконфигурирован в тесте)
		nil,   // oidcAuth (OIDC не сконфигурирован в тесте)
		nil,                                  // loginGuard (anti-bruteforce off в тесте)
		apimiddleware.AuthLoginLimitConfig{}, // loginLimitCfg
		nil,                                  // logger
	)
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("buildRouter вернул %T, не chi.Routes", h)
	}

	// chi.Walk: каждый /{id}-метод зарегистрирован ровно по разу. Отсутствие
	// PATCH/DELETE = блокер (затенение sibling chi.Route); дубль = коллизия mount-а.
	// Teardown T1: GET /v1/cadences (list) на руте группы — отдельная huma-op; не
	// затеняет /{id}-узел и не конфликтует с POST / (create). getListCount=1 +
	// /{id}-узлы целы = list смонтирован достижимо БЕЗ shadow.
	var patchCount, deleteCount, getIDCount, getRunsCount, getListCount int
	if err := chi.Walk(routes, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		switch {
		case method == http.MethodPatch && pattern == "/v1/cadences/{id}":
			patchCount++
		case method == http.MethodDelete && pattern == "/v1/cadences/{id}":
			deleteCount++
		case method == http.MethodGet && pattern == "/v1/cadences/{id}":
			getIDCount++
		case method == http.MethodGet && pattern == "/v1/cadences/{id}/runs":
			getRunsCount++
		case method == http.MethodGet && pattern == "/v1/cadences/":
			getListCount++
		}
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if patchCount != 1 {
		t.Errorf("PATCH /v1/cadences/{id} встретился %d раз, want 1 (0 = блокер: sibling chi.Route затеняет → 405)", patchCount)
	}
	if deleteCount != 1 {
		t.Errorf("DELETE /v1/cadences/{id} встретился %d раз, want 1 (0 = блокер: sibling chi.Route затеняет → 405)", deleteCount)
	}
	if getIDCount != 1 {
		t.Errorf("GET /v1/cadences/{id} встретился %d раз, want 1 (read-роут на huma)", getIDCount)
	}
	if getRunsCount != 1 {
		t.Errorf("GET /v1/cadences/{id}/runs встретился %d раз, want 1 (read-роут на huma)", getRunsCount)
	}
	if getListCount != 1 {
		t.Errorf("GET /v1/cadences/ встретился %d раз, want 1 (Teardown T1: list-роут на huma, БЕЗ shadow с /{id})", getListCount)
	}
}

// cadenceRestTestID — валидный ULID (26 Crockford-base32) для path-{id} guard-ов.
const cadenceRestTestID = "01HZ0000000000000000000000"

// cadenceRestStoredRow — pgx.Row под cadence.scanCadence (27 dest, parity
// handler-теста cadenceFullRow): минимальная stored-строка command-kind (cron +
// service-target), достаточная для round-trip PATCH/Get. service-target +
// incReader=nil → per-target scope пропущен после bare-check (parity newCadenceHandler).
type cadenceRestStoredRow struct{ id string }

func (r cadenceRestStoredRow) Scan(dest ...any) error {
	now := time.Now().UTC()
	*dest[0].(*string) = r.id
	*dest[1].(*string) = "hourly"
	*dest[2].(*bool) = true
	*dest[3].(*string) = "cron"
	*dest[4].(**int) = nil // interval_seconds
	cron := "0 * * * *"
	*dest[5].(**string) = &cron // cron_expr
	*dest[6].(*string) = "queue"
	*dest[7].(*string) = "command"
	*dest[8].(**string) = nil // scenario_name
	mod := "core.cmd.shell"
	*dest[9].(**string) = &mod // module
	*dest[10].(*json.RawMessage) = json.RawMessage(`{"coven":["prod"]}`)
	*dest[11].(*[]byte) = []byte(`{}`)
	*dest[12].(**string) = nil // batch_mode
	*dest[13].(**int) = nil    // batch_size
	*dest[14].(**int) = nil    // batch_percent
	*dest[15].(**int) = nil    // concurrency
	*dest[16].(**int) = nil    // fail_threshold
	*dest[17].(**int) = nil    // fail_threshold_percent
	*dest[18].(**float64) = nil
	*dest[19].(**float64) = nil
	*dest[20].(**bool) = nil   // require_alive
	*dest[21].(**string) = nil // on_failure
	*dest[22].(**time.Time) = nil
	*dest[23].(**time.Time) = nil
	*dest[24].(*string) = "archon-alice"
	*dest[25].(*time.Time) = now
	*dest[26].(*time.Time) = now
	return nil
}

// humaCadenceRestRouter монтирует cadence-rest huma-роуты (PATCH/DELETE/enable/
// disable) ровно по навеске router.go: RequirePermission на каждой группе +
// huma-операция с ПОЛНЫМ путём /{id}[/...] на группе /v1/cadences. store/enforcer/
// auditW параметризованы; selectByID настраивается под кейс (found/not-found).
func humaCadenceRestRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, auditW audit.Writer, store *strictFakeCadenceStore) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
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
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "cadence", "list", apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceGet(newHumaCadenceAPI(r), cadenceH) })
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "incarnation", "history", apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceRuns(newHumaCadenceAPI(r), cadenceH) })
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "cadence", "update", apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadencePatch(newHumaCadenceAPI(r), cadenceH) })
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "cadence", "delete", apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceDelete(newHumaCadenceAPI(r), cadenceH) })
			r.With(injectClaims, apimiddleware.RequireAnyPermission(enforcer, "cadence", []string{"enable", "update"}, apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceEnable(newHumaCadenceAPI(r), cadenceH) })
			r.With(injectClaims, apimiddleware.RequireAnyPermission(enforcer, "cadence", []string{"disable", "update"}, apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceDisable(newHumaCadenceAPI(r), cadenceH) })
		})
	})
	return r
}

// foundCadenceStore — store, отдающий stored-строку на Get (PATCH/enable/delete OK).
func foundCadenceStore() *strictFakeCadenceStore {
	return &strictFakeCadenceStore{selectByID: func(id string) pgx.Row { return cadenceRestStoredRow{id: id} }}
}

// --- PATCH ---

// TestHumaCadenceRest_Patch_WireOK — PATCH 200 + cadenceDTO (read-modify-write через
// PatchTyped).
func TestHumaCadenceRest_Patch_WireOK(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/cadences/"+cadenceRestTestID, strings.NewReader(`{"name":"renamed"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var dto struct {
		CadenceID string `json:"cadence_id"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if dto.CadenceID != cadenceRestTestID || dto.Name != "renamed" || dto.Kind != "command" {
		t.Errorf("dto = %+v, want cadence_id=%s name=renamed kind=command", dto, cadenceRestTestID)
	}
}

// TestHumaCadenceRest_Patch_SelfAuditRecorded — S6-SELF-AUDIT-GUARD: PATCH через huma
// пишет cadence.updated с непустым payload ВНУТРИ PatchTyped (handler, НЕ middleware).
func TestHumaCadenceRest_Patch_SelfAuditRecorded(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/cadences/"+cadenceRestTestID, strings.NewReader(`{"name":"renamed"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertSelfAudit(t, auditCap, audit.EventCadenceUpdated, "cadence_id")
}

// TestHumaCadenceRest_Patch_NotFound_NoAudit — 404 (cadence нет) НЕ пишет audit
// (PatchTyped не доходит до emitWrite).
func TestHumaCadenceRest_Patch_NotFound_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, &strictFakeCadenceStore{}) // selectByID=nil → 404
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/cadences/"+cadenceRestTestID, strings.NewReader(`{"name":"x"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 404 (%d событий) — write-путь не должен писать", len(auditCap.Events()))
	}
}

// TestHumaCadenceRest_Patch_RBACDeny_403 — RequirePermission(cadence.update) отбивает
// до huma-handler-а → 403, без audit.
func TestHumaCadenceRest_Patch_RBACDeny_403(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictDenyAll{}, auditCap, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/cadences/"+cadenceRestTestID, strings.NewReader(`{"name":"x"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 403 — RBAC-deny не должен доходить до handler-а")
	}
}

// TestHumaCadenceRest_Patch_UnknownField_400 — additionalProperties:false (huma) →
// unknown-поле → 400.
func TestHumaCadenceRest_Patch_UnknownField_400(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/cadences/"+cadenceRestTestID, strings.NewReader(`{"bogus":1}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown field); body=%s", rec.Code, rec.Body.String())
	}
}

// --- DELETE ---

// TestHumaCadenceRest_Delete_WireOK_204 — DELETE 204 No Content.
func TestHumaCadenceRest_Delete_WireOK_204(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 с непустым телом: %s", rec.Body.String())
	}
}

// TestHumaCadenceRest_Delete_SelfAuditRecorded — S6-SELF-AUDIT-GUARD: DELETE пишет
// cadence.deleted ВНУТРИ DeleteTyped.
func TestHumaCadenceRest_Delete_SelfAuditRecorded(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertSelfAudit(t, auditCap, audit.EventCadenceDeleted, "cadence_id")
}

// TestHumaCadenceRest_Delete_NotFound_NoAudit — DELETE 0 строк → 404, без audit.
func TestHumaCadenceRest_Delete_NotFound_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	store := &strictFakeCadenceStore{deleteNoRow: true}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 404 delete — write-путь не должен писать")
	}
}

// --- enable/disable ---

// TestHumaCadenceRest_Enable_WireAndAudit — enable 200 + {cadence_id,enabled:true} +
// S6-SELF-AUDIT cadence.updated.
func TestHumaCadenceRest_Enable_WireAndAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences/"+cadenceRestTestID+"/enable", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		CadenceID string `json:"cadence_id"`
		Enabled   bool   `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if reply.CadenceID != cadenceRestTestID || !reply.Enabled {
		t.Errorf("reply = %+v, want cadence_id=%s enabled=true", reply, cadenceRestTestID)
	}
	assertSelfAudit(t, auditCap, audit.EventCadenceUpdated, "cadence_id")
}

// TestHumaCadenceRest_Disable_WireAndAudit — disable 200 + enabled:false + audit.
func TestHumaCadenceRest_Disable_WireAndAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences/"+cadenceRestTestID+"/disable", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.Enabled {
		t.Errorf("disable enabled = true, want false")
	}
	assertSelfAudit(t, auditCap, audit.EventCadenceUpdated, "cadence_id")
}

// TestHumaCadenceRest_Enable_NotFound_NoAudit — enable несуществующего → 404, без audit.
func TestHumaCadenceRest_Enable_NotFound_NoAudit(t *testing.T) {
	auditCap := &auditCaptureWriter{}
	store := &strictFakeCadenceStore{setEnabledNoRow: true}
	r := humaCadenceRestRouter(t, strictAllowAll{}, auditCap, store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences/"+cadenceRestTestID+"/enable", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(auditCap.Events()) != 0 {
		t.Errorf("audit записан на 404 enable")
	}
}

// TestHumaCadenceRest_Enable_GoldenWire — golden-JSON byte-exact 200-reply enable.
func TestHumaCadenceRest_Enable_GoldenWire(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cadences/"+cadenceRestTestID+"/enable", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := normalizeJSONKeys(t, rec.Body.Bytes())
	const golden = `{"cadence_id":"01HZ0000000000000000000000","enabled":true}`
	if got != golden {
		t.Errorf("GOLDEN wire-дрейф enable-reply:\n got  = %s\n want = %s\n(набор ключей/$schema изменился — проверь cadenceToggleOutput/newHumaCadenceAPI)", got, golden)
	}
}

// --- GET /{id} (read) ---

// TestHumaCadenceRest_Get_WireOK — GET /{id} 200 + cadenceDTO (read-tier huma,
// БЕЗ audit). Доказывает достижимость read-роута на huma (часть фикса блокера).
func TestHumaCadenceRest_Get_WireOK(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var dto struct {
		CadenceID string `json:"cadence_id"`
		Kind      string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if dto.CadenceID != cadenceRestTestID || dto.Kind != "command" {
		t.Errorf("dto = %+v, want cadence_id=%s kind=command", dto, cadenceRestTestID)
	}
}

// TestHumaCadenceRest_Get_NotFound_404 — GET /{id} несуществующего → 404 (read-роут).
func TestHumaCadenceRest_Get_NotFound_404(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, &strictFakeCadenceStore{}) // selectByID=nil → 404
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaCadenceRest_Get_GoldenWire — golden-JSON byte-exact GET /{id} 200-reply.
// Ключи/omitempty/отсутствие $schema зафиксированы. Недетерминированные поля
// (created_at/updated_at) нормализованы; cadence_id фиксирован (path-{id}). Эталон
// совпадает с legacy strict GetCadence (та же toCadenceDTO-форма).
func TestHumaCadenceRest_Get_GoldenWire(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences/"+cadenceRestTestID, http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := normalizeCadenceTimestamps(t, rec.Body.Bytes())
	const golden = `{"cadence_id":"01HZ0000000000000000000000","created_at":"TS","created_by_aid":"archon-alice","cron_expr":"0 * * * *","enabled":true,"kind":"command","module":"core.cmd.shell","name":"hourly","overlap_policy":"queue","schedule_kind":"cron","target":{"coven":["prod"]},"updated_at":"TS"}`
	if got != golden {
		t.Errorf("GOLDEN wire-дрейф GET {id}:\n got  = %s\n want = %s\n(набор ключей/$schema изменился — проверь cadenceGetOutput/toCadenceDTO)", got, golden)
	}
}

// TestHumaCadenceRest_Runs_GoldenWire — golden-JSON byte-exact GET /{id}/runs 200
// (пустой набор: store отдаёт 0 прогонов). Фиксирует envelope-форму
// items/offset/limit/total (sharedapi.PagedResponse) == legacy strict ListCadenceRuns.
func TestHumaCadenceRest_Runs_GoldenWire(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences/"+cadenceRestTestID+"/runs", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := normalizeJSONKeys(t, rec.Body.Bytes())
	const golden = `{"items":[],"limit":50,"offset":0,"total":0}`
	if got != golden {
		t.Errorf("GOLDEN wire-дрейф GET {id}/runs:\n got  = %s\n want = %s\n(envelope-форма изменилась — проверь cadenceRunsOutput/CadenceRunsReply)", got, golden)
	}
}

// TestHumaCadenceRest_Runs_BadLimit_400 — out-of-range limit → 400 (CheckPageBounds в
// RunsTyped, parity legacy ParsePage; НЕ huma min/max).
func TestHumaCadenceRest_Runs_BadLimit_400(t *testing.T) {
	r := humaCadenceRestRouter(t, strictAllowAll{}, nil, foundCadenceStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences/"+cadenceRestTestID+"/runs?limit=99999", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (out-of-range limit); body=%s", rec.Code, rec.Body.String())
	}
}

// --- GET / (list) — Teardown T1 ---

// humaCadenceListRouter монтирует GET /v1/cadences (list) ровно по навеске router.go:
// RequirePermission(cadence.list) на группе + huma-операция с path "/" на группе
// /v1/cadences. store/enforcer параметризованы.
func humaCadenceListRouter(t *testing.T, enforcer apimiddleware.PermissionChecker, store *strictFakeCadenceStore) *chi.Mux {
	t.Helper()
	installHumaErrorOverride()
	cadenceH := handlers.NewCadenceHandler(store, nil, nil, enforcer, nil, nil, 0, nil)

	r := chi.NewRouter()
	injectClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := apimiddleware.InjectClaimsForTest(req.Context(), &keeperjwt.Claims{Subject: "archon-alice"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	r.Route("/v1", func(r chi.Router) {
		r.Route("/cadences", func(r chi.Router) {
			r.With(injectClaims, apimiddleware.RequirePermission(enforcer, "cadence", "list", apimiddleware.NoSelector)).
				Group(func(r chi.Router) { registerHumaCadenceList(newHumaCadenceAPI(r), cadenceH) })
		})
	})
	return r
}

// TestHumaCadenceRest_List_GoldenWire — golden-JSON byte-exact GET /v1/cadences 200
// (пустой набор: store отдаёт 0 строк/COUNT=0). Фиксирует envelope-форму
// items/offset/limit/total (sharedapi.PagedResponse) == legacy strict ListCadences:
// items non-nil [], никакого $schema, дефолтные offset=0/limit=50.
func TestHumaCadenceRest_List_GoldenWire(t *testing.T) {
	r := humaCadenceListRouter(t, strictAllowAll{}, &strictFakeCadenceStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := normalizeJSONKeys(t, rec.Body.Bytes())
	const golden = `{"items":[],"limit":50,"offset":0,"total":0}`
	if got != golden {
		t.Errorf("GOLDEN wire-дрейф GET /v1/cadences (list):\n got  = %s\n want = %s\n(envelope-форма изменилась — проверь cadenceListOutput/CadenceListReply)", got, golden)
	}
}

// TestHumaCadenceRest_List_BadLimit_400 — out-of-range limit → 400 (CheckPageBounds в
// ListTyped, parity legacy ParsePage; НЕ huma min/max).
func TestHumaCadenceRest_List_BadLimit_400(t *testing.T) {
	r := humaCadenceListRouter(t, strictAllowAll{}, &strictFakeCadenceStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences?limit=99999", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (out-of-range limit); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaCadenceRest_List_BadEnabledEnum_422 — enabled вне {true,false} → 422
// (huma schema-validate enum-mismatch; parity legacy "query 'enabled' must be …").
func TestHumaCadenceRest_List_BadEnabledEnum_422(t *testing.T) {
	r := humaCadenceListRouter(t, strictAllowAll{}, &strictFakeCadenceStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences?enabled=maybe", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad enabled enum); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaCadenceRest_List_BadKindEnum_422 — kind вне {scenario,command} → 422
// (huma schema-validate enum-mismatch; parity legacy ValidKind → 422).
func TestHumaCadenceRest_List_BadKindEnum_422(t *testing.T) {
	r := humaCadenceListRouter(t, strictAllowAll{}, &strictFakeCadenceStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences?kind=bogus", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad kind enum); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHumaCadenceRest_List_RBACDeny_403 — RequirePermission(cadence.list) отбивает до
// huma-handler-а → 403.
func TestHumaCadenceRest_List_RBACDeny_403(t *testing.T) {
	r := humaCadenceListRouter(t, strictDenyAll{}, &strictFakeCadenceStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/cadences", http.NoBody)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// normalizeCadenceTimestamps заменяет недетерминированные created_at/updated_at на
// плейсхолдер "TS" и переливает через map → sorted-marshal (golden byte-exact с
// фиксированным набором ключей; created_at/updated_at в эталоне = "TS"). Плейсхолдер
// без спец-символов — json.Marshal не HTML-эскейпит его.
func normalizeCadenceTimestamps(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; raw=%s", err, raw)
	}
	for _, k := range []string{"created_at", "updated_at"} {
		if _, ok := m[k]; ok {
			m[k] = "TS"
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}

// assertSelfAudit — общий S6-SELF-AUDIT-GUARD: capture-writer получил РОВНО event
// нужного типа с непустым payload, содержащим requiredKey, от archon-alice. Доказывает,
// что huma-роут довёл self-audit (emit ВНУТРИ handler-а) до writer-а на 2xx.
func assertSelfAudit(t *testing.T, cap *auditCaptureWriter, want audit.EventType, requiredKey string) {
	t.Helper()
	evs := cap.Events()
	if len(evs) == 0 {
		t.Fatalf("audit НЕ записан на успешном 2xx — huma сломал self-audit write-путь (%s)", want)
	}
	ev := evs[0]
	if ev.EventType != want {
		t.Errorf("event_type = %q, want %q", ev.EventType, want)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("archon_aid = %q, want archon-alice", ev.ArchonAID)
	}
	if len(ev.Payload) == 0 {
		t.Error("audit payload пуст — self-audit потерял доменный payload")
	}
	if ev.Payload[requiredKey] == nil {
		t.Errorf("audit payload без %q: %+v", requiredKey, ev.Payload)
	}
}

// normalizeJSONKeys перекладывает JSON-object через map → канонический marshal
// (ключи отсортированы детерминированно), сохраняя присутствие/отсутствие ключей и
// любые лишние поля (например, $schema — всплывёт в diff).
func normalizeJSONKeys(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("reply не JSON-object: %v; raw=%s", err, raw)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}
