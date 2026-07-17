package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	grpclib "google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// serviceArtifactLoader — узкая поверхность загрузчика Service-артефакта
// (материализация git-снапшота + parse манифеста), нужная [telemetrySource].
// Реализация — *artifact.ServiceLoader; интерфейс держит источник unit-fake-абельным.
type serviceArtifactLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
}

// telemetrySource — реализация [TelemetrySource] (ADR-072, NIM-87) над PG
// (souls + incarnation + soulprint) + реестром сервисов (git-координаты) +
// Service-загрузчиком + essence-резолвером. Wire-up в daemon: общий pool +
// d.serviceRegistry + d.serviceLoader + d.essenceResolver.
type telemetrySource struct {
	db       soul.ExecQueryRower
	resolver incarnation.ServiceResolver
	loader   serviceArtifactLoader
	essence  *essence.Resolver
	logger   *slog.Logger
}

// NewTelemetrySource собирает [TelemetrySource]. db — общий pool (souls +
// incarnation + soulprint). resolver — реестр сервисов (git-координаты по имени,
// d.serviceRegistry). loader — Service-загрузчик (d.serviceLoader). ess —
// essence-резолвер (d.essenceResolver). logger nil → slog.Default().
//
// resolver обязателен помимо loader-а: [artifact.ServiceLoader.Load] требует
// git-URL в ServiceRef (пустой Git — hard error), а инкарнация несёт только имя
// сервиса + версию — URL резолвится реестром (калька oracle_enqueuer / incarnation
// handlers).
func NewTelemetrySource(db soul.ExecQueryRower, resolver incarnation.ServiceResolver, loader serviceArtifactLoader, ess *essence.Resolver, logger *slog.Logger) TelemetrySource {
	if logger == nil {
		logger = slog.Default()
	}
	return &telemetrySource{db: db, resolver: resolver, loader: loader, essence: ess, logger: logger}
}

// selectIncarnationByCovensSQL — инкарнации, чьё имя (корневая Coven-метка,
// ADR-008) присутствует в covens хоста. ORDER BY name — детерминизм v1-политики
// «первая по имени».
const selectIncarnationByCovensSQL = `
SELECT name, service, service_version, spec
FROM incarnation
WHERE name = ANY($1)
ORDER BY name
`

// ResolveForSID резолвит эффективный telemetry-конфиг хоста (ADR-072, NIM-87):
//
//	souls.SelectBySID → covens/soulprint → incarnation по covens (первая по имени)
//	  → serviceRegistry.Resolve(inc.Service) (ref = inc.ServiceVersion)
//	  → loader.Load → art.Manifest.Telemetry + essence.Resolve(override)
//	  → ResolveEffectiveTelemetry(merge+clamp).
//
// (nil, nil) — «конфига нет»: хост не в реестре / без covens / без инкарнации.
// broadcast скипается, Soul остаётся на soul-local каденсе. Любой сбой резолва —
// (nil, err): broadcast проглотит warn-ом, стрим жив.
func (s *telemetrySource) ResolveForSID(ctx context.Context, sid string) (*keeperv1.TelemetryConfig, error) {
	su, err := soul.SelectBySID(ctx, s.db, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return nil, nil // хост ещё не в реестре — конфига нет
		}
		return nil, fmt.Errorf("telemetry: soul select %q: %w", sid, err)
	}

	inc, err := s.incarnationForCovens(ctx, su.Coven)
	if err != nil {
		return nil, err
	}
	if inc == nil {
		return nil, nil // нет инкарнации по covens — Soul на soul-local
	}

	ref, ok := s.resolver.Resolve(inc.Service)
	if !ok {
		return nil, fmt.Errorf("telemetry: service %q of incarnation %q not registered", inc.Service, inc.Name)
	}
	if inc.ServiceVersion != "" {
		// Катим развёрнутой версией сервиса, а не tip-ом ветки (калька
		// oracle_enqueuer.go / incarnation-handlers).
		ref.Ref = inc.ServiceVersion
	}

	art, err := s.loader.Load(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("telemetry: load service %q@%q: %w", ref.Name, ref.Ref, err)
	}

	essenceMap, err := s.essence.Resolve(essence.ResolveInput{
		ServiceDir:      art.LocalDir,
		OSFamily:        s.osFamilyForSID(ctx, sid),
		Covens:          su.Coven,
		IncarnationSpec: specEssence(inc),
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry: essence resolve (%q): %w", inc.Name, err)
	}

	// Опечатка оператора в essence-collectors (неизвестные имена молча
	// отфильтровываются) — видима в логах, иначе диагностировать её нечем.
	if unknown := essence.UnknownTelemetryCollectors(essenceMap); len(unknown) > 0 {
		s.logger.Warn("telemetry: ignored unknown telemetry collectors in essence",
			slog.String("incarnation", inc.Name),
			slog.Any("unknown", unknown),
		)
	}

	// art.Manifest гарантированно non-nil после успешного Load (иначе Load
	// вернул бы ошибку); Telemetry может быть nil — ResolveEffectiveTelemetry
	// nil-safe.
	return essence.ResolveEffectiveTelemetry(art.Manifest.Telemetry, essenceMap), nil
}

