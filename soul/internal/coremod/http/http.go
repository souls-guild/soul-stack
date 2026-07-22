// Package http implements the core module `core.http` ([ADR-015]) — a
// read-probe for an HTTP endpoint (health-check / API-readiness / version
// read). A declarative HTTP read-probe, deliberately narrowed to reads
// only.
//
// Verb MVP:
//   - probe: GET/HEAD request to url, response returned via register
//     (status / body / elapsed_ms / headers_keys). Host state is never
//     mutated (see below), so this is a verb form, not declarative state.
//
// changed semantics:
//   - changed = false ALWAYS, by construction: a read-probe never mutates
//     host state. Precedent: `core.exec.run` (module reports facts,
//     `changed_when:` at the scenario level interprets them).
//
// Idempotent by nature (no-op on state).
//
// Security ([ADR-016] "security first"). Secure-by-default: all three
// guards are armed, each lifted only via its own explicit opt-out param
// (orthogonal — lifting one doesn't weaken the others):
//   - https-only (default): http:// and file:// are rejected
//     (util.ValidateFetchURL — https by default, http(s) with allow_http).
//     Lift to http(s) via `allow_http: true` (file:// stays forbidden);
//   - SSRF guard (default): probes to metadata/loopback/RFC1918/link-local
//     are blocked by the actually-resolved IP (closes direct SSRF on cloud
//     metadata IAM 169.254.169.254 and DNS-rebind, see util.NewHTTPClient).
//     Lift for legitimate internal health checks via `allow_private: true`;
//   - TLS verification (default): system trust store. Lift for self-signed/
//     internal CA via `insecure_skip_verify: true` (MITM risk);
//   - redirects to non-https are blocked (util.CheckRedirect, downgrade
//     protection); with allow_http, an https→http downgrade hop is allowed
//     (AllowHTTPRedirect);
//   - headers are sensitive-by-construction ([ADR-010] §7.4): values are
//     never logged or returned (output only lists the requested header
//     keys).
//
// Lifting any guard returns a warning in output (`warnings` field,
// core.repo/core.url convention): the operator sees the guard was weakened.
// The warning carries only the host (never the full URL or headers).
//
// Mutating HTTP (POST/PUT/PATCH/DELETE) is deliberately deferred post-MVP
// to a separate ADR extension (likely `core.http.request`), which will also
// settle the changed contract for mutations. Verb `probe` stays strictly
// read-only.
//
// [ADR-010]: docs/adr/0010-templating.md
// [ADR-015]: docs/adr/0015-core-modules-mvp.md
// [ADR-016]: docs/adr/0016-parity-license.md
package http

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name is the module's canonical address.
const Name = "core.http"

// defaultTimeout is the default probe timeout when param timeout is unset.
// Shorter than core.url's (300s): probe is a health-check, not a download.
const defaultTimeout = 30 * time.Second

// defaultMethod is the default HTTP method. GET/HEAD only (read-only).
const defaultMethod = http.MethodGet

// maxBodyBytes hard-caps the readable response body (OOM protection on large
// responses). Bytes beyond the limit are discarded; output sets truncated=true.
const maxBodyBytes = 64 * 1024

// allowedMethods are the read-only methods allowed by verb probe. Mutating
// methods (POST/PUT/PATCH/DELETE) are deliberately absent — see package doc.
var allowedMethods = map[string]struct{}{
	http.MethodGet:  {},
	http.MethodHead: {},
}

// Module implements sdk/module.SoulModule for core.http.
//
// The HTTP client is built per-call by factory NewClient from opt-out flags
// (allow_private / allow_http / insecure_skip_verify). Three orthogonal bools
// = 2³=8 combinations, so pre-built client instances don't scale — the
// client is built just-in-time from the task's actual flags.
//
// NewClient is a field so unit tests can substitute a factory returning a
// fake HTTPDoer with no network access (and assert which HTTPClientOpts the
// module called it with).
type Module struct {
	// NewClient builds the HTTP client from the task's opt-out flags. In
	// production: util.NewHTTPClient (system TLS trust store, redirect
	// downgrade protection, dial-phase SSRF guard; each guard independently
	// lifted via an opts field). Tests substitute a fake HTTPDoer.
	NewClient func(util.HTTPClientOpts) util.HTTPDoer
}

func New() *Module {
	return &Module{
		NewClient: func(opts util.HTTPClientOpts) util.HTTPDoer { return util.NewHTTPClient(opts) },
	}
}

