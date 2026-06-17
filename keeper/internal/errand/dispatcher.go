package errand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// DefaultServerCap — sync-window timeout для `POST /v1/souls/{sid}/exec`
// (ADR-033 §3). Если ErrandResult не пришёл за это время, handler делает
// async-escalation (202 + Location), background-goroutine продолжает ждать
// до полного req.TimeoutSec → ErrandStatus.TIMED_OUT.
const DefaultServerCap = 30 * time.Second

// MaxTimeoutSeconds — server-cap полного timeout-а Errand-а (ADR-033 §3).
// Любое значение `timeout_seconds` выше этого клампируется до этого порога
// на validation-этапе.
const MaxTimeoutSeconds = 300

// MinTimeoutSeconds — нижняя граница timeout-а; ниже 1с теряет смысл (даже
// локальный shell-exec обычно занимает >100ms).
const MinTimeoutSeconds = 1

// DefaultTimeoutSeconds — дефолтный timeout, если оператор не указал явно
// (ADR-033 §3, default 30s).
const DefaultTimeoutSeconds = 30

// TTLDefault — срок жизни строки `errands` до purge_old_errands (ADR-033,
// reaper.md §purge_old_errands). 7 дней. Реализация purge — slice E4.
const TTLDefault = 7 * 24 * time.Hour

// Sentinel-ошибки Dispatcher-а. Caller (HTTP/MCP-handler) маппит в
// problem+json по типу.
var (
	// ErrSIDEmpty — пустой SID в DispatchRequest.
	ErrSIDEmpty = errors.New("errand: sid is empty")
	// ErrModuleEmpty — пустой module в DispatchRequest.
	ErrModuleEmpty = errors.New("errand: module is empty")
	// ErrTimeoutOutOfRange — timeout вне [1, 300].
	ErrTimeoutOutOfRange = errors.New("errand: timeout out of range")
	// ErrSoulNotConnected — Soul не подключён ни к одному keeper-инстансу
	// (пустой lease-holder + локально стрима нет). HTTP → 404 (логическая
	// «целевой Soul не доступен»), как у Outbound.
	ErrSoulNotConnected = errors.New("errand: soul not connected")
	// ErrErrandTerminal — попытка отменить Errand, который уже в терминальном
	// статусе (success/failed/timed_out/cancelled/module_not_allowed). HTTP →
	// 409 Conflict (slice E5).
	ErrErrandTerminal = errors.New("errand: cannot cancel terminal errand")
	// ErrEmptyErrandID — пустой errand_id в Cancel-запросе (slice E5).
	// HTTP-handler этот случай отсеивает раньше (path-param обязателен), но
	// sentinel остаётся для MCP/SDK-вызовов.
	ErrEmptyErrandID = errors.New("errand: errand_id is empty")
)

// Status-кинды applybus для Errand-семейства. Регистрируются на стороне
// applybus.EventKind (см. applybus/bus.go) — здесь только short-cut на
// строковые имена, чтобы dispatcher/handler не зависели от applybus-имён
// в обе стороны (любая правка имени видна в одном файле).
const (
	KindCompleted        = applybus.KindErrandCompleted
	KindFailed           = applybus.KindErrandFailed
	KindTimedOut         = applybus.KindErrandTimedOut
	KindCancelled        = applybus.KindErrandCancelled
	KindModuleNotAllowed = applybus.KindErrandModuleNotAllowed
)

// ResultEvent — payload, который публикуется в applybus после приёма
// ErrandResult Soul-side (events_errand.go) и читается Dispatcher-ом
// (wait-loop). JSON-tagged — cluster-bridge сериализует через Redis
// pub/sub envelope как json.RawMessage; local-bus передаёт `any`, и
// тот же тип распакуется без декодирования.
//
// Поля совпадают с proto ErrandResult (после mask+cap). Output —
// произвольная map (read-safe модули); для shell/exec — nil.
type ResultEvent struct {
	ErrandID        string         `json:"errand_id"`
	Status          Status         `json:"status"`
	ExitCode        *int32         `json:"exit_code,omitempty"`
	Stdout          string         `json:"stdout,omitempty"`
	Stderr          string         `json:"stderr,omitempty"`
	StdoutTruncated bool           `json:"stdout_truncated,omitempty"`
	StderrTruncated bool           `json:"stderr_truncated,omitempty"`
	DurationMs      *int64         `json:"duration_ms,omitempty"`
	ErrorMessage    string         `json:"error_message,omitempty"`
	Output          map[string]any `json:"output,omitempty"`
}

