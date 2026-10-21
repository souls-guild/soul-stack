package soul

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulforget"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// ScopeIncarnation is the one value `scope` takes: every current member of the
// running incarnation. The param exists to make the destructive intent explicit in
// the scenario; the enum leaves room for a narrower scope later.
const ScopeIncarnation = "incarnation"

// Forgetter is the forget path behind `DELETE /v1/souls/{sid}` and
// `keeper.soul.forget` (NIM-386), narrowed to what `core.soul.forgotten` needs.
// Prod is [PGForgetter] over [soulforget.EraseOwned].
type Forgetter interface {
	// Members returns every current member of incarnation (from
	// `incarnation_membership`) mapped to everything that ties it to incarnations:
	// ALL its memberships, this one included, and the incarnations whose Choir
	// holds a Voice of it. No members is an empty map.
	Members(ctx context.Context, incarnation string) (map[string]soulforget.Ties, error)
	// Forget erases sid exactly as `DELETE /v1/souls/{sid}` does, but only while it
	// is tied to incarnation alone — re-checked inside the transaction that deletes
	// it ([soulforget.EraseOwned]). A sid already gone is
	// [keepersoul.ErrSoulNotFound].
	Forget(ctx context.Context, sid, incarnation, reason string) (soulforget.Result, error)
}

// AuditWriter is the narrow audit dependency of `core.soul.forgotten`; it matches
// shared/audit.Writer.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// WithForget connects the forget path and the audit writer for state `forgotten`.
// Without a Forgetter the state fails as not configured rather than running degraded.
func (m *Module) WithForget(f Forgetter, a AuditWriter) *Module {
	m.forget = f
	m.audit = a
	return m
}

func validateForgotten(params *structpb.Struct) []string {
	if err := parseForgetScope(params); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// applyForgotten forgets every current member of the running incarnation — the last
// step of a service's destroy, the hosts released the way `DELETE /v1/souls/{sid}`
// releases one.
//
// It runs only in the incarnation's lifecycle destroy run ([util.IsDestroyRun]): in
// any other scenario it would let `incarnation.run` erase hosts past `soul.forget`
// and its host scope. Inside destroy the right is `incarnation.destroy`, which
// already authorizes removing the incarnation whole.
//
// The step takes no host list: the members are read from `incarnation_membership`
// for the incarnation on the module context, so no author input can name a host of
// another incarnation. A missing incarnation fails closed. A member that is ALSO a
// member of another incarnation, or holds a Choir Voice of one, is refused
// ([soulforget.OwnedBy]): forgetting it would cascade away that incarnation's roster
// row or Voice. Every member is checked before the first host is touched, so one
// shared host fails the step with nothing forgotten; the rule is checked again per
// host under the row lock inside the delete, so a tie that changes in between is
// refused rather than severed.
//
// No members is a no-op (changed=false) — a destroy of an incarnation that never got
// a host, or a step that already ran. A member whose `souls` row vanished
// between the read and the delete is skipped. Output: `forgotten` (sorted sids) and
// `warnings` (`<sid>: <what was not released>`, the REST reply's warnings[]), so a
// partial release is never shown as a plain success. One `soul.forgotten` audit event
// per host, `source: keeper_internal` — the only durable record, as on REST.
func (m *Module) applyForgotten(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	if err := parseForgetScope(req.Params); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !util.IsDestroyRun(ctx) {
		return util.SendFailed(stream, "core.soul.forgotten runs only in the incarnation's destroy run (DELETE /v1/incarnations/{id}, permission incarnation.destroy); this run is not one, or its kind is unknown; nothing was forgotten")
	}
	if m.forget == nil {
		return util.SendFailed(stream, "core.soul.forgotten is not configured on this Keeper (no forget path wired)")
	}
	incarnation := util.IncarnationFrom(ctx)
	if incarnation == "" {
		return util.SendFailed(stream, "core.soul.forgotten runs only in an incarnation's scenario run: the run's incarnation is unknown, so it has no members to forget; nothing was forgotten")
	}

	members, err := m.forget.Members(ctx, incarnation)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("read members of incarnation %s: %v; nothing was forgotten", incarnation, err))
	}
	sids := make([]string, 0, len(members))
	for sid := range members {
		sids = append(sids, sid)
	}
	sort.Strings(sids)

	var refused []string
	for _, sid := range sids {
		if oerr := soulforget.OwnedBy(sid, incarnation, members[sid]); oerr != nil {
			refused = append(refused, oerr.Error())
		}
	}
	if len(refused) > 0 {
		return util.SendFailed(stream, "refusing to forget any host — only a host tied to "+incarnation+
			" alone can be forgotten by its run: "+strings.Join(refused, "; ")+"; nothing was forgotten")
	}

	reason := forgetReason(ctx, incarnation)
	forgotten := []string{}
	warnings := []string{}
	for _, sid := range sids {
		res, ferr := m.forget.Forget(ctx, sid, incarnation, reason)
		if errors.Is(ferr, keepersoul.ErrSoulNotFound) {
			continue
		}
		if errors.Is(ferr, soulforget.ErrNotOwned) {
			return util.SendFailed(stream, fmt.Sprintf("forget %s: %v; it was NOT forgotten and its seeds are untouched, but the teardown notice that precedes the check had already gone to the other Keeper instances: a stream another instance held for it was closed and reconnects on its own (one held by this instance, or any on a Keeper without Redis, was not touched)%s",
				sid, ferr, alreadyForgotten(forgotten)))
		}
		if ferr != nil {
			return util.SendFailed(stream, fmt.Sprintf("forget %s: %v%s", sid, ferr, alreadyForgotten(forgotten)))
		}
		forgotten = append(forgotten, sid)
		for _, w := range res.Warnings {
			warnings = append(warnings, sid+": "+w)
		}
		if aerr := m.auditForgotten(ctx, sid, incarnation, res); aerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write for %s: %v%s", sid, aerr, alreadyForgotten(forgotten)))
		}
	}

	return util.SendFinal(stream, len(forgotten) > 0, map[string]any{
		"forgotten": toAnySlice(forgotten),
		"warnings":  toAnySlice(warnings),
	})
}

