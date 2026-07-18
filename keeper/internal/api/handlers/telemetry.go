package handlers

// Read-path host-vitals (NIM-86, ADR-006): two endpoints hand back a utilization
// snapshot of Soul agents from the Redis layer (keeperredis), NOT from PG — telemetry
// is volatile and lives under a TTL. GET /v1/souls/{sid}/telemetry — latest+window+freshness
// of one host; GET /v1/incarnations/{name}/telemetry — latest aggregate across the
// incarnation's hosts. RBAC reuses the soul-read-scope (the same Purview soul.list +
// soulpurview.InScope as soulprint/get).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

// telemetryAggregateHostCap — the host ceiling in an incarnation aggregate. The souls
// beyond it are truncated (the aggregate is an overview health glance, latest-only per host).
const telemetryAggregateHostCap = 2000

// UtilizationReader — the narrow surface of the Redis host-vitals layer needed by the
// telemetry handler (symmetric with [SoulPresence]). The real implementation is a wrapper over
// keeperredis.ReadUtilization/ReadUtilizationWindow, assembled in cmd/keeper;
// nil-Redis (dev/unit) → stale/empty without a panic. ok==false → no key (the host
// never sent utilization) — this is NOT an error.
type UtilizationReader interface {
	ReadUtilization(ctx context.Context, sid string) (keeperredis.UtilizationSnapshot, bool, error)
	ReadUtilizationWindow(ctx context.Context, sid string, limit int) ([]keeperredis.UtilizationPoint, error)
}

// SoulTelemetryReply — 200-body of GET /v1/souls/{sid}/telemetry: latest snapshot +
// a window for sparklines + honest freshness.
type SoulTelemetryReply struct {
	SID         string                   `json:"sid" doc:"SID (FQDN) of the Soul"`
	Stale       bool                     `json:"stale" doc:"true if the snapshot is stale (age > TTL) or there is no data"`
	CollectedAt *time.Time               `json:"collected_at,omitempty" doc:"Soul-side collection moment"`
	ReceivedAt  *time.Time               `json:"received_at,omitempty" doc:"Keeper-side receive moment"`
	Latest      *UtilizationLatest       `json:"latest,omitempty" doc:"the latest snapshot (nil if there is no data)"`
	Window      []UtilizationWindowPoint `json:"window,omitempty" doc:"a window of points for sparklines, newest-first"`
}

// UtilizationLatest — the expanded latest host-vitals snapshot.
type UtilizationLatest struct {
	CpuPct      float64         `json:"cpu_pct"`
	Load1       float64         `json:"load1"`
	Load5       float64         `json:"load5"`
	Load15      float64         `json:"load15"`
	MemUsedMb   int64           `json:"mem_used_mb"`
	MemTotalMb  int64           `json:"mem_total_mb"`
	SwapUsedMb  int64           `json:"swap_used_mb"`
	UptimeSec   int64           `json:"uptime_sec"`
	NetRxBps    int64           `json:"net_rx_bps"`
	NetTxBps    int64           `json:"net_tx_bps"`
	NetErrPs    int64           `json:"net_err_ps"`
	IntervalSec int32           `json:"interval_sec"`
	Disks       []TelemetryDisk `json:"disks,omitempty"`
}

// TelemetryDisk — usage of one mounted volume.
type TelemetryDisk struct {
	Mount       string `json:"mount"`
	UsedMb      int64  `json:"used_mb"`
	TotalMb     int64  `json:"total_mb"`
	InodesUsed  int64  `json:"inodes_used"`
	InodesTotal int64  `json:"inodes_total"`
}

// UtilizationWindowPoint — a compact window point (sparkline).
type UtilizationWindowPoint struct {
	CollectedAt time.Time `json:"collected_at"`
	CpuPct      float64   `json:"cpu_pct"`
	Load1       float64   `json:"load1"`
	MemUsedMb   int64     `json:"mem_used_mb"`
	MemTotalMb  int64     `json:"mem_total_mb"`
	NetRxBps    int64     `json:"net_rx_bps"`
	NetTxBps    int64     `json:"net_tx_bps"`
}

// IncarnationTelemetryReply — 200-body of GET /v1/incarnations/{name}/telemetry:
// latest+stale per host (no window — the payload is limited).
type IncarnationTelemetryReply struct {
	Incarnation string          `json:"incarnation"`
	Truncated   bool            `json:"truncated" doc:"true if the incarnation's members exceeded the cap and the host list is truncated (overview glance)"`
	Hosts       []HostTelemetry `json:"hosts"`
}

// HostTelemetry — the latest snapshot of one host in the incarnation aggregate.
type HostTelemetry struct {
	SID         string             `json:"sid"`
	Stale       bool               `json:"stale"`
	CollectedAt *time.Time         `json:"collected_at,omitempty"`
	Latest      *UtilizationLatest `json:"latest,omitempty"`
}

