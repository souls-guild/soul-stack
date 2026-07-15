package beacon

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

// HTTPUnhealthyName is the core-beacon address (`core.beacon.<name>`, VigilDef.check).
const HTTPUnhealthyName = beaconaddr.HTTPUnhealthy

const (
	stateHTTPHealthy   State = "healthy"
	stateHTTPUnhealthy State = "unhealthy"
)

// httpUnhealthyDefaultTimeout is the timeout for one health GET. Short: a
// beacon tick, not a download (mirrors core.http.probe).
const httpUnhealthyDefaultTimeout = 30 * time.Second

// HTTPUnhealthy is a core-beacon that observes an HTTP endpoint's health
// (ADR-030). Read-only: a single GET, body not read. State: "healthy" if the
// status code is in status_codes (default [200]), otherwise "unhealthy". A
// transport error (DNS/TLS/timeout/unreachable) is also "unhealthy" from the
// observer's perspective (an event of interest, not a Check error).
// healthy↔unhealthy transition is edge-triggered → Portent.
//
// Security is reused from core.http (opt-out security-vs-flexibility
// pattern, mirrors core.http.probe / core.url): util.ValidateFetchURL +
// util.NewHTTPClient (SSRF guard at the dial phase, redirect downgrade
// protection, system TLS trust store). Default is maximally secure (https +
// SSRF guard + TLS verification); for an internal target
// (`https://127.0.0.1:8443/health`, RFC1918) the operator explicitly raises
// the opt-out flags in VigilDef.params. data carries NO body/headers
// (sensitive) — only url and status code.
//
// No warn on guard-lowering here (unlike apply modules): beacon is a
// scheduled read-probe with no output-warnings channel; the explicit flag in
// Vigil.params already is the operator's consent.
//
// Params:
//   - `url` (string, required) — https endpoint (http:// only with allow_http);
//   - `status_codes` (list of int, optional, default [200]) — "healthy" codes;
//   - `timeout` (string duration, optional, default "30s");
//   - `allow_http` (bool, optional, default false) — accept http:// (lifts
//     https-only and redirect downgrade protection, doesn't open SSRF);
//   - `insecure_skip_verify` (bool, optional, default false) — skip TLS
//     verification (self-signed / internal CA);
//   - `allow_private` (bool, optional, default false) — lift the SSRF dial
//     guard (loopback/RFC1918 internal endpoint).
type HTTPUnhealthy struct {
	// NewClient is a field so unit tests can substitute a fake HTTPDoer
	// (no network access). In production — util.NewHTTPClient.
	NewClient func(util.HTTPClientOpts) util.HTTPDoer
}

// NewHTTPUnhealthy builds a beacon with the production HTTP client factory.
func NewHTTPUnhealthy() *HTTPUnhealthy {
	return &HTTPUnhealthy{
		NewClient: func(opts util.HTTPClientOpts) util.HTTPDoer { return util.NewHTTPClient(opts) },
	}
}

func (b *HTTPUnhealthy) Check(ctx context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	rawURL, err := util.StringParam(params, "url")
	if err != nil {
		return "", nil, err
	}
	allowHTTP, err := util.OptBoolParam(params, "allow_http")
	if err != nil {
		return "", nil, err
	}
	if verr := util.ValidateFetchURL(rawURL, allowHTTP); verr != nil {
		return "", nil, verr
	}
	allowPrivate, err := util.OptBoolParam(params, "allow_private")
	if err != nil {
		return "", nil, err
	}
	insecureSkipVerify, err := util.OptBoolParam(params, "insecure_skip_verify")
	if err != nil {
		return "", nil, err
	}
	wantCodes, err := util.OptIntSliceParam(params, "status_codes")
	if err != nil {
		return "", nil, err
	}
	if len(wantCodes) == 0 {
		wantCodes = []int64{http.StatusOK}
	}
	timeout, err := optBeaconTimeout(params, httpUnhealthyDefaultTimeout)
	if err != nil {
		return "", nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("build request for %s: %v", rawURL, err)
	}

	// The client is built from the task's opt-out flags (zero opts = maximally
	// secure client: SSRF guard + downgrade protection + TLS verification).
	// The three contours are orthogonal — allow_http doesn't open SSRF
	// (dial guard is separate).
	resp, derr := b.NewClient(util.HTTPClientOpts{
		AllowPrivate:       allowPrivate,
		InsecureSkipVerify: insecureSkipVerify,
		AllowHTTPRedirect:  allowHTTP,
	}).Do(req)
	if derr != nil {
		// A transport error means the endpoint is unreachable to the observer →
		// "unhealthy" (status 0). A valid state, not a Check error.
		return stateHTTPUnhealthy, httpData(rawURL, 0), nil
	}
	_ = resp.Body.Close()

	if containsStatus(wantCodes, resp.StatusCode) {
		return stateHTTPHealthy, httpData(rawURL, resp.StatusCode), nil
	}
	return stateHTTPUnhealthy, httpData(rawURL, resp.StatusCode), nil
}

// httpData carries ONLY url and status code. Response body and headers never
// land here: sensitive-by-construction (ADR-010 §7.4) — beacon doesn't leak
// payload into Portent/logs. status == 0 means a transport error (endpoint
// unreachable).
func httpData(url string, status int) *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"url":    url,
		"status": status,
	})
	return s
}

// containsStatus does a linear scan (status_codes is a short list, typically 1–3).
func containsStatus(codes []int64, status int) bool {
	for _, c := range codes {
		if c == int64(status) {
			return true
		}
	}
	return false
}
