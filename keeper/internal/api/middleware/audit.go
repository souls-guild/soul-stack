package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// AuditPayloadBuilder — функция, которая собирает payload audit-event-а
// после успешного выполнения handler-а. Получает claims (через
// [ClaimsFromContext]), request и response-status, возвращает map для
// `Event.Payload`. Маскинг секретов делает audit.Writer-сторона
// (через MaskSecrets), здесь можно класть raw-значения.
//
// nil-возврат → пустой payload (event пишется, payload = `{}`).
//
// Через builder, а не через захват response-body, чтобы не дублировать
// streaming-логику ResponseRecorder-ов: handler-у дешевле положить
// нужные поля прямо в context перед next() или вернуть из локальной
// замыкаемой переменной.
type AuditPayloadBuilder func(r *http.Request, status int) map[string]any

// auditCtxKey — non-exported context-key для payload-overrides от handler-а.
type auditCtxKey struct{}

// AuditPayload — payload, который handler хочет приложить к audit-event-у.
// Используется через [SetAuditPayload] и сливается с тем, что вернёт
// builder в [Audit].
type AuditPayload map[string]any

// SetAuditPayload кладёт payload в context. Handler вызывает её после
// успешного выполнения; audit-middleware читает в after-handler-фазе.
//
// Идемпотентно перезаписывает: повторный вызов заменяет предыдущее
// значение целиком.
func SetAuditPayload(r *http.Request, payload AuditPayload) {
	ctx := context.WithValue(r.Context(), auditCtxKey{}, payload)
	*r = *r.WithContext(ctx)
}

// auditPayloadFromContext возвращает payload, положенный handler-ом.
// nil — handler не вызывал SetAuditPayload.
func auditPayloadFromContext(ctx context.Context) AuditPayload {
	p, _ := ctx.Value(auditCtxKey{}).(AuditPayload)
	return p
}

// HumaAuditCarrier — мутируемый носитель audit-payload для full-typed huma-роутов
// (ADR-054 §Audit, вариант B). huma-middleware исполняется СНАРУЖИ handler-closure,
// а huma.Context иммутабелен (huma.WithValue создаёт новый context), поэтому
// классический [SetAuditPayload] (он делает *r.WithContext на *http.Request) к huma
// неприменим. Вместо этого huma-audit-middleware seed-ит *HumaAuditCarrier в
// request-context ДО next; huma-handler кладёт payload через [SetHumaAuditPayload];
// middleware читает ТОТ ЖЕ указатель после next. Указатель общий → мутация видна.
type HumaAuditCarrier struct {
	Payload AuditPayload
}

// HumaAuditCarrierKey — context-key для [HumaAuditCarrier]. Экспортирован: huma-
// audit-middleware (пакет api) seed-ит carrier по нему, [SetHumaAuditPayload]
// (тот же пакет middleware) читает по нему.
type HumaAuditCarrierKey struct{}

// SetHumaAuditPayload кладёт audit-payload на huma-context (параллель
// [SetAuditPayload] для full-typed huma-роутов, ADR-054 §Audit). huma-handler
// вызывает её внутри своей closure; huma-audit-middleware seed-ит carrier ДО next
// и читает payload после. carrier отсутствует (роут без huma-audit-навески / прямой
// вызов handler-а) → no-op (payload не записывается, audit пишется без него).
//
// Идемпотентно перезаписывает: повторный вызов заменяет предыдущее значение.
func SetHumaAuditPayload(ctx context.Context, payload AuditPayload) {
	if c, ok := ctx.Value(HumaAuditCarrierKey{}).(*HumaAuditCarrier); ok {
		c.Payload = payload
	}
}

// sourceCtxKey — non-exported context-key для audit-source, прокинутого в
// REST-handler в обход HTTP-роутера (MCP-tool-ы вызывают handler in-memory
// через httptest, минуя Operator-API chain, где source = api по умолчанию).
type sourceCtxKey struct{}

// WithScenarioInvocationSource кладёт audit-source в context для REST-handler-ов,
// вызванных не из HTTP-роутера, а напрямую (MCP-tool через httptest). Handler
// читает значение через [ScenarioInvocationSource] и пишет его в audit-event
// вместо дефолтного [audit.SourceAPI].
//
// Симметрично [SetAuditPayload]: тот же in-handler-context-идиом для метаданных
// audit-а, только source задаётся ДО вызова handler-а (caller-сторона), а не
// внутри (handler-сторона).
func WithScenarioInvocationSource(ctx context.Context, source audit.Source) context.Context {
	return context.WithValue(ctx, sourceCtxKey{}, source)
}

// ScenarioInvocationSource возвращает audit-source из context. Fallback —
// [audit.SourceAPI]: обычный HTTP-запрос через Operator-API chain не кладёт
// ключ, и source остаётся `api` (поведение до правки сохранено). MCP-tool
// проставляет [audit.SourceMCP] через [WithScenarioInvocationSource].
func ScenarioInvocationSource(ctx context.Context) audit.Source {
	if s, ok := ctx.Value(sourceCtxKey{}).(audit.Source); ok && s.Valid() {
		return s
	}
	return audit.SourceAPI
}

