package herald

// DeliveryWorker — claim-queue worker реальной webhook-доставки (ADR-052(d),
// S3). at-least-once: клеймит job из Redis-очереди, резолвит Herald-канал,
// делает SSRF-guarded webhook-POST, при сбое — retry с backoff; на терминале —
// audit `herald.delivered`/`herald.failed`.
//
// Конкурентные worker-ы (несколько на инстанс + N инстансов) безопасны: claim
// атомарен (BRPOPLPUSH), дубль доставки приемлем (at-least-once). Lease-ключ +
// mini-reaper ([RequeueExpired]) возвращают осиротевшие после крэша job-ы.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/netguard"
)

// defaultRetryBackoff — задержки перед повторной доставкой по номеру попытки
// (ADR-052(d): «retry с backoff»). Длина среза = число попыток ПОСЛЕ первой
// (всего 1 + len = 4 попытки). Подбор: первый retry быстрый (мигнувший endpoint
// чинится за секунды), дальше экспоненциально-растущий хвост покрывает
// кратковременный downtime приёмника (рестарт/деплой) без шторма попыток.
//
//	attempt 0 → сразу (первая доставка)
//	attempt 1 → +5s
//	attempt 2 → +30s
//	attempt 3 → +2m   (последняя попытка; после — терминальный fail)
var defaultRetryBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

// claimBlockTimeout — таймаут блокирующего claim-а (BRPOPLPUSH). По истечении
// worker проверяет ctx.Done и заходит на новый claim. Конечный (не 0), чтобы
// реагировать на shutdown даже при пустой очереди.
const claimBlockTimeout = 2 * time.Second

// leaseTTL — TTL lease-ключа claimed-job-а. Должен покрывать самую долгую
// доставку (delivery-timeout + backoff sleep current attempt) с запасом, иначе
// mini-reaper заберёт job, который ещё обрабатывается (→ дубль). Берётся как
// максимальный backoff + щедрый запас на сам HTTP-POST.
const leaseTTL = 5 * time.Minute

// leaseRenewInterval — период продления lease-ключа, пока job обрабатывается
// (sleep backoff + POST могут длиться дольше leaseTTL не должны → продлеваем).
const leaseRenewInterval = leaseTTL / 3

// DeliveryWorker — один claim-loop. В daemon поднимается несколько на инстанс.
type DeliveryWorker struct {
	Queue    queueBackend
	Heralds  HeraldReader
	KV       KVReader
	Audit    audit.Writer
	Logger   *slog.Logger
	Metrics  *DeliveryMetrics
	Resolver netguard.Resolver
	// Timeout — общий таймаут одного webhook-POST-а (0 → DefaultDeliveryTimeout).
	Timeout time.Duration
	// Backoff — задержки между попытками (nil → defaultRetryBackoff). Инжектируем
	// ради тестов (быстрые задержки) и будущей конфигурации; len определяет число
	// повторов, всего попыток = 1 + len(Backoff).
	Backoff []time.Duration
}

// backoff возвращает эффективный backoff-срез (nil → дефолт).
func (w *DeliveryWorker) backoff() []time.Duration {
	if w.Backoff == nil {
		return defaultRetryBackoff
	}
	return w.Backoff
}

// retryMax — максимальное число попыток (1 первая + len(backoff) повторов).
// attempt+1 >= retryMax → терминальный fail без перепостановки.
func (w *DeliveryWorker) retryMax() int {
	return 1 + len(w.backoff())
}

func (w *DeliveryWorker) validate() error {
	if w.Queue == nil {
		return errors.New("herald: DeliveryWorker.Queue is required")
	}
	if w.Heralds == nil {
		return errors.New("herald: DeliveryWorker.Heralds is required")
	}
	if w.Logger == nil {
		return errors.New("herald: DeliveryWorker.Logger is required")
	}
	if w.Resolver == nil {
		w.Resolver = netguard.DefaultResolver
	}
	return nil
}

// Run крутит claim-loop до отмены ctx. Возврат на ctx.Done без ошибки —
// graceful-shutdown. invalid-config → error (программная ошибка wire-up-а).
func (w *DeliveryWorker) Run(ctx context.Context) error {
	if err := w.validate(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		claimed, err := w.Queue.Claim(ctx, claimBlockTimeout)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			w.Logger.Warn("herald: claim failed", slog.Any("error", err))
			if !sleepCtx(ctx, time.Second) {
				return nil
			}
			continue
		}
		if claimed == nil {
			continue // пустая очередь — повторный claim
		}
		w.handle(ctx, claimed.Payload)
	}
}

