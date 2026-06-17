package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// OracleDeps — wire-up зависимости handler-а `PortentEvent` через EventStream
// (ADR-030, beacons reactor, срез S2). Приём Portent → match по реестру Decree →
// постановка named-scenario в work-queue.
//
// Обязательны при wire-up-е:
//   - DB — реестр decrees / oracle_fires (match + cooldown) + souls (резолв
//     covens субъекта по авторитетному SID);
//   - Where — sandbox-CEL для where-предикатов Decree (event.data);
//   - Enqueuer — постановка named-scenario в work-queue (ADR-027);
//   - AuditWriter — `oracle.fired` / `decree.circuit_tripped` на срабатывания.
//
// Опционально:
//   - Metrics — keeper_oracle_*-дескриптор (ADR-024 S4). nil → инструментация
//     выключена (nil-safe Observe*-методы — no-op), как Metrics в [AugurDeps].
//
// nil-OracleDeps (handler не wired up) → handler логирует warn и игнорирует
// Portent (минимально-инвазивный fallback на сборках без Oracle), как AugurDeps.
type OracleDeps struct {
	DB          oracleDB
	Where       *oracle.WhereEvaluator
	Enqueuer    ScenarioEnqueuer
	AuditWriter audit.Writer
	Metrics     *oracle.OracleMetrics

	// CircuitMaxFires / CircuitWindow — пороги circuit-breaker-а Oracle
	// (ADR-030(a), beacons S4): после CircuitMaxFires срабатываний одного Decree
	// за окно CircuitWindow он авто-disable-ится (enabled=false). Резолв дефолтов
	// — в daemon (пусто-поле→дефолт). CircuitMaxFires==0 → breaker OFF
	// (escape-hatch): BumpCircuit не вызывается, Decree никогда не авто-disable.
	CircuitMaxFires int
	CircuitWindow   time.Duration
}

// oracleDB — совмещённая поверхность PG, нужная Oracle-резолву: decrees /
// oracle_fires (oracle.ExecQueryRower) + souls-ридер (soul.ExecQueryRower) для
// covens субъекта из авторитетного registry. *pgxpool.Pool удовлетворяет обоим.
type oracleDB interface {
	oracle.ExecQueryRower
	soul.ExecQueryRower
}

// ScenarioEnqueuer — узкая поверхность постановки named-scenario в work-queue
// (ADR-027) от имени Oracle-реакции. Интерфейс (а не прямой импорт scenario/
// applyrun) держит Oracle-handler независимым от того, КАК резолвится
// incarnation/ServiceRef субъектного хоста — это решение wire-up-а (daemon).
//
// EnqueueScenario ставит сценарий action_scenario на хост subjectSID с входом
// actionInput (vault-ref КАК ЕСТЬ — инвариант A ADR-027). Возвращает apply_id
// поставленного прогона (для audit-correlation) либо ошибку резолва/постановки.
type ScenarioEnqueuer interface {
	EnqueueScenario(ctx context.Context, req EnqueueScenarioRequest) (applyID string, err error)
}

// EnqueueScenarioRequest — параметры постановки scenario от Oracle-реакции.
// SubjectSID — авторитетный SID хоста-отправителя (mTLS peer cert).
// IncarnationName — таргет-incarnation из Decree (РЕШЕНИЕ #1, вариант b):
// Enqueuer резолвит ServiceRef ИЗ неё (incarnation.service → реестр сервисов),
// а не из Decree. ScenarioName — action_scenario из Decree (whitelist).
// ActionInput — JSONB action_input из Decree (vault-ref КАК ЕСТЬ); nil/пустой →
// сценарий без input. DecreeName — имя сматчившего Decree (для audit/диагностики
// постановки).
type EnqueueScenarioRequest struct {
	SubjectSID      string
	IncarnationName string
	ScenarioName    string
	ActionInput     map[string]any
	DecreeName      string
}

func (d *OracleDeps) validate() error {
	if d.DB == nil {
		return errors.New("grpc: OracleDeps.DB is required")
	}
	if d.Where == nil {
		return errors.New("grpc: OracleDeps.Where is required")
	}
	if d.Enqueuer == nil {
		return errors.New("grpc: OracleDeps.Enqueuer is required")
	}
	if d.AuditWriter == nil {
		return errors.New("grpc: OracleDeps.AuditWriter is required")
	}
	return nil
}