// TelemetryHandler — the read endpoints for host-vitals. reader — the Redis layer; souls —
// the reusable scope gate (RBAC single-read + coven listing). All dependencies are
// immutable; safe for concurrent use.
type TelemetryHandler struct {
	reader UtilizationReader
	souls  *SoulHandler
	logger *slog.Logger
}

// NewTelemetryHandler creates the handler. reader nil (dev/unit without Redis) → no-op
// (stale/empty). souls — the same *SoulHandler that serves /v1/souls (the scope
// gate and coven listing); nil is only acceptable in spec-dump.
func NewTelemetryHandler(reader UtilizationReader, souls *SoulHandler, logger *slog.Logger) *TelemetryHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if reader == nil {
		reader = noopUtilizationReader{}
	}
	return &TelemetryHandler{reader: reader, souls: souls, logger: logger}
}

// TelemetrySpecStub — a non-empty *TelemetryHandler for huma-OpenAPI emission (parity
// with [SoulSpecStub]): during dump the domain handler is not invoked.
func TelemetrySpecStub() *TelemetryHandler {
	return &TelemetryHandler{reader: noopUtilizationReader{}, logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// noopUtilizationReader — a nil-Redis stub: any host reads as "no data"
// (stale). Keeps the handler nil-safe in dev/unit without Redis.
type noopUtilizationReader struct{}

func (noopUtilizationReader) ReadUtilization(context.Context, string) (keeperredis.UtilizationSnapshot, bool, error) {
	return keeperredis.UtilizationSnapshot{}, false, nil
}

func (noopUtilizationReader) ReadUtilizationWindow(context.Context, string, int) ([]keeperredis.UtilizationPoint, error) {
	return nil, nil
}

// GetTelemetry — GET /v1/souls/{sid}/telemetry: latest+window+freshness of one
// host. RBAC — the same scope gate as soulprint/get (out of scope / no host → 404,
// don't leak someone else's host). Missing/stale utilization → Stale=true, Latest=nil,
// WITHOUT an error (an old agent → graceful, not 500). Errors — *problemError.
func (h *TelemetryHandler) GetTelemetry(ctx context.Context, claims *jwt.Claims, sid string) (SoulTelemetryReply, error) {
	var zero SoulTelemetryReply
	if h.souls == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "telemetry not configured")}
	}
	if err := h.souls.AuthorizeReadScope(ctx, claims, sid); err != nil {
		return zero, err
	}
	return h.readSoulTelemetry(ctx, sid, true), nil
}

// AggregateByIncarnation — GET /v1/incarnations/{name}/telemetry: latest+stale across
// the incarnation's MEMBER hosts (incarnation_membership, NIM-124). Hosts — the
// soul-read-scoped member listing ([SoulHandler.SIDsInIncarnationInScope]); no
// members / no permissions → hosts:[] (NOT an error). No window (the payload is limited).
func (h *TelemetryHandler) AggregateByIncarnation(ctx context.Context, claims *jwt.Claims, name string) (IncarnationTelemetryReply, error) {
	reply := IncarnationTelemetryReply{Incarnation: name, Hosts: []HostTelemetry{}}
	if h.souls == nil {
		return reply, nil
	}
	sids, truncated, err := h.souls.SIDsInIncarnationInScope(ctx, claims, name, telemetryAggregateHostCap)
	if err != nil {
		h.logger.Error("telemetry.aggregate: host list lookup failed", slog.String("incarnation", name), slog.Any("error", err))
		return IncarnationTelemetryReply{}, &problemError{problem.New(problem.TypeInternalError, "", "aggregate telemetry failed")}
	}
	if truncated {
		h.logger.Warn("telemetry.aggregate: incarnation members exceeded the cap — list truncated",
			slog.String("incarnation", name), slog.Int("cap", telemetryAggregateHostCap))
		reply.Truncated = true
	}
	for _, sid := range sids {
		host := HostTelemetry{SID: sid, Stale: true}
		snap, ok, rerr := h.reader.ReadUtilization(ctx, sid)
		switch {
		case rerr != nil:
			h.logger.Warn("telemetry.aggregate: host read failed — stale", slog.String("sid", sid), slog.Any("error", rerr))
		case ok:
			host.Stale = staleByAge(snap.ReceivedAt)
			host.CollectedAt = nonZeroTime(snap.CollectedAt)
			host.Latest = latestFromSnapshot(snap)
		}
		reply.Hosts = append(reply.Hosts, host)
	}
	return reply, nil
}