// DispatchRequest — вход [Dispatcher.Dispatch]. Поля валидируются Dispatcher-ом
// (caller-у не нужно дублировать). TimeoutSec=0 → DefaultTimeoutSeconds.
type DispatchRequest struct {
	SID          string
	Module       string
	Input        map[string]any
	TimeoutSec   int
	DryRun       bool
	StartedByAID string
}

// DispatchResult — выход [Dispatcher.Dispatch]. Async=true → caller отдаёт
// HTTP 202 + {errand_id} + Location-header (sync-cap превышен, результат
// продолжит писаться background-горутиной в БД и доступен через
// `GET /v1/errands/{errand_id}`).
type DispatchResult struct {
	ErrandID        string
	Status          Status
	ExitCode        *int32
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	DurationMs      *int64
	ErrorMessage    string
	Output          map[string]any
	Async           bool
	// StartedAt — момент INSERT-а errands-строки (тот же `now` из Clock(),
	// что записан в Row.StartedAt). Один источник времени для персистентной
	// строки и sync-200-ответа; caller (handler) проецирует его в wire
	// `started_at` вместо фабрикации time.Now().
	StartedAt time.Time
}

// OutboundSender — отправка Errand-сообщений в локальный EventStream Soul-а.
// Узкая поверхность keeper/internal/grpc.Outbound: dispatcher зависит только
// от методов, не от полного Outbound (тестируется fake-ом).
//
// Local-only: возвращает ErrSoulNotConnected при отсутствии локального
// стрима. Cluster-routing делает Publisher (см. ниже) отдельным вызовом —
// Dispatcher сам выбирает путь по holder-KID lease-а.
type OutboundSender interface {
	SendErrand(ctx context.Context, sid string, req *keeperv1.ErrandRequest) error
	// SendCancelErrand — slice E5: shipping CancelErrand сообщения local-стриму
	// SID-а. Возвращает ErrSoulNotConnected как и SendErrand.
	SendCancelErrand(ctx context.Context, sid, errandID string) error
}

// RemotePublisher — публикация FromKeeper в `outbound:<sid>` (cluster-mode).
// Используется dispatcher-ом, когда holder lease — НЕ наш KID (remote
// keeper). Никаких имён каналов в этой поверхности — внутренности routing-
// слоя инкапсулированы в реализации (keeper/internal/grpc::Outbound).
type RemotePublisher interface {
	PublishErrand(ctx context.Context, sid string, req *keeperv1.ErrandRequest) error
	// PublishCancelErrand — slice E5: публикация CancelErrand в outbound:<sid>
	// pub/sub-канал (cross-keeper).
	PublishCancelErrand(ctx context.Context, sid, errandID string) error
}

// LeaseLookup — резолв holder-KID lease-а на SID (`soul:<sid>:lock`).
// Возвращает "" если lease отсутствует (Soul не подключён ни к одному
// инстансу). Реализация — обёртка над redis.ReadSoulLeaseHolder.
type LeaseLookup interface {
	ReadHolder(ctx context.Context, sid string) (string, error)
}

// ApplyBus — узкая поверхность applybus.EventBus, нужная Dispatcher-у.
// Sub/Pub по applyID-семантике; в нашем случае applyID = errand_id
// (имя канала `apply:<id>` несёт opaque-id, переименование в `events:<id>`
// — отложенный TODO, см. applybus/bus.go doc-comment).
type ApplyBus interface {
	Subscribe(ctx context.Context, applyID string) <-chan applybus.Event
	// SubscribeWithBridge — как Subscribe, но с явным управлением Redis-bridge
	// (S1, applybus-bottleneck). wantBridge=false → local-only подписка без
	// per-applyID Redis-Subscribe; используется, когда lease-holder целевого
	// SID == self-KID (событие придёт от local publisher того же инстанса).
	SubscribeWithBridge(ctx context.Context, applyID string, wantBridge bool) <-chan applybus.Event
}