// incarnationForCovens возвращает первую по имени инкарнацию, чьё имя есть в
// covens хоста (v1-политика). Пусто → (nil, nil). >1 совпадений — debug-лог
// (развилка координатору: хост-член нескольких инкарнаций).
func (s *telemetrySource) incarnationForCovens(ctx context.Context, covens []string) (*incarnation.Incarnation, error) {
	if len(covens) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, selectIncarnationByCovensSQL, covens)
	if err != nil {
		return nil, fmt.Errorf("telemetry: incarnation-by-covens query: %w", err)
	}
	defer rows.Close()

	var matches []*incarnation.Incarnation
	for rows.Next() {
		var (
			inc       incarnation.Incarnation
			specBytes []byte
		)
		if err := rows.Scan(&inc.Name, &inc.Service, &inc.ServiceVersion, &specBytes); err != nil {
			return nil, fmt.Errorf("telemetry: scan incarnation: %w", err)
		}
		if len(specBytes) > 0 {
			if err := json.Unmarshal(specBytes, &inc.Spec); err != nil {
				return nil, fmt.Errorf("telemetry: unmarshal incarnation spec %q: %w", inc.Name, err)
			}
		}
		incCopy := inc
		matches = append(matches, &incCopy)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("telemetry: incarnation-by-covens iter: %w", err)
	}

	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.Name
		}
		s.logger.Debug("telemetry: SID матчит несколько инкарнаций — берём первую по имени (v1)",
			slog.Any("incarnations", names))
	}
	return matches[0], nil
}

// osFamilyForSID best-effort извлекает soulprint.os.family для os-слоя essence.
// Свежий хост без soulprint (ErrSoulprintNotReceived) / любой сбой → "" (os-слой
// просто пропускается, не роняет резолв).
func (s *telemetrySource) osFamilyForSID(ctx context.Context, sid string) string {
	rec, err := soul.SelectSoulprint(ctx, s.db, sid)
	if err != nil {
		return ""
	}
	var facts map[string]any
	if err := json.Unmarshal(rec.FactsJSON, &facts); err != nil {
		return ""
	}
	return osFamilyOf(facts)
}

// osFamilyOf извлекает soulprint.os.family из last-reported фактов. Тривиальный
// дубль scenario-хелпера (сигнатура над map, а не над *topology.HostFacts —
// экспорт ради 3 строк избыточен).
func osFamilyOf(soulprint map[string]any) string {
	os, ok := soulprint["os"].(map[string]any)
	if !ok {
		return ""
	}
	family, _ := os["family"].(string)
	return family
}

// specEssence возвращает incarnation.spec.essence (override оператора) или nil.
// Тривиальный дубль scenario-хелпера (экспорт ради 3 строк избыточен).
func specEssence(inc *incarnation.Incarnation) map[string]any {
	if inc.Spec == nil {
		return nil
	}
	e, _ := inc.Spec["essence"].(map[string]any)
	return e
}

// broadcastTelemetryConfig раздаёт Soul-у эффективный telemetry-конфиг host-vitals
// одним [keeperv1.FromKeeper_TelemetryConfig] (ADR-072, NIM-87). Вызывается из
// [EventStream] в той же горутине после [broadcastVigils] и до старта send-loop-а
// — отправка напрямую stream.Send (порядок гарантирован, буфер не задействован).
//
// В отличие от snapshot-broadcast-ов (Sigil/Vigil, ReplaceAll даже пустым
// набором), «нет конфига» (хост без инкарнации) — это НЕ пустой конфиг, а
// НЕотправка: Soul держит soul-local каденс. Поэтому (nil, nil) от ResolveForSID
// → тихий скип.
//
// Best-effort:
//   - TelemetrySource=nil → no-op (dev/unit/push-обвязка);
//   - ResolveForSID вернул ошибку → warn, скип, стрим жив;
//   - (nil, nil) → тихий скип (конфига нет);
//   - stream.Send упал → warn (стрим уже сломан, receive-loop встретит EOF).
func (h *eventStreamHandler) broadcastTelemetryConfig(
	ctx context.Context,
	stream grpclib.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper],
	sid, sessionID string,
) {
	if h.deps.TelemetrySource == nil {
		return
	}
	cfg, err := h.deps.TelemetrySource.ResolveForSID(ctx, sid)
	if err != nil {
		h.logger.Warn("eventstream: telemetry config resolve failed — skipping",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}
	if cfg == nil {
		h.logger.Debug("eventstream: no telemetry config for sid — skip (soul-local cadence)",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_TelemetryConfig{TelemetryConfig: cfg},
	}
	if err := stream.Send(msg); err != nil {
		h.logger.Warn("eventstream: telemetry config send failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}
	h.deps.Metrics.ObserveMessage(directionToSoul)
	h.logger.Debug("eventstream: telemetry config sent",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.Bool("enabled", cfg.GetEnabled()),
		slog.Int("interval_sec", int(cfg.GetIntervalSec())),
		slog.Any("collectors", cfg.GetCollectors()),
	)
}
