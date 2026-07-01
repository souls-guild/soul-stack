package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// ClusterRegistry — read-поверхность Conclave-реестра живых Keeper-инстансов
// (ADR-006 amend), нужная `GET /v1/cluster`. Реальная реализация — тонкая
// обёртка над keeperredis.LiveKIDs / keeperredis.ReadInstanceMeta, собранная в
// cmd/keeper (тот же Redis-клиент, что у Conclave-renewal). nil (single-Keeper
// dev без Redis) → handler отдаёт self-only view (свой инстанс всегда виден).
type ClusterRegistry interface {
	// LiveKIDs — KID-ы живых инстансов (SCAN presence-ключей keeper:instance:*).
	LiveKIDs(ctx context.Context) ([]string, error)
	// InstanceMeta — сырое value presence-ключа инстанса (`{started_at,kid}`-JSON);
	// (_, false) если ключ истёк между LiveKIDs и этим чтением (гонка TTL).
	InstanceMeta(ctx context.Context, kid string) (string, bool, error)
}

// ClusterLeaderReader — read-поверхность «кто сейчас Reaper-лидер»: value ключа
// reaper.LeaderLeaseKey = KID держателя lease. Реализация в cmd/keeper —
// обёртка над keeperredis.PeekLeaseHolder(reaper.LeaderLeaseKey). nil / нет
// лидера (lease свободен) → is_reaper_leader=false у всех.
type ClusterLeaderReader interface {
	// ReaperLeaderHolder — KID текущего Reaper-лидера; (_, false) если lease
	// свободен (нет лидера прямо сейчас).
	ReaperLeaderHolder(ctx context.Context) (string, bool, error)
}

// ClusterHandler — GET /v1/cluster: HA-список Keeper-инстансов из Conclave +
// self-health текущего инстанса. Read-only, cluster-wide (без per-SID scope):
// показывает топологию кластера, а не Soul-ресурсы. Все зависимости immutable;
// safe for concurrent use.
type ClusterHandler struct {
	registry   ClusterRegistry
	leader     ClusterLeaderReader
	healthDeps health.Deps
	selfKID    string
	logger     *slog.Logger
}

// NewClusterHandler собирает handler. registry / leader nil-tolerant
// (single-Keeper dev без Redis → self-only view). selfKID — KID текущего
// инстанса (из конфига soulstack.kid); он ВСЕГДА присутствует в ответе, даже
// если Conclave-реестр недоступен. healthDeps — те же PG/Redis/Vault-pingers,
// что у `/readyz` (self_health считается через health.Check — единый источник).
func NewClusterHandler(registry ClusterRegistry, leader ClusterLeaderReader, healthDeps health.Deps, selfKID string, logger *slog.Logger) *ClusterHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ClusterHandler{
		registry:   registry,
		leader:     leader,
		healthDeps: healthDeps,
		selfKID:    selfKID,
		logger:     logger,
	}
}

