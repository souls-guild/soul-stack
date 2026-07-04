package api

// GET /v1/incarnations/{name}/runs/{apply_id}/events — live-ход прогона инкарнации
// (SSE, ADR-068 §A3). Симметрия существующему RunDetail-пути + прецеденту SSE
// `/mcp/events`, но на Operator-плоскости /v1 (свой query-token из канона).
//
// ПОЧЕМУ НЕ /mcp/events: та — MCP-плоскость (JSON-RPC tool-call streaming), свой auth,
// не читает access_token. Тащить web-UI в MCP-канал = смешение плоскостей. Operator-API
// /v1/* уже имеет query-token из коробки (middleware.extractToken для */events).
// ★ /mcp/events этим слайсом НЕ трогается — узкий дубль потока/маскинга здесь
// (ADR-068 §A3 «продублировать узко» — вместо расшаривания кода из mcp/sse.go).
//
// Регистрируется через huma.StreamResponse → операция попадает в OpenAPI-спеку
// (drift-guard TestFullSpec_CoversAllRoutes) при полном контроле тела стрима
// (heartbeat/max-lifetime/frame/маскинг/лимиты), которого huma/sse-хелпер не даёт.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// SSE-параметры (parity mcp/sse.go). Heartbeat не даёт proxy/LB закрыть idle-соединение;
// max-lifetime — потолок против FD/goroutine-утечки «зависших» клиентов; conn-лимиты —
// защита FD/goroutine-бюджета инстанса и от одного Архонта с сотнями стримов.
const (
	sseHeartbeatInterval = 30 * time.Second
	sseMaxLifetime       = 30 * time.Minute
	sseMaxConnsGlobal    = 256
	sseMaxConnsPerAID    = 16
)

// runEventsAccess — узкая поверхность резолва apply_id → владелец+incarnation для
// RBAC SSE-подписки. Прод — [applyrun.SelectAccessByApplyID] поверх pool; тест
// подставляет fake.
type runEventsAccess interface {
	Access(ctx context.Context, applyID string) (*applyrun.Access, error)
}

// runEventsPGAccess — прод-реализация [runEventsAccess] поверх Operator-pool-а.
type runEventsPGAccess struct {
	db applyrun.ExecQueryRower
}

func (a runEventsPGAccess) Access(ctx context.Context, applyID string) (*applyrun.Access, error) {
	return applyrun.SelectAccessByApplyID(ctx, a.db, applyID)
}

// runEventsDeps — зависимости SSE-handler-а прогона. Bus/Access/RBAC обеспечивают
// поток + RBAC-подписку; Limiter/Logger — resource-guard и наблюдаемость. Любой nil
// (кроме Limiter/Logger) → подписка отклоняется fail-closed (см. [authorizeRunEventsSSE]).
type runEventsDeps struct {
	Bus     *applybus.EventBus
	Access  runEventsAccess
	RBAC    apimiddleware.PermissionChecker
	Limiter *sseConnLimiter
	Logger  *slog.Logger
}

