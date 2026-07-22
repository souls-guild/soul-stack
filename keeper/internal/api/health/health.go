// Package health implements `/healthz` (liveness) and `/readyz` (readiness)
// for the Keeper.
//
// Routes outside `/v1/*`, no auth (operator-api.md § Health / Meta), not
// written to audit (high-frequency probes from k8s/balancer).
//
// `/healthz` is a static 200; "process is alive". `/readyz` pings
// dependencies (Postgres + Redis are required, Vault if configured); on
// failure it returns 503 with the list of failed checks in JSON. The full
// set of dependencies is needed for fail-fast: an unhealthy Keeper returns
// `not_ready`, and the LB routes traffic to a healthy cluster instance
// (ADR-002, Keeper HA cluster).
package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// perCheckTimeout is a hard per-dependency timeout for the ping operation.
// Without it `/readyz` (an unauthenticated endpoint) becomes a DoS vector:
// an attacker opens hundreds of parallel requests, each holding a
// PG connection until the overall request timeout (tens of seconds).
// 2s is roughly "enough for a healthy PG/Vault, hits the slow path".
const perCheckTimeout = 2 * time.Second

// Pinger is the minimal health-check interface for a single dependency.
// PG-pool and Vault-client both implement `Ping(ctx) error`, so
// each of them fits without an adapter.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps holds the dependencies whose availability `/readyz` checks. PG and
// Redis are required (without them Keeper cannot serve requests: registries
// live in Postgres, lease/heartbeat in Redis — ADR-005/006); Vault is
// optional (nil if not configured in the installation). Any nil-Pinger
// check is skipped and not mentioned in the response.
type Deps struct {
	PG    Pinger
	Redis Pinger
	Vault Pinger
}

// Handler holds the assembled health endpoints, registered on the router.
type Handler struct {
	deps Deps
}

// NewHandler assembles a handler from the dependencies. Any dependency can
// be nil — the corresponding check is then skipped (the handler does not
// mention it in the response).
func NewHandler(deps Deps) *Handler {
	return &Handler{deps: deps}
}

// Healthz writes 200 OK with a fixed body. Does not depend on the state of
// external systems (by definition of liveness — "the process responds").
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz checks all non-nil dependencies in parallel, each under the
// per-check timeout ([perCheckTimeout]). Overall latency = max across
// checks (not sum) — needed for k8s probes with a short period.
//
// On failure of any check — 503 with the list of statuses in `checks{}`.
type readyResp struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	checks, ok := Check(r.Context(), h.deps)

	resp := readyResp{Checks: checks}
	status := http.StatusOK
	if ok {
		resp.Status = "ok"
	} else {
		resp.Status = "not_ready"
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

// Check pings all non-nil dependencies from [Deps] in parallel (each under
// [perCheckTimeout]) and returns a map `name → "ok" | failure reason` plus
// an aggregated "all ok" flag. A nil-Pinger check is skipped (not included
// in the map). Single source of health semantics: `/readyz` ([Handler.Readyz])
// and `GET /v1/cluster` self_health (per-instance PG/Redis/Vault) compute
// availability the same way — no divergence between the liveness probe and
// the cluster view.
func Check(ctx context.Context, deps Deps) (map[string]string, bool) {
	type result struct {
		name string
		msg  string // empty = ok
	}
	type namedPinger struct {
		name string
		p    Pinger
	}
	candidates := []namedPinger{
		{"postgres", deps.PG},
		{"redis", deps.Redis},
		{"vault", deps.Vault},
	}
	pingers := make([]namedPinger, 0, len(candidates))
	for _, c := range candidates {
		if c.p != nil {
			pingers = append(pingers, c)
		}
	}

	results := make([]result, len(pingers))
	var wg sync.WaitGroup
	for i, item := range pingers {
		wg.Add(1)
		go func(i int, name string, p Pinger) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, perCheckTimeout)
			defer cancel()
			err := p.Ping(cctx)
			if err == nil {
				results[i] = result{name: name}
				return
			}
			// Distinguish a timeout (our per-check ctx expired) from a
			// transport error — much more useful to the operator: timeout
			// = "hanging", error = "failing with a clear reason".
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				results[i] = result{name: name, msg: "ping timeout (2s)"}
				return
			}
			results[i] = result{name: name, msg: "unreachable: " + err.Error()}
		}(i, item.name, item.p)
	}
	wg.Wait()

	checks := make(map[string]string, len(results))
	ok := true
	for _, res := range results {
		if res.msg == "" {
			checks[res.name] = "ok"
			continue
		}
		checks[res.name] = res.msg
		ok = false
	}
	return checks, ok
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