// StatusRecorder — wrap для http.ResponseWriter, запоминающий статус,
// фактически записанный handler-ом. Нужен audit-middleware, чтобы знать
// success/failure после ServeHTTP без модификации handler-кода.
//
// Экспортирован, чтобы [bridgeMiddleware] strict-слоя (пакет api) мог обернуть
// входящий writer в ТОТ ЖЕ recorder, что читает Audit, и положить его в bridge-
// контекст (br.w) — иначе доменный handler, пишущий в br.w, минует recorder
// Audit-а, и rec.status остаётся 0 (регрессия S6: audit молча не пишется на
// strict-роутах). Конструктор [NewStatusRecorder] + ctx-проброс
// [WithStatusRecorder]/[StatusRecorderFromContext] делят ОДИН объект между
// bridge (создал, отдал доменному handler-у) и Audit (читает статус из ctx).
//
// Не буферизует body — это потенциально большие payload-ы (список
// инстансов, JWT-токены). Audit пишет только status + handler-overridden
// payload.
type StatusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// NewStatusRecorder оборачивает w. Идемпотентный WriteHeader (см. ниже)
// исключает двойной WriteHeader при общем использовании bridge+Audit.
func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w}
}

// Status возвращает зафиксированный статус (0 — ничего не записано: panic /
// заранний отказ до WriteHeader/Write).
func (s *StatusRecorder) Status() int { return s.status }

func (s *StatusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *StatusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		// stdlib делает implicit WriteHeader(200) перед первым Write.
		s.status = http.StatusOK
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// recorderCtxKey — non-exported context-key для общего [StatusRecorder],
// которым делятся bridge (strict-слой) и Audit.
type recorderCtxKey struct{}

// WithStatusRecorder кладёт recorder в context. Вызывает [bridgeMiddleware]
// strict-слоя: он оборачивает входящий writer в recorder, отдаёт ЭТОТ recorder
// доменному handler-у (через br.w) и кладёт его в ctx — Audit ниже по цепочке
// читает из ctx статус, фактически записанный доменным handler-ом.
func WithStatusRecorder(ctx context.Context, rec *StatusRecorder) context.Context {
	return context.WithValue(ctx, recorderCtxKey{}, rec)
}

// StatusRecorderFromContext возвращает recorder, положенный [WithStatusRecorder]
// (nil — ключа нет: не-strict-роут / прямой вызов handler-а). Audit при non-nil
// читает статус из НЕГО, а не из собственного rec (тот на strict-роуте остался
// бы 0 — доменный handler пишет в br.w=этот recorder, минуя обёртку Audit-а).
func StatusRecorderFromContext(ctx context.Context) *StatusRecorder {
	rec, _ := ctx.Value(recorderCtxKey{}).(*StatusRecorder)
	return rec
}

// Audit — middleware-фабрика, пишущая audit-event с eventType после
// успешного выполнения handler-а.
//
// Контракт:
//   - Должен быть после [RequireJWT] (нужен claims.Subject для archon_aid).
//   - Должен быть после [RequirePermission] (audit пишется только на
//     прошедших RBAC-проверку запросах; иначе writer завалит audit_log
//     событиями неавторизованных попыток — отдельный канал, post-MVP).
//   - source = api (rbac.md → § Применение). archon_aid = claims.Subject.
//
// builder вызывается только при «успешном» статусе (2xx); 4xx/5xx
// пропускаются (RBAC-deny / validation-error / internal-error пишутся в
// логи и проблем-response, не в audit). При status≥300 событие не
// записывается.
//
// Ошибка writer.Write логируется через logger и НЕ влияет на response
// (response уже улетел клиенту — слишком поздно). Audit-pipeline
// best-effort на этом уровне (durability обеспечивает PG-COMMIT внутри
// auditpg.Writer).
func Audit(writer audit.Writer, eventType audit.EventType, builder AuditPayloadBuilder, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// На strict-роутах bridgeMiddleware УЖЕ обернул writer в общий
			// StatusRecorder, положил его в br.w (доменный handler пишет в него)
			// и в ctx. Тогда читаем статус из НЕГО — собственная обёртка увидела
			// бы 0 (handler пишет мимо неё в br.w). На не-strict-роутах recorder в
			// ctx нет — оборачиваем сами (legacy-путь сохранён).
			rec := StatusRecorderFromContext(r.Context())
			if rec == nil {
				rec = NewStatusRecorder(w)
			}
			next.ServeHTTP(rec, r)

			if rec.status >= 300 || rec.status == 0 {
				// 0 = handler ничего не записал (panic, заранне отказ).
				// 4xx/5xx — операция не выполнена, в audit_log не пишем.
				return
			}

			claims, ok := ClaimsFromContext(r.Context())
			if !ok {
				logger.Warn("audit middleware: missing claims in context",
					slog.String("path", r.URL.Path),
				)
				return
			}

			payload := mergeAuditPayload(builder, r, rec.status, auditPayloadFromContext(r.Context()))

			ev := &audit.Event{
				EventType: eventType,
				Source:    audit.SourceAPI,
				ArchonAID: claims.Subject,
				Payload:   payload,
			}
			// Использовать r.Context() напрямую опасно: HTTP-сервер
			// может отменить его сразу после write-response (клиент
			// разорвал соединение). Audit не должен теряться по этой
			// причине → используем Background. Trade-off: при shutdown
			// 10s-grace не покроет audit-write дольше grace-а; для
			// MVP-однотранзакционного INSERT-а это норма.
			if err := writer.Write(context.Background(), ev); err != nil {
				logger.Error("audit middleware: write failed",
					slog.String("event_type", string(eventType)),
					slog.String("archon_aid", claims.Subject),
					slog.Any("error", err),
				)
			}
		})
	}
}

// mergeAuditPayload сливает payload из builder-а и payload-override-а
// из context-а handler-а (handler-overrides выигрывают). nil/nil → nil.
func mergeAuditPayload(builder AuditPayloadBuilder, r *http.Request, status int, override AuditPayload) map[string]any {
	var base map[string]any
	if builder != nil {
		base = builder(r, status)
	}
	if override == nil {
		return base
	}
	if base == nil {
		return map[string]any(override)
	}
	for k, v := range override {
		base[k] = v
	}
	return base
}
