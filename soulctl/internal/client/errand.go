package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/api/wire"
)

// ErrandAPI holds typed methods for /v1/souls/{sid}/exec and /v1/errands*.
// The bodies are wire.* — this file used to keep "a local copy to avoid
// depending on keeper's internal packages", which is exactly the dependency
// NIM-776 removed by moving the declarations out of internal/.
type ErrandAPI struct {
	c *Client
}

// ErrandExecRequest is what a caller hands to [ErrandAPI.Exec]: the wire body
// plus the SID, which travels in the path and is therefore not part of it.
type ErrandExecRequest struct {
	SID  string
	Body wire.ErrandRunRequest
}

// ErrandListOptions holds query filters for GET /v1/errands. Not a wire type:
// these are query parameters, not a body.
type ErrandListOptions struct {
	SID          string
	Status       string
	StartedAfter string
	Limit        int
	Offset       int
}

// Exec is POST /v1/souls/{sid}/exec. Returns result + an async flag:
//   - 200 → (result, false, nil).
//   - 202 → (result-with-only-id-and-running-status, true, nil); the caller
//     then polls via Get.
//   - 4xx/5xx → (zero, false, *APIError).
func (a *ErrandAPI) Exec(ctx context.Context, req ErrandExecRequest) (wire.ErrandResult, bool, error) {
	if req.SID == "" {
		return wire.ErrandResult{}, false, fmt.Errorf("SID is empty")
	}
	if req.Body.Module == "" {
		return wire.ErrandResult{}, false, fmt.Errorf("module is empty")
	}
	path := "/v1/souls/" + url.PathEscape(req.SID) + "/exec"

	// 202 and 200 differ only in body shape, and wire.ErrandAccepted's two
	// fields (errand_id, status) are both present on wire.ErrandResult — so one
	// decode covers both and the async case is told apart by the fields below
	// rather than by a second raw-bytes call.
	var raw wire.ErrandResult
	if err := a.c.Do(ctx, "POST", path, req.Body, &raw); err != nil {
		return wire.ErrandResult{}, false, err
	}
	return raw, isErrandAsync(raw), nil
}

// Get is GET /v1/errands/{errand_id}. Keeper returns 200 for terminal states
// and 202 for running. Both forms are equally useful for the CLI: return
// result + an async flag.
func (a *ErrandAPI) Get(ctx context.Context, errandID string) (wire.ErrandResult, bool, error) {
	if errandID == "" {
		return wire.ErrandResult{}, false, fmt.Errorf("errand_id is empty")
	}
	var raw wire.ErrandResult
	if err := a.c.Do(ctx, "GET", "/v1/errands/"+url.PathEscape(errandID), nil, &raw); err != nil {
		return wire.ErrandResult{}, false, err
	}
	return raw, isErrandAsync(raw), nil
}

// isErrandAsync reports the minimal 202 body: Keeper answered with errand_id +
// status only. On a terminal status ∈ {success/failed/timed_out/cancelled/
// module_not_allowed} finished_at is populated, so its absence next to a
// running status is the marker.
func isErrandAsync(r wire.ErrandResult) bool {
	return r.Status == wire.ErrandResultStatusRunning && r.FinishedAt == nil
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
func (a *ErrandAPI) List(ctx context.Context, opts ErrandListOptions) (*wire.ErrandListReply, error) {
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
	var reply wire.ErrandListReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
