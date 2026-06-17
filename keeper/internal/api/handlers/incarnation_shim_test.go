package handlers

// ТЕСТ-ШИМ handler-native (T5d-2c-full): тонкие (w,r)-обёртки над *Typed-функциями
// incarnation-домена, воспроизводящие прежнюю (w,r)-семантику маршрутизации (декод
// body/query → *Typed → render статуса из problem.Type / 2xx + body). Через них существующие
// поведенческие тесты прогоняют РОВНО ту же бизнес-логику, что монтирует huma-роут, не таща
// httptest-биндинг huma в unit-тест. Прод (w,r)-методы сняты — это только тестовая оболочка.
//
// JSON-bind-тесты (BadJSON/UnknownField) к шимам НЕ применяются: strict-декод/required/
// additionalProperties делает huma на своём слое, доменная функция получает уже декодированное.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// shimLogger — discard-логгер для render-веток шима.
var shimLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))

// renderProblem пишет в recorder problem+json из *problemError *Typed-функции (status из
// problem.Type через problem.New). Не-problem → 500.
func renderProblem(rec *httptest.ResponseRecorder, err error) {
	if d, ok := AsProblemDetails(err); ok {
		problem.Write(rec, d)
		return
	}
	problem.Write(rec, problem.New(problem.TypeInternalError, "", "internal error"))
}

// shimClaims извлекает claims из request-контекста (через тот же middleware-ключ, что прод).
func shimClaims(r *http.Request) (*keeperjwt.Claims, bool) {
	return middleware.ClaimsFromContext(r.Context())
}

