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

// IssuedHost is one successfully prepared ready-made VM. The plaintext token
// stays wrapped until applyIssued deliberately places it in the current run's
// register output. It must never be formatted into errors, logs or audit data.
type IssuedHost struct {
	SID       string
	Token     bootstraptoken.PlainToken
	ExpiresAt time.Time
	Created   bool
	Reissued  bool
}

// Issuer atomically prepares the entire SID batch. A failure for any SID must
// roll back every Soul/token mutation in the call, so a failed task cannot
// leave a silently usable partial group.
type Issuer interface {
	IssueBatch(ctx context.Context, sids []string) ([]IssuedHost, error)
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

	issued, err := m.Issuer.IssueBatch(ctx, sids)
	if err != nil {
		return util.SendFailed(stream, "bootstrap issued: "+maskErr(err))
	}
	if len(issued) != len(sids) {
		return util.SendFailed(stream, fmt.Sprintf("bootstrap issued: issuer returned %d hosts for %d requested SIDs", len(issued), len(sids)))
	}

	hosts := make([]any, 0, len(issued))
	auditSIDs := make([]any, 0, len(issued))
	created, reissued := 0, 0
	for idx, h := range issued {
		if h.SID != sids[idx] {
			return util.SendFailed(stream, fmt.Sprintf("bootstrap issued: issuer returned sid %q at index %d, want %q", h.SID, idx, sids[idx]))
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
		auditSIDs = append(auditSIDs, h.SID)
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
	})
}