// AuditWriter — узкая поверхность shared/audit.Writer (симметрично pushorch).
type AuditWriter interface {
	Write(ctx context.Context, ev *audit.Event) error
}

// StoreAPI — узкая поверхность Store, нужная Dispatcher-у. Сужение
// (Insert/MarkTerminal/SweepOrphanRunning без read-методов List/Get) даёт
// (a) inject in-memory fake-а в unit-тестах без поднятия PG; (b) явный
// контракт «dispatcher = только write-path по errands». Реальный *Store
// удовлетворяет автоматически.
type StoreAPI interface {
	Insert(ctx context.Context, row Row) error
	MarkTerminal(ctx context.Context, id string, upd TerminalUpdate) (bool, error)
	SweepOrphanRunning(ctx context.Context, kid string, grace time.Duration, reason string) ([]string, error)
	// Get — slice E5: read-row для Cancel (lookup SID + проверка status='running').
	// ErrNotFound на отсутствие.
	Get(ctx context.Context, errandID string) (*Row, error)
}

// Deps — внешние зависимости Dispatcher-а. Все поля кроме Audit/Clock
// обязательны; nil-Audit → audit-events не пишутся (диагностика остаётся
// в логах). Clock nil → time.Now.
type Deps struct {
	Store       StoreAPI
	Outbound    OutboundSender
	Publisher   RemotePublisher
	LeaseLookup LeaseLookup
	ApplyBus    ApplyBus
	Logger      *slog.Logger
	Audit       AuditWriter
	Clock       func() time.Time
	ServerCap   time.Duration
	KID         string
}

// Dispatcher — синхронный orchestrator одного Errand-а. Одна Dispatch =
// один Errand. Concurrent-safe: state в БД + applybus pub/sub, никаких
// in-memory map-ов.
type Dispatcher struct {
	deps Deps
}