// vigilSource — реализация [VigilSource] поверх реестра vigils + souls
// (connect-time broadcast VigilSnapshot, ADR-030). Резолвит covens хоста из
// авторитетного souls-registry, затем active-набор Vigil по sid ∪ covens и
// проецирует его в транспортные [keeperv1.VigilDef]. Wire-up в daemon: тот же
// pool, что OracleDeps.DB.
type vigilSource struct {
	db oracleDB
}

// NewVigilSource собирает [VigilSource] поверх совмещённого pool-а (vigils +
// souls). db обязан удовлетворять [oracle.ExecQueryRower] и [soul.ExecQueryRower]
// (*pgxpool.Pool удовлетворяет обоим).
func NewVigilSource(db oracleDB) VigilSource {
	return &vigilSource{db: db}
}

func (s *vigilSource) ActiveVigilsForSID(ctx context.Context, sid string) ([]*keeperv1.VigilDef, error) {
	var covens []string
	su, err := soul.SelectBySID(ctx, s.db, sid)
	switch {
	case err == nil:
		covens = su.Coven
	case errors.Is(err, soul.ErrSoulNotFound):
		// Хост ещё не в реестре (онбординг не завершён) — coven-Vigil не
		// сматчит, но sid-Vigil может. Резолвим с пустыми covens.
	default:
		return nil, fmt.Errorf("grpc: vigil source covens resolve: %w", err)
	}

	vigils, err := oracle.SelectActiveVigilsForSubject(ctx, s.db, sid, covens)
	if err != nil {
		return nil, err
	}
	out := make([]*keeperv1.VigilDef, 0, len(vigils))
	for _, v := range vigils {
		def := &keeperv1.VigilDef{
			Name:     v.Name,
			Interval: v.IntervalSpec,
			Check:    v.CheckAddr,
		}
		if len(v.Params) > 0 {
			params := &structpb.Struct{}
			if err := params.UnmarshalJSON(v.Params); err != nil {
				return nil, fmt.Errorf("grpc: vigil %q params unmarshal: %w", v.Name, err)
			}
			def.Params = params
		}
		out = append(out, def)
	}
	return out, nil
}