// auditForgotten writes the `soul.forgotten` event REST and MCP write, with the run
// that caused it in place of the operator.
func (m *Module) auditForgotten(ctx context.Context, sid, incarnation string, res soulforget.Result) error {
	if m.audit == nil {
		return nil
	}
	warnings := res.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	payload := map[string]any{
		"sid":                  sid,
		"status_before":        res.StatusBefore,
		"seeds_revoked":        res.SeedsRevoked,
		"bootstraps_burned":    res.BootstrapsBurned,
		"memberships_severed":  res.MembershipsSevered,
		"choir_voices_removed": res.ChoirVoicesRemoved,
		"local_stream_closed":  res.LocalStreamClosed,
		"broadcast":            res.Broadcast,
		"cache_keys_purged":    res.CacheKeysPurged,
		"warnings":             toAnySlice(warnings),
		"incarnation":          incarnation,
	}
	if scope, ok := util.RunScopeFrom(ctx); ok {
		payload["scenario"] = scope.Scenario
		payload["apply_id"] = scope.ApplyID
	}
	return m.audit.Write(ctx, &audit.Event{
		EventType: audit.EventSoulForgotten,
		Source:    audit.SourceKeeperInternal,
		Payload:   payload,
	})
}

// forgetReason is what [soulforget] records as the revocation reason: the run, since
// no operator called this directly.
func forgetReason(ctx context.Context, incarnation string) string {
	if scope, ok := util.RunScopeFrom(ctx); ok && scope.Scenario != "" {
		return fmt.Sprintf("forgotten by scenario %s of incarnation %s (apply %s)", scope.Scenario, incarnation, scope.ApplyID)
	}
	return "forgotten by a scenario run of incarnation " + incarnation
}

func alreadyForgotten(sids []string) string {
	if len(sids) == 0 {
		return "; nothing was forgotten"
	}
	return "; already forgotten: " + strings.Join(sids, ", ")
}

// parseForgetScope checks params.scope — required, and [ScopeIncarnation] is the
// only value. Nothing else is accepted: in particular no host list.
func parseForgetScope(params *structpb.Struct) error {
	scope, err := util.StringParam(params, "scope")
	if err != nil {
		return err
	}
	if scope != ScopeIncarnation {
		return fmt.Errorf("param \"scope\": %q is not a scope (want %s)", scope, ScopeIncarnation)
	}
	for k := range params.GetFields() {
		if k != "scope" {
			return fmt.Errorf("param %q: unknown (forgotten takes only scope; it reads the incarnation's members itself)", k)
		}
	}
	return nil
}
