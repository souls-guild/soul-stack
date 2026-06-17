package config

// Store[T] — снимок типизированного конфига (`KeeperConfig` или `SoulConfig`)
// с atomic-swap-семантикой под [ADR-021](docs/architecture.md) (b) и (c):
//
//   - (b) hot-reload по SIGHUP: см. [shared/config/sighup.go];
//   - (c) validation pipeline + atomic swap: на любую error-диагностику
//     `Reload` НЕ выполняет swap, прошлый снимок остаётся актуальным.
//
// Контракт чтения:
//
//   - `Get()` возвращает `*T`. Указатель immutable, caller НЕ ДОЛЖЕН
//     мутировать поля возвращённой структуры. Для мутации с последующей
//     записью на диск использовать `Document()` + соответствующие
//     `Patch*`/`Save*` свободные функции пакета.
//   - Множественные конкурентные `Get()` безопасны: значение хранится в
//     `atomic.Pointer[T]`, read-side wait-free.
//
// Контракт перезагрузки:
//
//   - `Reload(ctx, source)` читает файл (`ReadFile`), парсит через
//     соответствующий `Load*FromBytes`, и при отсутствии error-диагностик
//     атомарно подменяет указатели снимка и `Document`. Source — значение
//     `ReloadSource` (см. константы `ReloadSourceSignal`/`API`/`MCP`);
//     попадает в audit-payload.
//   - На I/O-fatal — `ReloadResult.Swapped=false`,
//     `Phase = diag.PhaseParse`, в `Diagnostics` одна запись `io_error`.
//   - На validation-error — `Swapped=false`, `Phase` = первая error-фаза
//     из диагностик, `Diagnostics` содержит все собранные записи.
//   - На success — `Swapped=true`, `Phase=""`, `Diagnostics` — warnings (если есть).
//
// CorrelationID:
//
//   - 26-символьный ULID (Crockford base32, sortable timestamp prefix).
//     Формат нормирован [ADR-022(c)](docs/architecture.md); используется
//     одна реализация — `shared/audit.NewULID()` — чтобы avoid drift
//     между `config.reload_*` событиями и остальным audit-pipeline-ом.
//   - Каждый `Reload` генерирует свой ID; передаётся audit-пайплайну как
//     ключ корреляции запроса reload-а и его результата.
//
// ChangedPaths:
//
//   - Пустой slice в M0.3; вычисление diff source↔source отложено до M0.3.5.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// tracer для in-process span-а hot-reload-а конфига (ADR-024). Берёт
// глобальный TracerProvider, поднятый [obs.SetupOTel] в cmd/keeper и
// cmd/soul; при OTel disabled провайдер no-op — span бесплатен, код не
// ветвится (симметрично keeper/render, keeper/scenario).
var tracer = otel.Tracer("shared/config")

// storeKind — внутренний дискриминатор для выбора Load-функции при Reload.
// Внешним пакетам не нужен: конкретный тип хранилища определяется через
// конструкторы `LoadKeeperStore` / `LoadSoulStore`.
type storeKind int

const (
	storeKindKeeper storeKind = iota + 1
	storeKindSoul
)

// ReloadSource — closed enum инициаторов reload-а. Type alias на
// [audit.Source] (ADR-022(b)) — единый источник правды для source enum
// audit-pipeline-а; caller-ы hot-reload получают тот же набор констант,
// что и любой другой write-path-инициатор.
//
// Cast произвольной строки (`ReloadSource("hax0r")`) технически возможен —
// инвариант валидируется write-path-инициатором перед audit-INSERT
// (см. [audit.Source.Valid]).
type ReloadSource = audit.Source

const (
	ReloadSourceSignal = audit.SourceSignal
	ReloadSourceAPI    = audit.SourceAPI
	ReloadSourceMCP    = audit.SourceMCP
)

