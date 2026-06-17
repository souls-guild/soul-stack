package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// IncarnationsAPI — типизированные методы для /v1/incarnations/*. Открыт как
// поле Client.Incarnations.
type IncarnationsAPI struct {
	c *Client
}

// IncarnationListOptions — фильтры list. service/status/coven — query-params,
// limit/offset — pagination (operator-api.md → Pagination).
type IncarnationListOptions struct {
	Service string
	Status  string
	Coven   string
	Limit   int
	Offset  int
}

// IncarnationListItem повторяет shape IncarnationGetReply (openapi.yaml). Имена
// snake_case — UseProtoNames в HTTP-фасаде Keeper-а (operator-api.md → JSON
// field naming).
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

// IncarnationListReply — страница списка.
type IncarnationListReply struct {
	Items  []IncarnationListItem `json:"items"`
	Offset int32                 `json:"offset"`
	Limit  int32                 `json:"limit"`
	Total  int32                 `json:"total"`
}

// List — GET /v1/incarnations. `coven` openapi-схемой не задан как фильтр на
// этом endpoint-е (фильтр coven есть только у /v1/souls), поэтому фильтр
// применяется client-side после получения страницы. Сервер вернёт offset/limit/total
// для service/status, для coven значения будут не консистентны с total — это
// known limitation, отражена в README.
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

// Get — GET /v1/incarnations/{name}.
func (a *IncarnationsAPI) Get(ctx context.Context, name string) (*IncarnationListItem, error) {
	if name == "" {
		return nil, fmt.Errorf("incarnation name пуст")
	}
	var item IncarnationListItem
	if err := a.c.Do(ctx, "GET", "/v1/incarnations/"+url.PathEscape(name), nil, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// IncarnationRunRequest — тело POST /v1/incarnations/{name}/scenarios/{scenario}.
type IncarnationRunRequest struct {
	Input map[string]any `json:"input,omitempty"`
}

// IncarnationRunReply — 202-ответ с apply_id (ULID).
type IncarnationRunReply struct {
	ApplyID     string `json:"apply_id"`
	Incarnation string `json:"incarnation"`
	Scenario    string `json:"scenario"`
}

// Run — POST /v1/incarnations/{name}/scenarios/{scenario}. dry_run сервер
// принимает как query-параметр (по openapi явного описания не вижу; передадим
// как query, сервер либо примет, либо проигнорирует — это безопасно).
func (a *IncarnationsAPI) Run(ctx context.Context, name, scenario string, input map[string]any, dryRun bool) (*IncarnationRunReply, error) {
	if name == "" || scenario == "" {
		return nil, fmt.Errorf("incarnation/scenario пусты")
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

// StateHistoryEntry — запись /v1/incarnations/{name}/history.
type StateHistoryEntry struct {
	HistoryID    string          `json:"history_id"`
	Scenario     string          `json:"scenario"`
	StateBefore  json.RawMessage `json:"state_before,omitempty"`
	StateAfter   json.RawMessage `json:"state_after,omitempty"`
	ChangedByAID string          `json:"changed_by_aid"`
	ApplyID      string          `json:"apply_id"`
	CreatedAt    string          `json:"created_at"`
}

// IncarnationHistoryReply — страница state_history.
type IncarnationHistoryReply struct {
	Items  []StateHistoryEntry `json:"items"`
	Offset int32               `json:"offset"`
	Limit  int32               `json:"limit"`
	Total  int32               `json:"total"`
}

// History — GET /v1/incarnations/{name}/history.
func (a *IncarnationsAPI) History(ctx context.Context, name string, limit, offset int) (*IncarnationHistoryReply, error) {
	if name == "" {
		return nil, fmt.Errorf("incarnation name пуст")
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

// DriftReport — POST /v1/incarnations/{name}/check-drift. Полная shape
// в openapi.yaml → DriftReport / DriftHostReport / DriftSummary.
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

// CheckDrift — POST /v1/incarnations/{name}/check-drift с опциональным input.
func (a *IncarnationsAPI) CheckDrift(ctx context.Context, name string, input map[string]any) (*DriftReport, error) {
	if name == "" {
		return nil, fmt.Errorf("incarnation name пуст")
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
