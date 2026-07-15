package handlers

// TEST SHIM handler-native (T5d-2c-full): thin (w,r) wrappers over the *Typed functions
// of the incarnation domain, reproducing the previous (w,r) routing semantics (decode
// body/query → *Typed → render status from problem.Type / 2xx + body). Through them the
// existing behavioral tests exercise EXACTLY the same business logic that the huma route
// mounts, without dragging huma's httptest binding into the unit test. The production (w,r)
// methods have been removed — this is only a test shell.
//
// JSON bind tests (BadJSON/UnknownField) do NOT apply to the shims: strict decoding/required/
// additionalProperties are handled by huma at its own layer, the domain function receives
// already-decoded data.

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

// shimLogger — a discard logger for the shim's render branches.
var shimLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))

// renderProblem writes problem+json to the recorder from the *Typed function's *problemError
// (status from problem.Type via problem.New). Non-problem → 500.
func renderProblem(rec *httptest.ResponseRecorder, err error) {
	if d, ok := AsProblemDetails(err); ok {
		problem.Write(rec, d)
		return
	}
	problem.Write(rec, problem.New(problem.TypeInternalError, "", "internal error"))
}

// shimClaims extracts claims from the request context (via the same middleware key as production).
func shimClaims(r *http.Request) (*keeperjwt.Claims, bool) {
	return middleware.ClaimsFromContext(r.Context())
}

// incCreate — shim for POST /v1/incarnations: decode body → IncarnationCreateRequestInput → CreateTyped.
func incCreate(h *IncarnationHandler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	claims, _ := shimClaims(r)
	var body struct {
		Name           string         `json:"name"`
		Service        string         `json:"service"`
		Covens         []string       `json:"covens"`
		Input          map[string]any `json:"input"`
		Traits         map[string]any `json:"traits"`
		CreateScenario string         `json:"create_scenario"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	reply, err := h.CreateTyped(r.Context(), claims, IncarnationCreateRequestInput{
		Name: body.Name, Service: body.Service, Covens: body.Covens, Input: body.Input, Traits: body.Traits,
		CreateScenario: body.CreateScenario,
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

// incGet — shim for GET /v1/incarnations/{name}.
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

// incList — shim for GET /v1/incarnations: parses the query (offset/limit/service/status/coven/sort +
// state.<field>) → ListTyped with a scope resolver derived from claims.
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

// incHistory — shim for GET /v1/incarnations/{name}/history.
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

// incRun — shim for POST /v1/incarnations/{name}/scenarios/{scenario}.
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

// incUnlock — shim for POST /v1/incarnations/{name}/unlock.
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

// incUpgrade — shim for POST /v1/incarnations/{name}/upgrade.
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
		ApplyID    string  `json:"apply_id"`
		RunApplyID *string `json:"run_apply_id,omitempty"`
	}{reply.Body.ApplyID, reply.Body.RunApplyID}, shimLogger)
	return rec
}

// incCheckDrift — shim for POST /v1/incarnations/{name}/check-drift.
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

// incUpdateHosts — shim for PATCH /v1/incarnations/{name}/hosts.
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

// incSetTraits — shim for PUT /v1/incarnations/{name}/traits: decode body.traits →
// SetTraitsTyped.
func incSetTraits(h *IncarnationHandler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	claims, _ := shimClaims(r)
	name := chi.URLParam(r, "name")
	var body struct {
		Traits map[string]any `json:"traits"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	view, err := h.SetTraitsTyped(r.Context(), claims, name, body.Traits)
	if err != nil {
		renderProblem(rec, err)
		return rec
	}
	writeJSON(rec, http.StatusOK, shimGetReplyJSON(view), shimLogger)
	return rec
}

// shimGetReplyJSON — the wire shape of IncarnationGetView for test rendering (mirrors the json
// tags of the former IncarnationGetReply: covens/spec/state/status_details nullable-without-omitempty,
// last_drift_* omitempty). The tests care about key names/null semantics, not an enum-named schema.
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

// shimHistoryEntryJSON — the wire shape of StateHistoryView for test rendering.
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

// incDTOJSON — the decode shape of the GET/PATCH-hosts incarnation wire body for tests (mirrors
// the keys of the former incarnationDTO). Tests decode the shim's body into it and check the fields.
type incDTOJSON struct {
	Name          string         `json:"name"`
	Status        string         `json:"status"`
	Spec          map[string]any `json:"spec"`
	State         map[string]any `json:"state"`
	StatusDetails map[string]any `json:"status_details"`
	CreatedByAID  *string        `json:"created_by_aid"`
}

func rfc3339Nano(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.999999999Z07:00") }
