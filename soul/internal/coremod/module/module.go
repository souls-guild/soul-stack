// Package module implements the `core.module` core module ([ADR-065]) —
// delivering a SoulModule plugin to a Soul host.
//
// States:
//   - installed: an artifact covered by an active Sigil grant is pulled from
//     Keeper (FetchModule), verified, and atomically installed into the
//     catalog slot `<paths.modules>/<alias>/`. Idempotency is by artifact
//     sha256 against the active grant.
//
// The apply-flow order is normative ([ADR-065](f)): allow-check BEFORE fetch
// → idempotency → fetch → verify BEFORE materialization → atomic rename →
// hot-register the custom-module registry ([Deps.Rescan], ADR-065(d)).
//
// [ADR-065]: docs/adr/0065-core-module-installed.md
package module

import (
	"context"
	"fmt"
	"regexp"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — the canonical address prefix.
const Name = "core.module"

const stateInstalled = "installed"

// reAlias — the param name format: a registration alias, address level 1. Same
// kebab-case grammar as a module name, and a single path element, since the alias also
// names the slot directory and the artifact inside it.
//
// The pattern is [sharedplugin.AliasPattern], not a copy of it: this predicate and the
// Keeper-side producer of these params are the two ends NIM-524 found disagreeing, and
// the guard that now pins them together ([config.SynthesizeModuleInstalls]'s property
// test) is only worth anything if both ends read the same rule.
//
// This is a FORM check only. Whether an alias is reserved (`core`, `keeper`, `soul`, …)
// is decided at registration on the Keeper, where the operator picks it; by the time a
// grant reaches a Soul the name has already been accepted.
var reAlias = regexp.MustCompile(sharedplugin.AliasPattern)

// Fetcher — the FetchModule transport ([ADR-012] third RPC, ADR-065(a)).
// Implemented by soulgrpc.StreamSession; reaches the run via context
// ([WithFetcher]) — fetch is bound to a live EventStream session, the module
// itself is stateless.
//
// [ADR-012]: docs/adr/0012-keeper-soul-grpc.md
type Fetcher interface {
	FetchModule(ctx context.Context, req *keeperv1.PluginFetchRequest) (grpc.ServerStreamingClient[keeperv1.PluginChunk], error)
}

type fetcherKey struct{}

// WithFetcher puts the current session's FetchModule transport into the run
// ctx (pattern from augur.WithRun: the SoulModule state+params contract
// doesn't expose the session).
func WithFetcher(ctx context.Context, f Fetcher) context.Context {
	if f == nil {
		return ctx
	}
	return context.WithValue(ctx, fetcherKey{}, f)
}

func fetcherFrom(ctx context.Context) (Fetcher, bool) {
	f, ok := ctx.Value(fetcherKey{}).(Fetcher)
	return f, ok
}

// Deps — the module's host dependencies; wired up in buildRegistry (cmd/soul).
// The zero value is valid and means fail-closed: with no Sigil set, every
// install rejects with module_not_allowed (push-mode `soul apply` with no
// broadcast cache).
type Deps struct {
	// Sigils — the active local grant set (sigilcache runtime cache via the
	// pluginhost adapter). nil means no grants.
	Sigils sharedhost.SigilLookup
	// Anchors — Sigil signature trust anchors, for verifying downloaded bytes.
	Anchors *sharedhost.AnchorSet
	// ModulesRoot — root of Soul's module cache (paths.modules).
	ModulesRoot string
	// Rescan — hot-registers the custom-module registry after a successful
	// install (ADR-065(d)): the installed module becomes available to the
	// next task in the same run. nil means re-discover only happens at
	// daemon startup.
	Rescan func()
}

// Module — the sdk/module.SoulModule implementation for core.module.
type Module struct {
	deps Deps
}

func New(deps Deps) *Module { return &Module{deps: deps} }

// Validate: known-state + required come from the embedded core declaration; beyond
// that, semantics the input DSL can't express — the alias format of name.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)

	if util.ParamPresent(req.GetParams(), "name") {
		name, err := util.StringParam(req.GetParams(), "name")
		if err != nil {
			errs = append(errs, err.Error())
		} else if !reAlias.MatchString(name) {
			errs = append(errs, fmt.Sprintf("param %q: expected a registration alias (e.g. redis), got %q", "name", name))
		}
	}
	if _, err := util.OptStringParam(req.GetParams(), "ref"); err != nil {
		errs = append(errs, err.Error())
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no-op (no PlanReadSafe): the host applies default-deny on dry_run.
// Pure-read drift would need an allow-check + sha comparison without side
// effects — a separate slice if actually requested.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.GetState() != stateInstalled {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q (want %s)", req.GetState(), stateInstalled))
	}
	return m.applyInstalled(stream, req)
}