// ReloadCallback — opt-in подписчик на успешные swap-ы [Store.Reload].
// Вызывается **только** при `ReloadResult.Swapped=true`; на validation-fail /
// I/O-fatal subscriber-ы не получают уведомлений (старый снимок остаётся
// актуальным, и реагировать не на что).
//
// Контракт аргументов:
//
//   - `old` — указатель на снимок ДО swap-а. Может быть `nil` для
//     случая, когда initial load свалился на validation-error, а
//     первый успешный Reload подменил `nil → *T`.
//   - `new` — указатель на снимок ПОСЛЕ swap-а. Всегда non-nil
//     (callback вызывается только при Swapped=true).
//
// Оба указателя — snapshot-значения с atomic-swap-семантикой [Store]: их
// **запрещено мутировать** (см. doc-comment [Store.Get]).
//
// Subscriber запускается в **отдельной goroutine** per-callback (защита от
// блокировки одного subscriber-а другими). **Порядок вызова не
// гарантируется** — subscribers НЕ должны зависеть от order. Panic внутри
// callback-а ловится `recover` + slog.Error и не валит другие callback-и.
type ReloadCallback[T any] func(old, new *T)

// Store — типизированный конфигурационный снимок с atomic-swap-семантикой.
// Параметризован конкретным типом конфига (`KeeperConfig`/`SoulConfig`);
// внутренний `kind` определяет, какой `Load*FromBytes` вызвать при `Reload`.
type Store[T any] struct {
	snapshot atomic.Pointer[T]

	mu   sync.Mutex // защищает doc/path/opts при Reload
	doc  *Document
	path string
	kind storeKind
	opts ValidateOptions

	// auditWriter — опциональный [audit.Writer]. Если не nil, на каждый
	// Reload эмитится `config.reload_succeeded`/`config.reload_failed`
	// событие через [audit.Writer.Write] (best-effort; ошибка write-а
	// логируется через slog.Warn, но не блокирует возврат
	// [ReloadResult]). nil — backward compat: Reload работает без
	// audit-эмиссии (конструктор `LoadKeeperStore`/`LoadSoulStore` без
	// суффикса WithAudit).
	auditWriter audit.Writer

	// subsMu защищает slice subscribers. Отдельный mutex от `mu` — чтобы
	// notify-фаза (после swap-а) могла читать список без блокировки
	// reload-pipeline-а, и наоборот: OnReload-вызов из callback-а
	// (вложенный subscribe / unsubscribe) не deadlock-ится с reload-mutex-ом.
	//
	// RWMutex: notify читает slice под RLock (allows concurrent OnReload),
	// subscribe/unsubscribe — под Lock.
	subsMu sync.RWMutex

	// subscribers — slice опциональных reload-callback-ов. Каждый
	// зарегистрирован через [Store.OnReload]. Порядок не значим (callback-и
	// вызываются в отдельных goroutine-ах).
	//
	// Тип stored: `*subscription[T]` — pointer-обёртка, чтобы unsubscribe
	// мог идентифицировать запись по адресу без необходимости сравнивать
	// функции (несравнимые в Go).
	subscribers []*subscription[T]
}

// subscription — внутренняя запись subscriber-а. Хранит сам callback и его
// pointer-identity, который unsubscribe использует для O(n)-поиска и
// удаления. Поле `cb` immutable за время жизни subscription-а; pointer
// сам по себе — стабильный identity, что делает unsubscribe идемпотентным.
type subscription[T any] struct {
	cb ReloadCallback[T]
}

// ReloadResult — payload одного reload-а. Подразумевается дальнейшее
// форматирование caller-ом в audit-event `config.reload_succeeded` или
// `config.reload_failed` (имена событий и dual-write — отдельный slice M0.4).
type ReloadResult struct {
	// Swapped — был ли применён atomic swap. false означает, что снимок
	// остался прежним (I/O fatal или validation-error).
	Swapped bool

	// Source — кто инициировал reload. См. константы `ReloadSourceSignal` /
	// `ReloadSourceAPI` / `ReloadSourceMCP`.
	Source ReloadSource

	// Phase — фаза первой error-диагностики, если reload свалился; пустая
	// для успешного reload-а или для случая, когда диагностик уровня error
	// не было.
	Phase diag.Phase

	// Diagnostics — все диагностики этого reload-а (error + warning + hint).
	Diagnostics []diag.Diagnostic

	// ChangedPaths — YAML-пути изменившихся полей в формате goccy
	// ("$.auth.jwt.signing_key_ref"). Пустой в M0.3 (вычисление diff —
	// отложено в M0.3.5). Поле объявлено сейчас, чтобы консьюмеры
	// (audit-pipeline M0.4, Operator API) могли разрабатываться параллельно.
	ChangedPaths []string

	// CorrelationID — 26-символьный ULID (Crockford base32),
	// уникальный на reload. Формат нормирован [ADR-022(c)] и совпадает с
	// `audit_log.correlation_id` для events `config.reload_*`.
	CorrelationID string

	// Timestamp — момент завершения reload-а (момент решения о swap/не-swap).
	Timestamp time.Time
}

