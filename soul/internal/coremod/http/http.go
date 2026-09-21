// Package http implements the Soul-side `core.http` module ([ADR-015]) —
// read-only HTTP probes and explicit mutating HTTP API requests.
//
// Verbs:
//   - probe: GET/HEAD request to url, response returned via register
//     (status / body / elapsed_ms / headers_keys). Host state is never
//     mutated;
//   - request: one explicit POST/PUT/PATCH/DELETE request. A successful
//     response reports changed=true. Idempotency and retry belong to the
//     caller's API/scenario contract; the module never retries internally.
//
// changed semantics:
//   - probe: changed=false always, by construction;
//   - request: changed=true only when the one explicit mutating response has
//     a status listed in status_codes.
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
//   - probe redirects to non-https are blocked (util.CheckRedirect, downgrade
//     protection); with allow_http, its https→http downgrade hop is allowed.
//     request stops at the first response so 307/308 cannot replay a mutation;
//   - headers are sensitive-by-construction ([ADR-010] §7.4): values are
//     never logged or returned (output only lists the requested header
//     keys). An echoed value is masked by BYTE COVERAGE — every occurrence of
//     every sensitive value marks its bytes, and each maximal covered run
//     becomes one mask. Not per-occurrence replacement: occurrences can
//     overlap (two values sharing a boundary byte, or one value overlapping
//     itself when its first byte equals its last), and a left-to-right
//     replacer has already consumed past the second occurrence's start, so it
//     leaves up to len(value)-1 plaintext bytes of a credential in the body.
//     Header values are masked BEFORE vault-refs, so a value that itself
//     carries an unresolved ref is still matched whole.
//     A truncated response body additionally drops a trailing fragment of a
//     header value: a cut lands before redaction runs, so a straddling value
//     would survive as a plaintext prefix. There are THREE cuts, and each is
//     repaired — the byte cap, the rune trim right after it, and the output
//     cap that masking can push the text back over. The first two are
//     repaired while the bytes are still raw, before UTF-8 repair, because a
//     fragment ending inside a multi-byte rune is rewritten to U+FFFD and
//     cannot be found afterwards. Both cuts descend together over ONE
//     precomputed fragment table; the table is never rebuilt mid-descent,
//     because the rune trim removes one byte per step and the response body
//     is endpoint-controlled — a per-step rescan is a remote CPU sink.
//     Only an INCOMPLETE value is dropped: one that ends whole at the cut is
//     left to the masker, which is sound precisely because coverage unions
//     overlapping occurrences. The descent repeats until the cut is clean,
//     because removing one fragment can uncover another underneath it.
//     The request body param is not covered: it is payload, not a credential
//     (see docs/module/core/http/README.md).
//
// One Apply opens exactly one Do call. The one transport-level exception this
// module does not intercept: net/http treats a request carrying
// Idempotency-Key / X-Idempotency-Key as replayable and may resend it once
// when a pooled connection dies before any response byte. That is opt-in by
// the author — the header's whole meaning is "safe to repeat" — and silently
// stripping it would contradict them.
//
// Lifting any guard returns a warning in output (`warnings` field,
// core.repo/core.url convention): the operator sees the guard was weakened.
// The warning carries only the host (never the full URL or headers).
//
// Neither verb executes a subprocess or writes the filesystem. The module's
// only declared capability is network_outbound.
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

// defaultTimeout is the default per-request timeout when param timeout is unset.
// Shorter than core.url's (300s): this module talks to APIs, not downloads.
const defaultTimeout = 30 * time.Second

// defaultProbeMethod is the default method of the backward-compatible probe.
// request deliberately has no default: mutation must always be explicit.
const defaultProbeMethod = http.MethodGet

// maxBodyBytes hard-caps the readable response body (OOM protection on large
// responses). Bytes beyond the limit are discarded; output sets truncated=true.
const maxBodyBytes = 64 * 1024

// probeMethods and requestMethods keep the read/mutate boundary structural:
// no state accepts a method from the other set.
var probeMethods = map[string]struct{}{
	http.MethodGet:  {},
	http.MethodHead: {},
}

var requestMethods = map[string]struct{}{
	http.MethodPost:   {},
	http.MethodPut:    {},
	http.MethodPatch:  {},
	http.MethodDelete: {},
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
// module called it with). request additionally sets DisableRedirects, which
// is a mutation-cardinality invariant rather than a guard opt-out.
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
// by default, http(s) with allow_http), the disjoint per-verb method sets,
// and timeout duration parsing. These are critical (ADR-016: SSRF/http-
// downgrade and the read/mutate boundary are rejected at Validate). Bool-flag type checks (allow_private/
// allow_http/insecure_skip_verify) are here too, so a bad type fails before
// Apply. known-state/required intentionally duplicate the manifest — no single
// source is possible without these semantics in the DSL.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case "probe":
		if _, merr := normalizedProbeMethod(req.Params); merr != nil {
			errs = append(errs, merr.Error())
		}
	case "request":
		if _, merr := normalizedRequestMethod(req.Params); merr != nil {
			errs = append(errs, merr.Error())
		}
		if _, berr := util.OptStringParam(req.Params, "body"); berr != nil {
			errs = append(errs, berr.Error())
		}
		if _, cerr := util.OptStringParam(req.Params, "content_type"); cerr != nil {
			errs = append(errs, cerr.Error())
		}
	default:
		errs = append(errs, fmt.Sprintf("unknown verb %q (want probe|request)", req.State))
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

	if _, herr := util.OptStringMapParam(req.Params, "headers"); herr != nil {
		errs = append(errs, herr.Error())
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

// Plan is a no-op (no PlanReadSafe). core.http is a verb module: neither a
// probe nor an imperative mutation has desired host state to diff. The host
// therefore applies default-deny for dry_run instead of reporting a false clean.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	switch req.State {
	case "probe":
		return m.applyProbe(stream, req)
	case "request":
		return m.applyRequest(stream, req)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown verb %q", req.State))
	}
}

// normalizedProbeMethod returns the read-only probe method: empty → GET;
// otherwise GET|HEAD, compared upper-cased for backward compatibility.
func normalizedProbeMethod(params *structpb.Struct) (string, error) {
	raw, err := util.OptStringParam(params, "method")
	if err != nil {
		return "", err
	}
	if raw == "" {
		return defaultProbeMethod, nil
	}
	m := strings.ToUpper(raw)
	if _, ok := probeMethods[m]; !ok {
		return "", fmt.Errorf("param %q: unsupported method %q (want GET|HEAD)", "method", raw)
	}
	return m, nil
}

// normalizedRequestMethod requires an explicit mutating method. GET/HEAD are
// rejected here even though net/http could send them: probe owns all reads.
func normalizedRequestMethod(params *structpb.Struct) (string, error) {
	raw, err := util.StringParam(params, "method")
	if err != nil {
		return "", err
	}
	m := strings.ToUpper(raw)
	if _, ok := requestMethods[m]; !ok {
		return "", fmt.Errorf("param %q: unsupported method %q (want POST|PUT|PATCH|DELETE)", "method", raw)
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
