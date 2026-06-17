package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// SSE-параметры. Heartbeat-comment отправляется чтобы proxy/load-balancer
// не закрывал соединение по idle-timeout-у (типично 30–60 с). Comment-line
// `:keepalive\n\n` — стандартный SSE-приём, не материализуется как event-data
// на клиенте.
const (
	sseHeartbeatInterval = 30 * time.Second
	sseQueryApplyID      = "apply_id"

	// sseMaxLifetime — жёсткий потолок жизни одного SSE-стрима (M4: защита
	// от FD/goroutine-утечки на «зависших» клиентах, которые держат
	// соединение, но не читают). По истечении стрим закрывается; клиент
	// переподключается (SSE auto-reconnect). 30 мин с запасом покрывают
	// типичный apply-прогон.
	sseMaxLifetime = 30 * time.Minute

	// sseMaxConnsGlobal / sseMaxConnsPerAID — лимиты одновременных стримов
	// (M4). Global защищает FD/goroutine-бюджет инстанса; per-AID — от
	// одного Архонта, открывающего сотни стримов. Превышение → HTTP 429.
	sseMaxConnsGlobal = 256
	sseMaxConnsPerAID = 16
)

// applyAccessStore — узкая поверхность над applyrun-CRUD, нужная SSE-handler-у
// для RBAC-проверки подписки (M1). Прод-реализация — [applyAccessPG] поверх
// pgxpool.Pool; unit-тесты подставляют fake.
type applyAccessStore interface {
	// Access резолвит apply_id → владелец прогона + incarnation. Возвращает
	// applyrun.ErrApplyRunNotFound, если прогона нет.
	Access(ctx context.Context, applyID string) (*applyrun.Access, error)
}

// sseDeps — узкие зависимости SSE-handler-а. Bus берётся отдельно от
// HandlerDeps (для tools/call), чтобы EventBus не тянулся в unit-тесты
// JSON-RPC-стороны.
//
// Access и RBAC обеспечивают RBAC-проверку подписки (M1): подписаться на
// apply_id может только инициатор прогона (started_by_aid) или Архонт с
// `incarnation.get` на соответствующей incarnation. Если Access == nil —
// подписка отклоняется (fail-closed): без access-store нельзя резолвить
// владельца прогона, а fail-open открыл бы чужой стрим. Прод всегда
// прокидывает Access.
type sseDeps struct {
	JWTVerifier *jwt.Verifier
	Bus         *applybus.EventBus
	Access      applyAccessStore
	RBAC        PermissionChecker
	Limiter     *sseConnLimiter
	Logger      *slog.Logger
}

// sseConnLimiter — счётчик активных SSE-стримов: глобальный + per-AID (M4).
// Потокобезопасен. Acquire резервирует слот (или возвращает false при
// превышении), Release освобождает.
type sseConnLimiter struct {
	mu        sync.Mutex
	maxGlobal int
	maxPerAID int
	global    int
	perAID    map[string]int
}

// newSSEConnLimiter собирает limiter. maxGlobal/maxPerAID <= 0 трактуется
// как «лимит выключен» по соответствующей оси.
func newSSEConnLimiter(maxGlobal, maxPerAID int) *sseConnLimiter {
	return &sseConnLimiter{
		maxGlobal: maxGlobal,
		maxPerAID: maxPerAID,
		perAID:    make(map[string]int),
	}
}

// Acquire резервирует слот под aid. Возвращает false, если превышен
// глобальный или per-AID лимит (caller отдаёт 429, слот НЕ занят).
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

// Release освобождает слот aid. Идемпотентность не гарантируется — caller
// вызывает Release ровно один раз на успешный Acquire (через defer).
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