// handle обрабатывает один claimed job: разбор, lease-renewal, доставка, retry/
// терминал. Битый payload — дроп (Ack без перепостановки): положить мог только
// marshalJob, аномалию не зацикливаем.
func (w *DeliveryWorker) handle(ctx context.Context, payload []byte) {
	job, err := unmarshalJob(payload)
	if err != nil {
		w.Logger.Warn("herald: dropping unparsable delivery job", slog.Any("error", err))
		_ = w.Queue.Ack(ctx, "", payload)
		return
	}

	// lease-renewal на время обработки job-а (backoff sleep + POST). Останавливаем
	// renew ДО терминала: stopRenew отменяет renewCtx и ДОЖИДАЕТСЯ выхода горутины,
	// чтобы in-flight SetLease не пере-создал lease-ключ ПОСЛЕ Ack-DEL (stray-ключ
	// до истечения TTL). Cancel без синхронизации этого не гарантирует — горутина
	// могла уже пройти select и войти в SetLease.
	renewCtx, cancelRenew := context.WithCancel(ctx)
	if err := w.Queue.SetLease(renewCtx, job.ID, leaseTTL); err != nil {
		w.Logger.Warn("herald: set lease failed", slog.String("job_id", job.ID), slog.Any("error", err))
	}
	renewDone := make(chan struct{})
	go w.renewLease(renewCtx, job.ID, renewDone)
	stopRenew := func() {
		cancelRenew()
		<-renewDone
	}
	defer stopRenew()

	// Backoff перед попыткой attempt>0 (requeue положил job сразу, задержку держим
	// тут, чтобы не плодить delay-очередей). Прерываемо по ctx.
	bo := w.backoff()
	if job.Attempt > 0 && job.Attempt-1 < len(bo) {
		if !sleepCtx(ctx, bo[job.Attempt-1]) {
			return // shutdown — job останется в processing, mini-reaper вернёт
		}
	}

	w.Metrics.observeAttempt(job.Herald)
	statusCode, derr := w.deliver(ctx, job)
	if derr == nil {
		stopRenew() // renew стоит ДО Ack-DEL — иначе stray lease-ключ
		w.terminalDelivered(ctx, job, statusCode, payload)
		return
	}

	// Сбой доставки. Терминал без retry: устойчивая ошибка (канал снесён/выключен,
	// SSRF-guard отверг URL, битый payload) — повтор не поможет. Иначе retry с
	// backoff, пока не исчерпан retryMax.
	if isTerminalNoRetry(derr) || job.Attempt+1 >= w.retryMax() {
		stopRenew() // renew стоит ДО Ack-DEL — иначе stray lease-ключ
		w.terminalFailed(ctx, job, derr, payload)
		return
	}
	w.requeue(ctx, job, payload, derr)
}

