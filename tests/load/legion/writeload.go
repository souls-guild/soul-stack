package legion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// WriteLoadOptions — parameters of the write axis (docs/testing/load-testing.md): a
// write+audit-path profile under load via create→delete cycles of safe entities.
// Unlike axis B (read-only/dry-resolve, no registry mutation), this axis measures
// the write path itself: each POST goes through validation+persist+audit-INSERT, each
// DELETE — cascading delete+audit-INSERT. Entities are chosen so the create→delete
// cycle is self-cleaning (no data accumulation in the registry) and NOT
// Tempo-limited (not Voyage/Errand). Each iteration deletes what it
// created; the final sweep is a safety net against leaks if a per-iteration delete fails.
type WriteLoadOptions struct {
	BaseURL     string        // http://127.0.0.1:8080 (OpenAPI listener, plain HTTP in dev)
	JWT         string        // admin Archon token (Authorization: Bearer ...)
	Concurrency int           // number of parallel workers (write is heavier than read -> fewer)
	Duration    time.Duration // run duration
}

// writeEntity — description of one create->delete entity hammered by the write axis.
// Iteration names are built as legionload-<kind>-w<worker>-<seq>, characters only
// [a-z0-9-] (patterns of all 4 endpoints forbid _ and . — see spec/live confirmation).
type writeEntity struct {
	kind       string                   // short name for report/entity names (a-z0-9-)
	listPath   string                   // GET /v1/<entity> for the final sweep
	createPath string                   // POST path (relative to BaseURL)
	createBody func(name string) []byte // create body with a unique name
	deletePath func(name string) string // DELETE path by name (relative to BaseURL)
}

// writeEntities — table of safe self-cleaning entities of the write axis.
// Bodies are minimal (required fields only). herald.config.url — MUST be
// https:// (netguard blocks http/loopback on this endpoint), otherwise POST returns 4xx.
func writeEntities() []writeEntity {
	jsonBody := func(m map[string]any) []byte {
		b, _ := json.Marshal(m)
		return b
	}
	return []writeEntity{
		{
			kind:       "synod",
			listPath:   "/v1/synods",
			createPath: "/v1/synods",
			createBody: func(name string) []byte { return jsonBody(map[string]any{"name": name}) },
			deletePath: func(name string) string { return "/v1/synods/" + name },
		},
		{
			kind:       "role",
			listPath:   "/v1/roles",
			createPath: "/v1/roles",
			createBody: func(name string) []byte { return jsonBody(map[string]any{"name": name}) },
			deletePath: func(name string) string { return "/v1/roles/" + name },
		},
		{
			kind:       "push-provider",
			listPath:   "/v1/push-providers",
			createPath: "/v1/push-providers",
			createBody: func(name string) []byte { return jsonBody(map[string]any{"name": name}) },
			deletePath: func(name string) string { return "/v1/push-providers/" + name },
		},
		{
			kind:       "herald",
			listPath:   "/v1/heralds",
			createPath: "/v1/heralds",
			createBody: func(name string) []byte {
				return jsonBody(map[string]any{
					"name": name,
					"type": "webhook",
					"config": map[string]any{
						// MUST be https:// — netguard blocks http/loopback.
						"url": "https://example.com/hook",
					},
				})
			},
			deletePath: func(name string) string { return "/v1/heralds/" + name },
		},
	}
}

// WriteEntityStat — aggregate for one entity: create and delete are measured separately
// (two report lines per entity — POST <kind> and DELETE <kind>).
type WriteEntityStat struct {
	Kind   string
	Create EndpointStat // POST: req=successful 201, err=non-201/transport
	Delete EndpointStat // DELETE: req=successful 204, err=non-204/transport
}

// WriteLoadReport — write axis summary: per-kind create/delete stats + sweep +
// first error across all endpoints.
type WriteLoadReport struct {
	Entities []WriteEntityStat
	Swept    int           // how many residual legionload-* the final sweep removed
	Wall     time.Duration // actual duration of the create->delete cycle
	FirstErr string        // first error (with kind+operation name)
}