// LoadKeeperStore читает `keeper.yml` и оборачивает результат в Store.
//
// Возвращает Store даже при validation-errors: первый снимок может быть
// `nil` (через `Get()` вернётся zero-value pointer), но caller увидит
// диагностики и решит, прерывать ли старт. На I/O-fatal Store не создаётся
// — возвращаются (nil, diags, err).
func LoadKeeperStore(path string, opts ValidateOptions) (*Store[KeeperConfig], []diag.Diagnostic, error) {
	cfg, doc, diags, err := LoadKeeper(path, opts)
	if err != nil {
		return nil, diags, err
	}
	s := &Store[KeeperConfig]{
		doc:  doc,
		path: path,
		kind: storeKindKeeper,
		opts: opts,
	}
	if cfg != nil && !diag.HasErrors(diags) {
		s.snapshot.Store(cfg)
	}
	return s, diags, nil
}

// LoadSoulStore — то же для `soul.yml`. См. `LoadKeeperStore`.
func LoadSoulStore(path string, opts ValidateOptions) (*Store[SoulConfig], []diag.Diagnostic, error) {
	cfg, doc, diags, err := LoadSoul(path, opts)
	if err != nil {
		return nil, diags, err
	}
	s := &Store[SoulConfig]{
		doc:  doc,
		path: path,
		kind: storeKindSoul,
		opts: opts,
	}
	if cfg != nil && !diag.HasErrors(diags) {
		s.snapshot.Store(cfg)
	}
	return s, diags, nil
}

// LoadKeeperStoreWithAudit — то же что [LoadKeeperStore], плюс инъекция
// [audit.Writer]. На каждый [Store.Reload] Store будет эмитить событие
// `config.reload_succeeded` или `config.reload_failed` (см. контракт
// файла + [ADR-022(j)](docs/architecture.md) для payload-структуры).
//
// w может быть nil — в этом случае поведение идентично [LoadKeeperStore].
// Это удобно для конструкторов, у которых Writer ещё не инициализирован
// (например, до подъёма Postgres-пула на bootstrap-фазе): caller передаёт
// nil сейчас и переинициализирует Store позже.
//
// audit-write выполняется best-effort: ошибка [audit.Writer.Write]
// логируется через `slog.Warn`, но не блокирует возврат [ReloadResult] и
// не меняет `Swapped`. Атомарный swap снимка не зависит от успеха
// audit-эмиссии (audit — наблюдаемость, не корректность).
func LoadKeeperStoreWithAudit(path string, opts ValidateOptions, w audit.Writer) (*Store[KeeperConfig], []diag.Diagnostic, error) {
	s, diags, err := LoadKeeperStore(path, opts)
	if s != nil {
		s.auditWriter = w
	}
	return s, diags, err
}

// LoadSoulStoreWithAudit — то же для `soul.yml`. См. [LoadKeeperStoreWithAudit].
func LoadSoulStoreWithAudit(path string, opts ValidateOptions, w audit.Writer) (*Store[SoulConfig], []diag.Diagnostic, error) {
	s, diags, err := LoadSoulStore(path, opts)
	if s != nil {
		s.auditWriter = w
	}
	return s, diags, err
}

// SetAuditWriter инъектирует [audit.Writer] в уже созданный Store —
// late-binding-вариант [LoadKeeperStoreWithAudit] / [LoadSoulStoreWithAudit].
//
// Нужен порядку init бинаря `keeper`/`soul`, где Store создаётся ДО подъёма
// audit-writer-а (Vault → pool → migrations → writer, выверенная
// последовательность). Caller создаёт Store через `LoadKeeperStore` (без
// audit), а после поднятия writer-а вызывает `SetAuditWriter(w)` — далее
// каждый [Store.Reload] эмитит `config.reload_succeeded`/`config.reload_failed`
// (см. [Store.emitAudit], ADR-022(j)).
//
// w может быть nil (тогда audit-эмиссия выключена — back-compat). Безопасен
// для вызова до запуска [WatchSIGHUP]; конкурентный вызов с Reload не
// предполагается (вызывается один раз на init-фазе) — поле защищено `mu`
// на запись, чтение в `emitAudit` идёт под тем же `mu` через snapshot.
func (s *Store[T]) SetAuditWriter(w audit.Writer) {
	s.mu.Lock()
	s.auditWriter = w
	s.mu.Unlock()
}