// newRunEventsDeps собирает прод-deps поверх applybus + Operator-pool + enforcer.
// db/rbac/bus nil → SSE-route в router.go не монтируется (opt-in wire-up).
func newRunEventsDeps(bus *applybus.EventBus, db applyrun.ExecQueryRower, rbac apimiddleware.PermissionChecker, logger *slog.Logger) *runEventsDeps {
	if bus == nil || db == nil || rbac == nil {
		return nil
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &runEventsDeps{
		Bus:     bus,
		Access:  runEventsPGAccess{db: db},
		RBAC:    rbac,
		Limiter: newSSEConnLimiter(sseMaxConnsGlobal, sseMaxConnsPerAID),
		Logger:  logger,
	}
}

// incRunEventsInput — huma-input GET .../runs/{apply_id}/events. Name/ApplyID — path.
type incRunEventsInput struct {
	Name    string `path:"name" doc:"имя инкарнации"`
	ApplyID string `path:"apply_id" doc:"ULID прогона; чужой/несуществующий → 403 (anti-enum)"`
}

func incRunEventsOperation() huma.Operation {
	op := huma.Operation{
		OperationID:   "streamIncarnationRunEvents",
		Method:        http.MethodGet,
		Path:          "/{name}/runs/{apply_id}/events",
		Summary:       "Live-ход прогона инкарнации (SSE)",
		Description:   "text/event-stream: task.executed/apply.completed/failed/cancelled по apply_id. Auth: Bearer ИЛИ ?access_token=<short-jwt> из POST /v1/sse-token. Доступ: инициатор ИЛИ incarnation.get/history; чужой/несуществующий apply_id → 403 (anti-enum, parity /mcp/events). Секреты в payload маскируются.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError},
	}
	// Явно декларируем text/event-stream (parity huma/sse-хелпера) — тело потоковое,
	// именованной схемы не заводим (inline string), чтобы не плодить тех-имя в спеке.
	op.Responses = map[string]*huma.Response{
		"200": {
			Description: "SSE-поток apply-событий прогона",
			Content: map[string]*huma.MediaType{
				"text/event-stream": {Schema: &huma.Schema{Type: huma.TypeString}},
			},
		},
	}
	return op
}

// registerHumaIncarnationRunEvents монтирует GET .../runs/{apply_id}/events как
// потоковый (huma.StreamResponse). deps nil → no-op (opt-in wire-up). Route под /v1
// RequireJWT-цепочкой (query-token */events из канона) БЕЗ chi-RequireAction: RBAC
// «инициатор ИЛИ incarnation.get/history» не выражается existence-gate-ом (инициатор
// может не иметь права) — вся авторизация in-handler (parity /mcp/events authorizeSSE).
func registerHumaIncarnationRunEvents(humaAPI huma.API, deps *runEventsDeps) {
	if deps == nil {
		return
	}
	huma.Register(humaAPI, incRunEventsOperation(), func(ctx context.Context, in *incRunEventsInput) (*huma.StreamResponse, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok || claims == nil {
			return nil, incMissingClaims()
		}
		// anti-enum: ЛЮБОЙ отказ (не найден / чужая инкарнация / нет прав) → одинаковый
		// 403, неотличимый от «нет доступа» (ULID угадываем, parity /mcp/events).
		if !authorizeRunEventsSSE(ctx, deps, claims.Subject, in.Name, in.ApplyID) {
			return nil, sseForbidden()
		}
		// conn-limit (M4): слот занимаем ТОЛЬКО под авторизованную подписку, освобождаем
		// в defer тела стрима (huma гарантированно вызывает Body у StreamResponse).
		if deps.Limiter != nil && !deps.Limiter.Acquire(claims.Subject) {
			return nil, sseTooManyStreams()
		}
		aid := claims.Subject
		applyID := in.ApplyID
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			if deps.Limiter != nil {
				defer deps.Limiter.Release(aid)
			}
			streamRunEvents(hctx, deps, applyID, aid)
		}}, nil
	})
}

// authorizeRunEventsSSE — RBAC-проверка подписки на apply_id (ADR-068 §A3, parity
// /mcp/events authorizeSSE). fail-closed на любом сбое/nil-deps. Порядок:
//   - Access/RBAC nil или lookup-ошибка/не найден → deny (anti-enum, неотличимо от отказа);
//   - apply_id принадлежит ДРУГОЙ инкарнации (не path-{name}) → deny (чужой прогон);
//   - инициатор прогона (started_by_aid == sub) → allow;
//   - иначе allow при incarnation.get ЛИБО incarnation.history на инкарнации.
func authorizeRunEventsSSE(ctx context.Context, deps *runEventsDeps, sub, name, applyID string) bool {
	if deps.Access == nil {
		return false
	}
	acc, err := deps.Access.Access(ctx, applyID)
	if err != nil {
		return false
	}
	// apply_id обязан быть прогоном ИМЕННО этой инкарнации (path-{name}); иначе — чужой
	// прогон, deny без раскрытия, что apply_id живёт в другой инкарнации.
	if acc.IncarnationName != name {
		return false
	}
	if acc.StartedByAID != nil && *acc.StartedByAID == sub {
		return true
	}
	if deps.RBAC == nil {
		return false
	}
	if deps.RBAC.Check(sub, "incarnation", "get", map[string]string{"incarnation": name}) == nil {
		return true
	}
	if deps.RBAC.Check(sub, "incarnation", "history", map[string]string{"incarnation": name}) == nil {
		return true
	}
	return false
}

