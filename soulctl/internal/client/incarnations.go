package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// IncarnationsAPI holds typed methods for /v1/incarnations/*. Exposed as the
// Client.Incarnations field.
type IncarnationsAPI struct {
	c *Client
}

// IncarnationListOptions holds list filters. service/status/coven are
// query params, limit/offset is pagination (operator-api.md → Pagination).
type IncarnationListOptions struct {
	Service string
	Status  string
	Coven   string
	Limit   int
	Offset  int
}

// IncarnationListItem mirrors the IncarnationGetReply shape (openapi.yaml).
// snake_case names come from UseProtoNames in Keeper's HTTP facade
// (operator-api.md → JSON field naming).
type IncarnationListItem struct {
	Name               string          `json:"name"`
	Service            string          `json:"service"`
	ServiceVersion     string          `json:"service_version"`
	StateSchemaVersion int32           `json:"state_schema_version"`
	Covens             []string        `json:"covens"`
	Spec               json.RawMessage `json:"spec,omitempty"`
	State              json.RawMessage `json:"state,omitempty"`
	Status             string          `json:"status"`
	StatusDetails      json.RawMessage `json:"status_details,omitempty"`
	CreatedByAID       string          `json:"created_by_aid"`
	CreatedAt          string          `json:"created_at"`
	UpdatedAt          string          `json:"updated_at"`
	LastDriftCheckAt   string          `json:"last_drift_check_at,omitempty"`
	LastDriftSummary   json.RawMessage `json:"last_drift_summary,omitempty"`
}

// IncarnationListReply is a list page.
type IncarnationListReply struct {
	Items  []IncarnationListItem `json:"items"`
	Offset int32                 `json:"offset"`
	Limit  int32                 `json:"limit"`
	Total  int32                 `json:"total"`
}

