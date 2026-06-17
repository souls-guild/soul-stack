package sigil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
)

// ErrPluginNotInCache — допуск (allow) запрошен для плагина, которого нет в
// кеше host-а (нет слота `<cacheRoot>/<ns>-<name>/` или в нём нет валидного
// бинаря/manifest). Transport маппит в 404. Обёртка над
// [pluginhost.ErrSlotNotFound] — service-граница не должна протекать
// pluginhost-sentinel-ом в handler.
var ErrPluginNotInCache = errors.New("sigil: plugin not found in host cache")

// SlotReader — поверхность чтения слота плагина из кеша по (namespace, name).
// Реализуется [cacheSlotReader] поверх [pluginhost.ReadSlot] /
// [pluginhost.SlotCommitSHA] (с фиксированным cacheRoot); сужение до интерфейса
// позволяет unit-тестировать Service без реального кеш-каталога. Вариант C: ref
// в чтении НЕ участвует (single-active-слот через current-symlink).
//
// SlotCommitSHA возвращает commit_sha АКТИВНОГО слота (target current-symlink-а,
// A1-S1) — audit-метку происхождения, пишущуюся в plugin_sigils при allow
// (ADR-026(g), ВНЕ подписи). Отсутствие/повреждение current → [ErrSlotNotFound]
// (fail-closed, симметрично ReadSlot).
type SlotReader interface {
	ReadSlot(namespace, name string) (*pluginhost.SlotContents, error)
	SlotCommitSHA(namespace, name string) (string, error)
}

// cacheSlotReader адаптирует [pluginhost.ReadSlot] / [pluginhost.SlotCommitSHA]
// (с фиксированным cacheRoot) к интерфейсу [SlotReader]. Production-wire-up в
// `keeper run` биндит cacheRoot.
type cacheSlotReader struct {
	cacheRoot string
}

func (r cacheSlotReader) ReadSlot(namespace, name string) (*pluginhost.SlotContents, error) {
	return pluginhost.ReadSlot(r.cacheRoot, namespace, name)
}

func (r cacheSlotReader) SlotCommitSHA(namespace, name string) (string, error) {
	return pluginhost.SlotCommitSHA(r.cacheRoot, namespace, name)
}

// NewCacheSlotReader строит [SlotReader] поверх кеша Keeper-host-а с
// фиксированным cacheRoot (`keeper.yml` / [pluginhost.DefaultCacheRoot]).
func NewCacheSlotReader(cacheRoot string) SlotReader {
	return cacheSlotReader{cacheRoot: cacheRoot}
}

// Store — поверхность реестра plugin_sigils, нужная [Service]. Реализуется
// пакетным CRUD (Insert / Revoke / ListActive) поверх pgx-pool-а через
// [NewPGStore]; сужение до интерфейса изолирует Service от прямого pgx-pool-а
// в unit-тестах.
type Store interface {
	Insert(ctx context.Context, s *Sigil) error
	Revoke(ctx context.Context, namespace, name, ref, revokedByAID string) error
	ListActive(ctx context.Context) ([]*Sigil, error)
}

// pgStore — адаптер пакетного CRUD plugin_sigils к интерфейсу [Store]. Держит
// pool (или tx) и делегирует в Insert / Revoke / ListActive этого пакета.
type pgStore struct {
	db ExecQueryRower
}

// NewPGStore оборачивает pgx-pool (любой [ExecQueryRower]) в [Store].
func NewPGStore(db ExecQueryRower) Store {
	return &pgStore{db: db}
}

func (s *pgStore) Insert(ctx context.Context, rec *Sigil) error {
	return Insert(ctx, s.db, rec)
}

func (s *pgStore) Revoke(ctx context.Context, namespace, name, ref, revokedByAID string) error {
	return Revoke(ctx, s.db, namespace, name, ref, revokedByAID)
}

func (s *pgStore) ListActive(ctx context.Context) ([]*Sigil, error) {
	return ListActive(ctx, s.db)
}

// Invalidator — поверхность cluster-wide Sigil-инвалидации (ADR-026, S6c).
// После успешного commit-а Allow/Revoke [Service] вызывает Invalidate, чтобы
// КАЖДАЯ Keeper-нода (включая мутирующую) re-broadcast-ила active-набор своим
// подключённым Soul-ам — иначе Soul на другой ноде работает с устаревшим кешем
// (revoked-допуск ещё «доверен», новый allow ещё не доехал). Реализуется в
// `keeper run` адаптером поверх [keeperredis.PublishSigilInvalidate]; в
// single-Keeper/dev-режиме (без Redis) не подключён — допуски доедут на
// connect-time broadcast при следующем reconnect Soul-а.
//
// Invalidate — best-effort: ошибку публикации НЕ возвращает (мутация уже
// зафиксирована в БД), реализация логирует и глотает.
type Invalidator interface {
	Invalidate(ctx context.Context)
}

// ServiceDeps — зависимости [Service]. Все поля immutable после конструктора.
type ServiceDeps struct {
	Signer *Signer
	Store  Store
	Slots  SlotReader
	Logger *slog.Logger
}