// handlePortentEvent — обработчик payload-а [keeperv1.PortentEvent] (ADR-030).
//
// SID берётся из mTLS peer cert сессии (передан caller-ом), НЕ из
// PortentEvent.sid — авторитет идентичности Soul-а это сертификат (ADR-012(i),
// ADR-030: beacon-событие = недоверенный вход).
//
// Поток (default-deny):
//  1. SelectDecreesByBeacon(beacon_name) — enabled-Decree-ы на этот Vigil.
//     Пусто → ничего (нет правила → нет действия).
//  2. covens субъекта из souls-registry (НЕ из payload).
//  3. для каждого Decree: SubjectMatches (sid/coven) — нет → skip;
//     where-CEL (если задан) над event.data — false → skip.
//  4. cooldown-check per-(decree, subject): в окне → skip + debug-лог.
//  5. EnqueueScenario(action_scenario, action_input) → RecordFire → audit
//     oracle.fired.
//
// Любой сбой на одном Decree не прерывает обработку остальных (best-effort,
// несколько Decree могут реагировать на один Portent). default-deny строго:
// любая неуверенность (no subject-match / where false / resolve-сбой) →
// пропуск, не действие.
func (h *eventStreamHandler) handlePortentEvent(ctx context.Context, sid, sessionID string, evt *keeperv1.PortentEvent) {
	deps := h.deps.Oracle
	if deps == nil {
		h.logger.Warn("eventstream: PortentEvent received but oracle not wired up",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	if evt == nil {
		h.logger.Warn("eventstream: PortentEvent payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}

	beacon := evt.GetBeaconName()
	if beacon == "" {
		h.logger.Warn("eventstream: PortentEvent with empty beacon_name — ignoring",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}

	// Принят валидный Portent (непустой beacon_name) — знаменатель прочих метрик.
	deps.Metrics.ObservePortentReceived()

	decrees, err := oracle.SelectDecreesByBeacon(ctx, deps.DB, beacon)
	if err != nil {
		h.logger.Warn("eventstream: oracle select decrees failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.String("beacon", beacon),
			slog.Any("error", err),
		)
		return
	}
	if len(decrees) == 0 {
		// Default-deny: нет матчащего Decree → событие не вызывает действия.
		h.logger.Debug("eventstream: oracle no decree for beacon — default-deny",
			slog.String("sid", sid), slog.String("beacon", beacon))
		return
	}

	// covens субъекта из авторитетного souls-registry (НЕ из payload).
	covens, err := h.oracleSubjectCovens(ctx, deps, sid)
	if err != nil {
		h.logger.Warn("eventstream: oracle subject covens resolve failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}

	// where-CEL читает полный event (typed payload V5-1 + legacy data, оба
	// стиля доступа): передаём evt целиком в evaluateDecree. Активация
	// собирается в WhereEvaluator.EvalEvent.
	for _, decree := range decrees {
		h.evaluateDecree(ctx, deps, sid, sessionID, beacon, decree, covens, evt)
	}
}

// oracleSubjectCovens резолвит covens субъекта по авторитетному SID из реестра
// souls. ErrSoulNotFound → пустые covens (хост ещё не зарегистрирован; sid-Decree
// всё равно может сматчить по SID, coven-Decree — нет).
func (h *eventStreamHandler) oracleSubjectCovens(ctx context.Context, deps *OracleDeps, sid string) ([]string, error) {
	s, err := soul.SelectBySID(ctx, deps.DB, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return s.Coven, nil
}

// subjectInIncarnation сообщает, принадлежит ли хост-отправитель таргет-
// incarnation Decree-а: incarnation.name — корневая Coven-метка (ADR-008),
// поэтому членство сводится к incarnationName ∈ covens субъекта. covens —
// авторитетные из souls-registry. Пустой incarnationName (теоретически
// невозможен — NOT NULL в схеме) трактуется fail-closed как «не член».
func subjectInIncarnation(incarnationName string, covens []string) bool {
	if incarnationName == "" {
		return false
	}
	for _, c := range covens {
		if c == incarnationName {
			return true
		}
	}
	return false
}

// evaluateDecree применяет один Decree к Portent-у: subject-match → where-CEL →
// membership-check → cooldown → enqueue → record fire → audit. Любой сбой
// логируется и НЕ валит обработку прочих Decree-ов (best-effort). default-deny:
// каждая неуверенность — пропуск.
func (h *eventStreamHandler) evaluateDecree(
	ctx context.Context,
	deps *OracleDeps,
	sid, sessionID, beacon string,
	decree *oracle.Decree,
	covens []string,
	evt *keeperv1.PortentEvent,
) {
	if !oracle.SubjectMatches(decree, sid, covens) {
		h.logger.Debug("eventstream: oracle decree subject mismatch — skip",
			slog.String("sid", sid), slog.String("decree", decree.Name))
		return
	}

	// Membership sanity-check (РЕШЕНИЕ #3): хост-отправитель должен принадлежать
	// таргет-incarnation Decree-а. incarnation.name — корневая Coven-метка
	// (ADR-008), поэтому членство = incarnation_name ∈ covens субъекта (covens
	// авторитетны из souls-registry). Не член → skip + warn, fire НЕ пишется,
	// audit oracle.fired НЕ пишется: запрет ставить сценарий incarnation-а на
	// хост вне него (защита от cross-incarnation-эскалации, ADR-030(b)).
	if !subjectInIncarnation(decree.IncarnationName, covens) {
		h.logger.Warn("eventstream: oracle decree subject not in target incarnation — skip",
			slog.String("sid", sid),
			slog.String("decree", decree.Name),
			slog.String("incarnation", decree.IncarnationName),
		)
		return
	}

	if decree.WhereCEL != nil && *decree.WhereCEL != "" {
		ok, err := deps.Where.EvalEvent(*decree.WhereCEL, evt)
		if err != nil {
			// Битый where_cel (compile-ошибка) — конфигурационная проблема
			// Decree-а; default-deny: не срабатываем, логируем для оператора.
			h.logger.Warn("eventstream: oracle decree where-CEL compile failed — skip (default-deny)",
				slog.String("sid", sid),
				slog.String("decree", decree.Name),
				slog.Any("error", err),
			)
			return
		}
		if !ok {
			h.logger.Debug("eventstream: oracle decree where-CEL false — skip",
				slog.String("sid", sid), slog.String("decree", decree.Name))
			return
		}
	}

	now := time.Now().UTC()

	// Cooldown-check per-(decree, subject) (loop-prevention, ADR-030(a)).
	lastFired, hasFired, err := oracle.LastFiredAt(ctx, deps.DB, decree.Name, sid)
	if err != nil {
		h.logger.Warn("eventstream: oracle cooldown read failed — skip (fail-safe)",
			slog.String("sid", sid), slog.String("decree", decree.Name), slog.Any("error", err))
		return
	}
	if oracle.WithinCooldown(decree.Cooldown, lastFired, hasFired, now) {
		deps.Metrics.ObserveCooldownBlocked()
		h.logger.Debug("eventstream: oracle decree within cooldown — skip",
			slog.String("sid", sid),
			slog.String("decree", decree.Name),
			slog.String("cooldown", decree.Cooldown),
		)
		return
	}

	// Decree прошёл весь фильтр (subject/membership/where/cooldown) и будет
	// поставлен — фиксируем match ДО enqueue (учтём попытку даже при сбое enqueue;
	// расхождение matched↔enqueued = ошибки постановки, видимы по серии).
	deps.Metrics.ObserveDecreeMatched()

	// action_input (JSONB) → map для RunSpec. Битый JSON — конфигурационная
	// ошибка Decree-а (валидируется на service-слое S3); default-deny: skip.
	var actionInput map[string]any
	if len(decree.ActionInput) > 0 {
		if err := json.Unmarshal(decree.ActionInput, &actionInput); err != nil {
			h.logger.Warn("eventstream: oracle decree action_input is not a JSON object — skip",
				slog.String("sid", sid), slog.String("decree", decree.Name), slog.Any("error", err))
			return
		}
	}

	applyID, err := deps.Enqueuer.EnqueueScenario(ctx, EnqueueScenarioRequest{
		SubjectSID:      sid,
		IncarnationName: decree.IncarnationName,
		ScenarioName:    decree.ActionScenario,
		ActionInput:     actionInput,
		DecreeName:      decree.Name,
	})
	if err != nil {
		h.logger.Warn("eventstream: oracle scenario enqueue failed",
			slog.String("sid", sid),
			slog.String("decree", decree.Name),
			slog.String("scenario", decree.ActionScenario),
			slog.Any("error", err),
		)
		return
	}
	deps.Metrics.ObserveScenarioEnqueued()

	// Cooldown-state фиксируется ПОСЛЕ успешной постановки: запись срабатывания
	// без фактического enqueue ложно заблокировала бы будущие реакции.
	if err := oracle.RecordFire(ctx, deps.DB, decree.Name, sid, now); err != nil {
		// Прогон уже поставлен — fire-record best-effort: при сбое cooldown на
		// эту пару не активируется (возможен повтор до следующего успешного
		// record), но idempotent scenario гасит петлю на уровне action (ADR-030(a)).
		h.logger.Warn("eventstream: oracle record fire failed — cooldown not persisted",
			slog.String("sid", sid), slog.String("decree", decree.Name), slog.Any("error", err))
	}

	// circuit-breaker (ADR-030(a), beacons S4): второй барьер loop-prevention
	// после cooldown. cooldown гасит per-(decree, subject); circuit-breaker
	// считает срабатывания правила СУММАРНО (fixed-window) и при превышении
	// порога авто-disable-ит Decree. breaker-off (max_fires==0, escape-hatch) →
	// BumpCircuit не вызываем вовсе. now — тот же момент, что cooldown/audit.
	h.tripCircuitIfTripped(ctx, deps, decree, now)

	h.auditOracleFired(ctx, sid, beacon, decree.Name, decree.ActionScenario, applyID)
	h.logger.Info("eventstream: oracle fired",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.String("beacon", beacon),
		slog.String("decree", decree.Name),
		slog.String("scenario", decree.ActionScenario),
		slog.String("apply_id", applyID),
	)
}

// tripCircuitIfTripped инкрементирует fixed-window счётчик срабатываний Decree-а
// и, при достижении порога, авто-disable-ит правило (circuit-breaker, ADR-030(a)).
// Вызывается ПОСЛЕ успешного enqueue+RecordFire (только реально поставленное
// срабатывание считается). breaker-off (CircuitMaxFires==0) — no-op (BumpCircuit
// не вызывается). Любой сбой best-effort: логируется и НЕ валит обработку (прогон
// уже поставлен).
//
// Trip выполняется single-winner-ом: TripDecree переводит enabled true→false
// атомарно, и только инстанс с RowsAffected==1 пишет metric+audit+warn — при
// конкурентном trip-е с нескольких Keeper-инстансов алертит ровно один.
func (h *eventStreamHandler) tripCircuitIfTripped(ctx context.Context, deps *OracleDeps, decree *oracle.Decree, now time.Time) {
	if deps.CircuitMaxFires <= 0 {
		return // breaker OFF (escape-hatch)
	}

	cnt, err := oracle.BumpCircuit(ctx, deps.DB, decree.Name, now, deps.CircuitWindow)
	if err != nil {
		h.logger.Warn("eventstream: oracle circuit bump failed — breaker counter not updated",
			slog.String("decree", decree.Name), slog.Any("error", err))
		return
	}
	if cnt < deps.CircuitMaxFires {
		return
	}

	tripped, err := oracle.TripDecree(ctx, deps.DB, decree.Name, now)
	if err != nil {
		h.logger.Warn("eventstream: oracle circuit trip failed — decree not auto-disabled",
			slog.String("decree", decree.Name), slog.Any("error", err))
		return
	}
	if !tripped {
		// Другой Keeper-инстанс уже выиграл trip (или Decree снят оператором) —
		// не дублируем alert/audit/metric.
		return
	}

	deps.Metrics.ObserveCircuitTripped()
	h.auditDecreeCircuitTripped(ctx, decree.Name, cnt, deps.CircuitWindow)
	h.logger.Warn("eventstream: oracle circuit tripped — decree auto-disabled",
		slog.String("decree", decree.Name),
		slog.Int("fire_count", cnt),
		slog.String("window", deps.CircuitWindow.String()),
		slog.Int("max_fires", deps.CircuitMaxFires),
	)
}

// auditDecreeCircuitTripped пишет событие `decree.circuit_tripped` на авто-disable
// Decree-а circuit-breaker-ом (ADR-030(a)). Payload — свойство правила (decree /
// fire_count / window / trigger), БЕЗ subject/beacon/event.data: недоверенный
// payload события не кладём, trip привязан к правилу, а не к конкретному хосту.
// Best-effort: сбой audit-а не отменяет уже выполненный авто-disable.
func (h *eventStreamHandler) auditDecreeCircuitTripped(ctx context.Context, decree string, fireCount int, window time.Duration) {
	if err := h.deps.Oracle.AuditWriter.Write(ctx, &audit.Event{
		EventType: audit.EventDecreeCircuitTripped,
		Source:    audit.SourceSoulGRPC,
		Payload: map[string]any{
			"decree":     decree,
			"fire_count": fireCount,
			"window":     window.String(),
			"trigger":    "circuit_breaker",
		},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		h.logger.Warn("eventstream: oracle circuit-tripped audit write failed",
			slog.String("decree", decree),
			slog.Any("error", err),
		)
	}
}

// auditOracleFired пишет событие `oracle.fired` на срабатывание reactor-а
// (ADR-030(b), категория soul_grpc). Значения event.data в payload НЕ кладём
// (недоверенный источник). Best-effort: сбой audit-а не отменяет уже
// поставленный прогон (паттерн прочих event-handler-ов).
func (h *eventStreamHandler) auditOracleFired(ctx context.Context, sid, beacon, decree, scenario, applyID string) {
	if err := h.deps.Oracle.AuditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventOracleFired,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: applyID,
		Payload: map[string]any{
			"sid":      sid,
			"beacon":   beacon,
			"decree":   decree,
			"scenario": scenario,
			"apply_id": applyID,
		},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		h.logger.Warn("eventstream: oracle audit write failed",
			slog.String("sid", sid),
			slog.String("decree", decree),
			slog.Any("error", err),
		)
	}
}