// streamRunEvents гонит apply-события applyID в SSE-поток до отключения клиента,
// max-lifetime или закрытия шины. Frame `event/id/data`, heartbeat 30s (parity
// mcp/sse.go). Payload маскируется [audit.MaskSecrets] на write-path (второй барьер
// поверх секрет-гигиены publisher-ов).
func streamRunEvents(hctx huma.Context, deps *runEventsDeps, applyID, aid string) {
	hctx.SetHeader("Content-Type", "text/event-stream")
	hctx.SetHeader("Cache-Control", "no-cache")
	hctx.SetHeader("Connection", "keep-alive")
	hctx.SetHeader("X-Accel-Buffering", "no")

	bw := hctx.BodyWriter()
	flusher := unwrapFlusher(bw)
	if d := unwrapWriteDeadliner(bw); d != nil {
		// SSE — long-lived: снимаем WriteTimeout http.Server для этого запроса.
		_ = d.SetWriteDeadline(time.Time{})
	}

	// Немедленный flush заголовков (200) + SSE-комментарий: EventSource onopen
	// срабатывает ДО первого события/heartbeat (client-open immediacy, parity
	// mcp/sse.go WriteHeader+Flush). Иначе huma коммитит 200 лишь на первой записи —
	// клиент висел бы до heartbeat 30s.
	_, _ = bw.Write([]byte(":ok\n\n"))
	flush(flusher)

	ctx, cancel := context.WithTimeout(hctx.Context(), sseMaxLifetime)
	defer cancel()

	ch := deps.Bus.Subscribe(ctx, applyID)

	deps.Logger.Info("v1/sse: run-events subscriber opened",
		slog.String("apply_id", applyID), slog.String("aid", aid))
	defer deps.Logger.Info("v1/sse: run-events subscriber closed",
		slog.String("apply_id", applyID), slog.String("aid", aid))

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := bw.Write([]byte(":keepalive\n\n")); err != nil {
				return
			}
			flush(flusher)
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := writeRunEventFrame(bw, ev); err != nil {
				return
			}
			flush(flusher)
		}
	}
}

// writeRunEventFrame сериализует apply-событие в SSE-frame `event/id/data` с маскингом
// payload (H1, второй барьер). Узкий дубль mcp writeSSEEvent — /mcp/events не трогаем.
func writeRunEventFrame(w io.Writer, ev applybus.Event) error {
	masked := maskRunEventPayload(ev.Payload)
	payloadJSON, err := json.Marshal(masked)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	_, err = fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n", ev.Kind, ev.ApplyID, payloadJSON)
	return err
}

// maskRunEventPayload приводит payload к masked-форме (H1): map — напрямую MaskSecrets;
// raw-JSON (cross-Keeper bridge) — декод→маск→map; иное — как есть. Узкий дубль mcp
// maskSSEPayload (ADR-068 §A3).
func maskRunEventPayload(payload any) any {
	switch p := payload.(type) {
	case nil:
		return nil
	case map[string]any:
		return audit.MaskSecrets(p)
	case json.RawMessage:
		return maskRunEventRawJSON(p)
	case []byte:
		return maskRunEventRawJSON(p)
	default:
		return payload
	}
}

func maskRunEventRawJSON(raw []byte) any {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return json.RawMessage(raw)
	}
	return audit.MaskSecrets(m)
}

// --- потоковые helper-ы (unwrap flusher/deadliner из BodyWriter huma-адаптера) ---

func flush(f http.Flusher) {
	if f != nil {
		f.Flush()
	}
}

func unwrapFlusher(w io.Writer) http.Flusher {
	for {
		if f, ok := w.(http.Flusher); ok {
			return f
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
}

type writeDeadliner interface{ SetWriteDeadline(time.Time) error }

func unwrapWriteDeadliner(w io.Writer) writeDeadliner {
	for {
		if d, ok := w.(writeDeadliner); ok {
			return d
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
}

// --- problem-ответы SSE-route ---

func sseForbidden() huma.StatusError {
	return humaProblemError{Details: problemWithStatus(problem.TypeForbidden, http.StatusForbidden, "forbidden: no access to this run")}
}

func sseTooManyStreams() huma.StatusError {
	return humaProblemError{Details: problemWithStatus(problem.TypeTempoExceeded, http.StatusTooManyRequests, "too many concurrent event streams; retry later")}
}

// --- conn-limiter (глобальный + per-AID, parity mcp sseConnLimiter, узкий дубль) ---

type sseConnLimiter struct {
	mu        sync.Mutex
	maxGlobal int
	maxPerAID int
	global    int
	perAID    map[string]int
}

func newSSEConnLimiter(maxGlobal, maxPerAID int) *sseConnLimiter {
	return &sseConnLimiter{maxGlobal: maxGlobal, maxPerAID: maxPerAID, perAID: make(map[string]int)}
}

// Acquire резервирует слот под aid; false при превышении global/per-AID лимита.
func (l *sseConnLimiter) Acquire(aid string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.maxGlobal > 0 && l.global >= l.maxGlobal {
		return false
	}
	if l.maxPerAID > 0 && l.perAID[aid] >= l.maxPerAID {
		return false
	}
	l.global++
	l.perAID[aid]++
	return true
}

// Release освобождает слот aid (ровно один раз на успешный Acquire).
func (l *sseConnLimiter) Release(aid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.global > 0 {
		l.global--
	}
	if n := l.perAID[aid]; n > 1 {
		l.perAID[aid] = n - 1
	} else {
		delete(l.perAID, aid)
	}
}
