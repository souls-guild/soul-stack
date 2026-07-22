package legion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// voyageTerminalStatuses -- terminal Voyage statuses (ADR-043): the run is
// finished, polling stops. running/pending/scheduled are non-terminal (keep
// waiting).
var voyageTerminalStatuses = map[string]struct{}{
	"succeeded":      {},
	"failed":         {},
	"partial_failed": {},
	"cancelled":      {},
}

// VoyageRunOptions -- parameters for one command Voyage against the fleet
// (axis C load, docs/testing/load-testing.md §2). command is chosen over
// scenario: the command batch unit = HOST (resolves coven->souls.sid
// directly), so Keeper dispatches ErrandRequest to the WHOLE N-fleet --
// direct dispatch load across all stubs. A scenario Voyage by coven resolves
// coven into INCARNATIONS (unit = incarnation), which does not load dispatch
// across the fleet and would require seeding an incarnation + host binding.
type VoyageRunOptions struct {
	BaseURL      string         // http://127.0.0.1:8080 (OpenAPI listener)
	JWT          string         // admin-Archon token (errand.run)
	Coven        string         // target coven (= legion --coven)
	Module       string         // command module (default core.cmd.shell)
	Input        map[string]any // module params (CEL-rendered keeper-side)
	Concurrency  int            // top-level voyage.concurrency: >0 -> included in the create body; 0 -> do NOT send (keeper default=1+one Leg)
	PollInterval time.Duration  // GET /v1/voyages/{id} poll period
	Timeout      time.Duration  // max wait for terminal
}

// VoyageRunReport -- outcome of one command Voyage.
type VoyageRunReport struct {
	VoyageID    string
	ScopeSize   int           // number of units (hosts) resolved into the snapshot
	CreateLat   time.Duration // POST /v1/voyages latency (accept + resolve + persist)
	EndToEnd    time.Duration // from POST to terminal status (dispatch->ErrandResult->commit->audit)
	FinalStatus string
	Succeeded   int
	Failed      int
	Polls       int // number of GET /v1/voyages/{id} polls until terminal
}

// RunCommandVoyage creates ONE command Voyage against the legion's coven and
// polls until terminal status, measuring end-to-end orchestration latency
// (POST -> dispatch ErrandRequest to N stubs -> N ErrandResult ->
// voyage-target-terminal -> audit-INSERT). Returns VoyageID for cleanup even
// on a poll error (caller must remove the Voyage from PG -- DELETE via API
// is unavailable for terminal Voyages).
func RunCommandVoyage(ctx context.Context, opts VoyageRunOptions) (*VoyageRunReport, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("legion: empty BaseURL for Voyage")
	}
	if opts.JWT == "" {
		return nil, fmt.Errorf("legion: empty JWT for Voyage (admin token required)")
	}
	if opts.Coven == "" {
		return nil, fmt.Errorf("legion: empty coven for Voyage (nothing to target)")
	}
	module := opts.Module
	if module == "" {
		module = "core.cmd.shell"
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	reqBody := map[string]any{
		"kind":   "command",
		"module": module,
		"input":  opts.Input,
		"target": map[string]any{"coven": []string{opts.Coven}},
	}
	// concurrency=0 -> do NOT send the field (keeper default concurrency=1 +
	// one Leg over the whole scope = sequential, field shape -- top-level
	// "concurrency" *int omitempty, huma_voyage_op.go:47, minimum:1
	// maximum:500).
	if opts.Concurrency > 0 {
		reqBody["concurrency"] = opts.Concurrency
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("legion: marshal voyage body: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}

	createStart := time.Now()
	created, err := createVoyage(ctx, client, opts.BaseURL, opts.JWT, body)
	if err != nil {
		return nil, err
	}
	rep := &VoyageRunReport{
		VoyageID:  created.VoyageID,
		ScopeSize: created.ScopeSize,
		CreateLat: time.Since(createStart),
	}

	// Poll until terminal: end-to-end = from accepting create to terminal
	// status.
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			rep.EndToEnd = time.Since(createStart)
			rep.FinalStatus = "ctx_cancelled"
			return rep, ctx.Err()
		}
		if time.Now().After(deadline) {
			rep.EndToEnd = time.Since(createStart)
			if rep.FinalStatus == "" {
				rep.FinalStatus = "timeout"
			}
			return rep, fmt.Errorf("legion: Voyage %s did not reach terminal within %s (last status %q)",
				rep.VoyageID, timeout, rep.FinalStatus)
		}

		status, succeeded, failed, gerr := getVoyageStatus(ctx, client, opts.BaseURL, opts.JWT, rep.VoyageID)
		rep.Polls++
		if gerr != nil {
			// Transient GET error -- retry until deadline (does not abort
			// the run).
			select {
			case <-ctx.Done():
			case <-time.After(poll):
			}
			continue
		}
		rep.FinalStatus = status
		rep.Succeeded = succeeded
		rep.Failed = failed
		if _, terminal := voyageTerminalStatuses[status]; terminal {
			rep.EndToEnd = time.Since(createStart)
			return rep, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}
}

// voyageCreateResp -- fields of the 202 body of POST /v1/voyages that the
// legion needs.
type voyageCreateResp struct {
	VoyageID  string `json:"voyage_id"`
	ScopeSize int    `json:"scope_size"`
	Status    string `json:"status"`
}

func createVoyage(ctx context.Context, client *http.Client, base, jwt string, body []byte) (voyageCreateResp, error) {
	var out voyageCreateResp
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/voyages", bytes.NewReader(body))
	if err != nil {
		return out, fmt.Errorf("legion: build create voyage: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("legion: POST /v1/voyages: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return out, fmt.Errorf("legion: POST /v1/voyages: HTTP %d: %s", resp.StatusCode, truncate(raw, 400))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("legion: decode create voyage: %w", err)
	}
	if out.VoyageID == "" {
		return out, fmt.Errorf("legion: POST /v1/voyages: empty voyage_id in response: %s", truncate(raw, 400))
	}
	return out, nil
}

// voyageGetResp -- fields of GET /v1/voyages/{id} that the legion needs
// (status + summary).
type voyageGetResp struct {
	Status  string `json:"status"`
	Summary *struct {
		Succeeded int `json:"succeeded"`
		Failed    int `json:"failed"`
	} `json:"summary"`
}

func getVoyageStatus(ctx context.Context, client *http.Client, base, jwt, id string) (status string, succeeded, failed int, err error) {
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/voyages/"+id, nil)
	if rerr != nil {
		return "", 0, 0, rerr
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, derr := client.Do(req)
	if derr != nil {
		return "", 0, 0, derr
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(raw, 200))
	}
	var out voyageGetResp
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return "", 0, 0, uerr
	}
	if out.Summary != nil {
		succeeded = out.Summary.Succeeded
		failed = out.Summary.Failed
	}
	return out.Status, succeeded, failed, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