// NewDispatcher валидирует deps и возвращает dispatcher. Возврат ошибки —
// неконфигурация caller-а (wire-up).
func NewDispatcher(deps Deps) (*Dispatcher, error) {
	if deps.Store == nil {
		return nil, errors.New("errand: dispatcher Store is required")
	}
	if deps.Outbound == nil {
		return nil, errors.New("errand: dispatcher Outbound is required")
	}
	// Publisher / LeaseLookup опциональны: single-keeper-сборка без Redis
	// деградирует на local-only routing (holder неизвестен → пробуем local
	// Outbound, на отсутствии стрима — ErrSoulNotConnected).
	if deps.ApplyBus == nil {
		return nil, errors.New("errand: dispatcher ApplyBus is required")
	}
	if deps.Logger == nil {
		return nil, errors.New("errand: dispatcher Logger is required")
	}
	if deps.KID == "" {
		return nil, errors.New("errand: dispatcher KID is required")
	}
	if deps.ServerCap <= 0 {
		deps.ServerCap = DefaultServerCap
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	return &Dispatcher{deps: deps}, nil
}

// Dispatch — основной flow. См. doc-comment пакета (контур одного Errand-а).
//
// Шаги:
//  1. Validate (sid/module/timeout), clamp TimeoutSec в [Min, Max].
//  2. Generate errand_id (ULID).
//  3. INSERT row(status='running', started_by_kid=self).
//  4. Write audit `errand.invoked`.
//  5. Subscribe applybus(`apply:<errand_id>`) — ДО отправки, чтобы не
//     пропустить event при быстрой реакции Soul-а.
//  6. Resolve holder → SendErrand local либо Publish remote.
//  7. Wait sync до min(TimeoutSec, ServerCap):
//     - получен ResultEvent → MarkTerminal → return sync.
//     - timeout = TimeoutSec (≤ServerCap) → MarkTerminal(timed_out) →
//     return sync с status=TIMED_OUT.
//     - timeout = ServerCap (<TimeoutSec) → spawn background goroutine
//     (waitAsync) и return async=true с status=RUNNING.
func (d *Dispatcher) Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	if err := validateDispatch(&req); err != nil {
		return DispatchResult{}, err
	}

	errandID := audit.NewULID()
	now := d.deps.Clock().UTC()
	row := Row{
		ErrandID:     errandID,
		SID:          req.SID,
		Module:       req.Module,
		Input:        req.Input,
		Status:       StatusRunning,
		StartedByAID: req.StartedByAID,
		StartedByKID: d.deps.KID,
		StartedAt:    now,
		TTLAt:        now.Add(TTLDefault),
	}
	if err := d.deps.Store.Insert(ctx, row); err != nil {
		return DispatchResult{}, fmt.Errorf("errand: insert: %w", err)
	}

	d.writeInvoked(ctx, errandID, req)

	// Резолвим lease-holder ДО Subscribe (S1, applybus-bottleneck): если
	// holder == self-KID, событие придёт от local publisher того же инстанса
	// через local-bus → per-applyID Redis-bridge не нужен. wantBridge=false
	// снимает Redis-Subscribe для локально-подключённых Souls (частый случай),
	// устраняя maxclients-cliff. Консервативный дефолт wantBridge=true при
	// ошибке lookup-а или отсутствии LeaseLookup (holder неизвестен — bridge
	// как раньше). Потеря события при holder-flip ≠ зависание: in-process
	// wait-timer завершит Errand в timed_out (см. select на timer.C ниже).
	holder, lookupOK := d.resolveHolder(ctx, req.SID, errandID)
	// `holder != ""` избыточен при непустом KID (holder==KID уже непуст), но
	// оставлен защитой от misconfig-а с пустым KID: holder=="" && KID=="" не
	// должно трактоваться как self → bridge остаётся включён.
	wantBridge := !(lookupOK && holder == d.deps.KID && holder != "")

	// Subscribe ДО отправки: pub/sub late-subscribe теряет события (см.
	// applybus.EventBus doc-comment). Жизнь подписки — до завершения
	// sync-wait либо до окончания async-горутины.
	subCtx, subCancel := context.WithCancel(context.Background())
	events := d.deps.ApplyBus.SubscribeWithBridge(subCtx, errandID, wantBridge)

	if err := d.send(ctx, req.SID, errandID, req, holder, lookupOK); err != nil {
		subCancel()
		// Маркируем строку failed: Errand даже до Soul-а не доехал,
		// status=running остался бы орфаном до sweep-а.
		_, mErr := d.deps.Store.MarkTerminal(ctx, errandID, TerminalUpdate{
			Status:       StatusFailed,
			ErrorMessage: err.Error(),
		})
		if mErr != nil {
			d.deps.Logger.Warn("errand: mark terminal after send-fail failed",
				slog.String("errand_id", errandID),
				slog.String("sid", req.SID),
				slog.Any("error", mErr))
		}
		d.writeTerminal(ctx, errandID, req, ResultEvent{
			ErrandID:     errandID,
			Status:       StatusFailed,
			ErrorMessage: err.Error(),
		})
		return DispatchResult{
			ErrandID:     errandID,
			Status:       StatusFailed,
			ErrorMessage: err.Error(),
			StartedAt:    now,
		}, err
	}

	timeoutDur := time.Duration(req.TimeoutSec) * time.Second
	syncCap := d.deps.ServerCap
	syncWait := timeoutDur
	if syncWait > syncCap {
		syncWait = syncCap
	}

	startedAt := d.deps.Clock()
	timer := time.NewTimer(syncWait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		// Caller-ctx отменён (клиент разорвал соединение / shutdown HTTP-
		// сервера). Не блокируем background-goroutine — она дочитает event
		// или истечёт по своему таймеру; БД-строка останется running до
		// её работы. subscribe отдаётся goroutine-ом.
		go d.waitAsync(subCtx, subCancel, events, errandID, req, syncWait, timeoutDur)
		return DispatchResult{ErrandID: errandID, Status: StatusRunning, Async: true, StartedAt: now}, nil
	case ev := <-events:
		subCancel()
		res := d.applyResult(ctx, errandID, req, ev, time.Since(startedAt))
		res.StartedAt = now
		return res, nil
	case <-timer.C:
		// Timer-elapsed: либо sync дожали timeout (TimeoutSec ≤ ServerCap),
		// либо ServerCap наступил раньше TimeoutSec → async.
		if timeoutDur <= syncCap {
			// Sync-timeout = TimeoutSec → терминал timed_out.
			subCancel()
			res := d.markTimedOut(ctx, errandID, req, time.Since(startedAt))
			res.StartedAt = now
			return res, nil
		}
		// ServerCap < TimeoutSec → async-escalation. background-goroutine
		// продолжит читать events до полного TimeoutSec.
		remaining := timeoutDur - syncCap
		go d.waitAsync(subCtx, subCancel, events, errandID, req, syncCap, syncCap+remaining)
		return DispatchResult{ErrandID: errandID, Status: StatusRunning, Async: true, StartedAt: now}, nil
	}
}

