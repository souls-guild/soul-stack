package vault

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// StateAbsent is the delete state, the mirror of [StatePresent]: address
// `core.vault.kv-absent` (NIM-1040).
const StateAbsent = "kv-absent"

// ScopeIncarnation is the one value `scope` takes: the running incarnation's own
// derived subtree. The param exists to make the destructive intent explicit in the
// scenario; the enum leaves room for a narrower scope later.
const ScopeIncarnation = "incarnation"

func validateAbsent(req *pluginv1.ValidateRequest) []string {
	if err := parseAbsentScope(req.Params); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// applyAbsent permanently deletes every KV entry under the running incarnation's own
// namespace `<mount>/<service>/<incarnation>/` — on KV v2 the metadata, so every
// version goes ([keepervault.Client.DeleteKV]).
//
// The step takes no path. The subtree is derived from the run's owner on the module
// context by [config.IncarnationVaultPrefix], the same derivation every declared
// secret is written under, so the author can neither name a path under another owner
// nor one under this service's root ([ADR-0083] §7 and its amendment 2026-10-21).
// A missing service or incarnation fails closed.
//
// It runs only in the incarnation's lifecycle destroy run ([util.IsDestroyRun]): in
// any other scenario it would let `incarnation.run` irreversibly delete every
// declared secret of a live incarnation. Inside destroy the right is
// `incarnation.destroy`, which already authorizes removing the incarnation whole.
//
// The subtree is walked in full before the first delete, so a failed listing deletes
// nothing. An empty or absent subtree is a no-op (changed=false). Output `deleted`
// carries the paths that existed and were deleted, sorted — names, never values. The
// audit event is written whenever something was deleted, including before a failure
// on a later entry: those entries are gone whatever the step's outcome.
func (m *Module) applyAbsent(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	if err := parseAbsentScope(req.Params); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !util.IsDestroyRun(ctx) {
		return util.SendFailed(stream, "core.vault.kv-absent runs only in the incarnation's destroy run (DELETE /v1/incarnations/{id}, permission incarnation.destroy); this run is not one, or its kind is unknown; nothing was deleted")
	}
	service, incarnation := util.ServiceFrom(ctx), util.IncarnationFrom(ctx)
	if service == "" || incarnation == "" {
		return util.SendFailed(stream, "core.vault.kv-absent runs only in an incarnation's scenario run: the run's service or incarnation is unknown, so its namespace cannot be derived; nothing was deleted")
	}
	prefix, err := config.IncarnationVaultPrefix(m.Mount, service, incarnation)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("derive the incarnation's Vault namespace: %v; nothing was deleted", err))
	}
	// The trailing `/` makes every `dir + name` the walk builds a path INSIDE the
	// subtree. It does not reach Vault — vault/api joins the request path with
	// path.Join, which drops it; what keeps `prod-10/…` out of `prod-1`'s listing
	// is that Vault's LIST answers a folder's children, not a string prefix. The
	// incarnation root itself is a namespace, not a secret, and is not deleted.
	root := prefix + "/"

	entries, err := m.walk(ctx, root)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("vault list under %q: %v; nothing was deleted", root, err))
	}

	var deleted []string
	var delErr error
	for _, p := range entries {
		existed, derr := m.Vault.DeleteKV(ctx, p)
		if derr != nil {
			delErr = fmt.Errorf("vault delete %q: %w", p, derr)
			break
		}
		if existed {
			deleted = append(deleted, p)
		}
	}

	if m.Audit != nil && len(deleted) > 0 {
		ev := &audit.Event{
			EventType: audit.EventVaultKVAbsent,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"paths":       toAnySlice(deleted),
				"service":     service,
				"incarnation": incarnation,
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			if delErr == nil {
				delErr = fmt.Errorf("audit write: %w", werr)
			} else {
				delErr = fmt.Errorf("%w; audit write: %v", delErr, werr)
			}
		}
	}
	if delErr != nil {
		msg := delErr.Error()
		if len(deleted) > 0 {
			msg += "; already deleted: " + strings.Join(deleted, ", ")
		}
		return util.SendFailed(stream, msg)
	}

	return util.SendFinal(stream, len(deleted) > 0, map[string]any{
		"deleted": toAnySlice(deleted),
	})
}

// walk returns every KV entry under dir (which ends with `/`), depth-first and in
// listing order, which [keepervault.Client.ListKVEntries] sorts. A listed name is
// trusted as ONE path segment (a trailing `/` marks a folder); anything else — empty,
// `.`, `..`, an embedded `/` — could lead a delete out of the subtree, so it stops the
// walk instead.
func (m *Module) walk(ctx context.Context, dir string) ([]string, error) {
	names, err := m.Vault.ListKVEntries(ctx, dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		child, folder := strings.CutSuffix(n, "/")
		if child == "" || child == "." || child == ".." || strings.Contains(child, "/") {
			return nil, fmt.Errorf("unexpected entry name %q under %q", n, dir)
		}
		if !folder {
			out = append(out, dir+child)
			continue
		}
		sub, err := m.walk(ctx, dir+child+"/")
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	return out, nil
}

// parseAbsentScope checks params.scope — required, and [ScopeIncarnation] is the only
// value.
func parseAbsentScope(params *structpb.Struct) error {
	scope, err := util.StringParam(params, "scope")
	if err != nil {
		return err
	}
	if scope != ScopeIncarnation {
		return fmt.Errorf("param \"scope\": %q is not a scope (want %s)", scope, ScopeIncarnation)
	}
	for k := range params.GetFields() {
		if k != "scope" {
			return fmt.Errorf("param %q: unknown (kv-absent takes only scope; it derives what it deletes)", k)
		}
	}
	return nil
}