// Get возвращает текущий снимок. Указатель immutable — caller не должен
// мутировать поля. Для модификации с последующей записью использовать
// `Document()` + `Patch*` + `Save*`.
//
// При failed initial load (validation-errors на первом чтении файла)
// `Get() == nil`, но `Document() != nil` — используй Document для
// Patch/Save и повтори Reload после исправления файла. После первого
// успешного Reload `Get()` начинает возвращать валидный указатель.
//
// Согласованность пары `Get()` ↔ `Document()` между вызовами не
// гарантируется: между ними может произойти Reload-swap. Caller, которому
// нужен согласованный snapshot+doc, должен опираться только на один из
// двух (например, `Document()` и парсить нужные поля сам).
func (s *Store[T]) Get() *T {
	return s.snapshot.Load()
}

// Document возвращает текущий AST-handle (для Patch/Save). Replace-ится
// под локом одновременно со снимком на успешном Reload. О согласованности
// с `Get()` — см. doc-comment `Get()`.
func (s *Store[T]) Document() *Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doc
}

// Path возвращает путь к файлу, с которым связан Store. Immutable за время
// жизни Store-а — Reload читает тот же путь.
func (s *Store[T]) Path() string {
	return s.path
}

// Reload перечитывает файл и атомарно подменяет снимок при отсутствии
// error-диагностик. См. doc-comment файла для контракта по полям результата.
//
// `ctx` зарезервирован под будущие отмены долгих semantic-проверок
// (например, vault reachability в `AllowNetworkCalls=true` режиме);
// в M0.3 валидация синхронная, ctx не прерывается внутри.
//
// `Timestamp` — момент принятия решения о swap/не-swap, выставляется
// прямо перед return на каждой ветке.
//
// При `Swapped=true` всем зарегистрированным через [Store.OnReload]
// подписчикам шлются уведомления (отдельные goroutine-ы, recover-panic).
// На `Swapped=false` notify не происходит — старый снимок продолжает
// действовать. См. [ReloadCallback] для контракта аргументов.
func (s *Store[T]) Reload(ctx context.Context, source ReloadSource) ReloadResult {
	// In-process span на весь hot-reload (parse → validation → semantic →
	// swap) — ADR-024. source совпадает с reload-audit-event-ом
	// (config.reload_succeeded/failed); span — отдельный observability-канал,
	// не дублирует audit. Секретов (содержимое конфига, vault-значения) в
	// атрибуты НЕ кладём: только source + исход + путь файла. При OTel
	// disabled tracer no-op — Start/End бесплатны.
	ctx, span := tracer.Start(ctx, "config.reload",
		trace.WithAttributes(
			attribute.String("source", string(source)),
			attribute.String("path", s.path),
		),
	)
	defer span.End()

	prev := s.snapshot.Load()
	res := s.reload(source)
	s.emitAudit(ctx, res)
	if res.Swapped {
		span.SetAttributes(attribute.String("outcome", "ok"))
		s.notify(prev, s.snapshot.Load())
	} else {
		span.SetAttributes(
			attribute.String("outcome", "failed"),
			attribute.String("phase", string(res.Phase)),
		)
		// Первая error-диагностика как span-error: видимость причины
		// провала в трейсе без раскрытия содержимого конфига (code+message
		// диагностики — те же, что в reload-audit validation_errors).
		if d := firstErrorDiag(res.Diagnostics); d != nil {
			span.RecordError(fmt.Errorf("%s: %s", d.Code, d.Message))
		}
		span.SetStatus(codes.Error, "config_reload_failed")
	}
	return res
}

