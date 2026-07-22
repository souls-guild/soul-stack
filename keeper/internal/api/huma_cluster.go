package api

// Registration and spec-dump of the CLUSTER domain (GET /v1/cluster) on huma full-typed.
// READ route, no audit (list/get pattern). The domain ClusterHandler.GetTyped is extracted
// into handlers/cluster.go; here — a thin wrapper claims → GetTyped → projection of reply
// into the native wire-DTO.

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// clusterInstanceEntry — native element of the HA list (wire shape). kid + started_at
// (optional, omitempty — nil when meta is unread) + alive + is_reaper_leader.
type clusterInstanceEntry struct {
	KID            string     `json:"kid"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	Alive          bool       `json:"alive"`
	IsReaperLeader bool       `json:"is_reaper_leader"`
}

// clusterReply — native 200 body of GET /v1/cluster. instances — always present
// (non-nil slice → `[]`, not `null`); self_kid — KID of the current instance; self_health
// — map `postgres|redis|vault → "ok"|reason` (same shape as checks in /readyz).
type clusterReply struct {
	Instances  []clusterInstanceEntry `json:"instances"`
	SelfKID    string                 `json:"self_kid"`
	SelfHealth map[string]string      `json:"self_health"`
}

// newClusterReply projects the domain handlers.ClusterView into the native wire-DTO.
// instances forced non-nil ([] instead of null — stable wire for the UI).
func newClusterReply(v handlers.ClusterView) clusterReply {
	instances := make([]clusterInstanceEntry, 0, len(v.Instances))
	for i := range v.Instances {
		instances = append(instances, clusterInstanceEntry{
			KID:            v.Instances[i].KID,
			StartedAt:      v.Instances[i].StartedAt,
			Alive:          v.Instances[i].Alive,
			IsReaperLeader: v.Instances[i].IsReaperLeader,
		})
	}
	return clusterReply{
		Instances:  instances,
		SelfKID:    v.SelfKID,
		SelfHealth: v.SelfHealth,
	}
}

// registerHumaClusterGet mounts GET /v1/cluster via huma (READ, no audit).
// clusterH nil → no-op (dev/tests without cluster wire-up: the route is not mounted).
// RBAC soul.list — on the group. claims is not used by the handler (cluster-wide view),
// but is passed through for consistency with the other read wrappers.
func registerHumaClusterGet(humaAPI huma.API, clusterH *handlers.ClusterHandler) {
	if clusterH == nil {
		return
	}
	huma.Register(humaAPI, clusterGetOperation(), func(ctx context.Context, _ *clusterGetInput) (*clusterGetOutput, error) {
		reply, err := clusterH.GetTyped(ctx, claimsOrNil(ctx))
		if err != nil {
			return nil, soulProblem(err)
		}
		return &clusterGetOutput{Body: newClusterReply(reply.Body)}, nil
	})
}

// HumaClusterSpecYAML assembles the OpenAPI fragment for GET /v1/cluster as a YAML string
// without mounting on a real router. Hook for the spec-merge target and the guard test
// (parity HumaSoulSpecYAML). Delegates to the generic humaDumpSpec via the same register.
func HumaClusterSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaClusterGet(api, handlers.ClusterSpecStub())
		return nil
	})
}