// buildSSEHandler — `GET /mcp/events?apply_id=<ULID>`. JWT-auth идентичен
// `POST /mcp`: scheme-insensitive Bearer.
//
// Контракт ответа:
//
//   - Content-Type: text/event-stream
//   - Cache-Control: no-cache, X-Accel-Buffering: no — отключаем буферизацию
//     reverse-proxy (nginx) и кеширование браузера.
//   - Каждое событие из bus → `event: <kind>\ndata: <json>\nid: <apply_id>\n\n`.
//   - Heartbeat каждые 30 с — `:keepalive\n\n`.
//
// На auth-ошибку отдаём HTTP 401 / 400 / 403 / 429 c JSON-error-body (не
// SSE-формат) — клиент ещё не подписался, нет смысла открывать stream только
// чтобы сразу закрыть.
//
// RBAC (M1): подписка на apply_id разрешена только инициатору прогона
// (apply_runs.started_by_aid == JWT.sub) либо Архонту с `incarnation.get`
// на соответствующей incarnation. Несуществующий apply_id → 403 (anti-enum:
// неотличимо от отказа доступа — ULID угадываем).
//
// Resource-limits (M4): глобальный + per-AID лимит одновременных стримов
// (429 при превышении) и жёсткий max-lifetime потолок (стрим закрывается,
// клиент переподключается).
func buildSSEHandler(deps sseDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed,
				"method not allowed (use GET)")
			return
		}

		token, ok := jwt.ParseBearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeJSONError(w, http.StatusUnauthorized,
				"missing or malformed Authorization header (expect: Bearer <jwt>)")
			return
		}
		claims, err := deps.JWTVerifier.Verify(token)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized,
				"unauthenticated: "+jwt.ClassifyVerifyErr(err))
			return
		}

		applyID := strings.TrimSpace(r.URL.Query().Get(sseQueryApplyID))
		if applyID == "" {
			writeJSONError(w, http.StatusBadRequest,
				"missing required query param 'apply_id'")
			return
		}

		// RBAC-проверка подписки (M1). Выполняется ДО Acquire/headers —
		// отказ возвращается как обычный JSON-error, стрим не открывается.
		if !authorizeSSE(r.Context(), deps, claims.Subject, applyID) {
			writeJSONError(w, http.StatusForbidden,
				"forbidden: no access to this apply_id")
			return
		}

		// Connection-limit (M4). Acquire после RBAC — слот занимаем только
		// для авторизованных подписок. Release строго через defer.
		if deps.Limiter != nil {
			if !deps.Limiter.Acquire(claims.Subject) {
				writeJSONError(w, http.StatusTooManyRequests,
					"too many concurrent event streams; retry later")
				return
			}
			defer deps.Limiter.Release(claims.Subject)
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			// Сервер настроен на http.Server без flush-капабилити —
			// это конфигурационный bug (HTTP/1.1 в Go-stdlib даёт Flusher
			// поверх response-writer-а по умолчанию). Возвращаем 500, чтобы
			// клиент не висел в ожидании.
			deps.Logger.Error("mcp/sse: ResponseWriter does not implement http.Flusher")
			writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// Nginx/Apache reverse-proxy уважают этот header и не буферизуют.
		w.Header().Set("X-Accel-Buffering", "no")

		// SSE — long-lived stream; глобальный http.Server WriteTimeout
		// (60s) разорвал бы соединение. Снимаем deadline для этого
		// одного запроса. http.NewResponseController появился в Go 1.20.
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			// stdlib http.Server поддерживает SetWriteDeadline — если
			// контроллер вернул ошибку, это нестандартный middleware
			// в стеке. Логируем и продолжаем: события всё равно пойдут,
			// но клиента отрубит по WriteTimeout-у.
			deps.Logger.Warn("mcp/sse: SetWriteDeadline failed", slog.Any("error", err))
		}

		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Max-lifetime потолок (M4): производный ctx, который отменяется по
		// истечении sseMaxLifetime ИЛИ при отключении клиента (r.Context()).
		ctx, cancel := context.WithTimeout(r.Context(), sseMaxLifetime)
		defer cancel()

		ch := deps.Bus.Subscribe(ctx, applyID)

		deps.Logger.Info("mcp/sse: subscriber opened",
			slog.String("apply_id", applyID),
			slog.String("aid", claims.Subject),
		)
		defer deps.Logger.Info("mcp/sse: subscriber closed",
			slog.String("apply_id", applyID),
			slog.String("aid", claims.Subject),
		)

		heartbeat := time.NewTicker(sseHeartbeatInterval)
		defer heartbeat.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				if _, err := w.Write([]byte(":keepalive\n\n")); err != nil {
					return
				}
				flusher.Flush()
			case ev, ok := <-ch:
				if !ok {
					// Bus закрыл канал — обычно одновременно с ctx.Done()
					// (наша же goroutine). Дополнительный return на случай
					// гонки.
					return
				}
				if err := writeSSEEvent(w, ev); err != nil {
					deps.Logger.Warn("mcp/sse: write event failed",
						slog.String("apply_id", ev.ApplyID),
						slog.String("kind", string(ev.Kind)),
						slog.Any("error", err),
					)
					return
				}
				flusher.Flush()
			}
		}
	}
}