// RunWriteLoad runs create->delete cycles of safe entities across Concurrency
// workers for Duration. Each worker round-robins the entity table: each iteration
// creates an entity with a UNIQUE name (legionload-<kind>-w<worker>-<seq>), and on
// 201 immediately deletes it, measuring create and delete latency SEPARATELY per
// kind. On a non-201 create, delete is NOT sent (nothing to delete) — the error is
// recorded in Create.Errors. After the cycle — a best-effort sweep: for each entity,
// GET the list, filter by the legionload- prefix, DELETE residuals (a safety net
// against leaks if a delete fails mid-cycle).
func RunWriteLoad(ctx context.Context, opts WriteLoadOptions) (*WriteLoadReport, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("legion: empty BaseURL for write load")
	}
	if opts.JWT == "" {
		return nil, fmt.Errorf("legion: empty JWT for write load (admin token required)")
	}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 8
	}

	ents := writeEntities()

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        conc * 2,
			MaxIdleConnsPerHost: conc * 2,
			MaxConnsPerHost:     conc * 2,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	// Separate create/delete accumulators per entity.
	createAcc := make([]endpointAcc, len(ents))
	deleteAcc := make([]endpointAcc, len(ents))

	loadCtx := ctx
	var cancel context.CancelFunc
	if opts.Duration > 0 {
		loadCtx, cancel = context.WithTimeout(ctx, opts.Duration)
		defer cancel()
	}

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			seq := 0
			for loadCtx.Err() == nil {
				for i := range ents {
					if loadCtx.Err() != nil {
						return
					}
					seq++
					name := fmt.Sprintf("legionload-%s-w%d-%d", ents[i].kind, worker, seq)
					if writeCreate(loadCtx, client, opts.BaseURL, opts.JWT, ents[i].createPath, ents[i].createBody(name), &createAcc[i]) {
						writeDelete(loadCtx, client, opts.BaseURL, opts.JWT, ents[i].deletePath(name), &deleteAcc[i])
					}
				}
			}
		}(w)
	}
	wg.Wait()
	wall := time.Since(start)

	rep := &WriteLoadReport{
		Entities: make([]WriteEntityStat, len(ents)),
		Wall:     wall,
	}
	for i := range ents {
		rep.Entities[i] = WriteEntityStat{
			Kind:   ents[i].kind,
			Create: createAcc[i].finalize("POST "+ents[i].kind, wall),
			Delete: deleteAcc[i].finalize("DELETE "+ents[i].kind, wall),
		}
		if rep.FirstErr == "" && createAcc[i].firstHTTPErr != "" {
			rep.FirstErr = "POST " + ents[i].kind + ": " + createAcc[i].firstHTTPErr
		}
		if rep.FirstErr == "" && deleteAcc[i].firstHTTPErr != "" {
			rep.FirstErr = "DELETE " + ents[i].kind + ": " + deleteAcc[i].firstHTTPErr
		}
	}

	// Final sweep — on a background context (like souls-cleanup): clean up
	// residual legionload-* even if the main ctx is already cancelled (Ctrl-C/Duration).
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	rep.Swept = sweepResidual(sctx, client, opts.BaseURL, opts.JWT, ents)
	scancel()
	if rep.Swept > 0 {
		fmt.Printf("[write] sweep: removed %d residual\n", rep.Swept)
	}
	return rep, nil
}

// writeCreate sends a POST create and records latency/error. Returns true
// only on HTTP 201 (there is something to delete); otherwise records the error and
// returns false (no delete sent). Context cancellation (Duration expired) is a
// normal end of the run, not an error.
func writeCreate(ctx context.Context, client *http.Client, base, jwt, path string, body []byte, acc *endpointAcc) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		acc.recordErr(err.Error())
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		acc.recordErr(fmt.Sprintf("HTTP %d", resp.StatusCode))
		return false
	}
	acc.record(time.Since(t0))
	return true
}

// writeDelete sends a DELETE and records latency/error. Success is HTTP 204.
func writeDelete(ctx context.Context, client *http.Client, base, jwt, path string, acc *endpointAcc) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+path, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		acc.recordErr(err.Error())
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		acc.recordErr(fmt.Sprintf("HTTP %d", resp.StatusCode))
		return
	}
	acc.record(time.Since(t0))
}

// sweepResidual — best-effort safety net against leaks: for each entity GET the
// list, filter names by the legionload- prefix, DELETE each. Bounded (in case
// delete failed mid-cycle — otherwise the lists are already empty). Normally
// per-iteration delete cleans everything up and sweep removes 0. Sweep errors are
// silently swallowed — this is cleanup, not something being measured.
func sweepResidual(ctx context.Context, client *http.Client, base, jwt string, ents []writeEntity) int {
	const maxPerKind = 5000 // safety cap: don't loop over a huge foreign list
	total := 0
	for i := range ents {
		names := listResidualNames(ctx, client, base, jwt, ents[i].listPath)
		swept := 0
		for _, name := range names {
			if swept >= maxPerKind {
				break
			}
			if !strings.HasPrefix(name, "legionload-") {
				continue
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+ents[i].deletePath(name), nil)
			if err != nil {
				continue
			}
			req.Header.Set("Authorization", "Bearer "+jwt)
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				swept++
			}
		}
		total += swept
	}
	return total
}

// listResidualNames does a single GET <listPath> and extracts name fields from the
// response. All 4 list endpoints return either a flat array of objects or a
// wrapper {"items":[...]} — we handle both variants via the name field. Errors ->
// empty list (sweep skips this kind, the main per-iteration delete already cleaned
// up regardless).
func listResidualNames(ctx context.Context, client *http.Client, base, jwt, listPath string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+listPath, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	// First try the wrapper {"items":[{name}...]}, then a flat array [{name}].
	var wrapped struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped.Items) > 0 {
		out := make([]string, 0, len(wrapped.Items))
		for _, it := range wrapped.Items {
			if it.Name != "" {
				out = append(out, it.Name)
			}
		}
		return out
	}
	var flat []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &flat) == nil {
		out := make([]string, 0, len(flat))
		for _, it := range flat {
			if it.Name != "" {
				out = append(out, it.Name)
			}
		}
		return out
	}
	return nil
}