// readSoulTelemetry assembles the reply for one host. A Redis read error → a stale-empty
// reply (telemetry is best-effort, not 500). withWindow=false — latest only.
func (h *TelemetryHandler) readSoulTelemetry(ctx context.Context, sid string, withWindow bool) SoulTelemetryReply {
	reply := SoulTelemetryReply{SID: sid, Stale: true}
	snap, ok, err := h.reader.ReadUtilization(ctx, sid)
	if err != nil {
		h.logger.Warn("telemetry: utilization read failed — returning stale", slog.String("sid", sid), slog.Any("error", err))
		return reply
	}
	if !ok {
		return reply
	}
	reply.Stale = staleByAge(snap.ReceivedAt)
	reply.CollectedAt = nonZeroTime(snap.CollectedAt)
	reply.ReceivedAt = nonZeroTime(snap.ReceivedAt)
	reply.Latest = latestFromSnapshot(snap)
	if withWindow {
		pts, werr := h.reader.ReadUtilizationWindow(ctx, sid, keeperredis.UtilizationWindowSize)
		if werr != nil {
			h.logger.Warn("telemetry: window read failed — window omitted", slog.String("sid", sid), slog.Any("error", werr))
		} else {
			reply.Window = windowFromPoints(pts)
		}
	}
	return reply
}

// staleByAge — the snapshot is stale if more than the TTL has passed since receipt.
func staleByAge(received time.Time) bool {
	return time.Since(received) > keeperredis.UtilizationTTL
}

// nonZeroTime — a pointer to a non-empty time (zero → nil → omitempty).
func nonZeroTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	tt := t
	return &tt
}

func latestFromSnapshot(s keeperredis.UtilizationSnapshot) *UtilizationLatest {
	l := &UtilizationLatest{
		CpuPct: s.CPUPct, Load1: s.Load1, Load5: s.Load5, Load15: s.Load15,
		MemUsedMb: s.MemUsedMB, MemTotalMb: s.MemTotalMB, SwapUsedMb: s.SwapUsedMB, UptimeSec: s.UptimeSec,
		NetRxBps: s.NetRxBps, NetTxBps: s.NetTxBps, NetErrPs: s.NetErrPs, IntervalSec: s.IntervalSec,
	}
	if len(s.Disks) > 0 {
		l.Disks = make([]TelemetryDisk, 0, len(s.Disks))
		for _, d := range s.Disks {
			l.Disks = append(l.Disks, TelemetryDisk{
				Mount: d.Mount, UsedMb: d.UsedMB, TotalMb: d.TotalMB,
				InodesUsed: d.InodesUsed, InodesTotal: d.InodesTotal,
			})
		}
	}
	return l
}

func windowFromPoints(pts []keeperredis.UtilizationPoint) []UtilizationWindowPoint {
	if len(pts) == 0 {
		return nil
	}
	out := make([]UtilizationWindowPoint, 0, len(pts))
	for _, p := range pts {
		out = append(out, UtilizationWindowPoint{
			CollectedAt: p.CollectedAt, CpuPct: p.CPUPct, Load1: p.Load1,
			MemUsedMb: p.MemUsedMB, MemTotalMb: p.MemTotalMB,
			NetRxBps: p.NetRxBps, NetTxBps: p.NetTxBps,
		})
	}
	return out
}

// AuthorizeReadScope — the RBAC gate for single-host read host-vitals: the same scope as
// GetTyped/GetSoulprintTyped (a valid sid → present in the registry → in the operator's
// soul-read-scope). nil → authorized; *problemError 422/404/500 otherwise. Out-of-scope and
// not-found both give a single 404 (don't leak someone else's host).
func (h *SoulHandler) AuthorizeReadScope(ctx context.Context, claims *jwt.Claims, sid string) error {
	if !soul.ValidSID(sid) {
		return &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}
	s, err := soul.SelectBySID(ctx, h.pool, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		}
		h.logger.Error("soul.telemetry: scope select failed", slog.String("sid", sid), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "get soul failed")}
	}
	if !soulpurview.InScope(h.readScopeForClaims(claims), sid, s.Coven) {
		return &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
	}
	return nil
}

// SIDsInIncarnationInScope — the SIDs of an incarnation's MEMBER hosts within the
// bounds of the operator's soul-read-scope (ADR-008 amendment 2026-07-17/NIM-124:
// membership is the `incarnation_membership` relation, no longer
// `incarnation.name = ANY(coven)`). Lists members (SelectIncarnationMembers) up to
// capN, then filters InScope (coven+regex dimensions) — under-showing is safe
// (fail-closed). truncated=true when the members hit capN (the list may be
// incomplete). An empty scope → nil. ⚠️ the cap is applied BEFORE the scope filter:
// on an incarnation > capN a scoped operator whose in-scope hosts are in the tail
// will see fewer — acceptable for an overview aggregate (fail-closed); a full
// scope-pushdown is a candidate for NIM-87+.
func (h *SoulHandler) SIDsInIncarnationInScope(ctx context.Context, claims *jwt.Claims, incName string, capN int) (sids []string, truncated bool, err error) {
	scope := h.readScopeForClaims(claims)
	if scope.Empty {
		return nil, false, nil
	}
	if capN < 1 {
		capN = 1
	}
	items, err := soul.SelectIncarnationMembers(ctx, h.pool, incName, capN)
	if err != nil {
		return nil, false, err
	}
	out := make([]string, 0, len(items))
	for _, s := range items {
		if soulpurview.InScope(scope, s.SID, s.Coven) {
			out = append(out, s.SID)
		}
	}
	return out, len(items) >= capN, nil
}
