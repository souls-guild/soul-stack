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

// ClusterRegistry — read surface of the Conclave registry of live Keeper instances
// (ADR-006 amend), needed by `GET /v1/cluster`. The real implementation is a thin
// wrapper over keeperredis.LiveKIDs / keeperredis.ReadInstanceMeta, assembled in
// cmd/keeper (the same Redis client as Conclave renewal). nil (single-Keeper
// dev without Redis) → the handler returns a self-only view (its own instance is always visible).
type ClusterRegistry interface {
	// LiveKIDs — KIDs of live instances (SCAN of the keeper:instance:* presence keys).
	LiveKIDs(ctx context.Context) ([]string, error)
	// InstanceMeta — raw value of an instance's presence key (`{started_at,kid}` JSON);
	// (_, false) if the key expired between LiveKIDs and this read (TTL race).
	InstanceMeta(ctx context.Context, kid string) (string, bool, error)
}

// ClusterLeaderReader — read surface for "who is the Reaper leader right now": the value
// of key reaper.LeaderLeaseKey = KID of the lease holder. The cmd/keeper implementation is
// a wrapper over keeperredis.PeekLeaseHolder(reaper.LeaderLeaseKey). nil / no
// leader (lease is free) → is_reaper_leader=false for everyone.
type ClusterLeaderReader interface {
	// ReaperLeaderHolder — KID of the current Reaper leader; (_, false) if the lease
	// is free (no leader right now).
	ReaperLeaderHolder(ctx context.Context) (string, bool, error)
}

// ClusterHandler — GET /v1/cluster: HA list of Keeper instances from Conclave +
// self-health of the current instance. Read-only, cluster-wide (no per-SID scope):
// shows cluster topology, not Soul resources. All dependencies are immutable;
// safe for concurrent use.
type ClusterHandler struct {
	registry   ClusterRegistry
	leader     ClusterLeaderReader
	healthDeps health.Deps
	selfKID    string
	logger     *slog.Logger
}

// NewClusterHandler assembles the handler. registry / leader are nil-tolerant
// (single-Keeper dev without Redis → self-only view). selfKID is the current
// instance's KID (from config soulstack.kid); it is ALWAYS present in the response, even
// if the Conclave registry is unavailable. healthDeps are the same PG/Redis/Vault pingers
// as `/readyz` (self_health is computed via health.Check — a single source).
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

// ClusterSpecStub — a non-empty *ClusterHandler stub for generating the huma OpenAPI
// fragment (parity SoulSpecStub): the domain handler does not execute during dump, but
// huma.Register requires non-nil.
func ClusterSpecStub() *ClusterHandler {
	return &ClusterHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ClusterInstanceView — one row of the HA list (flat domain projection). KID +
// start moment + alive flag + Reaper-leadership marker. StartedAt is pointer-
// optional: nil if meta failed to read / parse (key expired via TTL race, or a
// fail-safe bare-KID value instead of JSON).
type ClusterInstanceView struct {
	KID            string
	StartedAt      *time.Time
	Alive          bool
	IsReaperLeader bool
}

// ClusterView — flat domain projection of the 200 body of GET /v1/cluster. Instances is a
// non-nil slice (empty Conclave → self-only). SelfKID is the current instance's KID.
// SelfHealth is a map `postgres|redis|vault → "ok"|reason` (the same shape as the
// checks in /readyz).
type ClusterView struct {
	Instances  []ClusterInstanceView
	SelfKID    string
	SelfHealth map[string]string
}

// ClusterReply — result of [ClusterHandler.GetTyped] (handler-native). Body is a
// domain projection; package api projects it into the native wire-DTO.
type ClusterReply struct {
	Body ClusterView
}

// GetTyped — domain function GET /v1/cluster: assembles the HA list from Conclave +
// self-health. fail-safe (OPPOSITE of soul-list fail-closed): if the Conclave registry is
// unavailable (Redis is down) — NOT 500, but a self-only view (the current instance is always
// visible; its self_health shows redis:unreachable — the operator sees the reason).
//
// Reaper leader: the value of key reaper.LeaderLeaseKey is compared against each KID.
// A holder read error is fail-safe: is_reaper_leader=false for everyone (we don't drop
// the whole view over one volatile marker).
func (h *ClusterHandler) GetTyped(ctx context.Context, _ *jwt.Claims) (ClusterReply, error) {
	kids := h.liveKIDs(ctx)
	leader, hasLeader := h.reaperLeader(ctx)

	instances := make([]ClusterInstanceView, 0, len(kids))
	for _, kid := range kids {
		inst := ClusterInstanceView{
			KID:            kid,
			Alive:          true, // KID in LiveKIDs ⇒ presence key is alive.
			IsReaperLeader: hasLeader && kid == leader,
		}
		if startedAt, ok := h.instanceStartedAt(ctx, kid); ok {
			inst.StartedAt = &startedAt
		}
		instances = append(instances, inst)
	}

	// Stable ordering (KID) — deterministic output independent of the
	// Redis SCAN cursor order.
	sort.Slice(instances, func(i, j int) bool { return instances[i].KID < instances[j].KID })

	selfHealth, _ := health.Check(ctx, h.healthDeps)

	return ClusterReply{Body: ClusterView{
		Instances:  instances,
		SelfKID:    h.selfKID,
		SelfHealth: selfHealth,
	}}, nil
}

// liveKIDs returns the KIDs of live instances from Conclave. fail-safe: nil registry
// (dev without Redis) or a SCAN error → self-only ([selfKID]), so `GET
// /v1/cluster` always shows at least the current instance. If self is missing from the
// result (registration race at startup) — we append it.
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

// selfOnly — degraded list containing only the current KID (empty when
// selfKID is unset — a theoretical dev case without config).
func (h *ClusterHandler) selfOnly() []string {
	if h.selfKID == "" {
		return nil
	}
	return []string{h.selfKID}
}

// reaperLeader reads the KID of the current Reaper leader (value reaper.LeaderLeaseKey).
// fail-safe: nil reader / error / no leader → ("", false) (is_reaper_leader
// false for everyone — a volatile marker doesn't drop the whole view).
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

// instanceStartedAt reads and parses started_at from the presence key's meta. fail-safe:
// nil registry / expired key / non-JSON value (RegisterInstance fail-safe wrote a
// bare KID) / malformed timestamp → (_, false) (StartedAt is omitted from the response, the
// instance stays in the list — the start moment is diagnostic, not critical).
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