// deliver доставляет один job. Возврат (statusCode, nil) на 2xx; иначе ошибка
// (SSRF-guard / транспорт / non-2xx) — caller решает retry/терминал.
//
// Двухклассовая модель (ADR-052 amendment): резолв канала + Enabled-проверка +
// ЕДИНЫЙ SSRF-guard (validateDeliveryEndpoint + guardedDeliveryClient) — общие
// для ВСЕХ HTTP-типов; per-type request-builder ([HeraldTransport]) строит URL/
// тело/заголовки/подпись. Новый HTTP-тип НЕ может обойти guard — deliver() зовёт
// его сам, а не транспорт. Классификация non-2xx (isTerminalStatus) — общая.
func (w *DeliveryWorker) deliver(ctx context.Context, job *DeliveryJob) (int, error) {
	h, err := w.Heralds.HeraldByName(ctx, job.Herald)
	if err != nil {
		if errors.Is(err, ErrHeraldNotFound) {
			// Канал снесён между постановкой и доставкой — терминальный fail
			// (ретраить некуда). Возвращаем ошибку без retry-смысла; handle
			// исчерпает попытки/упрётся в not-found на каждой — форсируем fail
			// через спец-ошибку.
			return 0, fmt.Errorf("%w", errTerminalNoRetry{err})
		}
		return 0, fmt.Errorf("herald: resolve channel: %w", err)
	}
	if !h.Enabled {
		// Канал выключен — не доставляем (терминал, не retry).
		return 0, errTerminalNoRetry{fmt.Errorf("herald: channel %q disabled", h.Name)}
	}

	tr, ok := transportFor(h.Type)
	if !ok {
		// Тип без HTTP-транспорта (неизвестный / не-HTTP-класс без своей ветки) —
		// доставлять нечем, устойчивая ошибка: терминал без retry.
		return 0, errTerminalNoRetry{fmt.Errorf("herald: channel %q type %q has no transport", h.Name, h.Type)}
	}

	dr, err := tr.BuildRequest(ctx, h, job, w.KV)
	if err != nil {
		// Транспорт сам классифицировал ошибку (terminal-no-retry для битого
		// config / Vault-сбой transient) — пробрасываем как есть.
		return 0, err
	}

	// SSRF-валидация URL ПЕРЕД запросом (config мог измениться после create) —
	// ЕДИНАЯ точка для всех HTTP-типов.
	if err := validateDeliveryEndpoint(dr.req.URL.String(), dr.httpAllowed, dr.allowPrivate); err != nil {
		// Guard отверг — это устойчивая ошибка конфигурации, ретраить
		// бессмысленно: терминальный fail.
		return 0, errTerminalNoRetry{err}
	}

	client := guardedDeliveryClient(w.Resolver, dr.allowPrivate, w.Timeout)
	resp, err := client.Do(dr.req)
	if err != nil {
		return 0, fmt.Errorf("herald: delivery POST: %w", err)
	}
	defer resp.Body.Close()
	// Дренируем тело (с лимитом) — переиспользование keep-alive-соединения.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("herald: delivery returned status %d", resp.StatusCode)
		if isTerminalStatus(resp.StatusCode) {
			// 4xx (кроме 408/429) — устойчивая клиентская ошибка (auth/route/
			// payload): повтор не поможет, терминал без retry.
			return resp.StatusCode, errTerminalNoRetry{err}
		}
		// 408/429/5xx — транзиентно (rate-limit / перегрузка / рестарт приёмника):
		// retry с backoff.
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

// isTerminalStatus сообщает, что HTTP-статус — устойчивая ошибка без смысла в
// retry: любой 4xx, КРОМЕ 408 Request Timeout и 429 Too Many Requests (оба —
// транзиентные и ретраятся вместе с 5xx/таймаутами/transport-сбоями).
func isTerminalStatus(code int) bool {
	if code < 400 || code >= 500 {
		return false
	}
	return code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

// buildPayload собирает JSON-тело webhook-POST-а. Формат (зафиксирован,
// ADR-052(d)/(h)/(i)):
//
//	{
//	  "event_type":   "<area>.<action>",
//	  "occurred_at":  "<RFC3339>",
//	  "herald":       "<channel-name>",
//	  "tiding":       "<rule-name>",
//	  "payload":      { ... },  // payload audit-события (опц. суженный projection-ом)
//	  "annotations":  { ... }   // опц. статика оператора (опускается при пустых)
//	}
//
// БЕЗ обогащения секретами: payload — копия уже-замаскированного audit-payload-а
// (инвариант A ADR-027); прогоняем MaskSecrets ещё раз перед отправкой (defence
// in depth: даже если job сконструирован минуя audit-маскинг, наружу секрет не
// уйдёт).
//
// Порядок (ADR-052(h)): MaskSecrets → projection → annotations. projection
// применяется к УЖЕ-замаскированному payload-у (allow-list не может вытащить
// поле, которого после маскинга нет — секрет-гигиена сохранена). annotations —
// статика оператора, мержится отдельным верхнеуровневым ключом (НЕ в payload).
// Подпись (если задан signingKey) считается caller-ом ([deliver]) от ЭТОГО
// финального тела — после projection+annotations.
func buildPayload(job *DeliveryJob) ([]byte, error) {
	payload := audit.MaskSecrets(job.PayloadCopy)
	if len(job.Projection) > 0 {
		payload = projectPayload(payload, job.Projection)
	}
	out := webhookPayload{
		EventType:   string(job.EventType),
		OccurredAt:  job.OccurredAt.UTC().Format(time.RFC3339),
		Herald:      job.Herald,
		Tiding:      job.Tiding,
		Payload:     payload,
		Annotations: job.Annotations,
	}
	return marshalWebhookPayload(out)
}

// projectPayload строит подмножество src по allow-list путей projection
// (ADR-052(h)). Путь — точечная нотация (`summary.succeeded`, `voyage_id`);
// синтаксис уже провалидирован на CRUD ([ValidateProjection]) — здесь только
// резолв против фактической payload-формы.
//
// Форма результата — ВЛОЖЕННАЯ: путь `summary.succeeded` → {"summary":
// {"succeeded": N}}. Так приёмник парсит спроецированный payload тем же кодом,
// что и полную форму (тот же ключевой контракт, лишь суженный набор полей);
// плоская форма ("summary.succeeded": N) ввела бы второй несовместимый формат
// одного и того же поля и ломала бы приёмник при переключении правила
// projection↔полный.
//
// Отсутствующий путь — ПРОПУСК (не ошибка, не null): оператор подписался на
// поле, которого в этом конкретном событии нет — это норма (каталог payload-форм
// разнороден по EventType). При полном промахе всех путей вернётся пустой объект.
func projectPayload(src map[string]any, paths []string) map[string]any {
	out := map[string]any{}
	for _, path := range paths {
		val, ok := resolvePath(src, strings.Split(path, "."))
		if !ok {
			continue // путь отсутствует в этом payload-е — пропуск
		}
		// deep-copy листа ПЕРЕД вставкой: иначе при коллизии префиксов
		// (`summary` + `summary.failed`) широкий путь положил бы в out ссылку на
		// вложенную map из src, а последующая глубокая вставка домутировала бы
		// её — то есть исказила бы исходный src. buildPayload зовётся повторно на
		// каждый retry-attempt над тем же job.PayloadCopy, поэтому src обязан
		// оставаться неизменным: projectPayload(src, paths) НЕ мутирует src ни
		// при каком наборе paths.
		insertPath(out, strings.Split(path, "."), deepCopyValue(val))
	}
	return out
}

// deepCopyValue рекурсивно копирует значение, попадающее в проекцию, чтобы out
// не делил с src ни одной вложенной map/slice. Scalar-листья (string/число/bool/
// nil) иммутабельны — возвращаются как есть. Покрывает только формы, реально
// встречающиеся в audit-payload-е (вложенные map[string]any и []any); прочие
// типы листьев копируются по значению.
func deepCopyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, el := range x {
			out[k] = deepCopyValue(el)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = deepCopyValue(el)
		}
		return out
	default:
		return v
	}
}

