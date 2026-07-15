package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// VoyagesAPI holds typed methods for /v1/voyages/* (ADR-043). A Voyage is a
// unified batch run (kind=scenario|command), async by default.
type VoyagesAPI struct {
	c *Client
}

// VoyageTarget is a Voyage's invocation-time scope (ADR-043 §4). For
// kind=scenario, Incarnations/Service/Coven matter (resolved into
// incarnation names); for kind=command, SIDs/Coven/Where matter (AND-merged
// into a SID snapshot).
type VoyageTarget struct {
	Incarnations []string `json:"incarnations,omitempty"`
	Service      string   `json:"service,omitempty"`
	SIDs         []string `json:"sids,omitempty"`
	Coven        []string `json:"coven,omitempty"`
	Where        string   `json:"where,omitempty"`
}

// VoyageCreateRequest is the body for POST /v1/voyages. kind is required;
// scenario_name is for kind=scenario, module is for kind=command.
//
// Batch/MaxFailures are raw strings in N|N% format (ADR-043 amend). The
// client does NOT parse or validate them: Keeper is the grammar authority
// (fail-closed 422 on garbage/conflicting formats). Empty/omitted means
// unset.
type VoyageCreateRequest struct {
	Kind         string         `json:"kind"`
	ScenarioName string         `json:"scenario_name,omitempty"`
	Module       string         `json:"module,omitempty"`
	Input        map[string]any `json:"input,omitempty"`
	Target       VoyageTarget   `json:"target"`
	BatchSize    int            `json:"batch_size,omitempty"`
	Batch        string         `json:"batch,omitempty"`
	MaxFailures  string         `json:"max_failures,omitempty"`
	Concurrency  int            `json:"concurrency,omitempty"`
	OnFailure    string         `json:"on_failure,omitempty"`
	DryRun       bool           `json:"dry_run,omitempty"`
	ScheduleAt   string         `json:"schedule_at,omitempty"`
}

// VoyageCreateReply is Create's 202 response.
type VoyageCreateReply struct {
	VoyageID  string `json:"voyage_id"`
	Kind      string `json:"kind"`
	ScopeSize int    `json:"scope_size"`
	Status    string `json:"status"`
	Location  string `json:"location"`
}

// VoyageSummary is the aggregated run outcome (jsonb summary column).
type VoyageSummary struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

// Voyage is a snapshot of a single Voyage (GET /v1/voyages/{id}).
type Voyage struct {
	VoyageID    string         `json:"voyage_id"`
	Kind        string         `json:"kind"`
	Status      string         `json:"status"`
	ScopeSize   int            `json:"scope_size"`
	CurrentDone int            `json:"current_done"`
	StartedAt   string         `json:"started_at"`
	FinishedAt  string         `json:"finished_at,omitempty"`
	Summary     *VoyageSummary `json:"summary,omitempty"`
}

// VoyageListOptions holds filters for GET /v1/voyages.
type VoyageListOptions struct {
	Kind   string
	Status []string
	Offset int
	Limit  int
}

// VoyageListReply is a list page.
type VoyageListReply struct {
	Items  []Voyage `json:"items"`
	Offset int      `json:"offset"`
	Limit  int      `json:"limit"`
	Total  int      `json:"total"`
}

// VoyageCancelReply is the response for DELETE /v1/voyages/{id}.
type VoyageCancelReply struct {
	VoyageID string `json:"voyage_id"`
	Status   string `json:"status"`
}

// Create is POST /v1/voyages (ADR-043). Async by default: always 202.
func (a *VoyagesAPI) Create(ctx context.Context, req VoyageCreateRequest) (*VoyageCreateReply, error) {
	if req.Kind == "" {
		return nil, fmt.Errorf("kind пуст")
	}
	var reply VoyageCreateReply
	if err := a.c.Do(ctx, "POST", "/v1/voyages", req, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Get is GET /v1/voyages/{id}.
func (a *VoyagesAPI) Get(ctx context.Context, voyageID string) (*Voyage, error) {
	if voyageID == "" {
		return nil, fmt.Errorf("voyage_id пуст")
	}
	var reply Voyage
	if err := a.c.Do(ctx, "GET", "/v1/voyages/"+url.PathEscape(voyageID), nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// List is GET /v1/voyages (multi-value status, OR semantics).
func (a *VoyagesAPI) List(ctx context.Context, opts VoyageListOptions) (*VoyageListReply, error) {
	q := url.Values{}
	if opts.Kind != "" {
		q.Set("kind", opts.Kind)
	}
	for _, s := range opts.Status {
		q.Add("status", s)
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	path := "/v1/voyages"
	if enc := q.Encode(); enc != "" {
		path = path + "?" + enc
	}
	var reply VoyageListReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// Cancel is DELETE /v1/voyages/{id} (ADR-043 S5): cancels pending/scheduled.
func (a *VoyagesAPI) Cancel(ctx context.Context, voyageID string) (*VoyageCancelReply, error) {
	if voyageID == "" {
		return nil, fmt.Errorf("voyage_id пуст")
	}
	var reply VoyageCancelReply
	if err := a.c.Do(ctx, "DELETE", "/v1/voyages/"+url.PathEscape(voyageID), nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
