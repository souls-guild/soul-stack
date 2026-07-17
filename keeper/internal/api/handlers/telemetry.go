package handlers

// Read-path host-vitals (NIM-86, ADR-006): два эндпоинта отдают снимок утилизации
// Soul-агентов из Redis-слоя (keeperredis), НЕ из PG — телеметрия волатильна и
// живёт под TTL. GET /v1/souls/{sid}/telemetry — latest+window+freshness одного
// хоста; GET /v1/incarnations/{name}/telemetry — агрегат latest по хостам
// инкарнации. RBAC переиспользует soul-read-scope (тот же Purview soul.list +
// soulpurview.InScope, что soulprint/get).

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

// telemetryAggregateHostCap — потолок хостов в агрегате инкарнации. Флот сверх
// него усекается (агрегат — обзорный glance здоровья, latest-only на хост).
const telemetryAggregateHostCap = 2000

// UtilizationReader — узкая поверхность Redis-слоя host-vitals, нужная telemetry-
// handler-у (симметрично [SoulPresence]). Реальная реализация — обёртка над
// keeperredis.ReadUtilization/ReadUtilizationWindow, собранная в cmd/keeper;
// nil-Redis (dev/unit) → stale/empty без паники. ok==false → ключа нет (хост не
// слал утилизацию) — это НЕ ошибка.
type UtilizationReader interface {
	ReadUtilization(ctx context.Context, sid string) (keeperredis.UtilizationSnapshot, bool, error)
	ReadUtilizationWindow(ctx context.Context, sid string, limit int) ([]keeperredis.UtilizationPoint, error)
}

// SoulTelemetryReply — 200-тело GET /v1/souls/{sid}/telemetry: latest-снимок +
// окно для спарклайнов + честная свежесть.
type SoulTelemetryReply struct {
	SID         string                   `json:"sid" doc:"SID (FQDN) Soul-а"`
	Stale       bool                     `json:"stale" doc:"true если снимок протух (возраст > TTL) или данных нет"`
	CollectedAt *time.Time               `json:"collected_at,omitempty" doc:"Soul-side момент сбора"`
	ReceivedAt  *time.Time               `json:"received_at,omitempty" doc:"Keeper-side момент приёма"`
	Latest      *UtilizationLatest       `json:"latest,omitempty" doc:"последний снимок (nil если данных нет)"`
	Window      []UtilizationWindowPoint `json:"window,omitempty" doc:"окно точек для спарклайнов, newest-first"`
}

// UtilizationLatest — развёрнутый последний снимок host-vitals.
type UtilizationLatest struct {
	CpuPct     float64         `json:"cpu_pct"`
	Load1      float64         `json:"load1"`
	Load5      float64         `json:"load5"`
	Load15     float64         `json:"load15"`
	MemUsedMb  int64           `json:"mem_used_mb"`
	MemTotalMb int64           `json:"mem_total_mb"`
	SwapUsedMb int64           `json:"swap_used_mb"`
	UptimeSec  int64           `json:"uptime_sec"`
	Disks      []TelemetryDisk `json:"disks,omitempty"`
}

// TelemetryDisk — использование одного примонтированного тома.
type TelemetryDisk struct {
	Mount   string `json:"mount"`
	UsedMb  int64  `json:"used_mb"`
	TotalMb int64  `json:"total_mb"`
}

// UtilizationWindowPoint — компактная точка окна (спарклайн).
type UtilizationWindowPoint struct {
	CollectedAt time.Time `json:"collected_at"`
	CpuPct      float64   `json:"cpu_pct"`
	Load1       float64   `json:"load1"`
	MemUsedMb   int64     `json:"mem_used_mb"`
	MemTotalMb  int64     `json:"mem_total_mb"`
}