// resolvePath спускается по segments в m (сегмент за сегментом). ok=false —
// промежуточный сегмент отсутствует или не объект (лист достигнут раньше путь
// глубже). Резолвит только через map[string]any: проекция в элементы массива не
// поддерживается (синтаксис projection — только `[a-z0-9_]`-сегменты, индексов
// нет, [ValidateProjection]).
func resolvePath(m map[string]any, segments []string) (any, bool) {
	cur := any(m)
	for _, seg := range segments {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// insertPath восстанавливает вложенную структуру для segments в dst, кладя val в
// лист. Промежуточные объекты создаются по мере необходимости; коллизия (на пути
// уже лежит лист от более короткого projection-пути) разрешается в пользу более
// глубокой вставки — projection-пути не должны быть префиксами друг друга на
// практике, но детерминизм гарантируем.
func insertPath(dst map[string]any, segments []string, val any) {
	for i := 0; i < len(segments)-1; i++ {
		seg := segments[i]
		next, ok := dst[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			dst[seg] = next
		}
		dst = next
	}
	dst[segments[len(segments)-1]] = val
}

// renewLease продлевает lease-ключ job-а, пока renewCtx жив (handle обрабатывает
// job). Best-effort: ошибка продления логируется (debug), не валит доставку —
// худший исход — mini-reaper заберёт ещё-живой job и доставит повторно
// (at-least-once это допускает). close(done) на выходе — sync-точка для handle:
// гарантирует, что после возврата ни один SetLease уже не выполняется (иначе
// stray lease-ключ после терминального Ack-DEL).
func (w *DeliveryWorker) renewLease(ctx context.Context, jobID string, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(leaseRenewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Queue.SetLease(ctx, jobID, leaseTTL); err != nil {
				if w.Logger != nil {
					w.Logger.Debug("herald: lease renew failed", slog.String("job_id", jobID), slog.Any("error", err))
				}
			}
		}
	}
}

// requeue возвращает job на повтор с инкрементом attempt. backoff применяется
// при следующем claim-е (handle спит перед попыткой attempt>0).
func (w *DeliveryWorker) requeue(ctx context.Context, job *DeliveryJob, oldPayload []byte, cause error) {
	next := *job
	next.Attempt = job.Attempt + 1
	newPayload, err := marshalJob(&next)
	if err != nil {
		// Маршалинг не должен падать (job уже разобран) — на всякий fail-терминал.
		w.terminalFailed(ctx, job, fmt.Errorf("requeue marshal: %w", err), oldPayload)
		return
	}
	if err := w.Queue.Requeue(ctx, job.ID, oldPayload, newPayload); err != nil {
		w.Logger.Warn("herald: requeue failed", slog.String("job_id", job.ID), slog.Any("error", err))
		return
	}
	w.Metrics.observeRetry(job.Herald)
	w.Logger.Info("herald: delivery failed, scheduled retry",
		slog.String("herald", job.Herald), slog.String("tiding", job.Tiding),
		slog.Int("attempt", next.Attempt), slog.String("error", maskErr(cause)))
}

// terminalDelivered: успешная доставка — Ack + audit herald.delivered + метрика.
func (w *DeliveryWorker) terminalDelivered(ctx context.Context, job *DeliveryJob, statusCode int, payload []byte) {
	_ = w.Queue.Ack(ctx, job.ID, payload)
	w.Metrics.observeSucceeded(job.Herald)
	w.emitAudit(audit.EventHeraldDelivered, job, map[string]any{
		"herald":      job.Herald,
		"tiding":      job.Tiding,
		"event_type":  string(job.EventType),
		"attempt":     job.Attempt,
		"status_code": statusCode,
	})
	w.Logger.Info("herald: notification delivered",
		slog.String("herald", job.Herald), slog.String("tiding", job.Tiding),
		slog.Int("attempt", job.Attempt), slog.Int("status_code", statusCode))
}

// terminalFailed: исчерпан retry / no-retry-ошибка — Ack + audit herald.failed.
func (w *DeliveryWorker) terminalFailed(ctx context.Context, job *DeliveryJob, cause error, payload []byte) {
	_ = w.Queue.Ack(ctx, job.ID, payload)
	w.Metrics.observeFailed(job.Herald)
	w.emitAudit(audit.EventHeraldFailed, job, map[string]any{
		"herald":        job.Herald,
		"tiding":        job.Tiding,
		"event_type":    string(job.EventType),
		"attempt":       job.Attempt,
		"error_message": maskErr(cause),
	})
	w.Logger.Warn("herald: notification delivery failed terminally",
		slog.String("herald", job.Herald), slog.String("tiding", job.Tiding),
		slog.Int("attempt", job.Attempt), slog.String("error", maskErr(cause)))
}

// emitAudit пишет терминальное событие доставки. Background-ctx: эмит вне
// claim-ctx (тот мог отмениться при shutdown). source=keeper_internal,
// archon_aid="" (NULL), correlation_id = correlation_id события прогона.
func (w *DeliveryWorker) emitAudit(et audit.EventType, job *DeliveryJob, payload map[string]any) {
	if w.Audit == nil {
		return
	}
	ev := &audit.Event{
		EventType:     et,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: job.CorrelationID,
		Payload:       payload,
	}
	if err := w.Audit.Write(context.Background(), ev); err != nil {
		w.Logger.Warn("herald: terminal audit write failed",
			slog.String("event_type", string(et)), slog.Any("error", err))
	}
}

// errTerminalNoRetry оборачивает ошибку, на которой retry бессмыслен (устойчивая
// конфигурация/состояние: канал снесён/выключен, SSRF-guard отверг URL, битый
// payload). handle распознаёт её и форсирует терминальный fail без повторов.
type errTerminalNoRetry struct{ err error }

func (e errTerminalNoRetry) Error() string { return e.err.Error() }
func (e errTerminalNoRetry) Unwrap() error { return e.err }

func isTerminalNoRetry(err error) bool {
	var t errTerminalNoRetry
	return errors.As(err, &t)
}

// maskErr — текст ошибки, прогнанный через MaskSecrets (cause может транзитом
// нести vault-ref в сообщении). audit.MaskSecrets работает с payload-map —
// заворачиваем строку в map и достаём обратно.
func maskErr(err error) string {
	if err == nil {
		return ""
	}
	masked := audit.MaskSecrets(map[string]any{"e": err.Error()})
	if s, ok := masked["e"].(string); ok {
		return s
	}
	return "<masked>"
}

// sleepCtx ждёт d или ctx.Done. false → вышли по ctx.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
