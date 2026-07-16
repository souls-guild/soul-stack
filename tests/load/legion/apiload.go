package legion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// APILoadOptions -- axis B parameters (docs/testing/load-testing.md §2):
// concurrent run over fleet-dependent /v1 handlers on top of a background of
// N connected stubs. The cost of these handlers grows with souls registry
// size (presence resolve on list, roster resolve of coven on preview), so
// running them only makes sense against a live legion.
type APILoadOptions struct {
	BaseURL     string        // http://127.0.0.1:8080 (OpenAPI listener, plain HTTP in dev)
	JWT         string        // admin-Archon token (Authorization: Bearer ...)
	Coven       string        // target coven for POST /v1/voyages/preview (= legion --coven)
	Concurrency int           // number of parallel worker clients
	Duration    time.Duration // run duration
}

// endpoint -- description of one hammered handler in the axis B table. Safe
// handlers for the run: read-only GET-collection (hammer without mutating
// the registry) + the single read-like POST /v1/voyages/preview (dry-resolve,
// no Voyage creation, no audit). method+path are fixed; body is non-empty
// only for preview.
type endpoint struct {
	name   string // human-readable name (method+path) for report/FirstErr
	method string
	path   string // relative to BaseURL (with query for pagination)
	body   []byte // nil for GET
}

// EndpointStat -- latency aggregate for one handler over an axis B run.
type EndpointStat struct {
	Name       string        // human-readable name (method+path)
	Requests   int           // successful (2xx) requests
	Errors     int           // non-2xx / transport errors
	P50        time.Duration // median latency of successful requests
	P99        time.Duration
	Max        time.Duration
	Throughput float64 // successful req/s over the actual duration
}

// APILoadReport -- axis B outcome: per-endpoint stats + first error.
// Skipped -- handler paths excluded by the start probe (returned 404 "no such
// endpoint" -- not mounted in this keeper config, nothing to measure).
type APILoadReport struct {
	Endpoints []EndpointStat
	Skipped   []string      // handler paths excluded by the probe (404, not mounted)
	Wall      time.Duration // actual run duration
	FirstErr  string        // first error on any handler (with its name)
}

// endpointAcc -- thread-safe accumulator of measurements for one handler.
type endpointAcc struct {
	mu           sync.Mutex
	lat          []time.Duration
	reqs         int64
	errs         int64
	firstHTTPErr string
}

func (a *endpointAcc) record(d time.Duration) {
	a.mu.Lock()
	a.lat = append(a.lat, d)
	a.mu.Unlock()
	atomic.AddInt64(&a.reqs, 1)
}

func (a *endpointAcc) recordErr(msg string) {
	atomic.AddInt64(&a.errs, 1)
	a.mu.Lock()
	if a.firstHTTPErr == "" {
		a.firstHTTPErr = msg
	}
	a.mu.Unlock()
}

func (a *endpointAcc) finalize(name string, wall time.Duration) EndpointStat {
	a.mu.Lock()
	lat := a.lat
	a.mu.Unlock()
	st := EndpointStat{
		Name:     name,
		Requests: int(atomic.LoadInt64(&a.reqs)),
		Errors:   int(atomic.LoadInt64(&a.errs)),
	}
	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		st.P50 = percentile(lat, 50)
		st.P99 = percentile(lat, 99)
		st.Max = lat[len(lat)-1]
	}
	if wall > 0 {
		st.Throughput = float64(st.Requests) / wall.Seconds()
	}
	return st
}

// safeEndpoints assembles the table of handlers safe to run: 24 read-only
// GET-collections (hammer without mutating the registry) + the single
// read-like POST /v1/voyages/preview (dry-resolve by the legion's coven --
// does NOT create a Voyage and does not write audit). List handlers with
// pagination get ?limit=100, catalogs/me are bare. Paths checked against
// /openapi.json. previewBody -- the dry-resolve body.
func safeEndpoints(baseURL string, previewBody []byte) []endpoint {
	get := []string{
		"/v1/souls?limit=100",
		"/v1/audit?limit=100",
		"/v1/voyages?limit=100",
		"/v1/errands?limit=100",
		"/v1/incarnations",
		"/v1/cadences",
		"/v1/operators",
		"/v1/synods",
		"/v1/services",
		"/v1/push-runs?limit=100",
		"/v1/push-providers",
		"/v1/heralds",
		"/v1/decrees",
		"/v1/vigils",
		"/v1/tidings",
		"/v1/augur/omens",
		// rites requires a mandatory omen query param (valid per
		// ^[a-z0-9-]{1,63}$); without it the handler 422s. load-probe -- a
		// fixed valid omen, returns 200 with a (possibly empty) rites list.
		"/v1/augur/rites?omen=load-probe",
		"/v1/sigil/keys",
		"/v1/plugins/sigils",
		"/v1/modules",
		"/v1/event-types",
		"/v1/permissions",
		"/v1/roles",
		"/v1/me/permissions",
	}
	eps := make([]endpoint, 0, len(get)+1)
	for _, p := range get {
		eps = append(eps, endpoint{
			name:   "GET " + p,
			method: http.MethodGet,
			path:   baseURL + p,
		})
	}
	eps = append(eps, endpoint{
		name:   "POST /v1/voyages/preview (coven)",
		method: http.MethodPost,
		path:   baseURL + "/v1/voyages/preview",
		body:   previewBody,
	})
	return eps
}