// IncarnationTelemetryReply — 200-тело GET /v1/incarnations/{name}/telemetry:
// latest+stale на хост (без окна — payload ограничен).
type IncarnationTelemetryReply struct {
	Incarnation string          `json:"incarnation"`
	Truncated   bool            `json:"truncated" doc:"true если флот ковена превысил cap и список хостов усечён (обзорный glance)"`
	Hosts       []HostTelemetry `json:"hosts"`
}

// HostTelemetry — latest-снимок одного хоста в агрегате инкарнации.
type HostTelemetry struct {
	SID         string             `json:"sid"`
	Stale       bool               `json:"stale"`
	CollectedAt *time.Time         `json:"collected_at,omitempty"`
	Latest      *UtilizationLatest `json:"latest,omitempty"`
}

// TelemetryHandler — read-эндпоинты host-vitals. reader — Redis-слой; souls —
// переиспользуемый scope-гейт (RBAC single-read + coven-листинг). Все зависимости
// immutable; safe for concurrent use.
type TelemetryHandler struct {
	reader UtilizationReader
	souls  *SoulHandler
	logger *slog.Logger
}

// NewTelemetryHandler создаёт handler. reader nil (dev/unit без Redis) → no-op
// (stale/empty). souls — тот же *SoulHandler, что обслуживает /v1/souls (scope-
// гейт и coven-листинг); nil допустим только в spec-dump.
func NewTelemetryHandler(reader UtilizationReader, souls *SoulHandler, logger *slog.Logger) *TelemetryHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if reader == nil {
		reader = noopUtilizationReader{}
	}
	return &TelemetryHandler{reader: reader, souls: souls, logger: logger}
}

// TelemetrySpecStub — непустой *TelemetryHandler для huma-OpenAPI-эмиссии (parity
// [SoulSpecStub]): при dump доменный handler не вызывается.
func TelemetrySpecStub() *TelemetryHandler {
	return &TelemetryHandler{reader: noopUtilizationReader{}, logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// noopUtilizationReader — nil-Redis-заглушка: любой хост читается как «данных нет»
// (stale). Держит handler nil-safe в dev/unit без Redis.
type noopUtilizationReader struct{}

func (noopUtilizationReader) ReadUtilization(context.Context, string) (keeperredis.UtilizationSnapshot, bool, error) {
	return keeperredis.UtilizationSnapshot{}, false, nil
}

func (noopUtilizationReader) ReadUtilizationWindow(context.Context, string, int) ([]keeperredis.UtilizationPoint, error) {
	return nil, nil
}

// GetTelemetry — GET /v1/souls/{sid}/telemetry: latest+window+freshness одного
// хоста. RBAC — тот же scope-гейт, что soulprint/get (вне scope / нет хоста → 404,
// не палим чужой хост). Отсутствие/протухание утилизации → Stale=true, Latest=nil,
// БЕЗ ошибки (старый агент → graceful, не 500). Ошибки — *problemError.
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

// AggregateByIncarnation — GET /v1/incarnations/{name}/telemetry: latest+stale по
// хостам инкарнации (name — корневой Coven-label). Хосты — soul-read-scoped
// coven-листинг ([SoulHandler.SIDsInCovenInScope]); пустой флот / нет прав →
// hosts:[] (НЕ ошибка). Без окна (payload ограничен).
func (h *TelemetryHandler) AggregateByIncarnation(ctx context.Context, claims *jwt.Claims, name string) (IncarnationTelemetryReply, error) {
	reply := IncarnationTelemetryReply{Incarnation: name, Hosts: []HostTelemetry{}}
	if h.souls == nil {
		return reply, nil
	}
	sids, truncated, err := h.souls.SIDsInCovenInScope(ctx, claims, name, telemetryAggregateHostCap)
	if err != nil {
		h.logger.Error("telemetry.aggregate: список хостов упал", slog.String("incarnation", name), slog.Any("error", err))
		return IncarnationTelemetryReply{}, &problemError{problem.New(problem.TypeInternalError, "", "aggregate telemetry failed")}
	}
	if truncated {
		h.logger.Warn("telemetry.aggregate: флот ковена превысил cap — список усечён",
			slog.String("incarnation", name), slog.Int("cap", telemetryAggregateHostCap))
		reply.Truncated = true
	}
	for _, sid := range sids {
		host := HostTelemetry{SID: sid, Stale: true}
		snap, ok, rerr := h.reader.ReadUtilization(ctx, sid)
		switch {
		case rerr != nil:
			h.logger.Warn("telemetry.aggregate: чтение хоста упало — stale", slog.String("sid", sid), slog.Any("error", rerr))
		case ok:
			host.Stale = staleByAge(snap.ReceivedAt)
			host.CollectedAt = nonZeroTime(snap.CollectedAt)
			host.Latest = latestFromSnapshot(snap)
		}
		reply.Hosts = append(reply.Hosts, host)
	}
	return reply, nil
}

// readSoulTelemetry собирает reply одного хоста. Ошибка чтения Redis → stale-пустой
// reply (телеметрия best-effort, не 500). withWindow=false — только latest.
func (h *TelemetryHandler) readSoulTelemetry(ctx context.Context, sid string, withWindow bool) SoulTelemetryReply {
	reply := SoulTelemetryReply{SID: sid, Stale: true}
	snap, ok, err := h.reader.ReadUtilization(ctx, sid)
	if err != nil {
		h.logger.Warn("telemetry: чтение утилизации упало — отдаём stale", slog.String("sid", sid), slog.Any("error", err))
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
			h.logger.Warn("telemetry: чтение окна упало — окно опущено", slog.String("sid", sid), slog.Any("error", werr))
		} else {
			reply.Window = windowFromPoints(pts)
		}
	}
	return reply
}

// staleByAge — снимок протух, если с приёма прошло больше TTL.
func staleByAge(received time.Time) bool {
	return time.Since(received) > keeperredis.UtilizationTTL
}

// nonZeroTime — указатель на непустое время (zero → nil → omitempty).
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
	}
	if len(s.Disks) > 0 {
		l.Disks = make([]TelemetryDisk, 0, len(s.Disks))
		for _, d := range s.Disks {
			l.Disks = append(l.Disks, TelemetryDisk{Mount: d.Mount, UsedMb: d.UsedMB, TotalMb: d.TotalMB})
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
		})
	}
	return out
}