// incCreate — шим POST /v1/incarnations: декод body → IncarnationCreateRequestInput → CreateTyped.
func incCreate(h *IncarnationHandler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	claims, _ := shimClaims(r)
	var body struct {
		Name    string         `json:"name"`
		Service string         `json:"service"`
		Covens  []string       `json:"covens"`
		Input   map[string]any `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	reply, err := h.CreateTyped(r.Context(), claims, IncarnationCreateRequestInput{
		Name: body.Name, Service: body.Service, Covens: body.Covens, Input: body.Input,
	})
	if err != nil {
		renderProblem(rec, err)
		return rec
	}
	middleware.SetAuditPayload(r, reply.AuditPayload)
	writeJSON(rec, http.StatusAccepted, struct {
		ApplyID     *string `json:"apply_id,omitempty"`
		Incarnation string  `json:"incarnation"`
	}{ApplyID: reply.Body.ApplyID, Incarnation: reply.Body.Incarnation}, shimLogger)
	return rec
}

// incGet — шим GET /v1/incarnations/{name}.
func incGet(h *IncarnationHandler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	claims, _ := shimClaims(r)
	name := chi.URLParam(r, "name")
	view, err := h.GetTyped(r.Context(), name, h.GetInScopeFor(claims, "get"))
	if err != nil {
		renderProblem(rec, err)
		return rec
	}
	writeJSON(rec, http.StatusOK, shimGetReplyJSON(view), shimLogger)
	return rec
}

// incList — шим GET /v1/incarnations: парс query (offset/limit/service/status/coven/sort +
// state.<field>) → ListTyped с scope-резолвером из claims.
func incList(h *IncarnationHandler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	claims, _ := shimClaims(r)
	q := r.URL.Query()
	offset, limit := 0, 50
	if v := q.Get("offset"); v != "" {
		offset, _ = strconv.Atoi(v)
	}
	if v := q.Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	state := map[string][]string{}
	for key, vals := range q {
		if field, ok := strings.CutPrefix(key, "state."); ok {
			state[field] = vals
		}
	}
	qy := IncarnationListQuery{
		Offset: offset, Limit: limit, Service: q.Get("service"), Status: q.Get("status"),
		Coven: q.Get("coven"), SortBy: q.Get("sort"), SortDir: q.Get("sort_dir"), StateParams: state,
	}
	reply, err := h.ListTyped(r.Context(), qy, h.ResolveListScopeFor(r.Context(), claims))
	if err != nil {
		renderProblem(rec, err)
		return rec
	}
	items := make([]any, 0, len(reply.Items))
	for i := range reply.Items {
		items = append(items, shimGetReplyJSON(reply.Items[i]))
	}
	writeJSON(rec, http.StatusOK, struct {
		Items  []any `json:"items"`
		Offset int   `json:"offset"`
		Limit  int   `json:"limit"`
		Total  int   `json:"total"`
	}{items, reply.Offset, reply.Limit, reply.Total}, shimLogger)
	return rec
}

// incHistory — шим GET /v1/incarnations/{name}/history.
func incHistory(h *IncarnationHandler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	claims, _ := shimClaims(r)
	name := chi.URLParam(r, "name")
	q := r.URL.Query()
	offset, limit := 0, 50
	if v := q.Get("offset"); v != "" {
		offset, _ = strconv.Atoi(v)
	}
	if v := q.Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	reply, err := h.HistoryTyped(r.Context(), name, q.Get("apply_id"), offset, limit, h.GetInScopeFor(claims, "history"))
	if err != nil {
		renderProblem(rec, err)
		return rec
	}
	items := make([]any, 0, len(reply.Items))
	for i := range reply.Items {
		items = append(items, shimHistoryEntryJSON(reply.Items[i]))
	}
	writeJSON(rec, http.StatusOK, struct {
		Items  []any `json:"items"`
		Offset int   `json:"offset"`
		Limit  int   `json:"limit"`
		Total  int   `json:"total"`
	}{items, reply.Offset, reply.Limit, reply.Total}, shimLogger)
	return rec
}

// incRun — шим POST /v1/incarnations/{name}/scenarios/{scenario}.
func incRun(h *IncarnationHandler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	claims, _ := shimClaims(r)
	name := chi.URLParam(r, "name")
	scenarioName := chi.URLParam(r, "scenario")
	var body struct {
		Input map[string]any `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	reply, err := h.RunTyped(r.Context(), claims, name, scenarioName, body.Input)
	if err != nil {
		renderProblem(rec, err)
		return rec
	}
	middleware.SetAuditPayload(r, reply.AuditPayload)
	writeJSON(rec, http.StatusAccepted, struct {
		ApplyID     string `json:"apply_id"`
		Incarnation string `json:"incarnation"`
		Scenario    string `json:"scenario"`
	}{reply.Body.ApplyID, reply.Body.Incarnation, reply.Body.Scenario}, shimLogger)
	return rec
}

// incUnlock — шим POST /v1/incarnations/{name}/unlock.
func incUnlock(h *IncarnationHandler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	claims, _ := shimClaims(r)
	name := chi.URLParam(r, "name")
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	reply, err := h.UnlockTyped(r.Context(), claims, name, body.Reason)
	if err != nil {
		renderProblem(rec, err)
		return rec
	}
	middleware.SetAuditPayload(r, reply.AuditPayload)
	writeJSON(rec, http.StatusOK, struct {
		Name           string `json:"name"`
		PreviousStatus string `json:"previous_status"`
		Status         string `json:"status"`
		UnlockedAt     string `json:"unlocked_at"`
		UnlockedByAID  string `json:"unlocked_by_aid"`
	}{reply.Body.Name, reply.Body.PreviousStatus, reply.Body.Status, rfc3339Nano(reply.Body.UnlockedAt), reply.Body.UnlockedByAID}, shimLogger)
	return rec
}

// incUpgrade — шим POST /v1/incarnations/{name}/upgrade.
func incUpgrade(h *IncarnationHandler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	claims, _ := shimClaims(r)
	name := chi.URLParam(r, "name")
	var body struct {
		ToVersion string `json:"to_version"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	reply, err := h.UpgradeTyped(r.Context(), claims, name, body.ToVersion)
	if err != nil {
		renderProblem(rec, err)
		return rec
	}
	middleware.SetAuditPayload(r, reply.AuditPayload)
	writeJSON(rec, http.StatusAccepted, struct {
		ApplyID string `json:"apply_id"`
	}{reply.Body.ApplyID}, shimLogger)
	return rec
}

// incCheckDrift — шим POST /v1/incarnations/{name}/check-drift.
func incCheckDrift(h *IncarnationHandler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	claims, _ := shimClaims(r)
	name := chi.URLParam(r, "name")
	var body struct {
		Input map[string]any `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	report, err := h.CheckDriftTyped(r.Context(), claims, name, body.Input)
	if err != nil {
		renderProblem(rec, err)
		return rec
	}
	writeJSON(rec, http.StatusOK, report, shimLogger)
	return rec
}

// incUpdateHosts — шим PATCH /v1/incarnations/{name}/hosts.
func incUpdateHosts(h *IncarnationHandler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	claims, _ := shimClaims(r)
	name := chi.URLParam(r, "name")
	var body struct {
		Mode  string `json:"mode"`
		Hosts []struct {
			SID  string  `json:"sid"`
			Role *string `json:"role"`
		} `json:"hosts"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	items := make([]IncarnationSpecHostInput, len(body.Hosts))
	for i, hst := range body.Hosts {
		role := ""
		if hst.Role != nil {
			role = *hst.Role
		}
		items[i] = IncarnationSpecHostInput{SID: hst.SID, Role: role}
	}
	view, err := h.UpdateHostsTyped(r.Context(), claims, name, body.Mode, items)
	if err != nil {
		renderProblem(rec, err)
		return rec
	}
	writeJSON(rec, http.StatusOK, shimGetReplyJSON(view), shimLogger)
	return rec
}

// shimGetReplyJSON — wire-форма IncarnationGetView для тестового render (повторяет json-теги
// прежнего IncarnationGetReply: covens/spec/state/status_details nullable-без-omitempty,
// last_drift_* omitempty). Тестам важны имена ключей/null-семантика, не enum-named-схема.
func shimGetReplyJSON(v IncarnationGetView) any {
	type driftJSON struct {
		HostsClean       int    `json:"hosts_clean"`
		HostsDrifted     int    `json:"hosts_drifted"`
		HostsFailed      int    `json:"hosts_failed"`
		HostsUnsupported int    `json:"hosts_unsupported"`
		ScannedAt        string `json:"scanned_at"`
		TotalHosts       int    `json:"total_hosts"`
	}
	out := struct {
		Covens             []string        `json:"covens"`
		CreatedAt          string          `json:"created_at"`
		CreatedByAID       *string         `json:"created_by_aid"`
		LastDriftCheckAt   *string         `json:"last_drift_check_at,omitempty"`
		LastDriftSummary   *driftJSON      `json:"last_drift_summary,omitempty"`
		Name               string          `json:"name"`
		Service            string          `json:"service"`
		ServiceVersion     string          `json:"service_version"`
		Spec               *map[string]any `json:"spec"`
		State              *map[string]any `json:"state"`
		StateSchemaVersion int32           `json:"state_schema_version"`
		Status             string          `json:"status"`
		StatusDetails      *map[string]any `json:"status_details"`
		UpdatedAt          string          `json:"updated_at"`
	}{
		Covens: v.Covens, CreatedAt: rfc3339Nano(v.CreatedAt), CreatedByAID: v.CreatedByAID,
		Name: v.Name, Service: v.Service, ServiceVersion: v.ServiceVersion,
		Spec: ptrMapShim(v.Spec), State: ptrMapShim(v.State), StateSchemaVersion: v.StateSchemaVersion,
		Status: v.Status, StatusDetails: ptrMapShim(v.StatusDetails), UpdatedAt: rfc3339Nano(v.UpdatedAt),
	}
	if v.LastDriftCheckAt != nil {
		s := rfc3339Nano(*v.LastDriftCheckAt)
		out.LastDriftCheckAt = &s
	}
	if v.LastDriftSummary != nil {
		d := v.LastDriftSummary
		out.LastDriftSummary = &driftJSON{
			HostsClean: d.HostsClean, HostsDrifted: d.HostsDrifted, HostsFailed: d.HostsFailed,
			HostsUnsupported: d.HostsUnsupported, ScannedAt: rfc3339Nano(d.ScannedAt), TotalHosts: d.TotalHosts,
		}
	}
	return out
}

// shimHistoryEntryJSON — wire-форма StateHistoryView для тестового render.
func shimHistoryEntryJSON(v StateHistoryView) any {
	return struct {
		ApplyID      string          `json:"apply_id"`
		ChangedByAID *string         `json:"changed_by_aid,omitempty"`
		CreatedAt    string          `json:"created_at"`
		HistoryID    string          `json:"history_id"`
		Scenario     string          `json:"scenario"`
		StateAfter   *map[string]any `json:"state_after"`
		StateBefore  *map[string]any `json:"state_before"`
	}{
		ApplyID: v.ApplyID, ChangedByAID: v.ChangedByAID, CreatedAt: rfc3339Nano(v.CreatedAt),
		HistoryID: v.HistoryID, Scenario: v.Scenario,
		StateAfter: ptrMapShim(v.StateAfter), StateBefore: ptrMapShim(v.StateBefore),
	}
}

func ptrMapShim(m map[string]any) *map[string]any {
	if m == nil {
		return nil
	}
	return &m
}

// incDTOJSON — декод-форма wire-тела GET/PATCH-hosts incarnation для тестов (повторяет ключи
// прежнего incarnationDTO). Тесты декодируют тело шима в неё и проверяют поля.
type incDTOJSON struct {
	Name          string         `json:"name"`
	Status        string         `json:"status"`
	Spec          map[string]any `json:"spec"`
	State         map[string]any `json:"state"`
	StatusDetails map[string]any `json:"status_details"`
	CreatedByAID  *string        `json:"created_by_aid"`
}

func rfc3339Nano(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.999999999Z07:00") }