// Service — бизнес-логика Sigil (allow / revoke / list) поверх S3 (Signer +
// Store) и кеша host-а (SlotReader). Один источник правды для transport-
// фасада (OpenAPI — S4a, MCP — S4b): handler декодирует input → service-call →
// маппит sentinel-ошибки.
//
// Вариант C (ADR-026, operator-asserted ref): Allow читает ТЕКУЩИЙ бинарь+
// manifest из единственного слота `<cacheRoot>/<ns>-<name>/` (ключ без ref);
// `ref` приходит как operator-предоставленная метка и в lookup слота НЕ
// участвует. Authority целостности — sha256+подпись, не git-verified ref.
//
// Безопасен для конкурентного использования: deps immutable, состояние не
// держится (ed25519.Sign не мутирует ключ; атомарность — на уровне Store/PG).
type Service struct {
	// signer — atomic.Pointer, потому что R3 multi-anchor ротация (S6) подменяет
	// подписывающий Signer в рантайме (новый primary после Introduce/SetPrimary)
	// конкурентно с Allow, который читает его в [Service.Allow]. Замена — целым
	// указателем (Signer immutable после конструктора), lock-free чтение в hot-
	// path-е Allow. Всегда non-nil после [NewService] (конструктор проверяет).
	signer atomic.Pointer[Signer]
	store  Store
	slots  SlotReader
	logger *slog.Logger

	// inv — опциональный cluster-wide invalidator (S6c). Late-binding через
	// [Service.SetInvalidator]: Redis-клиент в `keeper run` поднимается ПОСЛЕ
	// NewService, поэтому инъекция отложена (паттерн rbac.Service.inv).
	// atomic.Pointer — конкурентная запись сеттером vs. чтение из мутаций без
	// отдельного mutex-а.
	inv atomic.Pointer[Invalidator]
}

// NewService собирает service. Signer / Store / Slots обязательны.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Signer == nil {
		return nil, errors.New("sigil: ServiceDeps.Signer is nil")
	}
	if d.Store == nil {
		return nil, errors.New("sigil: ServiceDeps.Store is nil")
	}
	if d.Slots == nil {
		return nil, errors.New("sigil: ServiceDeps.Slots is nil")
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	svc := &Service{store: d.Store, slots: d.Slots, logger: logger}
	svc.signer.Store(d.Signer)
	return svc, nil
}

// SetSigner атомарно подменяет подписывающий Signer (R3 multi-anchor «keeper
// Signer hot-reload», S6). Вызывается daemon-watcher-ом по cluster-сигналу
// `sigil:anchors-changed` после ротации ключей подписи (Introduce / SetPrimary /
// Retire): новый Signer несёт свежий primary (подпись новых допусков) и полный
// набор active-якорей. nil-вход игнорируется (defensive: подмена на nil лишила бы
// Allow подписи) — каждый build-Signer-путь в daemon возвращает non-nil либо
// ошибку. Потокобезопасно для конкурентного [Service.Allow].
func (s *Service) SetSigner(signer *Signer) {
	if signer == nil {
		return
	}
	s.signer.Store(signer)
}

// SetInvalidator late-binding-ом подключает cluster-wide invalidator (S6c).
// Вызывается из `keeper run` после подъёма Redis-клиента. nil — снять
// invalidator (вернуться к чистому connect-time broadcast-у). Идемпотентен,
// потокобезопасен. Паттерн идентичен [rbac.Service.SetInvalidator].
func (s *Service) SetInvalidator(inv Invalidator) {
	if inv == nil {
		s.inv.Store(nil)
		return
	}
	s.inv.Store(&inv)
}

// invalidate шлёт cluster-wide invalidate-сигнал после успешного commit-а
// allow/revoke-мутации (S6c). No-op, если invalidator не подключён
// (single-Keeper/dev). Best-effort: реализация Invalidate сама логирует и
// глотает ошибку publish-а — мутация уже зафиксирована, потеря сигнала
// компенсируется connect-time broadcast-ом.
func (s *Service) invalidate(ctx context.Context) {
	if p := s.inv.Load(); p != nil {
		(*p).Invalidate(ctx)
	}
}

// AllowInput — параметры [Service.Allow].
type AllowInput struct {
	Namespace string
	Name      string
	Ref       string
	CallerAID string
}