// resolveHolder — best-effort чтение lease-holder-а SID для выбора пути
// доставки И bridge-решения (S1). Возвращает (holder, lookupOK):
//
//   - LeaseLookup nil / Publisher nil → ("", false): single-keeper local-only;
//     routing идёт через Outbound без holder-ветвления.
//   - ошибка lookup-а → ("", false): fallback на local (Outbound) +
//     консервативный bridge (wantBridge=true).
//   - успешный lookup → (holder, true): holder=="" означает реально пустой
//     lease (Soul не подключён ни к одному инстансу) → ErrSoulNotConnected
//     в send. holder=self → local, holder=other → remote.
//
// lookupOK различает «нет авторитетного ответа» (false → fallback на Outbound,
// как прежний lookup-error путь) и «авторитетный пустой lease» (true,
// holder=="" → NotConnected без попытки local). Результат используется дважды:
// для wantBridge в Dispatch и как уже-резолвленный holder в send (без второго
// ReadHolder — устранён двойной lookup).
func (d *Dispatcher) resolveHolder(ctx context.Context, sid, errandID string) (string, bool) {
	if d.deps.LeaseLookup == nil || d.deps.Publisher == nil {
		return "", false
	}
	holder, err := d.deps.LeaseLookup.ReadHolder(ctx, sid)
	if err != nil {
		d.deps.Logger.Warn("errand: lease lookup failed, fallback to local",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
			slog.Any("error", err))
		return "", false
	}
	return holder, true
}

// send выбирает путь доставки ErrandRequest: local (Outbound.SendErrand) либо
// remote (Publisher.PublishErrand) по уже-резолвленному holder-у lease-а
// (см. [Dispatcher.resolveHolder]). Двойной ReadHolder устранён — holder и
// lookupOK приходят параметрами.
//
// Алгоритм:
//   - !lookupOK (LeaseLookup/Publisher nil ИЛИ ошибка lookup-а) → local-only
//     fallback через Outbound. Стрима нет → ErrSoulNotConnected.
//   - lookupOK && holder == "" → авторитетно пустой lease → Soul не подключён
//     ни к одному инстансу → ErrSoulNotConnected.
//   - lookupOK && holder == self → Local.
//   - lookupOK && holder == other → Remote (publisher).
//
// Сам holder Outbound тоже проверяет внутри SendApply/SendCancel — это не
// race-free: holder мог смениться между resolveHolder и Send-ом, Outbound
// вернёт ErrSoulNotConnected, caller пробрасывает наверх (Errand fail).
func (d *Dispatcher) send(ctx context.Context, sid, errandID string, req DispatchRequest, holder string, lookupOK bool) error {
	pbReq, err := buildProtoRequest(errandID, req)
	if err != nil {
		return fmt.Errorf("errand: build proto: %w", err)
	}

	if !lookupOK {
		// Local-only / lookup-fallback: пробуем напрямую через Outbound. Если
		// стрима нет, Outbound вернёт ErrSoulNotConnected (мы его
		// переоборачиваем своим sentinel-ом, чтобы caller не зависел от
		// grpc-пакета).
		if err := d.deps.Outbound.SendErrand(ctx, sid, pbReq); err != nil {
			d.deps.Logger.Warn("errand: local-only send failed",
				slog.String("sid", sid),
				slog.String("errand_id", errandID),
				slog.Any("error", err))
			return ErrSoulNotConnected
		}
		return nil
	}

	if holder == "" {
		return ErrSoulNotConnected
	}
	if holder == d.deps.KID {
		if err := d.deps.Outbound.SendErrand(ctx, sid, pbReq); err != nil {
			d.deps.Logger.Warn("errand: local send (holder=self) failed",
				slog.String("sid", sid),
				slog.String("errand_id", errandID),
				slog.Any("error", err))
			return ErrSoulNotConnected
		}
		return nil
	}
	if err := d.deps.Publisher.PublishErrand(ctx, sid, pbReq); err != nil {
		d.deps.Logger.Warn("errand: remote publish failed",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
			slog.String("holder", holder),
			slog.Any("error", err))
		return ErrSoulNotConnected
	}
	return nil
}