// OnReload регистрирует subscriber-callback на успешные Reload-swap-ы.
// Возвращает функцию `unsubscribe` для отмены подписки.
//
// Контракт:
//
//   - `fn` вызывается **только** при `ReloadResult.Swapped=true`. На
//     validation-fail / I/O-fatal subscriber не уведомляется.
//   - Каждый вызов происходит в **отдельной goroutine** — slow subscriber
//     не блокирует ни Reload, ни других подписчиков.
//   - Порядок вызовов между subscriber-ами **не определён**.
//   - Panic внутри `fn` ловится `recover` + slog.Error с полем
//     `correlation_id` пустое (notify не привязан к Reload-correlation,
//     но callback может сам читать поля из new-snapshot).
//   - `unsubscribe` идемпотентна: повторный вызов — no-op. Вызов из
//     callback-а самого subscriber-а безопасен (RWMutex, без deadlock).
//   - `fn == nil` — panic (программная ошибка caller-а, аналогично
//     `signal.Notify(nil, ...)`).
//
// Видимость snapshot-а в callback-е: на момент вызова `s.snapshot.Load()`
// гарантированно возвращает `new` (swap уже произошёл). Конкурентный
// последующий Reload может ещё раз поменять snapshot — callback при этом
// видит **тот** snapshot, который вызвал notify (через аргумент `new`).
func (s *Store[T]) OnReload(fn ReloadCallback[T]) func() {
	if fn == nil {
		panic("config.Store.OnReload: callback is nil")
	}
	sub := &subscription[T]{cb: fn}
	s.subsMu.Lock()
	s.subscribers = append(s.subscribers, sub)
	s.subsMu.Unlock()

	return func() {
		s.subsMu.Lock()
		defer s.subsMu.Unlock()
		for i, x := range s.subscribers {
			if x == sub {
				// Сдвигаем хвост и обнуляем последний элемент, чтобы
				// не держать unreachable callback-references в backing
				// array (важно для долго живущих Store-ов, через которые
				// прокручиваются subscribe/unsubscribe-серии).
				last := len(s.subscribers) - 1
				s.subscribers[i] = s.subscribers[last]
				s.subscribers[last] = nil
				s.subscribers = s.subscribers[:last]
				return
			}
		}
	}
}

// notify рассылает уведомление об успешном swap-е всем зарегистрированным
// subscriber-ам. Snapshot slice-а под RLock, чтобы:
//
//   - параллельные OnReload-вызовы (Lock) дожидались окончания snapshot-а;
//   - unsubscribe из callback-а самого subscriber-а не deadlock-ился
//     (RWMutex permitted upgrade via separate goroutine — callback
//     запускается в отдельной goroutine, и Lock в его unsubscribe-вызове
//     не пересекается с уже отпущенным RLock).
//
// Каждый callback — в своей goroutine с recover-panic.
func (s *Store[T]) notify(old, new *T) {
	s.subsMu.RLock()
	subs := make([]*subscription[T], len(s.subscribers))
	copy(subs, s.subscribers)
	s.subsMu.RUnlock()

	for _, sub := range subs {
		go func(cb ReloadCallback[T]) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("config: ReloadCallback panicked",
						"panic", r,
					)
				}
			}()
			cb(old, new)
		}(sub.cb)
	}
}

// reload — внутреннее тело Reload без audit-эмиссии. Вынесено, чтобы
// emitAudit обернул единый ReloadResult, а не дублировался на каждой
// return-ветке. ctx читателю не нужен (валидация синхронная в M0.3);
// audit-вызов получает ctx из Reload-обёртки.
func (s *Store[T]) reload(source ReloadSource) ReloadResult {
	res := ReloadResult{
		Source:        source,
		CorrelationID: newCorrelationID(),
	}

	src, err := os.ReadFile(s.path)
	if err != nil {
		res.Phase = diag.PhaseParse
		res.Diagnostics = []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    s.path,
			Code:    "io_error",
			Message: err.Error(),
		}}
		res.Timestamp = time.Now()
		return res
	}

	var (
		newCfgKeeper *KeeperConfig
		newCfgSoul   *SoulConfig
		newDoc       *Document
		diags        []diag.Diagnostic
	)

	switch s.kind {
	case storeKindKeeper:
		newCfgKeeper, newDoc, diags, _ = LoadKeeperFromBytes(s.path, src, s.opts)
	case storeKindSoul:
		newCfgSoul, newDoc, diags, _ = LoadSoulFromBytes(s.path, src, s.opts)
	default:
		// Программная ошибка — конструкторы должны выставить kind.
		res.Phase = diag.PhaseParse
		res.Diagnostics = []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    s.path,
			Code:    "io_error",
			Message: fmt.Sprintf("config: Store has unknown kind %d", s.kind),
		}}
		res.Timestamp = time.Now()
		return res
	}

	res.Diagnostics = diags

	if diag.HasErrors(diags) {
		res.Phase = firstErrorPhase(diags)
		res.Timestamp = time.Now()
		return res
	}

	s.mu.Lock()
	switch s.kind {
	case storeKindKeeper:
		// Cast через any: дженерик `Store[T]` не знает, что T==KeeperConfig
		// именно для этого kind-а. Гарантирует конструктор.
		s.snapshot.Store(any(newCfgKeeper).(*T))
	case storeKindSoul:
		s.snapshot.Store(any(newCfgSoul).(*T))
	}
	s.doc = newDoc
	s.mu.Unlock()

	res.Swapped = true
	res.Timestamp = time.Now()
	return res
}