// Validate is NOT fully delegated to util.ValidateAgainstManifest (unlike
// core.exec): beyond known-state + required, core.http has semantic checks
// the manifest DSL can't express — URL scheme (ValidateFetchURL, https-only
// by default, http(s) with allow_http), method enum (GET|HEAD), timeout
// duration parsing. These are critical (ADR-016: SSRF/http-downgrade/mutating
// methods are rejected at Validate). Bool-flag type checks (allow_private/
// allow_http/insecure_skip_verify) are here too, so a bad type fails before
// Apply. known-state/required intentionally duplicate http.yaml — no single
// source is possible without these semantics in the DSL.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != "probe" {
		errs = append(errs, fmt.Sprintf("unknown verb %q (want probe)", req.State))
	}

	// allow_http is checked before url: its value determines which scheme
	// ValidateFetchURL accepts (https-only if false, http(s) if true).
	allowHTTP, berr := util.OptBoolParam(req.Params, "allow_http")
	if berr != nil {
		errs = append(errs, berr.Error())
	}

	rawURL, err := util.StringParam(req.Params, "url")
	if err != nil {
		errs = append(errs, err.Error())
	} else if serr := util.ValidateFetchURL(rawURL, allowHTTP); serr != nil {
		errs = append(errs, serr.Error())
	}

	if _, merr := normalizedMethod(req.Params); merr != nil {
		errs = append(errs, merr.Error())
	}

	if _, serr := util.OptIntSliceParam(req.Params, "status_codes"); serr != nil {
		errs = append(errs, serr.Error())
	}

	if _, berr := util.OptBoolParam(req.Params, "allow_private"); berr != nil {
		errs = append(errs, berr.Error())
	}

	if _, berr := util.OptBoolParam(req.Params, "insecure_skip_verify"); berr != nil {
		errs = append(errs, berr.Error())
	}

	if ts, terr := util.OptStringParam(req.Params, "timeout"); terr != nil {
		errs = append(errs, terr.Error())
	} else if ts != "" {
		if _, derr := parseTimeout(ts); derr != nil {
			errs = append(errs, derr.Error())
		}
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan is a no-op (no PlanReadSafe). core.http is a verb module (probe): an
// HTTP endpoint read-probe has no desired host state to diff via pure-read
// (changed is always false by construction, see package doc). Drift per
// ADR-031 is undefined here. The host applies default-deny: dry_run for
// core.http returns FAILED `plan.unsupported` — a deliberate refusal, not a
// false "no drift". probe itself is read-only by nature but outside the
// ADR-031 Plan/Apply contract.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// ErrandReadSafe is marker [sdkmodule.ErrandReadSafe] (ADR-033 §2): probe is
// a read-only HTTP request that never mutates host state (`changed = false`
// by construction, see package doc). Safe for ad-hoc invocation via Errand,
// so the module explicitly opts into the Errand-runner whitelist. Verb
// modules core.cmd.shell / core.exec.run stay in the hardcoded list
// (imperative by design); this declares the pattern for future read-safe
// core additions and symmetry with the sdk/module interface contract.
func (m *Module) ErrandReadSafe() {}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != "probe" {
		return util.SendFailed(stream, fmt.Sprintf("unknown verb %q", req.State))
	}
	return m.applyProbe(stream, req)
}

// normalizedMethod returns the probe HTTP method: empty param → defaultMethod;
// otherwise the value validated against allowedMethods. Returns an error for
// an unknown/mutating method. Compared upper-cased (get → GET).
func normalizedMethod(params *structpb.Struct) (string, error) {
	raw, err := util.OptStringParam(params, "method")
	if err != nil {
		return "", err
	}
	if raw == "" {
		return defaultMethod, nil
	}
	m := strings.ToUpper(raw)
	if _, ok := allowedMethods[m]; !ok {
		return "", fmt.Errorf("param %q: unsupported method %q (want GET|HEAD)", "method", raw)
	}
	return m, nil
}

// parseTimeout parses param timeout per the Soul Stack `duration` convention
// (Go time.ParseDuration + `<N>d` suffix) via the shared/config parser
// (symmetric with core.url).
func parseTimeout(s string) (time.Duration, error) {
	d, err := config.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("param %q: invalid duration %q", "timeout", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("param %q: must be positive, got %q", "timeout", s)
	}
	return d, nil
}