// ClusterSpecStub — непустой *ClusterHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (parity SoulSpecStub): при dump доменный handler не исполняется, но
// huma.Register требует non-nil.
func ClusterSpecStub() *ClusterHandler {
	return &ClusterHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ClusterInstanceView — одна строка HA-списка (плоская доменная проекция). KID +
// момент старта + alive-флаг + признак Reaper-лидерства. StartedAt — pointer-
// optional: nil, если meta не прочиталась / не распарсилась (ключ истёк по гонке
// TTL либо fail-safe-значение goloго KID вместо JSON).
type ClusterInstanceView struct {
	KID            string
	StartedAt      *time.Time
	Alive          bool
	IsReaperLeader bool
}

// ClusterView — плоская доменная проекция 200-тела GET /v1/cluster. Instances —
// non-nil slice (пустой Conclave → self-only). SelfKID — KID текущего инстанса.
// SelfHealth — карта `postgres|redis|vault → "ok"|причина` (та же форма, что
// checks в /readyz).
type ClusterView struct {
	Instances  []ClusterInstanceView
	SelfKID    string
	SelfHealth map[string]string
}

// ClusterReply — результат [ClusterHandler.GetTyped] (handler-native). Body —
// доменная проекция; пакет api проецирует её в native wire-DTO.
type ClusterReply struct {
	Body ClusterView
}

// GetTyped — доменная функция GET /v1/cluster: собирает HA-список из Conclave +
// self-health. fail-safe (ОБРАТНО soul-list fail-closed): если Conclave-реестр
// недоступен (Redis лёг) — НЕ 500, а self-only view (текущий инстанс всегда
// виден; его self_health покажет redis:unreachable — оператор увидит причину).
//
// Reaper-лидер: value ключа reaper.LeaderLeaseKey сравнивается с каждым KID.
// Ошибка чтения holder-а — fail-safe: is_reaper_leader=false у всех (не роняем
// весь view из-за одного волатильного признака).
func (h *ClusterHandler) GetTyped(ctx context.Context, _ *jwt.Claims) (ClusterReply, error) {
	kids := h.liveKIDs(ctx)
	leader, hasLeader := h.reaperLeader(ctx)

	instances := make([]ClusterInstanceView, 0, len(kids))
	for _, kid := range kids {
		inst := ClusterInstanceView{
			KID:            kid,
			Alive:          true, // KID в LiveKIDs ⇒ presence-ключ жив.
			IsReaperLeader: hasLeader && kid == leader,
		}
		if startedAt, ok := h.instanceStartedAt(ctx, kid); ok {
			inst.StartedAt = &startedAt
		}
		instances = append(instances, inst)
	}

	// Стабильный порядок (KID) — детерминированный вывод, не зависящий от
	// порядка SCAN-курсора Redis.
	sort.Slice(instances, func(i, j int) bool { return instances[i].KID < instances[j].KID })

	selfHealth, _ := health.Check(ctx, h.healthDeps)

	return ClusterReply{Body: ClusterView{
		Instances:  instances,
		SelfKID:    h.selfKID,
		SelfHealth: selfHealth,
	}}, nil
}

// liveKIDs возвращает KID-ы живых инстансов из Conclave. fail-safe: nil-registry
// (dev без Redis) либо ошибка SCAN-а → self-only ([selfKID]), чтобы `GET
// /v1/cluster` всегда показывал хотя бы текущий инстанс. Если self отсутствует в
// выборке (гонка регистрации на самом старте) — дописываем его.
func (h *ClusterHandler) liveKIDs(ctx context.Context) []string {
	if h.registry == nil {
		return h.selfOnly()
	}
	kids, err := h.registry.LiveKIDs(ctx)
	if err != nil {
		h.logger.Warn("cluster: LiveKIDs failed — self-only view (fail-safe)", slog.Any("error", err))
		return h.selfOnly()
	}
	for _, kid := range kids {
		if kid == h.selfKID {
			return kids
		}
	}
	if h.selfKID != "" {
		kids = append(kids, h.selfKID)
	}
	return kids
}

// selfOnly — деградированный список только из текущего KID (пустой при
// незаданном selfKID — теоретический dev-случай без конфига).
func (h *ClusterHandler) selfOnly() []string {
	if h.selfKID == "" {
		return nil
	}
	return []string{h.selfKID}
}

// reaperLeader читает KID текущего Reaper-лидера (value reaper.LeaderLeaseKey).
// fail-safe: nil-reader / ошибка / нет лидера → ("", false) (is_reaper_leader
// false у всех — волатильный признак не роняет весь view).
func (h *ClusterHandler) reaperLeader(ctx context.Context) (string, bool) {
	if h.leader == nil {
		return "", false
	}
	holder, ok, err := h.leader.ReaperLeaderHolder(ctx)
	if err != nil {
		h.logger.Warn("cluster: reaper leader read failed — is_reaper_leader=false (fail-safe)", slog.Any("error", err))
		return "", false
	}
	return holder, ok
}

// instanceStartedAt читает и парсит started_at из meta presence-ключа. fail-safe:
// nil-registry / истёкший ключ / не-JSON value (RegisterInstance fail-safe писал
// голый KID) / битый timestamp → (_, false) (StartedAt в ответе опущен, инстанс
// остаётся в списке — момент старта диагностический, не критичный).
func (h *ClusterHandler) instanceStartedAt(ctx context.Context, kid string) (time.Time, bool) {
	if h.registry == nil {
		return time.Time{}, false
	}
	raw, ok, err := h.registry.InstanceMeta(ctx, kid)
	if err != nil || !ok {
		return time.Time{}, false
	}
	var meta struct {
		StartedAt string `json:"started_at"`
	}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil || meta.StartedAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, meta.StartedAt)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
