// Package augur implements the `core.augur.fetch` core module (ADR-025,
// docs/keeper/augur.md) — a Soul-side read-probe for live access to an
// external system through the Augur broker.
//
// MVP verb:
//   - fetch: request a value from an Omen (vault KV / prometheus / elk) via
//     the Keeper at apply time. The module sends an AugurRequest over
//     EventStream and waits for a correlated AugurReply; on OK it puts
//     inline_data into the register output.
//
// changed semantics:
//   - changed = false ALWAYS, by construction and not configurable: a
//     read-probe never changes host state (precedent: core.http.probe /
//     core.exec.run).
//
// ADR-012(d) boundary: data arrives inline THROUGH the Keeper (delegate=false,
// MVP-1). No external token/credential reaches Soul — the Augur client only
// knows the request_id correlation, never the master credential. Delegation
// (delegate=true, scoped_*) is MVP-2 and isn't handled here (the Augur client
// returns an error on OK without inline_data).
//
// Authorization is Keeper-side (§6 augur.md): Omen existence, SID→covens,
// Rite, allow-list. DENIED/ERROR/UNSPECIFIED → step error without secret
// material.
package augur

import (
	"context"
	"errors"
	"fmt"

	soulaugur "github.com/souls-guild/soul-stack/soul/internal/augur"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — canonical module address top-level.
const Name = "core.augur"

// verbFetch — the only supported verb.
const verbFetch = "fetch"

// Module — sdk/module.SoulModule implementation for core.augur.
//
// The module does NOT hold the Augur client as a field: it arrives per-run
// via stream.Context() (soul/internal/augur.FromContext) — the client is
// bound to a specific EventStream session, while the module is stateless and
// reused across runs.
type Module struct{}

func New() *Module { return &Module{} }

// Validate checks the verb and required params (omen / query). known-state
// and required are deliberately duplicated with the manifest DSL — this only
// covers basic shape semantics; authorization (allow-list) is checked by the
// Keeper, not Soul.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.GetState() != verbFetch {
		errs = append(errs, fmt.Sprintf("unknown verb %q (want %s)", req.GetState(), verbFetch))
	}
	if _, err := util.StringParam(req.GetParams(), "omen"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.StringParam(req.GetParams(), "query"); err != nil {
		errs = append(errs, err.Error())
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.GetState() != verbFetch {
		return util.SendFailed(stream, fmt.Sprintf("unknown verb %q", req.GetState()))
	}
	return m.applyFetch(stream, req)
}

// applyFetch implements the `fetch` verb: one AugurRequest → AugurReply over
// EventStream. Error contract:
//   - Augur unavailable this run (push mode / no session) → failed;
//   - DENIED/ERROR/UNSPECIFIED from the Keeper → failed (reason without secret);
//   - stream timeout/disconnect (ctx / client closed) → failed;
//   - OK → changed=false + inline_data as the register output.
func (m *Module) applyFetch(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest) error {
	omen, err := util.StringParam(req.GetParams(), "omen")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	query, err := util.StringParam(req.GetParams(), "query")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	ctx := stream.Context()
	fetcher, applyID, ok := soulaugur.FromContext(ctx)
	if !ok {
		// Push mode (soul apply) or a session without Augur plumbing: the
		// broker is unavailable. We don't stay silent — a read-probe without
		// a broker is meaningless.
		return util.SendFailed(stream, "core.augur.fetch: Augur broker unavailable this run (no EventStream session)")
	}

	reply, ferr := fetcher.Fetch(ctx, applyID, omen, query)
	if ferr != nil {
		return util.SendFailed(stream, fetchErrorMessage(omen, ferr))
	}

	// OK: inline_data is a google.protobuf.Struct (§5.3 augur.md). We put it
	// into the register output as-is (a scalar is already wrapped by the
	// Keeper in {value:..}; a map is a natural object). changed=false by
	// construction — it's a read-probe.
	out := reply.GetInlineData().AsMap()
	return util.SendFinal(stream, false, out)
}

// fetchErrorMessage builds a clear step-error message without secret
// material. The reason (from the Keeper / transport) is already free of
// values/tokens (§8 augur.md), but we add the Omen name for diagnostics —
// query/value are NOT logged (query may carry a path to a secret).
func fetchErrorMessage(omen string, err error) string {
	switch {
	case errors.Is(err, soulaugur.ErrDenied):
		return fmt.Sprintf("core.augur.fetch: access to Omen %q denied: %v", omen, err)
	case errors.Is(err, soulaugur.ErrRemote):
		return fmt.Sprintf("core.augur.fetch: Omen %q returned an error: %v", omen, err)
	case errors.Is(err, soulaugur.ErrClientClosed):
		return fmt.Sprintf("core.augur.fetch: EventStream session closed before Omen %q replied", omen)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("core.augur.fetch: request to Omen %q interrupted (%v)", omen, err)
	default:
		return fmt.Sprintf("core.augur.fetch: request to Omen %q failed: %v", omen, err)
	}
}
