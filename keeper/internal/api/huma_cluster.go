package api

// Регистрация и spec-dump CLUSTER-домена (GET /v1/cluster) на huma full-typed.
// READ-роут БЕЗ audit (паттерн list/get). Доменная ClusterHandler.GetTyped
// извлечена в handlers/cluster.go; здесь — тонкий конверт claims → GetTyped →
// проекция reply в native wire-DTO.

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// clusterInstanceEntry — native-элемент HA-списка (форма wire). kid + started_at
// (optional, omitempty — nil при непрочитанной meta) + alive + is_reaper_leader.
type clusterInstanceEntry struct {
	KID            string     `json:"kid"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	Alive          bool       `json:"alive"`
	IsReaperLeader bool       `json:"is_reaper_leader"`
}

// clusterReply — native 200-тело GET /v1/cluster. instances — всегда present
// (non-nil slice → `[]`, не `null`); self_kid — KID текущего инстанса; self_health
// — карта `postgres|redis|vault → "ok"|причина` (та же форма, что checks в /readyz).
type clusterReply struct {
	Instances  []clusterInstanceEntry `json:"instances"`
	SelfKID    string                 `json:"self_kid"`
	SelfHealth map[string]string      `json:"self_health"`
}

// newClusterReply проецирует доменный handlers.ClusterView в native wire-DTO.
// instances принудительно non-nil ([] вместо null — стабильный wire для UI).
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

// registerHumaClusterGet монтирует GET /v1/cluster через huma (READ, БЕЗ audit).
// clusterH nil → no-op (dev/тесты без cluster-wire-up: роут не монтируется).
// RBAC soul.list — на группе. claims не используется handler-ом (view cluster-wide),
// но пробрасывается для единообразия с прочими read-конвертами.
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

// HumaClusterSpecYAML собирает OpenAPI-фрагмент GET /v1/cluster как YAML-строку без
// монтирования на реальный router. Хук для spec-merge-таргета и guard-теста
// (parity HumaSoulSpecYAML). Делегирует generic humaDumpSpec через тот же register.
func HumaClusterSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaClusterGet(api, handlers.ClusterSpecStub())
		return nil
	})
}