// authorizeSSE — RBAC-проверка подписки на apply_id (M1).
//
// Правила:
//   - Access == nil → deny (fail-closed): без access-store невозможно
//     резолвить владельца/incarnation прогона, поэтому проверку нельзя
//     выполнить и подписка отклоняется. Прод всегда прокидывает Access.
//   - apply_id не найден → deny (anti-enum: неотличимо от отказа доступа).
//   - started_by_aid == sub → allow (инициатор всегда видит свой прогон).
//   - иначе → allow только при `incarnation.get` permission на incarnation
//     прогона (RBAC == nil → deny, кроме случая владельца выше).
//
// На любой инфраструктурной ошибке (PG недоступен) — deny (fail-closed):
// безопаснее отказать в подписке, чем открыть чужой стрим.
func authorizeSSE(ctx context.Context, deps sseDeps, sub, applyID string) bool {
	if deps.Access == nil {
		deps.Logger.Warn("mcp/sse: apply access-store not configured (deny)",
			slog.String("apply_id", applyID),
		)
		return false
	}
	acc, err := deps.Access.Access(ctx, applyID)
	if err != nil {
		if !errors.Is(err, applyrun.ErrApplyRunNotFound) {
			deps.Logger.Warn("mcp/sse: apply access lookup failed (deny)",
				slog.String("apply_id", applyID),
				slog.Any("error", err),
			)
		}
		return false
	}
	if acc.StartedByAID != nil && *acc.StartedByAID == sub {
		return true
	}
	if deps.RBAC == nil {
		return false
	}
	err = deps.RBAC.Check(sub, "incarnation", "get", map[string]string{
		"incarnation": acc.IncarnationName,
	})
	return err == nil
}

// writeSSEEvent сериализует одно apply-событие в SSE-frame:
//
//	event: <kind>
//	id:    <apply_id>
//	data:  <json payload>
//	<пустая строка>
//
// Payload прогоняется через [audit.MaskSecrets] перед сериализацией (H1):
// register-output / state_changes могут нести секреты (`bootstrap_token` и
// т.п.), которые иначе утекли бы в SSE-frame в открытом виде. Payload пишется
// как одна JSON-строка (json.Marshal не оставляет \n внутри результата,
// требование SSE «без многострочных data» выполняется автоматически).
func writeSSEEvent(w http.ResponseWriter, ev applybus.Event) error {
	masked := maskSSEPayload(ev.Payload)
	payloadJSON, err := json.Marshal(masked)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n",
		ev.Kind, ev.ApplyID, payloadJSON,
	); err != nil {
		return err
	}
	return nil
}

// maskSSEPayload приводит payload к masked-форме (H1). Payload приходит в
// двух формах:
//
//   - map[string]any — local-publish (events_taskevent.go / events_runresult.go);
//     маскируется напрямую через [audit.MaskSecrets].
//   - json.RawMessage / []byte — cross-Keeper cluster-bridge (applybus
//     forward); сначала декодируем в map[string]any, маскируем, потом
//     отдаём как map (json.Marshal в writeSSEEvent сериализует обратно).
//     Если payload — не-object JSON (массив/скаляр) или decode упал —
//     возвращаем исходное значение (маскировать нечего по ключам; vault-ref
//     в скаляре — крайне маловероятный edge для SSE-payload-контракта).
//
// Любой другой тип возвращается как есть (контракт payload-а — object).
func maskSSEPayload(payload any) any {
	switch p := payload.(type) {
	case nil:
		return nil
	case map[string]any:
		return audit.MaskSecrets(p)
	case json.RawMessage:
		return maskRawJSON(p)
	case []byte:
		return maskRawJSON(p)
	default:
		return payload
	}
}

// maskRawJSON декодирует raw-JSON-object, маскирует и возвращает map. На
// non-object JSON или ошибке декода возвращает исходный raw (caller
// сериализует его как есть).
func maskRawJSON(raw []byte) any {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return json.RawMessage(raw)
	}
	return audit.MaskSecrets(m)
}

// Payload-контракт SSE-канала — snake_case-ключи, единый набор для всех
// EventKind:
//
//   - apply.started     → {apply_id, kind, at}
//   - task.executed     → + {sid, task_idx, task_status, error?: {code,message,module}}
//   - apply.completed   → + {sid, run_status, state_changes?}
//   - apply.failed      → + {sid, run_status}
//   - apply.cancelled   → + {sid, run_status}
//
// Структура — `map[string]any` (а не typed-struct), потому что publisher-ы
// живут в пакете `keeper/internal/grpc` и тянуть typed-struct оттуда
// потребовало бы либо обратного import-а из mcp в grpc, либо третьего
// пакета. Контракт фиксируется в docs/keeper/mcp-tools.md (отдельный slice
// документации), а не Go-типом.
