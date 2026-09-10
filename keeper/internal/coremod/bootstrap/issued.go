// Package bootstrap implements the keeper-side core module `core.bootstrap`
// (ADR-063, docs/keeper/modules.md). Its single state `issued` mints bootstrap
// tokens for ready-made VMs: the pending Soul row and the hash of a fresh
// one-time token are written in one Postgres transaction, which is why minting
// happens in the Keeper and nowhere else.
//
// The second state, `delivered`, was REMOVED in NIM-834. It did two unrelated
// things — put the Soul binary on the host and hand the host its token — and
// installation has no portable form: every platform installs differently
// (image, cloud-init, its own shell, somebody else's fleet manager). Installing
// a host is now the site's own job. What the module guaranteed for free did NOT
// go with it: the three live-proven requirements it carried are recorded as
// requirements on whoever installs the host, in the ADR-063 amendment
// 2026-09-09 — redeem the token with `soul init` rather than merely writing it,
// pass it through STDIN and never argv, and activate the unit with
// `daemon-reload && enable && start` rather than a bare `start`.
package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name is the base module name without the state suffix (Registry key). The
// author form is `core.bootstrap.issued`; the state arrives in
// pluginv1.ApplyRequest.state and is checked in Validate and Apply.
const Name = "core.bootstrap"

// StateIssued is the only state this module has. `delivered` was removed in
// NIM-834 and is deliberately not special-cased here: a scenario still carrying
// that address must fail as loudly as any other unknown state.
const StateIssued = "issued"

// AuditWriter writes the `bootstrap.issued` event.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// Module implements sdk/module.SoulModule.
type Module struct {
	// Issuer atomically prepares pending agent Souls and their one-time tokens.
	// nil means the state is not configured — Apply says so instead of panicking.
	Issuer Issuer

	// Audit is the audit-writer. nil → the write is skipped.
	Audit AuditWriter
}

// unknownState is the refusal shared by Validate and Apply.
func unknownState(state string) string {
	return fmt.Sprintf("unknown state %q (want %q)", state, StateIssued)
}

func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	if req.State != StateIssued {
		return &pluginv1.ValidateReply{Ok: false, Errors: []string{unknownState(req.State)}}, nil
	}
	return validateIssued(req), nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != StateIssued {
		return util.SendFailed(stream, unknownState(req.State))
	}
	return m.applyIssued(req, stream)
}

// maskErr masks a possible secret leak (vault-ref / token) in an error text
// before it is returned in a failed event. Same substring filter as
// shared/audit, which cleans register output. The key `_` is non-secret.
func maskErr(err error) string {
	if err == nil {
		return ""
	}
	masked := audit.MaskSecrets(map[string]any{"_": err.Error()})
	if s, ok := masked["_"].(string); ok {
		return s
	}
	return "***MASKED***"
}

// IssuedHost is one successfully prepared ready-made VM. The plaintext token
// stays wrapped until applyIssued deliberately places it in the current run's
// register output. It must never be formatted into errors, logs or audit data.
//
// Onboarded marks the one entry that legitimately carries no token: a host of
// this run that was already up when issuance ran, passed through untouched
// instead of refused (NIM-780). Token and ExpiresAt are then zero, and every
// reader must branch on the flag before reaching for them.
type IssuedHost struct {
	SID       string
	Token     bootstraptoken.PlainToken
	ExpiresAt time.Time
	Created   bool
	Reissued  bool
	Onboarded bool
}

// Issuer atomically prepares the entire SID batch. A failure for any SID must
// roll back every Soul/token mutation in the call, so a failed task cannot
// leave a silently usable partial group. incarnationName is the run's
// incarnation ("" when unknown); it decides whether an already onboarded SID is
// this run's own host to converge over or somebody else's identity to refuse.
type Issuer interface {
	IssueBatch(ctx context.Context, sids []string, incarnationName string) ([]IssuedHost, error)
}

// SIDIssueError identifies the host that made a batch fail without carrying
// token material. It is returned by the PG implementation and preserved in the
// failed ApplyEvent for per-host diagnosis.
type SIDIssueError struct {
	SID string
	Err error
}

func (e *SIDIssueError) Error() string { return fmt.Sprintf("sid %q: %v", e.SID, e.Err) }
func (e *SIDIssueError) Unwrap() error { return e.Err }

func validateIssued(req *pluginv1.ValidateRequest) *pluginv1.ValidateReply {
	_, err := parseIssuedSIDs(req.Params)
	if err != nil {
		return &pluginv1.ValidateReply{Ok: false, Errors: []string{err.Error()}}
	}
	return &pluginv1.ValidateReply{Ok: true}
}

