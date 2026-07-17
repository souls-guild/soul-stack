package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// ErrandAPI holds typed methods for /v1/souls/{sid}/exec and /v1/errands*.
type ErrandAPI struct {
	c *Client
}

// ErrandExecRequest is the body for POST /v1/souls/{sid}/exec. SID lives in the path.
type ErrandExecRequest struct {
	SID            string         `json:"-"`
	Module         string         `json:"module"`
	Input          map[string]any `json:"input,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	DryRun         bool           `json:"dry_run,omitempty"`
}

// ErrandResult is the JSON response shape for /v1/souls/{sid}/exec (200) and
// /v1/errands/{errand_id} (200). Same fields as the keeper-side
// errandResultResponse — the client keeps a local copy to avoid depending on
// keeper's internal packages.
type ErrandResult struct {
	ErrandID        string         `json:"errand_id"`
	SID             string         `json:"sid"`
	Module          string         `json:"module"`
	Status          string         `json:"status"`
	ExitCode        *int32         `json:"exit_code,omitempty"`
	Stdout          string         `json:"stdout,omitempty"`
	Stderr          string         `json:"stderr,omitempty"`
	StdoutTruncated bool           `json:"stdout_truncated"`
	StderrTruncated bool           `json:"stderr_truncated"`
	DurationMs      *int64         `json:"duration_ms,omitempty"`
	ErrorMessage    string         `json:"error_message,omitempty"`
	Output          map[string]any `json:"output,omitempty"`
	StartedByAID    string         `json:"started_by_aid"`
	StartedAt       string         `json:"started_at"`
	FinishedAt      string         `json:"finished_at,omitempty"`
}

// errandAcceptedResponse is the 202 body on async escalation (the Errand
// result continues in the background). errand_id + status are the only
// stable fields.
type errandAcceptedResponse struct {
	ErrandID string `json:"errand_id"`
	Status   string `json:"status"`
}

// ErrandListOptions holds query filters for GET /v1/errands.
type ErrandListOptions struct {
	SID          string
	Status       string
	StartedAfter string
	Limit        int
	Offset       int
}

// ErrandListReply is a list page (paged response).
type ErrandListReply struct {
	Items  []ErrandResult `json:"items"`
	Offset int            `json:"offset"`
	Limit  int            `json:"limit"`
	Total  int            `json:"total"`
}

// Exec is POST /v1/souls/{sid}/exec. Returns result + an async flag:
//   - 200 → (result, false, nil).
//   - 202 → (result-with-only-id-and-running-status, true, nil); the caller
//     then polls via Get.
//   - 4xx/5xx → (zero, false, *APIError).
func (a *ErrandAPI) Exec(ctx context.Context, req ErrandExecRequest) (ErrandResult, bool, error) {
	if req.SID == "" {
		return ErrandResult{}, false, fmt.Errorf("SID is empty")
	}
	if req.Module == "" {
		return ErrandResult{}, false, fmt.Errorf("module is empty")
	}
	path := "/v1/souls/" + url.PathEscape(req.SID) + "/exec"

	// 202 and 200 differ only in body shape. Instead of a second raw-bytes
	// call, decode directly into a struct embedding ErrandResult: an empty
	// sid means no full result arrived, i.e. a 202/async response.
	var raw struct {
		ErrandResult
		// errandAcceptedResponse's fields are already covered by ErrandResult (errand_id, status).
	}
	if err := a.c.Do(ctx, "POST", path, req, &raw); err != nil {
		return ErrandResult{}, false, err
	}
	// Async marker: ErrandResult.Status == "running" and no finished_at — Keeper
	// returned the minimal 202 body (errand_id + status) in this case. On a
	// terminal status ∈ {success/failed/timed_out/cancelled/module_not_allowed},
	// finished_at is populated.
	async := raw.Status == "running" && raw.FinishedAt == ""
	return raw.ErrandResult, async, nil
}

// Get is GET /v1/errands/{errand_id}. Keeper returns 200 for terminal states
// and 202 for running. Both forms are equally useful for the CLI: return
// result + an async flag.
func (a *ErrandAPI) Get(ctx context.Context, errandID string) (ErrandResult, bool, error) {
	if errandID == "" {
		return ErrandResult{}, false, fmt.Errorf("errand_id is empty")
	}
	var raw ErrandResult
	if err := a.c.Do(ctx, "GET", "/v1/errands/"+url.PathEscape(errandID), nil, &raw); err != nil {
		return ErrandResult{}, false, err
	}
	async := raw.Status == "running" && raw.FinishedAt == ""
	return raw, async, nil
}

// Cancel is DELETE /v1/errands/{errand_id} (ADR-033 slice E5). Permission:
// errand.cancel. Returns nil on 204; *APIError on 404/409/500. The operator
// sees the final cancelled status via Get (poll) — Soul sends
// ErrandResult{CANCELLED} after receiving the CancelErrand signal.
func (a *ErrandAPI) Cancel(ctx context.Context, errandID string) error {
	if errandID == "" {
		return fmt.Errorf("errand_id is empty")
	}
	return a.c.Do(ctx, "DELETE", "/v1/errands/"+url.PathEscape(errandID), nil, nil)
}

// List is GET /v1/errands. Query parameters are built from opts.
func (a *ErrandAPI) List(ctx context.Context, opts ErrandListOptions) (*ErrandListReply, error) {
	q := url.Values{}
	if opts.SID != "" {
		q.Set("sid", opts.SID)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.StartedAfter != "" {
		q.Set("started_after", opts.StartedAfter)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	path := "/v1/errands"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var reply ErrandListReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