// Allow допускает плагин (namespace, name) под operator-asserted меткой ref в
// allow-list plugin_sigils.
//
// Шаги (вариант C):
//  1. читает текущий бинарь+manifest из слота `<cacheRoot>/<ns>-<name>/`
//     (ref в чтении НЕ участвует); нет слота → [ErrPluginNotInCache];
//  2. читает commit_sha АКТИВНОГО слота (target current-symlink-а) — audit-метку
//     происхождения (ADR-026(g)); нет/битый current → [ErrPluginNotInCache]
//     (fail-closed: происхождение допуска обязано быть зафиксировано);
//  3. подписывает блок Sigil-а Signer-ом над (ns, name, ref, binary_sha256,
//     manifest_bytes) — commit_sha в блок НЕ входит, подпись не меняется;
//  4. вставляет запись в реестр (commit_sha — отдельной audit-колонкой); уже
//     активная запись на (ns, name, ref) → [ErrSigilAlreadyActive].
//
// Возвращает sha256 допущенного бинаря (hex) — handler кладёт его в 201-ответ.
func (s *Service) Allow(ctx context.Context, in AllowInput) (string, error) {
	slot, err := s.slots.ReadSlot(in.Namespace, in.Name)
	if err != nil {
		if errors.Is(err, pluginhost.ErrSlotNotFound) {
			return "", fmt.Errorf("%w: %s-%s", ErrPluginNotInCache, in.Namespace, in.Name)
		}
		return "", fmt.Errorf("sigil: read plugin slot: %w", err)
	}

	// commit_sha АКТИВНОГО слота (target current-symlink-а). Источник тот же
	// current, что ReadSlot следует при чтении бинаря/manifest, — значит
	// commit_sha согласован ровно с подписанным бинарём. fail-closed: legacy-слот
	// без current даёт ErrSlotNotFound → не допускаем без зафиксированного
	// происхождения (тот же контракт, что отсутствие самого слота).
	commitSHA, err := s.slots.SlotCommitSHA(in.Namespace, in.Name)
	if err != nil {
		if errors.Is(err, pluginhost.ErrSlotNotFound) {
			return "", fmt.Errorf("%w: %s-%s (no resolved commit_sha)", ErrPluginNotInCache, in.Namespace, in.Name)
		}
		return "", fmt.Errorf("sigil: read plugin slot commit_sha: %w", err)
	}

	signature, err := s.signer.Load().Sign(in.Namespace, in.Name, in.Ref, slot.BinarySHA256, slot.ManifestBytes)
	if err != nil {
		return "", fmt.Errorf("sigil: sign: %w", err)
	}

	manifestJSON, err := manifestYAMLToJSON(slot.ManifestBytes)
	if err != nil {
		return "", fmt.Errorf("sigil: convert manifest to JSON: %w", err)
	}

	rec := &Sigil{
		Namespace: in.Namespace,
		Name:      in.Name,
		Ref:       in.Ref,
		SHA256:    slot.BinarySHA256,
		CommitSHA: commitSHA,
		Signature: signature,
		// ManifestRaw — ТЕ ЖЕ байты, что ушли в Sign выше (единый ReadSlot),
		// byte-exact канон для S6-verify/broadcast. Manifest — производная JSONB-
		// проекция для query/audit. Разводить источники нельзя: инвариант
		// «подписаны ровно эти байты» разъедется.
		ManifestRaw:  slot.ManifestBytes,
		Manifest:     manifestJSON,
		AllowedByAID: in.CallerAID,
	}
	if err := s.store.Insert(ctx, rec); err != nil {
		return "", err
	}
	// Cluster-wide re-broadcast active-набора всем подключённым Soul-ам (S6c):
	// новый допуск должен доехать near-instant, не дожидаясь reconnect-а.
	s.invalidate(ctx)
	return slot.BinarySHA256, nil
}

// Revoke отзывает активный допуск (namespace, name, ref). Активной записи нет →
// [ErrSigilNotFound].
func (s *Service) Revoke(ctx context.Context, namespace, name, ref, callerAID string) error {
	if err := s.store.Revoke(ctx, namespace, name, ref, callerAID); err != nil {
		return err
	}
	// Cluster-wide re-broadcast active-набора (S6c): отозванный допуск исчезает
	// из свежего набора → fail-closed на Soul-стороне. Семантика стирания из
	// кеша — см. ограничение в [eventStreamHandler.rebroadcastSigils] /
	// connect-time replace.
	s.invalidate(ctx)
	return nil
}

// SigilView — проекция активной записи для list-выдачи. БЕЗ signature и
// manifest: signature — сырой крипто-материал (не для API), manifest — крупный
// JSONB query/audit-слой (не лента allow-list-а). Симметрично rbac.RoleView.
type SigilView struct {
	Namespace    string
	Name         string
	Ref          string
	SHA256       string
	AllowedByAID string
	AllowedAt    time.Time
	RevokedAt    *time.Time
}

// List возвращает ленту активных допусков (новые первыми) без signature/
// manifest.
func (s *Service) List(ctx context.Context) ([]SigilView, error) {
	recs, err := s.store.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SigilView, 0, len(recs))
	for _, r := range recs {
		out = append(out, SigilView{
			Namespace:    r.Namespace,
			Name:         r.Name,
			Ref:          r.Ref,
			SHA256:       r.SHA256,
			AllowedByAID: r.AllowedByAID,
			AllowedAt:    r.AllowedAt,
			RevokedAt:    r.RevokedAt,
		})
	}
	return out, nil
}

// manifestYAMLToJSON конвертирует сырые байты manifest.yaml в JSON для JSONB-
// колонки plugin_sigils.manifest (query/audit-слой, НЕ канон для verify —
// канон держится на сырых байтах через NormalizeManifestBytes, S3↔S6).
// Используется тот же goccy/go-yaml, что и в shared/plugin-парсере.
func manifestYAMLToJSON(yamlBytes []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(yamlBytes, &v); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("json marshal: %w", err)
	}
	return out, nil
}