// parseIssuedSIDs validates the public input contract before any mutation:
// non-empty, unique canonical FQDN/SID values, excluding synthetic runner SIDs.
func parseIssuedSIDs(params *structpb.Struct) ([]string, error) {
	sids, err := util.StringSliceParam(params, "sids")
	if err != nil {
		return nil, err
	}
	if len(sids) == 0 {
		return nil, fmt.Errorf("param %q: empty list (no ready-made VMs to onboard)", "sids")
	}
	seen := make(map[string]int, len(sids))
	for i, sid := range sids {
		if !keepersoul.ValidSID(sid) || keepersoul.IsReservedSID(sid) {
			return nil, fmt.Errorf("param %q[%d]: invalid sid %q", "sids", i, sid)
		}
		if first, dup := seen[sid]; dup {
			return nil, fmt.Errorf("param %q[%d]: duplicate sid %q (first at index %d)", "sids", i, sid, first)
		}
		seen[sid] = i
	}
	return sids, nil
}

func (m *Module) applyIssued(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	sids, err := parseIssuedSIDs(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if m.Issuer == nil {
		return util.SendFailed(stream, "bootstrap issued: issuer not configured (wire the Keeper Postgres pool)")
	}

	issued, err := m.Issuer.IssueBatch(ctx, sids, util.IncarnationFrom(ctx))
	if err != nil {
		return util.SendFailed(stream, "bootstrap issued: "+maskErr(err))
	}
	if len(issued) != len(sids) {
		return util.SendFailed(stream, fmt.Sprintf("bootstrap issued: issuer returned %d hosts for %d requested SIDs", len(issued), len(sids)))
	}

	hosts := make([]any, 0, len(issued))
	auditSIDs := make([]any, 0, len(issued))
	created, reissued, skipped := 0, 0, 0
	for idx, h := range issued {
		if h.SID != sids[idx] {
			return util.SendFailed(stream, fmt.Sprintf("bootstrap issued: issuer returned sid %q at index %d, want %q", h.SID, idx, sids[idx]))
		}
		auditSIDs = append(auditSIDs, h.SID)
		if h.Onboarded {
			// Already onboarded host of this run (NIM-780): the SID keeps its slot
			// so the list still answers for every requested host in order, and the
			// flag carries WHY it has no `bootstrap_token` — the same entry shape
			// core.cloud.created produced for a pass-through, which
			// core.bootstrap.delivered skips instead of failing on the absence.
			//
			// The pair is contradictory: a converged host is one nothing was
			// written for, so issuance data beside the flag means the issuer did
			// two mutually exclusive things and this output would have to silently
			// drop one of them.
			//
			// Refusing does NOT undo the mint — IssueBatch has already committed by
			// the time we look, and any token it made is in Postgres either way
			// (it does expire on its own TTL, and the next issuance for that SID
			// invalidates it). What refusing buys is that the contradiction is
			// reported instead of half-applied: the alternative is a host silently
			// skipped while a capability was minted for it, which reads as a clean
			// converge in the register and in the audit. IssuerPG cannot produce
			// this; the check is for the next implementation of the interface.
			if h.Token.Reveal() != "" || !h.ExpiresAt.IsZero() {
				return util.SendFailed(stream, fmt.Sprintf("bootstrap issued: issuer returned sid %q as onboarded AND with issuance data", h.SID))
			}
			skipped++
			hosts = append(hosts, map[string]any{
				"sid":       h.SID,
				"onboarded": true,
			})
			continue
		}
		plain := h.Token.Reveal()
		if plain == "" || h.ExpiresAt.IsZero() {
			return util.SendFailed(stream, fmt.Sprintf("bootstrap issued: issuer returned incomplete delivery data for sid %q", h.SID))
		}
		entry := map[string]any{
			"sid":             h.SID,
			"bootstrap_token": plain,
			"expires_at":      h.ExpiresAt.UTC().Format(time.RFC3339),
			"created":         h.Created,
			"reissued":        h.Reissued,
		}
		hosts = append(hosts, entry)
		if h.Created {
			created++
		}
		if h.Reissued {
			reissued++
		}
	}

	if m.Audit != nil {
		ev := &audit.Event{
			EventType: audit.EventBootstrapIssued,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"action":   StateIssued,
				"count":    float64(len(issued)),
				"created":  float64(created),
				"reissued": float64(reissued),
				"skipped":  float64(skipped),
				"sids":     auditSIDs,
			},
		}
		if err := m.Audit.Write(ctx, ev); err != nil {
			return util.SendFailed(stream, "bootstrap issued: audit write: "+maskErr(err))
		}
	}

	// bootstrap_token is intentionally revealed only here, in the ephemeral
	// register output consumed by core.bootstrap.delivered. All observable
	// copies of task output pass through audit.MaskSecrets, whose token-key rule
	// redacts it; the token is never copied into incarnation.state or audit.
	return util.SendFinal(stream, true, map[string]any{
		"action":   StateIssued,
		"hosts":    hosts,
		"count":    float64(len(issued)),
		"created":  float64(created),
		"reissued": float64(reissued),
		"skipped":  float64(skipped),
	})
}
