// Package url implements the `core.url` core module ([ADR-015]) — downloads a
// file from a URL (analogous to Ansible's get_url).
//
// State:
//   - fetched: downloads the file at url to path. Idempotent via checksum (if
//     given) or by comparing SHA-256 of the content.
//
// Security ([ADR-016] "security first", secure-by-default + explicit opt-out):
// maximally strict by default, the operator lifts each guard individually:
//   - https:// only — http:// and file:// are rejected in Validate (guards
//     against downgrade and local-FS reads); lifted by allow_http (permits
//     http://, but does NOT open up SSRF — the dial-guard is separate);
//   - SSRF guard: dialing metadata/loopback/RFC1918/link-local addresses is
//     blocked based on the actually-resolved IP (closes direct SSRF and DNS
//     rebinding); lifted by allow_private (for legitimate internal endpoints),
//     see util.NewHTTPClient;
//   - TLS — system trust store by default; chain verification is lifted by
//     insecure_skip_verify (self-signed / internal CA, MITM risk);
//   - checksum verification happens on the temp file BEFORE it's published to
//     path: a bad hash never materializes (supply-chain protection);
//   - headers are sensitive-by-construction ([ADR-010] §7.4): values are never
//     logged and never land in output/register;
//   - lifting any guard flag adds a warning to the ApplyEvent output (the
//     warnings field, core.repo/core.http convention): the operator sees the
//     weakened guard in RunResult. Only the host goes into the warning (never
//     the full URL or headers — they may carry secrets).
//
// The three flags are orthogonal: allow_http weakens ONLY the scheme check,
// allow_private — ONLY the dial-guard, insecure_skip_verify — ONLY the TLS chain.
//
// [ADR-010]: docs/adr/0010-templating.md
// [ADR-015]: docs/adr/0015-core-modules-mvp.md
// [ADR-016]: docs/adr/0016-parity-license.md
package url

import (
	"context"
	"fmt"
	"os/user"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name is the canonical module address.
const Name = "core.url"

// defaultTimeout is the default request timeout when the timeout param is unset.
const defaultTimeout = 300 * time.Second

// Module implements sdk/module.SoulModule for core.url.
//
// NewClient / Lookup{User,Group} are fields so unit tests can swap them in
// (pattern mirrors util.Runner / LookupUser injection in sibling modules;
// NewClient is the same test seam as core.http).
type Module struct {
	// NewClient builds an HTTP client for the per-Apply set of opt-out flags
	// (allow_http / allow_private / insecure_skip_verify). Production uses
	// util.NewHTTPClient (New()'s default); tests swap in a fake-client
	// constructor. Each Apply rebuilds the client from its own parsed flags —
	// flags from one task must not leak into another.
	NewClient func(util.HTTPClientOpts) util.HTTPDoer
	// LookupUser / LookupGroup are swap points for owner/group unit tests.
	LookupUser  func(name string) (*user.User, error)
	LookupGroup func(name string) (*user.Group, error)
}

func New() *Module {
	return &Module{
		// Default factory: system trust store, downgrade protection on
		// redirects, SSRF guard — all governed by the passed flags (zero-value
		// opts = the most secure client, see util.NewHTTPClient).
		NewClient: func(opts util.HTTPClientOpts) util.HTTPDoer {
			return util.NewHTTPClient(opts)
		},
		LookupUser:  user.Lookup,
		LookupGroup: user.LookupGroup,
	}
}

// Validate is NOT fully delegated to util.ValidateAgainstManifest (unlike
// core.exec): beyond known-state + required, core.url has semantic checks the
// manifest DSL can't express — URL scheme (util.ValidateFetchURL: https by
// default, http(s) with allow_http), checksum format ("sha256:<hex>"), timeout
// duration parsing. These are critical (ADR-016 "security first": http
// downgrade/SSRF is rejected at Validate). known-state/required intentionally
// duplicate url.yaml — a single source isn't possible without these semantics
// in the DSL.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != "fetched" {
		errs = append(errs, fmt.Sprintf("unknown state %q (want fetched)", req.State))
	}

	// opt-out flags are type-checked here too; allow_http affects which url
	// schemes are valid (https-only vs http(s)), so it's parsed before the url check.
	allowHTTP, err := util.OptBoolParam(req.Params, "allow_http")
	if err != nil {
		errs = append(errs, err.Error())
	}
	if _, berr := util.OptBoolParam(req.Params, "insecure_skip_verify"); berr != nil {
		errs = append(errs, berr.Error())
	}
	if _, berr := util.OptBoolParam(req.Params, "allow_private"); berr != nil {
		errs = append(errs, berr.Error())
	}

	rawURL, err := util.StringParam(req.Params, "url")
	if err != nil {
		errs = append(errs, err.Error())
	} else if serr := util.ValidateFetchURL(rawURL, allowHTTP); serr != nil {
		errs = append(errs, serr.Error())
	}

	if _, err := util.StringParam(req.Params, "path"); err != nil {
		errs = append(errs, err.Error())
	}

	if cs, err := util.OptStringParam(req.Params, "checksum"); err != nil {
		errs = append(errs, err.Error())
	} else if cs != "" {
		if _, _, cerr := parseChecksum(cs); cerr != nil {
			errs = append(errs, cerr.Error())
		}
	}

	if ts, err := util.OptStringParam(req.Params, "timeout"); err != nil {
		errs = append(errs, err.Error())
	} else if ts != "" {
		if _, derr := parseTimeout(ts); derr != nil {
			errs = append(errs, derr.Error())
		}
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan is a no-op (no PlanReadSafe). core.url doesn't declare a read-safe Plan
// in the MVP; the host applies default-deny on dry_run (FAILED
// `plan.unsupported`). Reason: pure-read drift ("does this need downloading?")
// for the checksum-less branch requires a HEAD (or GET) request to the remote,
// which Apply doesn't do before mutating (Apply GETs straight into a temp
// file). The checksum branch is theoretically derivable from an existing read
// (sha256 of the local file vs. checksum), but implementing half the contract
// would make dry_run unpredictable (depends on whether the task has a
// checksum). A full pure-read path is a separate slice: either a HEAD-probe
// with opt-out flags symmetric to Apply, or an explicit checksum-based split.
// Default-deny for now.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != "fetched" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	return m.applyFetched(stream, req)
}

// parseTimeout parses the timeout param per Soul Stack's `duration` convention
// (Go time.ParseDuration + `<N>d` suffix), via the shared shared/config parser.
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