// emitAudit публикует одно `config.reload_*` событие в [audit.Writer].
// Безопасен при `s.auditWriter == nil` (no-op). Контракт по ADR-022(j):
//
//   - EventType  = config.reload_succeeded | config.reload_failed.
//   - Source     = [ReloadSource] (alias на [audit.Source]).
//   - Payload    = `{ "path": ..., ?"phase": ..., ?"validation_errors": [...],
//     ?"changed_paths": [...] }` — опциональные ключи присутствуют только
//     при failure (phase/validation_errors) или при наличии данных
//     (changed_paths). В M0.3 changed_paths всегда пустой
//     (diff source↔source отложен в M0.3.5).
//   - CorrelationID — тот же, что [ReloadResult.CorrelationID] (один
//     request — одна цепочка событий).
//   - CreatedAt — тот же, что [ReloadResult.Timestamp].
//
// audit-write выполняется best-effort: ошибка [audit.Writer.Write]
// логируется через `slog.Warn`, но не пропагируется — наблюдаемость не
// должна ломать hot-reload.
func (s *Store[T]) emitAudit(ctx context.Context, res ReloadResult) {
	// Снимок writer-а под `mu`: [SetAuditWriter] может выставить его
	// late-binding на init-фазе. Чтение копии (а не повторное обращение к
	// полю) делает race-free и сам nil-check, и последующий Write.
	s.mu.Lock()
	w := s.auditWriter
	s.mu.Unlock()
	if w == nil {
		return
	}

	et := audit.EventConfigReloadSucceeded
	if !res.Swapped {
		et = audit.EventConfigReloadFailed
	}

	payload := map[string]any{
		"path": s.path,
	}
	if !res.Swapped {
		payload["phase"] = string(res.Phase)
		if ve := audit.FormatDiagnostics(res.Diagnostics); ve != nil {
			payload["validation_errors"] = ve
		}
	}
	if len(res.ChangedPaths) > 0 {
		payload["changed_paths"] = res.ChangedPaths
	}

	ev := &audit.Event{
		AuditID:       audit.NewULID(),
		EventType:     et,
		Source:        res.Source,
		CorrelationID: res.CorrelationID,
		Payload:       payload,
		CreatedAt:     res.Timestamp,
	}

	if err := w.Write(ctx, ev); err != nil {
		slog.Warn("audit write failed for config.reload event",
			"path", s.path,
			"source", string(res.Source),
			"event_type", string(et),
			"correlation_id", res.CorrelationID,
			"error", err,
		)
	}
}

// firstErrorPhase — фаза первой error-диагностики, ключ к
// `config.reload_failed.phase` audit-event-а.
func firstErrorPhase(ds []diag.Diagnostic) diag.Phase {
	for i := range ds {
		if ds[i].Level == diag.LevelError {
			return ds[i].Phase
		}
	}
	return ""
}

// newCorrelationID — 26-символьный ULID (см. [audit.NewULID]).
// Делегируется в `shared/audit` — единственный источник ULID-генерации
// в проекте; формат совпадает с `audit_id` и `correlation_id` в
// `audit_log` ([ADR-022(c)](docs/architecture.md)).
func newCorrelationID() string {
	return audit.NewULID()
}