// List is GET /v1/incarnations. `coven` isn't defined as a filter on this
// endpoint by the openapi schema (the coven filter only exists on
// /v1/souls), so the filter is applied client-side after fetching the page.
// The server returns offset/limit/total for service/status; for coven the
// values won't be consistent with total — this is a known limitation,
// documented in the README.
func (a *IncarnationsAPI) List(ctx context.Context, opts IncarnationListOptions) (*IncarnationListReply, error) {
	q := url.Values{}
	if opts.Service != "" {
		q.Set("service", opts.Service)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	path := "/v1/incarnations"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var reply IncarnationListReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	if opts.Coven != "" {
		filtered := reply.Items[:0]
		for _, it := range reply.Items {
			for _, c := range it.Covens {
				if c == opts.Coven {
					filtered = append(filtered, it)
					break
				}
			}
		}
		reply.Items = filtered
	}
	return &reply, nil
}

// Get is GET /v1/incarnations/{name}.
func (a *IncarnationsAPI) Get(ctx context.Context, name string) (*IncarnationListItem, error) {
	if name == "" {
		return nil, fmt.Errorf("incarnation name is empty")
	}
	var item IncarnationListItem
	if err := a.c.Do(ctx, "GET", "/v1/incarnations/"+url.PathEscape(name), nil, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// IncarnationRunRequest is the body for POST /v1/incarnations/{name}/scenarios/{scenario}.
type IncarnationRunRequest struct {
	Input map[string]any `json:"input,omitempty"`
}

// IncarnationRunReply is the 202 response with apply_id (ULID).
type IncarnationRunReply struct {
	ApplyID     string `json:"apply_id"`
	Incarnation string `json:"incarnation"`
	Scenario    string `json:"scenario"`
}

// Run is POST /v1/incarnations/{name}/scenarios/{scenario}. The server
// accepts dry_run as a query parameter (no explicit description in openapi;
// we pass it as a query param — the server will either honor it or ignore
// it, which is safe either way).
func (a *IncarnationsAPI) Run(ctx context.Context, name, scenario string, input map[string]any, dryRun bool) (*IncarnationRunReply, error) {
	if name == "" || scenario == "" {
		return nil, fmt.Errorf("incarnation/scenario are empty")
	}
	path := fmt.Sprintf("/v1/incarnations/%s/scenarios/%s", url.PathEscape(name), url.PathEscape(scenario))
	if dryRun {
		path += "?dry_run=true"
	}
	body := IncarnationRunRequest{Input: input}
	var reply IncarnationRunReply
	if err := a.c.Do(ctx, "POST", path, body, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// StateHistoryEntry is a record from /v1/incarnations/{name}/history.
type StateHistoryEntry struct {
	HistoryID    string          `json:"history_id"`
	Scenario     string          `json:"scenario"`
	StateBefore  json.RawMessage `json:"state_before,omitempty"`
	StateAfter   json.RawMessage `json:"state_after,omitempty"`
	ChangedByAID string          `json:"changed_by_aid"`
	ApplyID      string          `json:"apply_id"`
	CreatedAt    string          `json:"created_at"`
}

// IncarnationHistoryReply is a state_history page.
type IncarnationHistoryReply struct {
	Items  []StateHistoryEntry `json:"items"`
	Offset int32               `json:"offset"`
	Limit  int32               `json:"limit"`
	Total  int32               `json:"total"`
}

// History is GET /v1/incarnations/{name}/history.
func (a *IncarnationsAPI) History(ctx context.Context, name string, limit, offset int) (*IncarnationHistoryReply, error) {
	if name == "" {
		return nil, fmt.Errorf("incarnation name is empty")
	}
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	path := "/v1/incarnations/" + url.PathEscape(name) + "/history"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var reply IncarnationHistoryReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// DriftReport is the response for POST /v1/incarnations/{name}/check-drift.
// Full shape in openapi.yaml → DriftReport / DriftHostReport / DriftSummary.
type DriftReport struct {
	CheckedAt   string             `json:"checked_at"`
	Incarnation string             `json:"incarnation"`
	ScenarioRef string             `json:"scenario_ref"`
	Hosts       []DriftHostReport  `json:"hosts"`
	Summary     DriftSummaryCounts `json:"summary"`
}

type DriftHostReport struct {
	SID    string            `json:"sid"`
	Status string            `json:"status"`
	Tasks  []DriftTaskResult `json:"tasks"`
}

type DriftTaskResult struct {
	Idx     int    `json:"idx"`
	Module  string `json:"module"`
	Action  string `json:"action,omitempty"`
	Changed bool   `json:"changed"`
	Message string `json:"message,omitempty"`
}

type DriftSummaryCounts struct {
	HostsDrifted     int `json:"hosts_drifted"`
	HostsClean       int `json:"hosts_clean"`
	HostsUnsupported int `json:"hosts_unsupported"`
	HostsFailed      int `json:"hosts_failed"`
}

// CheckDrift is POST /v1/incarnations/{name}/check-drift with optional input.
func (a *IncarnationsAPI) CheckDrift(ctx context.Context, name string, input map[string]any) (*DriftReport, error) {
	if name == "" {
		return nil, fmt.Errorf("incarnation name is empty")
	}
	body := map[string]any{}
	if len(input) > 0 {
		body["input"] = input
	}
	var report DriftReport
	if err := a.c.Do(ctx, "POST", "/v1/incarnations/"+url.PathEscape(name)+"/check-drift", body, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// RunSummary is one row of the runs list (GET /v1/incarnations/{name}/runs).
// Shape in openapi.yaml → RunSummaryEntry.
type RunSummary struct {
	ApplyID      string  `json:"apply_id"`
	Scenario     string  `json:"scenario"`
	Status       string  `json:"status"`
	StartedAt    string  `json:"started_at"`
	FinishedAt   *string `json:"finished_at,omitempty"`
	StartedByAID *string `json:"started_by_aid,omitempty"`
}

// IncarnationRunsReply is a page of runs.
type IncarnationRunsReply struct {
	Items  []RunSummary `json:"items"`
	Offset int32        `json:"offset"`
	Limit  int32        `json:"limit"`
	Total  int32        `json:"total"`
}

// RunNotice is one advisory finding a host reported during a run (ADR-0076(u)).
// Independent of the host's status: the task RAN, and something about how it was
// asked is on its way out. Today `code` is only `deprecated_param`; `message`
// carries the deadline and the replacement.
type RunNotice struct {
	Code    string `json:"code"`
	Module  string `json:"module"`
	Param   string `json:"param,omitempty"`
	Message string `json:"message"`
}

// RunHostStatus is one host's line in the run detail. FailedTaskIdx /
// FailedPlanIndex / ErrorSummary are populated only on a failed host; Notices is
// populated whenever that host had something to report, failure or not.
type RunHostStatus struct {
	SID             string      `json:"sid"`
	Status          string      `json:"status"`
	Passage         int         `json:"passage"`
	FailedTaskIdx   *int        `json:"failed_task_idx,omitempty"`
	FailedPlanIndex *int        `json:"failed_plan_index,omitempty"`
	ErrorSummary    *string     `json:"error_summary,omitempty"`
	Attempt         int32       `json:"attempt"`
	CancelRequested bool        `json:"cancel_requested"`
	Notices         []RunNotice `json:"notices,omitempty"`
}

// RunDetail is the response for GET /v1/incarnations/{name}/runs/{apply_id}:
// header + per-host slice. Shape in openapi.yaml → RunDetailReply.
type RunDetail struct {
	ApplyID      string          `json:"apply_id"`
	Scenario     string          `json:"scenario"`
	Status       string          `json:"status"`
	StartedAt    string          `json:"started_at"`
	FinishedAt   *string         `json:"finished_at,omitempty"`
	StartedByAID *string         `json:"started_by_aid,omitempty"`
	Hosts        []RunHostStatus `json:"hosts"`
	Input        map[string]any  `json:"input,omitempty"`
}

// Runs is GET /v1/incarnations/{name}/runs.
func (a *IncarnationsAPI) Runs(ctx context.Context, name string, limit, offset int) (*IncarnationRunsReply, error) {
	if name == "" {
		return nil, fmt.Errorf("incarnation name is empty")
	}
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	path := "/v1/incarnations/" + url.PathEscape(name) + "/runs"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var reply IncarnationRunsReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// RunDetail is GET /v1/incarnations/{name}/runs/{apply_id}.
func (a *IncarnationsAPI) RunDetail(ctx context.Context, name, applyID string) (*RunDetail, error) {
	if name == "" {
		return nil, fmt.Errorf("incarnation name is empty")
	}
	if applyID == "" {
		return nil, fmt.Errorf("apply_id is empty")
	}
	var detail RunDetail
	path := "/v1/incarnations/" + url.PathEscape(name) + "/runs/" + url.PathEscape(applyID)
	if err := a.c.Do(ctx, "GET", path, nil, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}