// waitAsync — продолжение wait-loop-а в background-goroutine после
// sync-escalation. ctxBus — background-ctx подписки applybus; subCancel —
// отмена этой подписки.
//
// elapsedAtSpawn — сколько уже прошло на момент входа (для duration_ms
// при таймауте). totalTimeout — полный req.TimeoutSec в Duration.
func (d *Dispatcher) waitAsync(
	ctxBus context.Context, subCancel context.CancelFunc,
	events <-chan applybus.Event,
	errandID string, req DispatchRequest,
	elapsedAtSpawn, totalTimeout time.Duration,
) {
	defer subCancel()
	remaining := totalTimeout - elapsedAtSpawn
	if remaining <= 0 {
		// На границе: полный timeout уже истёк к моменту spawn-а. Сразу в TIMED_OUT.
		bg := context.Background()
		d.markTimedOut(bg, errandID, req, elapsedAtSpawn)
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	bg := context.Background()
	select {
	case ev, ok := <-events:
		if !ok {
			// Канал закрыт (подписка свернулась) — больше ждать нечего,
			// маркируем timed_out как defense-in-depth (running-строка
			// иначе зависнет до sweep-а).
			d.markTimedOut(bg, errandID, req, elapsedAtSpawn)
			return
		}
		d.applyResult(bg, errandID, req, ev, elapsedAtSpawn)
	case <-timer.C:
		d.markTimedOut(bg, errandID, req, totalTimeout)
	case <-ctxBus.Done():
		// bus-ctx закрыт извне (только subCancel — но мы его сами
		// откладываем). На всякий пожарный — log+return без MarkTerminal.
		d.deps.Logger.Debug("errand: async wait ctx cancelled (external)",
			slog.String("errand_id", errandID))
	}
}

// applyResult принимает applybus.Event, нормализует payload (local-typed
// либо cluster-RawMessage), и переводит errands-строку в терминал.
// Возвращает DispatchResult для sync-caller-а (async-вариант игнорирует
// возврат — фоновая горутина).
func (d *Dispatcher) applyResult(
	ctx context.Context,
	errandID string, req DispatchRequest,
	ev applybus.Event, elapsed time.Duration,
) DispatchResult {
	res, ok := decodeResultEvent(ev.Payload)
	if !ok {
		d.deps.Logger.Warn("errand: result event payload decode failed",
			slog.String("errand_id", errandID),
			slog.String("kind", string(ev.Kind)))
		// Failsafe: trans-ит в FAILED (вместо вечного running).
		res = ResultEvent{
			ErrandID:     errandID,
			Status:       StatusFailed,
			ErrorMessage: "errand: malformed result event payload",
		}
	}
	if res.ErrandID == "" {
		res.ErrandID = errandID
	}

	upd := TerminalUpdate{
		Status:          res.Status,
		ExitCode:        res.ExitCode,
		Stdout:          res.Stdout,
		Stderr:          res.Stderr,
		StdoutTruncated: res.StdoutTruncated,
		StderrTruncated: res.StderrTruncated,
		DurationMs:      res.DurationMs,
		ErrorMessage:    res.ErrorMessage,
		Output:          res.Output,
	}
	if upd.DurationMs == nil {
		ms := elapsed.Milliseconds()
		upd.DurationMs = &ms
	}

	changed, err := d.deps.Store.MarkTerminal(ctx, errandID, upd)
	if err != nil {
		d.deps.Logger.Error("errand: mark terminal failed",
			slog.String("errand_id", errandID),
			slog.Any("error", err))
	}
	if changed {
		d.writeTerminal(ctx, errandID, req, res)
	}

	return DispatchResult{
		ErrandID:        errandID,
		Status:          res.Status,
		ExitCode:        res.ExitCode,
		Stdout:          res.Stdout,
		Stderr:          res.Stderr,
		StdoutTruncated: res.StdoutTruncated,
		StderrTruncated: res.StderrTruncated,
		DurationMs:      upd.DurationMs,
		ErrorMessage:    res.ErrorMessage,
		Output:          res.Output,
		Async:           false,
	}
}

// markTimedOut — общий путь для sync-timeout и async-timeout.
func (d *Dispatcher) markTimedOut(ctx context.Context, errandID string, req DispatchRequest, elapsed time.Duration) DispatchResult {
	ms := elapsed.Milliseconds()
	upd := TerminalUpdate{
		Status:       StatusTimedOut,
		DurationMs:   &ms,
		ErrorMessage: fmt.Sprintf("errand timed out after %ds", req.TimeoutSec),
	}
	changed, err := d.deps.Store.MarkTerminal(ctx, errandID, upd)
	if err != nil {
		d.deps.Logger.Error("errand: mark timed_out failed",
			slog.String("errand_id", errandID),
			slog.Any("error", err))
	}
	if changed {
		d.writeTerminal(ctx, errandID, req, ResultEvent{
			ErrandID:     errandID,
			Status:       StatusTimedOut,
			DurationMs:   &ms,
			ErrorMessage: upd.ErrorMessage,
		})
	}
	return DispatchResult{
		ErrandID:     errandID,
		Status:       StatusTimedOut,
		DurationMs:   &ms,
		ErrorMessage: upd.ErrorMessage,
		Async:        false,
	}
}

// writeInvoked пишет audit-event `errand.invoked` (ADR-033, event_types.go).
// Payload — sid/module/errand_id/timeout/dry_run; `input` НЕ кладётся
// (может нести vault-резолвленные секреты).
func (d *Dispatcher) writeInvoked(ctx context.Context, errandID string, req DispatchRequest) {
	if d.deps.Audit == nil {
		return
	}
	ev := &audit.Event{
		EventType:     audit.EventTypeErrandInvoked,
		Source:        audit.SourceAPI,
		ArchonAID:     req.StartedByAID,
		CorrelationID: errandID,
		Payload: map[string]any{
			"sid":             req.SID,
			"module":          req.Module,
			"errand_id":       errandID,
			"timeout_seconds": req.TimeoutSec,
			"dry_run":         req.DryRun,
		},
	}
	if err := d.deps.Audit.Write(ctx, ev); err != nil {
		d.deps.Logger.Warn("errand: audit invoked failed",
			slog.String("errand_id", errandID),
			slog.Any("error", err))
	}
}

// writeTerminal пишет audit-event для терминала. Маппинг status →
// EventType:
//
//   - success                → errand.completed
//   - failed | module_not_allowed → errand.failed
//   - timed_out             → errand.timed_out
//   - cancelled             → errand.cancelled (slice E5)
//
// Source — soul_grpc (write-path происходит от соответствующего
// FromSoul.ErrandResult-handler-а; для send-fail / timed_out — Keeper-
// internal, но source=soul_grpc выбран для единого фильтра по событию).
//
// archon_aid в payload НЕ кладётся (соответствует контракту в
// event_types.go: payload без archon_aid, инициатор в colon-поле).
func (d *Dispatcher) writeTerminal(ctx context.Context, errandID string, req DispatchRequest, res ResultEvent) {
	if d.deps.Audit == nil {
		return
	}
	var eventType audit.EventType
	switch res.Status {
	case StatusSuccess:
		eventType = audit.EventTypeErrandCompleted
	case StatusTimedOut:
		eventType = audit.EventTypeErrandTimedOut
	case StatusCancelled:
		eventType = audit.EventTypeErrandCancelled
	default:
		// failed / module_not_allowed / неизвестное → errand.failed.
		eventType = audit.EventTypeErrandFailed
	}

	payload := map[string]any{
		"sid":       req.SID,
		"module":    req.Module,
		"errand_id": errandID,
	}
	if res.ExitCode != nil {
		payload["exit_code"] = *res.ExitCode
	}
	if res.DurationMs != nil {
		payload["duration_ms"] = *res.DurationMs
	}
	if res.StdoutTruncated {
		payload["stdout_truncated"] = true
	}
	if res.StderrTruncated {
		payload["stderr_truncated"] = true
	}
	if res.ErrorMessage != "" && eventType != audit.EventTypeErrandCompleted {
		payload["error_message"] = res.ErrorMessage
	}

	source := audit.SourceSoulGRPC
	// errand.cancelled инициирует архонт через API → source=api с aid.
	// В текущем slice E2 cancel ещё нет — событие пишут только из
	// дальнейшего slice E5; но если кто-то всё же приведёт сюда
	// status=cancelled, корректно отметим источник.
	var aid string
	if res.Status == StatusCancelled {
		source = audit.SourceAPI
		aid = req.StartedByAID
	}

	ev := &audit.Event{
		EventType:     eventType,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: errandID,
		Payload:       payload,
	}
	if err := d.deps.Audit.Write(ctx, ev); err != nil {
		d.deps.Logger.Warn("errand: audit terminal failed",
			slog.String("errand_id", errandID),
			slog.String("event_type", string(eventType)),
			slog.Any("error", err))
	}
}

// validateDispatch проверяет и normalize DispatchRequest in-place.
// TimeoutSec=0 → DefaultTimeoutSeconds; <Min / >Max → ErrTimeoutOutOfRange
// (caller отдаёт 422). Пустые поля → sentinel-ошибки.
func validateDispatch(req *DispatchRequest) error {
	if req.SID == "" {
		return ErrSIDEmpty
	}
	if req.Module == "" {
		return ErrModuleEmpty
	}
	if req.TimeoutSec == 0 {
		req.TimeoutSec = DefaultTimeoutSeconds
	}
	if req.TimeoutSec < MinTimeoutSeconds || req.TimeoutSec > MaxTimeoutSeconds {
		return ErrTimeoutOutOfRange
	}
	return nil
}

// decodeResultEvent распаковывает applybus.Event.Payload в ResultEvent.
// Поддерживает оба формата:
//
//   - local Publish: payload — это уже ResultEvent (или map[string]any,
//     если опубликован SoulGRPC-handler-ом через json.Marshal на cluster-
//     bridge → деградирует в map при перепаковке).
//   - cluster Subscribe: payload — json.RawMessage (см.
//     keeper/internal/redis/applybus.go::ApplyEvent.Payload). Unmarshal-им
//     напрямую в ResultEvent.
//
// При неизвестной форме возвращает ok=false → caller сам решает
// (failsafe в applyResult).
func decodeResultEvent(payload any) (ResultEvent, bool) {
	switch p := payload.(type) {
	case ResultEvent:
		return p, true
	case *ResultEvent:
		if p == nil {
			return ResultEvent{}, false
		}
		return *p, true
	case json.RawMessage:
		var out ResultEvent
		if err := json.Unmarshal(p, &out); err != nil {
			return ResultEvent{}, false
		}
		return out, true
	case []byte:
		var out ResultEvent
		if err := json.Unmarshal(p, &out); err != nil {
			return ResultEvent{}, false
		}
		return out, true
	case map[string]any:
		// fallback: payload пришёл как generic-map (cross-keeper-route
		// после json.Marshal/Unmarshal без typed-target-а). Сериализуем-
		// десериализуем как ResultEvent.
		b, err := json.Marshal(p)
		if err != nil {
			return ResultEvent{}, false
		}
		var out ResultEvent
		if err := json.Unmarshal(b, &out); err != nil {
			return ResultEvent{}, false
		}
		return out, true
	default:
		return ResultEvent{}, false
	}
}