// RunAPILoad runs all safe fleet-dependent handlers (see safeEndpoints) in
// parallel across Concurrency workers for Duration. Each worker cycles
// through the whole handler list (round-robin), so each handler gets ~equal
// load share. Measures per-endpoint p50/p99/throughput. A non-2xx response
// (e.g. a disabled feature) is counted in that handler's Errors and does NOT
// abort the run; dry-resolve preview does NOT create a Voyage (read-like, no
// audit) -- the run is safe.
func RunAPILoad(ctx context.Context, opts APILoadOptions) (*APILoadReport, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("legion: empty BaseURL for API load")
	}
	if opts.JWT == "" {
		return nil, fmt.Errorf("legion: empty JWT for API load (admin token required)")
	}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 16
	}

	// Preview body: kind=command, target by the legion's coven. Read-like
	// dry-resolve -- same validation/resolve as create, but without creating
	// a Voyage and without audit.
	previewBody, err := json.Marshal(map[string]any{
		"kind":   "command",
		"module": "core.cmd.shell",
		"input":  map[string]any{"cmd": "echo ok"},
		"target": map[string]any{"coven": []string{opts.Coven}},
	})
	if err != nil {
		return nil, fmt.Errorf("legion: marshal preview body: %w", err)
	}

	allEps := safeEndpoints(opts.BaseURL, previewBody)

	// Pool of reusable connections: API load must not bottleneck on TCP
	// handshake/conn churn (we measure Keeper, not the client).
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        conc * 2,
			MaxIdleConnsPerHost: conc * 2,
			MaxConnsPerHost:     conc * 2,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	// Start probe: one request per handler. Some /v1 handlers are mounted on
	// the router conditionally (when their service's Deps is non-nil); in
	// dev config their services may not be wired -- then the router returns
	// 404 "no such endpoint". This is a config norm, not a harness bug: such
	// handlers are excluded from the load. Other statuses (422/403/5xx) are
	// real signals, measured by the load loop, not excluded.
	eps, skipped := probeEndpoints(ctx, client, allEps, opts.JWT)
	accs := make([]endpointAcc, len(eps))

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
		go func() {
			defer wg.Done()
			for loadCtx.Err() == nil {
				for i := range eps {
					if loadCtx.Err() != nil {
						return
					}
					doRequest(loadCtx, client, eps[i].method, eps[i].path, opts.JWT, eps[i].body, &accs[i])
				}
			}
		}()
	}
	wg.Wait()
	wall := time.Since(start)

	rep := &APILoadReport{
		Endpoints: make([]EndpointStat, len(eps)),
		Skipped:   skipped,
		Wall:      wall,
	}
	for i := range eps {
		rep.Endpoints[i] = accs[i].finalize(eps[i].name, wall)
		if rep.FirstErr == "" && accs[i].firstHTTPErr != "" {
			rep.FirstErr = eps[i].name + ": " + accs[i].firstHTTPErr
		}
	}
	return rep, nil
}

// probeEndpoints sends ONE probe request per handler before the load loop
// and splits the table into mounted and skipped. The skip criterion is
// exactly HTTP 404 (the handler is conditionally not mounted in this keeper
// config). Any other outcome (2xx/422/403/5xx/transport error) -> the
// handler is considered mounted and goes into the load: non-404 statuses are
// real signals, must not be silenced (the "no silent cap" principle). The
// skip is logged explicitly in one [api] probe: ... line with the list of
// paths.
func probeEndpoints(ctx context.Context, client *http.Client, eps []endpoint, jwt string) (mounted []endpoint, skipped []string) {
	mounted = make([]endpoint, 0, len(eps))
	for i := range eps {
		if probeStatus(ctx, client, eps[i], jwt) == http.StatusNotFound {
			skipped = append(skipped, eps[i].path)
			continue
		}
		mounted = append(mounted, eps[i])
	}
	if len(skipped) > 0 {
		fmt.Printf("[api] probe: skipped %d unmounted handler(s) (404): %s\n",
			len(skipped), strings.Join(skipped, ", "))
	}
	return mounted, skipped
}

// probeStatus sends one probe request and returns the HTTP status. A
// transport error/timeout returns 0 (!= 404 -> the handler is not excluded:
// the load loop will account for drops in Errors itself). The probe request
// uses the same body as load (preview requires a valid body to resolve,
// otherwise it would return 4xx not because of mounting).
func probeStatus(ctx context.Context, client *http.Client, ep endpoint, jwt string) int {
	var rdr io.Reader
	if ep.body != nil {
		rdr = bytes.NewReader(ep.body)
	}
	req, err := http.NewRequestWithContext(ctx, ep.method, ep.path, rdr)
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	if ep.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// doRequest sends one request and records latency/error into acc. Context
// cancellation (Duration expired) is not counted as a handler error -- it's
// the normal end of the run.
func doRequest(ctx context.Context, client *http.Client, method, url, jwt string, body []byte, acc *endpointAcc) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return // normal end of the run
		}
		acc.recordErr(err.Error())
		return
	}
	// Drain the body: without this, the keep-alive connection won't be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		acc.recordErr(fmt.Sprintf("HTTP %d", resp.StatusCode))
		return
	}
	acc.record(time.Since(t0))
}