// AuthorizeReadScope — RBAC-гейт single-host read host-vitals: тот же scope, что
// GetTyped/GetSoulprintTyped (валидный sid → есть в реестре → в soul-read-scope
// оператора). nil → авторизован; *problemError 422/404/500 иначе. Вне scope и
// not-found дают один 404 (не палим чужой хост).
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

// SIDsInCovenInScope — SID-ы хостов ковена coven в границах soul-read-scope
// оператора (тот же Purview soul.list + soulpurview.InScope, что single-read).
// Перечисляет флот ковена (SelectAll, predicate `$1 = ANY(coven)`) до capN, затем
// фильтрует InScope (coven+regex-измерения) — под-показ безопасен (fail-closed).
// truncated=true, когда флот ковена упёрся в capN (список может быть неполным).
// Пустой scope → nil. ⚠️ cap применяется ДО scope-фильтра: на ковене > capN
// scoped-оператор, чьи in-scope хосты в хвосте, увидит меньше — приемлемо для
// обзорного агрегата (fail-closed); полный scope-pushdown — кандидат на NIM-87+.
func (h *SoulHandler) SIDsInCovenInScope(ctx context.Context, claims *jwt.Claims, coven string, capN int) (sids []string, truncated bool, err error) {
	scope := h.readScopeForClaims(claims)
	if scope.Empty {
		return nil, false, nil
	}
	if capN < 1 {
		capN = 1
	}
	items, _, err := soul.SelectAll(ctx, h.pool, soul.ListFilter{Coven: coven}, soul.ListScope{Unrestricted: true}, 0, capN)
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
