package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// SoulPool — поверхность над pgxpool.Pool, нужная Soul-handler-у.
//
//   - BeginTx — Create (souls-row + bootstrap-token атомарно) и IssueToken
//     (force-flow: expire active + insert new в одной транзакции).
//   - ExecQueryRower — List (count + select без транзакции, через
//     [soul.SelectAll]); read-only, транзакция избыточна.
//
// `*pgxpool.Pool` удовлетворяет обоим; unit-тесты — fake.
type SoulPool interface {
	soul.ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// PurviewResolver — read-поверхность enforcer для scope-границы оператора:
// «верни [rbac.Purview] (верхнюю границу видимости/таргетинга) для
// (resource, action)». Реализуют [rbac.Enforcer] и [rbac.Holder] (hot-reload-
// aware). Обобщает прежний CovenScoper (S0 `(covens, unrestricted)`) на полный
// Purview — один резолвер для обоих souls-потребителей scope:
//   - `GET /v1/souls` scoped-видимость (ADR-047 S3b, через keeper/internal/
//     soulpurview) — coven-pushdown в SQL;
//   - bulk coven-assign scope-intersection (`POST /v1/souls/coven`) — coven-
//     измерение Purview даёт прежнюю `(covens, unrestricted)`-форму [soul.BulkScope].
//
// Симметрично least-privilege subset-check в rbac.Service (тот режет выдачу
// прав, этот — объём массовой мутации/видимости).
type PurviewResolver interface {
	ResolvePurview(aid, resource, action string) rbac.Purview
}

// SoulPresence — узкая поверхность batch-проверки «жив ли Redis SID-lease»
// (живой EventStream), нужная presence-overlay-у `GET /v1/souls` (ADR-006(a)).
// Симметрична [topology.SoulLeaseChecker]: реальная реализация — обёртка над
// [keeperredis.SoulsStreamAlive], собранная в cmd/keeper; production wire-up
// передаёт тот же Redis-клиент, что и topology-резолверу.
//
// presence — авторитет online/offline (ADR-006(a)); PG-колонка `souls.status`
// — лишь лениво-сверяемый Reaper-ом снимок «последнего известного», поэтому на
// hot-path-е reconnect-а она не флипается в connected, и read-path обязан
// деривировать presence из lease, а не отдавать stale-снимок.
//
// Возвращает множество SID-ов с живым lease. nil-checker (single-instance dev
// без Redis / unit-тесты) → overlay выключен, отдаётся PG-снимок status как есть
// (в single-instance он когерентен с фактом стрима по построению, симметрично
// reaper-у и topology-резолверу).
type SoulPresence interface {
	SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error)
}

// SoulHandler — endpoints онбординга Soul-ов: Create + IssueToken + List +
// AssignCoven (bulk).
//
// При force-reissue освобождённый токен помечается системным маркером
// [bootstraptoken.SystemKIDForceReissue] в `bootstrap_tokens.used_by_kid`
// (см. IssueToken). Все зависимости immutable; safe for concurrent use.
type SoulHandler struct {
	pool     SoulPool
	scoper   PurviewResolver
	presence SoulPresence
	logger   *slog.Logger

	// scopeEvalInnerPageSize — внутренний page-size keyset-eval-а (ADR-047
	// S3b-2a). 0 → [scopeEvalInnerPageSize] (prod-дефолт). Переопределяется
	// только в тестах (малое значение, чтобы смоделировать многостраничный
	// добор без подъёма флота на десятки тысяч).
	scopeEvalInnerPageSize int

	// scopeEvalMaxInnerPages — cap внутренних keyset-итераций на один запрос
	// (ADR-047 S3b-2a). 0 → [scopeEvalMaxInnerPages] (prod-дефолт). Инъекция
	// только для тестов: малый cap моделирует cap-выход (узкий regex на большом
	// флоте) без подъёма десятков тысяч хостов.
	scopeEvalMaxInnerPages int
}

// NewSoulHandler создаёт handler. scoper — read-поверхность scope-границы
// оператора ([PurviewResolver], production-wire-up передаёт rbac.Holder).
// Используется и `GET /v1/souls` scoped-видимостью (ADR-047 S3b), и bulk
// AssignCoven. nil допустим только в тестах, не использующих List/bulk-роут:
// List при nil-scoper fail-closed (пустой список — безопасный дефолт, НЕ весь
// флот), AssignCoven вернёт 500.
//
// presence — lease-overlay presence для `GET /v1/souls` (ADR-006(a)): при
// non-nil поле `status` в ответе List/Get деривируется из живого Redis
// SID-lease, а не отдаётся как stale PG-снимок (см. [SoulPresence]). nil →
// overlay выключен (single-instance dev / unit-тесты), отдаётся PG-снимок.
func NewSoulHandler(pool SoulPool, scoper PurviewResolver, presence SoulPresence, logger *slog.Logger) *SoulHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &SoulHandler{pool: pool, scoper: scoper, presence: presence, logger: logger}
}

// SoulSpecStub — непустой *SoulHandler-заглушка для генерации huma-OpenAPI-фрагмента
// (HumaSoulSpecYAML): при dump доменный handler не вызывается, но huma.Register требует
// non-nil для nil-проверки (parity [RoleSpecStub]). pool nil — handler в spec-режиме не
// исполняется.
func SoulSpecStub() *SoulHandler {
	return &SoulHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// soulCreateRequest — POST /v1/souls body. НЕ alias на [SoulCreateRequest]:
// домен несёт server-only поле `note` (запись в souls.note), которого нет в
// OpenAPI-схеме SoulCreateRequest; pure-alias уронил бы его (strict-декодер
// отверг бы `note` как unknown → 400). Поэтому остаётся доменным типом
// (симметрично soulCovenAssignResponse — там alias тоже невозможен).
//
// Поле `covens` совпадает по имени с OpenAPI SoulCreateRequest и с ответом
// (`SoulCreateReply.covens`): strict-декодер отвергает unknown-поля, поэтому
// расхождение имени запроса и схемы = 400 на документированном клиенте.
type soulCreateRequest struct {
	SID       string   `json:"sid"`
	Transport string   `json:"transport"`
	Covens    []string `json:"covens,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// NewSoulCreateRequest конструирует доменный [soulCreateRequest] из примитивов huma-body
// (soulCreateRequest unexported — huma-роут пакета api собирает запрос через этот
// конструктор, parity тонкого-конверта §Pattern шаг 3).
func NewSoulCreateRequest(sid, transport string, covens []string, note string) soulCreateRequest {
	return soulCreateRequest{SID: sid, Transport: transport, Covens: covens, Note: note}
}

// SoulCreateView — ПЛОСКАЯ доменная проекция 201-тела POST /v1/souls (handler-native
// T5d). Пакет api проецирует её в native-схему SoulCreateReply. status/transport — RAW
// string домена (native-тип в api держит enum-форму). Covens — non-nil slice (`&covens`
// после coalesceCoven → ключ всегда present). BootstrapToken/ExpiresAt — pointer-optional
// (присутствуют только для transport=agent; nil → ключ опущен native-типом). date-time —
// СЕКУНДНЫЙ wire (.UTC().Truncate(time.Second), parity легаси RFC3339-`.Format`).
type SoulCreateView struct {
	SID            string
	Transport      string
	Status         string
	Covens         []string
	RegisteredAt   time.Time
	CreatedByAID   string
	BootstrapToken *string
	ExpiresAt      *time.Time
}

// soulCreateView строит доменную проекцию [SoulCreateView] из реестровой записи.
func soulCreateView(s *soul.Soul, createdByAID string) SoulCreateView {
	return SoulCreateView{
		SID:          s.SID,
		Transport:    string(s.Transport),
		Status:       string(s.Status),
		Covens:       coalesceCoven(s.Coven),
		RegisteredAt: s.RegisteredAt.UTC().Truncate(time.Second),
		CreatedByAID: createdByAID,
	}
}

// SoulCreateReply — результат [SoulHandler.CreateTyped] (handler-native). Несёт доменную
// проекцию 201-тела (SoulCreateView) + audit-поля. bootstrap_token присутствует только для
// transport=agent.
type SoulCreateReply struct {
	Body         SoulCreateView
	SID          string
	Transport    string
	Covens       []string
	CreatedByAID string
	TokenIssued  bool
}

// AuditPayload — audit-поля 201-Create (parity легаси SetAuditPayload; сам bootstrap-токен
// не пишется).
func (r SoulCreateReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"sid":            r.SID,
		"transport":      r.Transport,
		"covens":         r.Covens,
		"created_by_aid": r.CreatedByAID,
		"token_issued":   r.TokenIssued,
	}
}

// CreateTyped — извлечённая доменная функция POST /v1/souls (FULL-TYPED разворот ADR-054
// §Pattern): онбординг souls-row (+ bootstrap-токен для agent) без http-границы. req — уже
// декодированное тело. Ошибки — *problemError (422 невалидный sid/transport/coven; 409
// soul-exists; 500 PG-сбой); успех — [SoulCreateReply] (201-тело + audit-поля).
func (h *SoulHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req soulCreateRequest) (SoulCreateReply, error) {
	var zero SoulCreateReply
	if req.SID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'sid' is required")}
	}
	if !soul.ValidSID(req.SID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'sid' must match "+soul.SIDPattern)}
	}
	transport, ok := parseTransport(req.Transport)
	if !ok {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'transport' is required and must be one of: agent, ssh")}
	}
	for _, label := range req.Covens {
		if !soul.ValidCoven(label) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"coven label "+label+" must match "+soul.CovenPattern)}
		}
	}

	creator := claims.Subject
	s := &soul.Soul{
		SID:          req.SID,
		Transport:    transport,
		Status:       soul.StatusPending,
		Coven:        req.Covens,
		CreatedByAID: &creator,
		Note:         req.Note,
	}

	issueToken := transport == soul.TransportAgent

	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		h.logger.Error("soul.create: begin tx failed", slog.String("sid", req.SID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create soul failed")}
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	if err := soul.Insert(ctx, tx, s); err != nil {
		if errors.Is(err, soul.ErrSoulAlreadyExists) {
			return zero, &problemError{problem.New(problem.TypeSoulExists, "", "soul "+req.SID+" already exists")}
		}
		if errors.Is(err, soul.ErrSoulCreatorNotFound) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"creator AID "+creator+" not found in operators registry")}
		}
		h.logger.Error("soul.create: insert failed",
			slog.String("sid", req.SID), slog.String("by_aid", creator), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create soul failed")}
	}

	resp := soulCreateView(s, creator)
	if issueToken {
		plain, err := bootstraptoken.Generate()
		if err != nil {
			h.logger.Error("soul.create: token generate failed", slog.String("sid", req.SID), slog.Any("error", err))
			return zero, &problemError{problem.New(problem.TypeInternalError, "", "create soul failed")}
		}
		rec, err := bootstraptoken.Insert(ctx, tx, req.SID, plain.Hash(), bootstraptoken.DefaultTokenTTL, &creator)
		if err != nil {
			h.logger.Error("soul.create: token insert failed", slog.String("sid", req.SID), slog.Any("error", err))
			return zero, &problemError{problem.New(problem.TypeInternalError, "", "create soul failed")}
		}
		token := plain.Reveal()
		expiresAt := rec.ExpiresAt.UTC().Truncate(time.Second)
		resp.BootstrapToken = &token
		resp.ExpiresAt = &expiresAt
	}

	if err := tx.Commit(ctx); err != nil {
		h.logger.Error("soul.create: commit failed", slog.String("sid", req.SID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "create soul failed")}
	}
	committed = true

	return SoulCreateReply{
		Body:         resp,
		SID:          s.SID,
		Transport:    string(s.Transport),
		Covens:       coalesceCoven(s.Coven),
		CreatedByAID: creator,
		TokenIssued:  issueToken,
	}, nil
}

// SoulIssueTokenView — ПЛОСКАЯ доменная проекция 200-тела POST /v1/souls/{sid}/issue-token
// (handler-native T5d). Пакет api проецирует её в native-схему SoulIssueTokenReply. Все поля
// required; expires_at — СЕКУНДНЫЙ wire (.UTC().Truncate(time.Second), parity легаси).
// bootstrap_token отдаётся один раз (SENSITIVE; secret-mask вырезает из логов).
type SoulIssueTokenView struct {
	SID            string
	BootstrapToken string
	ExpiresAt      time.Time
}

// soulIssueTokenView строит доменную проекцию [SoulIssueTokenView].
func soulIssueTokenView(sid, token string, expiresAt time.Time) SoulIssueTokenView {
	return SoulIssueTokenView{
		SID:            sid,
		BootstrapToken: token,
		ExpiresAt:      expiresAt.UTC().Truncate(time.Second),
	}
}

// SoulIssueTokenReply — результат [SoulHandler.IssueTokenTyped] (handler-native). Несёт
// доменную проекцию 200-тела (SoulIssueTokenView: sid/bootstrap_token/expires_at) + audit-
// поля. bootstrap_token отдаётся один раз (SENSITIVE).
type SoulIssueTokenReply struct {
	Body            SoulIssueTokenView
	SID             string
	Force           bool
	ExpiredPrevious bool
	ExpiresAtRFC    string
}

// AuditPayload — audit-поля 200-IssueToken. Ключи БЕЗ `token`-substring (audit secret-mask
// редактирует любой ключ с `token` в `***MASKED***`); сам токен не пишется.
func (r SoulIssueTokenReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"sid":              r.SID,
		"force":            r.Force,
		"expired_previous": r.ExpiredPrevious,
		"expires_at":       r.ExpiresAtRFC,
	}
}

// IssueTokenTyped — извлечённая доменная функция POST /v1/souls/{sid}/issue-token
// (FULL-TYPED): повторная выписка bootstrap-токена (transport=agent) без http-границы.
// Ошибки — *problemError (422 невалидный sid / transport=ssh; 404 нет soul; 409 активный
// токен без force; 500 PG-сбой); успех — [SoulIssueTokenReply] (200-тело + audit-поля).
func (h *SoulHandler) IssueTokenTyped(ctx context.Context, claims *jwt.Claims, sid string, force bool) (SoulIssueTokenReply, error) {
	var zero SoulIssueTokenReply
	if !soul.ValidSID(sid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}

	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		h.logger.Error("soul.issue-token: begin tx failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	s, err := soul.SelectBySID(ctx, tx, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		}
		h.logger.Error("soul.issue-token: select failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
	}
	if s.Transport != soul.TransportAgent {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"soul "+sid+" has transport "+string(s.Transport)+"; bootstrap tokens are only issued for transport=agent")}
	}

	creator := claims.Subject
	var expiredPrevious bool
	if force {
		_, expired, err := bootstraptoken.ExpireActiveBySID(ctx, tx, sid, bootstraptoken.SystemKIDForceReissue)
		if err != nil {
			h.logger.Error("soul.issue-token: expire active failed", slog.String("sid", sid), slog.Any("error", err))
			return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
		}
		expiredPrevious = expired
	}

	plain, err := bootstraptoken.Generate()
	if err != nil {
		h.logger.Error("soul.issue-token: generate failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
	}
	rec, err := bootstraptoken.Insert(ctx, tx, sid, plain.Hash(), bootstraptoken.DefaultTokenTTL, &creator)
	if err != nil {
		if errors.Is(err, bootstraptoken.ErrTokenActiveExists) {
			return zero, &problemError{problem.New(problem.TypeBootstrapTokenActive, "",
				"soul "+sid+" already has an active bootstrap token; pass ?force=true to expire it and reissue")}
		}
		h.logger.Error("soul.issue-token: insert failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
	}

	if err := tx.Commit(ctx); err != nil {
		h.logger.Error("soul.issue-token: commit failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue token failed")}
	}
	committed = true

	return SoulIssueTokenReply{
		Body:            soulIssueTokenView(sid, plain.Reveal(), rec.ExpiresAt),
		SID:             sid,
		Force:           force,
		ExpiredPrevious: expiredPrevious,
		ExpiresAtRFC:    rec.ExpiresAt.UTC().Format(time.RFC3339),
	}, nil
}

// SoulListView — ПЛОСКАЯ доменная проекция одной строки реестра `souls` (handler-native
// T5d): shared element списка `GET /v1/souls` И get-Body `GET /v1/souls/{sid}`. Пакет api
// проецирует её в native-схему SoulListEntry. Проекция реестра; fingerprint SoulSeed-а и
// любые секреты сознательно НЕ включены. status/transport — RAW string домена (native-тип
// в api держит enum-форму). covens — non-nullable slice (nil → `[]` через coalesceCoven).
// LastSeenAt/LastSeenByKid/RequestedAt/CreatedByAID — pointer-nullable (native-тип без
// omitempty → `null` при nil). date-time — НАНОСЕКУНДНЫЙ wire (голый UTC, без Truncate).
type SoulListView struct {
	SID           string
	Transport     string
	Status        string
	Covens        []string
	Traits        map[string]any
	LastSeenAt    *time.Time
	LastSeenByKid *string
	RegisteredAt  time.Time
	RequestedAt   *time.Time
	CreatedByAID  *string
}

// toSoulListView проецирует [soul.Soul] в доменный [SoulListView]. date-time — `.UTC()`
// БЕЗ Truncate (pgx уже отдаёт UTC, усечение сломало бы байт-в-байт). covens nil → `[]`.
func toSoulListView(s *soul.Soul) SoulListView {
	item := SoulListView{
		SID:           s.SID,
		Transport:     string(s.Transport),
		Status:        string(s.Status),
		Covens:        coalesceCoven(s.Coven),
		Traits:        coalesceTraits(s.Traits),
		LastSeenByKid: s.LastSeenByKID,
		RegisteredAt:  s.RegisteredAt.UTC(),
		CreatedByAID:  s.CreatedByAID,
	}
	if s.LastSeenAt != nil {
		t := s.LastSeenAt.UTC()
		item.LastSeenAt = &t
	}
	if s.RequestedAt != nil {
		t := s.RequestedAt.UTC()
		item.RequestedAt = &t
	}
	return item
}

// overlayPresence деривирует поле `status` ответа из живого Redis SID-lease
// (ADR-006(a)): PG-колонка `souls.status` — лениво-сверяемый Reaper-ом снимок
// «последнего известного», она НЕ флипается в connected на hot-path-е reconnect-а,
// поэтому read-path обязан определять presence по lease, иначе переподключившийся
// Soul висит `disconnected` до следующего тика Reaper-а (live-баг). Симметрично
// [topology.Resolver.filterAlive] — единый presence-источник для всех consumer-ов.
//
// Overlay затрагивает ТОЛЬКО presence-снимковые статусы (`connected`/`disconnected`):
//   - lease жив  → `connected`;
//   - lease мёртв → `disconnected`.
//
// Lifecycle-статусы (`pending`/`revoked`/`expired`/`destroyed`) — НЕ presence, а
// состояние онбординга/терминал; они не имеют lease-семантики и отдаются как есть
// (то же исключение, что в topology.rosterSQL).
//
// presence==nil (single-instance dev / unit) → no-op: PG-снимок отдаётся как есть.
// Ошибка Redis-проверки → fail-safe: warn + PG-снимок без overlay (сетевой сбой
// Redis не должен искажать весь список в одну сторону).
func (h *SoulHandler) overlayPresence(ctx context.Context, items []SoulListView) {
	if h.presence == nil || len(items) == 0 {
		return
	}
	sids := make([]string, 0, len(items))
	for i := range items {
		if presenceSnapshotStatus(items[i].Status) {
			sids = append(sids, items[i].SID)
		}
	}
	if len(sids) == 0 {
		return
	}
	alive, err := h.presence.SoulsStreamAlive(ctx, sids)
	if err != nil {
		h.logger.Warn("soul.presence: lease check failed — returning PG snapshot (fail-safe)",
			slog.Any("error", err))
		return
	}
	for i := range items {
		if !presenceSnapshotStatus(items[i].Status) {
			continue
		}
		if _, ok := alive[items[i].SID]; ok {
			items[i].Status = string(soul.StatusConnected)
		} else {
			items[i].Status = string(soul.StatusDisconnected)
		}
	}
}

// presenceSnapshotStatus — статус, чья семантика = presence-снимок (online/offline),
// а значит участвует в lease-overlay. Лифецикл-статусы исключены (см. overlayPresence).
func presenceSnapshotStatus(status string) bool {
	switch soul.Status(status) {
	case soul.StatusConnected, soul.StatusDisconnected:
		return true
	}
	return false
}

// scopeEvalInnerPageSize — внутренний page-size keyset-eval-а: handler читает
// флот окном этого размера и Go-OR-фильтрует, набирая клиентский limit. НЕ
// грузит весь флот одной выборкой (ADR-047 §Перф, симметрично bulkChunkSize).
const scopeEvalInnerPageSize = 2000

// scopeEvalMaxInnerPages — cap внутренних keyset-итераций на ОДИН клиентский
// запрос (~20 страниц = 40k просмотренных строк). За cap handler отдаёт что
// собрал + next_cursor (клиент дочитает следующим запросом) — защита от
// patho-кейса «очень узкий regex на огромном флоте» (полный скан под одним
// HTTP-запросом недопустим).
const scopeEvalMaxInnerPages = 20

// GET /v1/souls (ListTyped) — видимость scoped по RBAC (ADR-047 S3b): оператор видит только
// хосты в своей scope-границе (`soul.list` Purview). scope прозрачен для клиента —
// деривируется из JWT, НЕ query-параметр; coven-query (filter) сужает ВНУТРИ scope (AND).
// Два режима, режим выбирает СЕРВЕР из Purview: coven-only/Unrestricted → offset-fast-path
// (SQL-pushdown, точный total, без next_cursor); regex-измерение → keyset-режим (S3b-2a,
// total_approximate, next_cursor). fail-closed (ОБРАТНО presence fail-SAFE): нет claims /
// nil-scoper / пустой Purview / битый regex → ПУСТОЙ список (не весь флот).

// SoulListInput — параметры [SoulHandler.ListTyped] (FULL-TYPED). Coven/Status/Transport —
// string-фильтры (пусто = не применять). Page/Cursor — уже распарсенные huma-слоем
// (offset+cursor конфликт и битый cursor разрешаются ДО ListTyped).
type SoulListInput struct {
	Coven     string
	Status    string
	Transport string
	Page      sharedapi.Page
	Cursor    *sharedapi.KeysetCursor
}

// SoulListReply — wire-тип ответа GET /v1/souls (paged-envelope SoulListView). Алиас на
// sharedapi.PagedResponse[SoulListView] — единая форма offset- и keyset-режимов (CURSOR,
// 6 полей). Пакет api проецирует его в native-envelope soulListReply через RegisterTypeAlias
// (handler-native T5d: element-схема SoulListView сводится на контрактную SoulListEntry).
type SoulListReply = sharedapi.PagedResponse[SoulListView]

// SoulStatsView — плоская доменная проекция 200-тела GET /v1/souls/stats (Souls
// Overview UI). Карты «значение оси → число хостов»; ключи — RAW string домена
// (status/transport в доменной форме). Transport — agent/ssh (НЕ pull/push):
// UI сам маппит на pull/push-лейблы. Пустые оси → пустые (не nil) карты, чтобы
// wire всегда нёс объект (а не null).
type SoulStatsView struct {
	ByStatus    map[string]int
	ByTransport map[string]int
	ByCoven     map[string]int
	Total       int
	StaleCount  int
}

// SoulStatsReply — результат [SoulHandler.StatsTyped]. Несёт доменную проекцию
// агрегата; пакет api проецирует её в native wire-DTO.
type SoulStatsReply struct {
	Body SoulStatsView
}

// StatsTyped — доменная функция GET /v1/souls/stats: агрегат реестра souls в
// границах Purview-scope оператора (тот же fail-closed scope-резолв, что и
// ListTyped). staleThreshold — cutoff «протухшего» last_seen_at, приходит из
// reaper.ResolveMarkDisconnectedStale (register-слой читает актуальный конфиг),
// чтобы stale_count совпадал с disconnect-порогом Reaper-а.
//
// fail-closed (симметрично ListTyped): нет claims / nil-scoper / пустой Purview →
// НУЛЕВОЙ агрегат (200, не 403) — не палим существование хостов вне scope.
// Ошибки — *problemError (500 PG). Partial-scope (soulprint/state-измерения,
// S3b-2b) деградирует в coven-агрегат: агрегат по scope-CTE НЕ применяет regex-
// измерение (coven-pushdown), поэтому regex-scoped оператор увидит агрегат ТОЛЬКО
// по coven-части своего Purview (строгое подмножество, never over-show) — то же
// поведение, что offset-fast-path списка.
func (h *SoulHandler) StatsTyped(ctx context.Context, claims *jwt.Claims, staleThreshold time.Duration) (SoulStatsReply, error) {
	scope, ok := h.resolveListScopeForClaims(claims)
	if !ok {
		// fail-closed: scope не определён → нулевой агрегат, НЕ весь флот.
		return SoulStatsReply{Body: emptySoulStatsView()}, nil
	}
	stats, err := soul.SelectStats(ctx, h.pool,
		soul.ListScope{Covens: scope.Covens, Unrestricted: scope.Unrestricted},
		staleThreshold)
	if err != nil {
		h.logger.Error("soul.stats: select failed", slog.Any("error", err))
		return SoulStatsReply{}, &problemError{problem.New(problem.TypeInternalError, "", "souls stats failed")}
	}
	return SoulStatsReply{Body: soulStatsView(stats)}, nil
}

// soulStatsView проецирует доменный [soul.Stats] в плоский wire-view (typed-карты
// осей → string-ключи). Инициализирует пустые карты как непустые, чтобы wire нёс
// объект даже для пустой оси.
func soulStatsView(s soul.Stats) SoulStatsView {
	v := SoulStatsView{
		ByStatus:    make(map[string]int, len(s.ByStatus)),
		ByTransport: make(map[string]int, len(s.ByTransport)),
		ByCoven:     make(map[string]int, len(s.ByCoven)),
		Total:       s.Total,
		StaleCount:  s.StaleCount,
	}
	for k, n := range s.ByStatus {
		v.ByStatus[string(k)] = n
	}
	for k, n := range s.ByTransport {
		v.ByTransport[string(k)] = n
	}
	for k, n := range s.ByCoven {
		v.ByCoven[k] = n
	}
	return v
}

// emptySoulStatsView — нулевой агрегат (fail-closed): пустые (не nil) карты + нули.
func emptySoulStatsView() SoulStatsView {
	return SoulStatsView{
		ByStatus:    map[string]int{},
		ByTransport: map[string]int{},
		ByCoven:     map[string]int{},
	}
}

// ListTyped — извлечённая доменная функция GET /v1/souls (FULL-TYPED): scoped-видимость
// (offset-fast-path либо keyset-режим, режим выбирает СЕРВЕР из Purview). fail-closed: нет
// claims / nil-scoper / пустой Purview / битый regex → ПУСТОЙ список (200). Ошибки —
// *problemError (422 невалидный status/transport-фильтр; 500 PG); успех — [SoulListReply].
func (h *SoulHandler) ListTyped(ctx context.Context, claims *jwt.Claims, in SoulListInput) (SoulListReply, error) {
	var zero SoulListReply
	var filter soul.ListFilter
	filter.Coven = in.Coven
	if in.Status != "" {
		st := soul.Status(in.Status)
		if !soul.ValidStatus(st) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"invalid 'status' filter: must be one of pending/connected/disconnected/revoked/expired/destroyed")}
		}
		filter.Status = st
	}
	if in.Transport != "" {
		t := soul.Transport(in.Transport)
		if !soul.ValidTransport(t) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"invalid 'transport' filter: must be one of agent/ssh")}
		}
		filter.Transport = t
	}

	scope, ok := h.resolveListScopeForClaims(claims)
	if !ok {
		// fail-closed: scope не определён → пустой список, НЕ весь флот (200, не 403).
		return h.emptyListReply(in.Page), nil
	}
	if scope.NeedsKeyset() {
		return h.listKeysetTyped(ctx, filter, scope, in.Page, in.Cursor)
	}
	return h.listOffsetTyped(ctx, filter, scope, in.Page)
}

// listOffsetTyped — coven-only / Unrestricted режим: SQL-pushdown offset-пагинация
// (backward-compatible путь S3b-0). total точен, next_cursor отсутствует.
func (h *SoulHandler) listOffsetTyped(ctx context.Context, filter soul.ListFilter, scope soulpurview.Scope, page sharedapi.Page) (SoulListReply, error) {
	items, total, err := soul.SelectAll(ctx, h.pool,
		filter, soul.ListScope{Covens: scope.Covens, Unrestricted: scope.Unrestricted},
		page.Offset, page.Limit)
	if err != nil {
		h.logger.Error("soul.list: select failed", slog.Any("filter", filter), slog.Any("error", err))
		return SoulListReply{}, &problemError{problem.New(problem.TypeInternalError, "", "list souls failed")}
	}
	dtos := make([]SoulListView, 0, len(items))
	for _, s := range items {
		dtos = append(dtos, toSoulListView(s))
	}
	h.overlayPresence(ctx, dtos)
	return SoulListReply{
		Items:  dtos,
		Offset: page.Offset,
		Limit:  page.Limit,
		Total:  total,
	}, nil
}

// listKeyset — regex-режим (ADR-047 S3b-2a): keyset-окно по `(registered_at,
// sid)` + Go-OR-постфильтр (covenMatch OR regexMatch) + добор до limit. Наличие
// regex отключает coven-SQL-pushdown (иначе AND сузил бы видимость НИЖЕ
// Purview); scope-union считается в Go. total не считается (total_approximate:true).
//
// Пользовательский filter (status/transport/coven из query-params) пробрасывается
// в [soul.ListForScopeEval] и применяется как SQL WHERE — он сужает ВНУТРИ scope
// (AND), не расширяет. Итоговая видимость хоста ⟺ (filter в SQL) AND (scope-union
// в Go-eval). Без этого keyset-режим молча игнорировал бы фильтры (regex-scoped
// оператор с `?status=connected` видел бы и pending-хосты — тихий недо-фильтр).
//
// Инвариант next_cursor — «продолжить с места, где перестали ПРОСМАТРИВАТЬ»:
// каждая прочитанная из БД строка Go-eval-ится (просмотрена), поэтому курсор
// кодирует ПОСЛЕДНЮЮ ПРОСМОТРЕННУЮ строку (`bound`), а не последнюю отданную.
//   - нормальный путь (набор по limit): скан останавливается на заполнении →
//     последняя просмотренная == последняя отданная, курсор не «убегает» вперёд.
//   - cap-выход (узкий regex, упёрлись в cap внутренних страниц): `bound` >
//     последней отданной — клиент дочитает скан со следующего запроса. Иначе
//     при пустой странице next_cursor не выдался бы (lastEmitted==nil), клиент
//     остановился бы, и матчащие хосты за первыми cap-страницами терялись бы
//     навсегда (нарушение keyset «без пропусков»).
//   - exhausted (БД пройдена): курсора нет — конец. Резюме от `bound` корректен:
//     всё ≤ bound просмотрено (прошедшие отданы, не-прошедшие отброшены — они вне
//     Purview), > bound ещё не тронуто → ни дублей, ни пропусков.
//
// scope-eval сужает набор ДО presence-overlay-я: presence навешивается только
// на прошедшие scope элементы (scope fail-CLOSED, presence fail-SAFE — два
// разных слоя, см. overlayPresence).
//
// Битый/слишком длинный regex (CompileScope error) → fail-closed: пустой список,
// НЕ 500 (scope-eval-error скрывает).
func (h *SoulHandler) listKeysetTyped(ctx context.Context, filter soul.ListFilter, scope soulpurview.Scope, page sharedapi.Page, cursor *sharedapi.KeysetCursor) (SoulListReply, error) {
	compiled, err := soulpurview.CompileScope(scope)
	if err != nil {
		// scope-eval-error fail-CLOSED: битый regex в Purview не показывает флот
		// и не падает в 500 — скрывает (пустой список).
		h.logger.Warn("soul.list: scope regex compile failed — fail-closed (пустой список)",
			slog.Any("error", err))
		return h.emptyListReply(page), nil
	}

	var bound *soul.KeysetCursorBound
	if cursor != nil {
		bound = &soul.KeysetCursorBound{RegisteredAt: cursor.RegisteredAt, SID: cursor.SID}
	}

	innerPageSize := scopeEvalInnerPageSize
	if h.scopeEvalInnerPageSize > 0 {
		innerPageSize = h.scopeEvalInnerPageSize
	}
	maxInnerPages := scopeEvalMaxInnerPages
	if h.scopeEvalMaxInnerPages > 0 {
		maxInnerPages = h.scopeEvalMaxInnerPages
	}

	collected := make([]SoulListView, 0, page.Limit)
	exhausted := false

	for pages := 0; pages < maxInnerPages; pages++ {
		rows, err := soul.ListForScopeEval(ctx, h.pool, filter, bound, innerPageSize)
		if err != nil {
			h.logger.Error("soul.list: scope-eval query failed", slog.Any("error", err))
			return SoulListReply{}, &problemError{problem.New(problem.TypeInternalError, "", "list souls failed")}
		}
		if len(rows) == 0 {
			exhausted = true
			break
		}
		filled := false
		for i := range rows {
			row := rows[i]
			// bound = последняя ПРОСМОТРЕННАЯ строка. Двигаем на каждой строке
			// (а не только на конце окна): при выходе по limt внутри окна bound
			// останавливается ровно на последней отданной, не убегая на хвост
			// окна, который мы НЕ просматривали.
			bound = &soul.KeysetCursorBound{RegisteredAt: row.RegisteredAt, SID: row.SID}
			if compiled.Visible(row.SID, row.Coven) {
				collected = append(collected, scopeEvalRowToListItem(row))
				if len(collected) == page.Limit {
					filled = true
					break
				}
			}
		}
		if filled {
			break
		}
		if len(rows) < innerPageSize {
			exhausted = true
			break
		}
	}

	h.overlayPresence(ctx, collected)

	resp := SoulListReply{
		Items:            collected,
		Offset:           page.Offset,
		Limit:            page.Limit,
		Total:            0,
		TotalApproximate: true,
	}
	// next_cursor отсутствует ТОЛЬКО когда БД исчерпана (весь флот просмотрен).
	// Иначе (набор по limit ИЛИ cap-выход) — есть ещё, курсор = последняя
	// ПРОСМОТРЕННАЯ строка (bound), чтобы клиент дочитал скан без пропусков.
	if !exhausted && bound != nil {
		enc := sharedapi.EncodeKeysetCursor(sharedapi.KeysetCursor{
			RegisteredAt: bound.RegisteredAt,
			SID:          bound.SID,
		})
		resp.NextCursor = &enc
	}
	return resp, nil
}

// emptyListReply — fail-closed/пустой ответ списка (200 + items:[]). total точен (пусто —
// точный факт): TotalApproximate опущен, без next_cursor.
func (h *SoulHandler) emptyListReply(page sharedapi.Page) SoulListReply {
	return SoulListReply{
		Items:  []SoulListView{},
		Offset: page.Offset,
		Limit:  page.Limit,
		Total:  0,
	}
}

// scopeEvalRowToListItem — проекция полной [soul.ScopeEvalRow] в [SoulListView].
// Карточка формо-идентична offset-режиму (toSoulListView): несёт status/
// transport/last_seen/created_by_aid/requested_at — иначе presence-overlay не
// флипнул бы status (он опускается на пустом снимке), и `GET /v1/souls` отдавал
// бы разную форму карточки по Purview оператора.
func scopeEvalRowToListItem(row soul.ScopeEvalRow) SoulListView {
	item := SoulListView{
		SID:           row.SID,
		Transport:     string(row.Transport),
		Status:        string(row.Status),
		Covens:        coalesceCoven(row.Coven),
		Traits:        coalesceTraits(row.Traits),
		LastSeenByKid: row.LastSeenByKID,
		RegisteredAt:  row.RegisteredAt.UTC(),
		CreatedByAID:  row.CreatedByAID,
	}
	if row.LastSeenAt != nil {
		t := row.LastSeenAt.UTC()
		item.LastSeenAt = &t
	}
	if row.RequestedAt != nil {
		t := row.RequestedAt.UTC()
		item.RequestedAt = &t
	}
	return item
}

// resolveListScope деривирует RBAC scope-границу `GET /v1/souls` из Purview
// оператора (ADR-047 S3b). Возвращает (scope, true) — применить; (_, false) —
// fail-closed (вызывающий отдаёт пустой список).
//
// fail-closed-ветки (НЕ весь флот при сомнении — ПРОТИВОПОЛОЖНО presence
// fail-safe):
//   - нет claims в контексте (защита: route под RequireJWT, недостижимо штатно);
//   - scoper не сконфигурирован (nil) — не строим scope, скрываем всё;
//   - Purview пуст ([soulpurview.Scope].Empty) — оператору не положено ни одного
//     хоста (default-deny без вычислимого измерения).
//
// Partial-scope (введены soulprint/state — S3b-2b): вычислимые измерения
// (coven/regex) применяются, soulprint/state опускаются (строгое подмножество,
// никогда НЕ over-show), warn-ом фиксируя недопоказ.
func (h *SoulHandler) resolveListScopeForClaims(claims *jwt.Claims) (soulpurview.Scope, bool) {
	if claims == nil || h.scoper == nil {
		return soulpurview.Scope{}, false
	}
	sc := soulpurview.Resolve(h.scoper.ResolvePurview(claims.Subject, "soul", "list"))
	if sc.Empty {
		return soulpurview.Scope{}, false
	}
	if sc.Partial {
		// coven/regex применяются; soulprint/state (S3b-2b) опускаются. Под-показ
		// безопасен (fail-closed-сторона), но фиксируем — оператор может
		// недосчитаться видимых хостов до реализации soulprint-постфильтра.
		h.logger.Warn("soul.list: scope содержит не-вычисляемые измерения (soulprint/state) — применены только coven/regex, часть доступных хостов скрыта (S3b-2b)",
			slog.String("aid", claims.Subject),
			slog.Any("covens", sc.Covens),
			slog.Any("regexes", sc.Regexes))
	}
	return sc, true
}

// GetTyped — извлечённая доменная функция GET /v1/souls/{sid} (FULL-TYPED): single-soul
// read со scope-гейтом. claims — для readScopeForClaims (вне scope → 404, не палим чужой
// хост). Ошибки — *problemError (422 невалидный sid; 404 нет soul / вне scope; 500 PG);
// успех — [SoulListView] (та же проекция, что в list).
func (h *SoulHandler) GetTyped(ctx context.Context, claims *jwt.Claims, sid string) (SoulListView, error) {
	var zero SoulListView
	if !soul.ValidSID(sid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}
	s, err := soul.SelectBySID(ctx, h.pool, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		}
		h.logger.Error("soul.get: select failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get soul failed")}
	}
	if !soulpurview.InScope(h.readScopeForClaims(claims), sid, s.Coven) {
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
	}
	dtos := []SoulListView{toSoulListView(s)}
	h.overlayPresence(ctx, dtos)
	return dtos[0], nil
}

// readScopeForClaims деривирует scope-границу single-read из Purview оператора
// (`soul.list`, coven-измерение в пилоте). fail-closed: nil claims / nil-scoper →
// [soulpurview.Scope]{Empty:true} → [soulpurview.InScope] false → 404. Симметрично
// [SoulHandler.resolveListScope], но Scope (single-object через InScope), а не
// [soul.ListScope] (SQL-pushdown списка).
func (h *SoulHandler) readScopeForClaims(claims *jwt.Claims) soulpurview.Scope {
	if claims == nil || h.scoper == nil {
		return soulpurview.Scope{Empty: true}
	}
	return soulpurview.Resolve(h.scoper.ResolvePurview(claims.Subject, "soul", "list"))
}

// soulprintReadReply — 200-body GET /v1/souls/{sid}/soulprint. Проекция
// `souls.{soulprint_facts, soulprint_collected_at, soulprint_received_at}`. Имя
// типа = контрактное имя схемы рукописи (docs/keeper/openapi.yaml :6858 →
// SoulprintReadReply): huma DefaultSchemaNamer капитализирует первую букву →
// "SoulprintReadReply".
//
// typed_facts — byte-passthrough `json.RawMessage` (категория D, ADR-051/ADR-018
// amend): сырые JSONB-байты `souls.soulprint_facts` отдаются AS-IS, без
// unmarshal→map→re-marshal. Этим гарантируется forward-compat — новые proto-поля
// `SoulprintFacts` Soul-агента доезжают на wire без рекомпиляции Keeper-а (Keeper
// не парсит содержимое). Порядок ключей — PG-jsonb-нормализованный (jsonb-колонка
// перенормализует при записи). Прежний `map[string]any`-путь сортировал ключи
// лексикографически на re-marshal; byte-passthrough отдаёт jsonb-порядок —
// одноразовый намеренный wire-change порядка ключей (под guard-тестом).
type soulprintReadReply struct {
	SID         string          `json:"sid"`
	TypedFacts  json.RawMessage `json:"typed_facts"`
	CollectedAt *time.Time      `json:"collected_at,omitempty"`
	ReceivedAt  *time.Time      `json:"received_at,omitempty"`
}

// GetSoulprint — GET /v1/souls/{sid}/soulprint.
//
// Возвращает последний полученный typed-SoulprintReport (`SoulprintFacts`,
// ADR-018). Permission — `soul.list` (тот же, что для get; rbac.md note —
// `soul.get` отложен до отдельного PR, list-permission покрывает чтение
// детали и soulprint — симметрично service.list / omen.list).
//
// Видимость scoped по RBAC (ADR-047 S3b-1, тот же [readScope]-гейт, что у Get):
// scope-проверка идёт ДО раскрытия фактов — covens хоста берутся отдельным
// фетчем реестра ([soul.SelectBySID]; SelectSoulprint их не возвращает). Вне
// scope / fail-closed → 404 (как для несуществующего SID, не палим существование
// чужого хоста и не раскрываем его факты).
//
// Контракт:
//   - 200 + `{sid, typed_facts, collected_at, received_at}`.
//   - 404 (not-found) — SID отсутствует в реестре `souls` ЛИБО вне scope оператора.
//   - 410 (gone, soulprint-not-received) — запись Soul-а есть, но
//     `SoulprintReport` ни разу не приходил (`soulprint_facts IS NULL`).
//     Отдельный код vs 404 — UI решает «нет данных пока» vs «нет хоста».
//   - 422 — невалидный path-SID.
//
// typed_facts — byte-passthrough (категория D): сырой JSONB отдаётся as-is, без
// unmarshal-валидации, поэтому прежний 500 на «битый JSONB» снят — storage-
// инвариант (eventstream пишет через `protojson.Marshal` валидный JSON в jsonb-
// колонку, которая сама отвергнет невалидный JSON на записи) гарантирует
// валидность, и Keeper не дублирует проверку (forward-compat — не парсим).

// SoulprintReadReply — экспортированный алиас на wire-тип ответа GET /v1/souls/{sid}/soulprint
// (typed_facts byte-passthrough). Через него huma-роут (пакет api) типизирует 200-output.
type SoulprintReadReply = soulprintReadReply

// GetSoulprintTyped — извлечённая доменная функция GET /v1/souls/{sid}/soulprint (FULL-TYPED):
// typed-SoulprintReport со scope-гейтом ДО раскрытия фактов. Ошибки — *problemError (422
// невалидный sid; 404 нет soul / вне scope; 410 soulprint не получен; 500 PG); успех —
// [SoulprintReadReply].
func (h *SoulHandler) GetSoulprintTyped(ctx context.Context, claims *jwt.Claims, sid string) (SoulprintReadReply, error) {
	var zero SoulprintReadReply
	if !soul.ValidSID(sid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}

	// scope-гейт ДО раскрытия фактов: covens хоста — из реестра (SelectSoulprint их
	// не несёт). not-found хоста и not-found из-за scope дают один 404.
	s, err := soul.SelectBySID(ctx, h.pool, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		}
		h.logger.Error("soul.soulprint.get: scope select failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get soulprint failed")}
	}
	if !soulpurview.InScope(h.readScopeForClaims(claims), sid, s.Coven) {
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
	}

	rec, err := soul.SelectSoulprint(ctx, h.pool, sid)
	if err != nil {
		switch {
		case errors.Is(err, soul.ErrSoulNotFound):
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		case errors.Is(err, soul.ErrSoulprintNotReceived):
			return zero, &problemError{problem.New(problem.TypeSoulprintNotReceived, "",
				"soulprint for soul "+sid+" has not been received yet")}
		}
		h.logger.Error("soul.soulprint.get: select failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get soulprint failed")}
	}

	resp := soulprintReadReply{
		SID:        rec.SID,
		TypedFacts: json.RawMessage(rec.FactsJSON),
	}
	if !rec.CollectedAt.IsZero() {
		t := rec.CollectedAt
		resp.CollectedAt = &t
	}
	if !rec.ReceivedAt.IsZero() {
		t := rec.ReceivedAt
		resp.ReceivedAt = &t
	}
	return resp, nil
}

// SoulHistoryItemView — ПЛОСКАЯ доменная проекция одной записи per-host timeline
// (handler-native T5d). Пакет api проецирует её в native-схему SoulHistoryItem. Type — RAW
// string домена (native-тип в api держит enum-форму). Поля, специфичные для одного источника,
// pointer-optional (incarnation/scenario — только scenario; module — только errand; voyage_id —
// back-link на Voyage; nil → ключ опущен native-типом). date-time — СЕКУНДНЫЙ wire.
type SoulHistoryItemView struct {
	Type        string
	ID          string
	Status      string
	StartedAt   time.Time
	FinishedAt  *time.Time
	Incarnation *string
	Scenario    *string
	Module      *string
	VoyageID    *string
}

// SoulHistoryView — доменный результат GET /v1/souls/{sid}/history (handler-native T5d).
// Пакет api проецирует {SID, Items, Offset, Limit, Total} → native SoulHistoryReply
// (самостоятельный envelope с top-level sid, НЕ generic PagedResponse).
type SoulHistoryView struct {
	SID    string
	Items  []SoulHistoryItemView
	Offset int
	Limit  int
	Total  int
}

// toSoulHistoryItemView — проекция [soul.HistoryItem] в доменный [SoulHistoryItemView].
// date-time started_at/finished_at — СЕКУНДНЫЙ wire (.UTC().Truncate(time.Second), parity
// легаси RFC3339-секунд). incarnation/scenario/module/voyage_id — пустая строка → nil → ключ
// опущен (байт-в-байт со старым `string ...,omitempty`-DTO).
func toSoulHistoryItemView(it soul.HistoryItem) SoulHistoryItemView {
	dto := SoulHistoryItemView{
		Type:      string(it.Type),
		ID:        it.ID,
		Status:    it.Status,
		StartedAt: it.StartedAt.UTC().Truncate(time.Second),
	}
	if it.Incarnation != "" {
		v := it.Incarnation
		dto.Incarnation = &v
	}
	if it.Scenario != "" {
		v := it.Scenario
		dto.Scenario = &v
	}
	if it.Module != "" {
		v := it.Module
		dto.Module = &v
	}
	if it.FinishedAt != nil {
		t := it.FinishedAt.UTC().Truncate(time.Second)
		dto.FinishedAt = &t
	}
	if it.VoyageID != nil {
		dto.VoyageID = it.VoyageID
	}
	return dto
}

// GET /v1/souls/{sid}/history (HistoryTyped) — агрегированный per-host timeline:
// scenario-задачи (`apply_runs`) + ad-hoc exec (`errands`) под целевым SID, merge по
// started_at DESC. Permission — `soul.list`. Видимость scoped по RBAC (ADR-047 §г, паттерн
// 1:1 с GetSoulprint): scope-проверка ДО раскрытия timeline — covens хоста отдельным фетчем
// реестра ([soul.SelectBySID]). Вне scope / fail-closed → 404 (не палим чужой хост).
// Revoked-оператор отрезается revoked-aware [rbac.Enforcer.ResolvePurview].

// SoulHistoryInput — параметры [SoulHandler.HistoryTyped] (FULL-TYPED). Types — multi-value
// (scenario|errand). Since — нулевой time.Time → фильтр границы не применяется (huma на bad
// date-time даёт 400 при bind; legacy 422 недостижим через router — единственный source).
// Offset/Limit — диапазон enforce-ит CheckPageBounds → 400.
type SoulHistoryInput struct {
	SID    string
	Types  []string
	Since  time.Time
	Offset int
	Limit  int
}

// HistoryTyped — извлечённая доменная функция GET /v1/souls/{sid}/history (FULL-TYPED):
// per-host timeline (apply_runs + errands) со scope-гейтом ДО раскрытия. Ошибки —
// *problemError (400 out-of-range pagination; 422 невалидный sid / type; 404 нет soul / вне
// scope; 500 PG); успех — [SoulHistoryView].
func (h *SoulHandler) HistoryTyped(ctx context.Context, claims *jwt.Claims, in SoulHistoryInput) (SoulHistoryView, error) {
	var zero SoulHistoryView
	if !soul.ValidSID(in.SID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}
	if err := sharedapi.CheckPageBounds(in.Offset, in.Limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	// scope-гейт ДО раскрытия timeline: covens хоста — из реестра (SelectHistory их
	// не несёт). not-found хоста и not-found из-за scope дают один 404.
	s, err := soul.SelectBySID(ctx, h.pool, in.SID)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+in.SID+" not found")}
		}
		h.logger.Error("soul.history: scope select failed", slog.String("sid", in.SID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get soul history failed")}
	}
	if !soulpurview.InScope(h.readScopeForClaims(claims), in.SID, s.Coven) {
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+in.SID+" not found")}
	}

	filter := soul.HistoryFilter{SID: in.SID, Since: in.Since}
	for _, t := range in.Types {
		ht := soul.HistoryType(t)
		if !soul.ValidHistoryType(ht) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "query 'type' must be one of scenario/errand")}
		}
		filter.Types = append(filter.Types, ht)
	}

	items, total, err := soul.SelectHistory(ctx, h.pool, filter, in.Offset, in.Limit)
	if err != nil {
		h.logger.Error("soul.history: select failed", slog.String("sid", in.SID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get soul history failed")}
	}

	dtos := make([]SoulHistoryItemView, 0, len(items))
	for _, it := range items {
		dtos = append(dtos, toSoulHistoryItemView(it))
	}
	return SoulHistoryView{SID: in.SID, Items: dtos, Offset: in.Offset, Limit: in.Limit, Total: total}, nil
}

// SoulCovenAssignInput — NATIVE request-форма POST /v1/souls/coven (handler-native T5d).
// Заменяет SoulCovenAssignRequest: huma-input (пакет api) биндит тело по своим полям,
// затем зовёт AssignCovenTyped с этой плоской моделью.
//
// `label` (одна метка) и `labels` (набор) XOR по mode:
//   - mode=append/remove → label обязателен, labels запрещён;
//   - mode=replace → labels обязателен (может быть пустым = «снять все»), label запрещён.
//
// Selector — подмножество словаря таргетинга soul.* (all/sids/coven/incarnation/status).
// Свободный CEL-предикат сознательно НЕ поддержан (ломает доказуемость scope-проверки).
type SoulCovenAssignInput struct {
	Mode     string
	Label    string
	Labels   []string
	DryRun   bool
	Selector SoulCovenAssignSelectorInput
}

// SoulCovenAssignSelectorInput — NATIVE форма селектора coven-assign (handler-native T5d).
type SoulCovenAssignSelectorInput struct {
	All         bool
	SIDs        []string
	Coven       string
	Incarnation string
	Status      string
}

// covenAssignFields — value-снимок полей запроса coven-assign (nil → zero), сохраняет
// прежнюю логику XOR/валидации без рассеянных nil-чеков.
type covenAssignFields struct {
	Mode     string
	Label    string
	Labels   []string
	DryRun   bool
	Selector covenAssignSelectorFields
}

// covenAssignSelectorFields — value-снимок pointer-optional полей селектора.
type covenAssignSelectorFields struct {
	All         bool
	SIDs        []string
	Coven       string
	Incarnation string
	Status      string
}

func derefCovenAssign(req SoulCovenAssignInput) covenAssignFields {
	return covenAssignFields{
		Mode:   req.Mode,
		Label:  req.Label,
		Labels: req.Labels,
		DryRun: req.DryRun,
		Selector: covenAssignSelectorFields{
			All:         req.Selector.All,
			SIDs:        req.Selector.SIDs,
			Coven:       req.Selector.Coven,
			Incarnation: req.Selector.Incarnation,
			Status:      req.Selector.Status,
		},
	}
}

// soulCovenAssignResponse — 200 body. status ∈ completed | partial. Для
// mode=replace `label` отсутствует, `labels` отражает применённый набор
// (включая пустой [] для «снять все»). MarshalJSON решает XOR на сериализации:
// `omitempty` на []string не годится (пустой набор для replace надо отдать как
// `[]`, а не опустить поле). json-теги — для документации формы (Marshal
// делает map; Unmarshal не используется на server-side).
type soulCovenAssignResponse struct {
	Mode    string   `json:"mode"`
	Label   string   `json:"label,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	HasSet  bool     `json:"-"`
	Matched int      `json:"matched"`
	Changed int      `json:"changed"`
	Status  string   `json:"status"`
	DryRun  bool     `json:"dry_run"`
}

// SoulCovenAssignResponse — экспортированный алиас на внутренний wire-тип (custom MarshalJSON
// XOR label↔labels), через который huma-роут (пакет api) типизирует output без форка wire-
// формы (huma строит схему из полей, сериализует через тот же тип; схему имени выравнивает
// alias soulCovenAssignReply в huma_soul_envelope.go).
type SoulCovenAssignResponse = soulCovenAssignResponse

// MarshalJSON собирает поля XOR-сериализацией label↔labels по HasSet.
func (r soulCovenAssignResponse) MarshalJSON() ([]byte, error) {
	out := map[string]any{
		"mode":    r.Mode,
		"matched": r.Matched,
		"changed": r.Changed,
		"status":  r.Status,
		"dry_run": r.DryRun,
	}
	if r.HasSet {
		labels := r.Labels
		if labels == nil {
			labels = []string{}
		}
		out["labels"] = labels
	} else {
		out["label"] = r.Label
	}
	return json.Marshal(out)
}

// AssignCoven — POST /v1/souls/coven.
//
// Массово добавляет (append) / снимает (remove) ОДНУ Coven-метку либо
// ЗАМЕНЯЕТ (replace) набор Coven-меток на хостах под selector ∩ scope
// оператора. Coven — холодная PG-метка: чистый UPDATE souls, без Redis.
// Permission-гейт (`soul.coven-assign`) ставит middleware; scope-intersection
// (целевые хосты ⊆ scope + назначаемая метка / каждая из replace-набора ∈
// scope) — здесь, ДО UPDATE: без него bulk = privilege-escalation.
//
// XOR-форма тела: для append/remove обязателен `label`, `labels` запрещён;
// для replace обязателен `labels` (может быть пустым = снять все), `label`
// запрещён.
//
// dry_run (body или `?dry_run=true`) — вернуть matched под selector ∩ scope
// без UPDATE.
//
// Контракт:
//   - 200 + {mode, label?, labels?, matched, changed, status, dry_run}.
//   - 400 — невалидный JSON.
//   - 422 — невалидный mode / label(s) / status / incarnation / пустой
//     selector / метка вне scope оператора / XOR-нарушение (label+labels).
//   - 500 — scoper не сконфигурирован или ошибка БД.
//
// partial-семантика (часть чанков закоммичена, затем фейл) отдаётся как 200
// со `status: partial` — закоммиченные изменения идемпотентно до-повторяются
// оператором, откатывать их небезопасно. PG-ошибка ДО первого коммита (count
// или первый чанк) — 500.

// SoulCovenAssignReply — результат [SoulHandler.AssignCovenTyped] (handler-native).
// Несёт 200-тело ([soulCovenAssignResponse] с custom MarshalJSON XOR label↔labels) +
// audit-payload.
type SoulCovenAssignReply struct {
	Body         soulCovenAssignResponse
	AuditPayload middleware.AuditPayload
}

// AssignCovenTyped — доменная функция POST /v1/souls/coven (handler-native): bulk
// coven-assign со scope-intersection. rawReq — native input; dryRunQuery — флаг из
// `?dry_run=true` (OR с body.dry_run). Ошибки — *problemError (422 невалидный mode/label(s)/
// selector / XOR-нарушение / метка вне scope; 500 scoper nil / PG); успех —
// [SoulCovenAssignReply] (200-тело + audit-payload, в т.ч. partial-семантика → 200).
func (h *SoulHandler) AssignCovenTyped(ctx context.Context, claims *jwt.Claims, rawReq SoulCovenAssignInput, dryRunQuery bool) (SoulCovenAssignReply, error) {
	var zero SoulCovenAssignReply
	if h.scoper == nil {
		h.logger.Error("soul.coven-assign: scoper not configured")
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "coven-assign unavailable")}
	}
	req := derefCovenAssign(rawReq)

	mode := soul.CovenMode(req.Mode)
	if !soul.ValidCovenMode(mode) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'mode' must be one of: append, remove, replace")}
	}

	// XOR label↔labels по mode. append/remove оперируют ОДНОЙ меткой
	// (array_append/array_remove над scalar-ом); replace принимает НАБОР
	// целиком. Смешение полей — программная ошибка вызывающего, отвергаем
	// до семантической валидации.
	switch mode {
	case soul.CovenAppend, soul.CovenRemove:
		if len(req.Labels) > 0 {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'labels' is allowed only for mode=replace; use 'label' for append/remove")}
		}
		if !soul.ValidCoven(req.Label) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'label' must match "+soul.CovenPattern)}
		}
		if err := h.covenLabelValidator().Validate(req.Label); err != nil {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		}
	case soul.CovenReplace:
		if req.Label != "" {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'label' is allowed only for mode=append/remove; use 'labels' for replace")}
		}
		// Пустой набор разрешён (= снять все метки); валидируем формат и
		// проводим через label-валидатор каждую метку.
		for _, l := range req.Labels {
			if !soul.ValidCoven(l) {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "labels entry "+l+" must match "+soul.CovenPattern)}
			}
			if err := h.covenLabelValidator().Validate(l); err != nil {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
			}
		}
	}

	sel := soul.BulkSelector{
		All:         req.Selector.All,
		SIDs:        req.Selector.SIDs,
		Coven:       req.Selector.Coven,
		Incarnation: req.Selector.Incarnation,
	}
	if req.Selector.Status != "" {
		st := soul.Status(req.Selector.Status)
		if !soul.ValidStatus(st) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"selector 'status' must be one of pending/connected/disconnected/revoked/expired/destroyed")}
		}
		sel.Status = st
	}
	for _, s := range req.Selector.SIDs {
		if !soul.ValidSID(s) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'sids' entry "+s+" must match "+soul.SIDPattern)}
		}
	}
	if req.Selector.Coven != "" && !soul.ValidCoven(req.Selector.Coven) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'coven' must match "+soul.CovenPattern)}
	}
	if req.Selector.Incarnation != "" && !incarnation.ValidName(req.Selector.Incarnation) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'incarnation' must match "+incarnation.NamePattern)}
	}

	// coven-scope bulk-операции = coven-измерение Purview оператора (тот же
	// резолвер, что у List — обобщённый PurviewResolver).
	pv := h.scoper.ResolvePurview(claims.Subject, "soul", "coven-assign")
	scope := soul.BulkScope{Covens: pv.Covens, Unrestricted: pv.Unrestricted}

	dryRun := req.DryRun || dryRunQuery

	// Для replace гейт (b) применяем явно до dry_run-COUNT (как BulkAssignCoven
	// делает для append): out-of-scope метка должна отвергаться ДО любого
	// обращения к БД, иначе COUNT даст misleading-matched.
	if mode == soul.CovenReplace && !scope.Unrestricted {
		labelScope := soulpurview.Scope{Covens: scope.Covens}
		for _, l := range req.Labels {
			if !soulpurview.InScope(labelScope, "", []string{l}) {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "label is outside operator coven-scope")}
			}
		}
	}

	if dryRun {
		matched, err := soul.CountBulkMatched(ctx, h.pool, sel, scope)
		if err != nil {
			return zero, h.bulkErrorToProblem(err)
		}
		return h.buildCovenAssignReply(req, mode, scope, soul.Report{Matched: matched, Status: soul.BulkCompleted}, true), nil
	}

	var (
		rep soul.Report
		err error
	)
	if mode == soul.CovenReplace {
		rep, err = soul.BulkReplaceCoven(ctx, h.pool, sel, scope, req.Labels)
	} else {
		rep, err = soul.BulkAssignCoven(ctx, h.pool, sel, scope, req.Label, mode)
	}
	if err != nil {
		// partial: часть чанков закоммичена — отдаём 200 + status:partial, чтобы
		// оператор видел сделанное и мог до-повторить (идемпотентно).
		if rep.Status == soul.BulkPartial {
			h.logger.Warn("soul.coven-assign: partial",
				slog.String("label", req.Label),
				slog.Any("labels", req.Labels),
				slog.String("mode", req.Mode),
				slog.Int("changed", rep.Changed),
				slog.Int("chunks", rep.ChunksCommitted),
				slog.Any("error", err),
			)
			return h.buildCovenAssignReply(req, mode, scope, rep, false), nil
		}
		return zero, h.bulkErrorToProblem(err)
	}

	return h.buildCovenAssignReply(req, mode, scope, rep, false), nil
}

// bulkErrorToProblem маппит ошибки bulk-слоя в *problemError.
func (h *SoulHandler) bulkErrorToProblem(err error) error {
	switch {
	case errors.Is(err, soul.ErrBulkEmptySelector):
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"selector matches no hosts: set one of all/sids/coven/status")}
	case errors.Is(err, soul.ErrBulkLabelOutOfScope):
		return &problemError{problem.New(problem.TypeValidationFailed, "", "label is outside operator coven-scope")}
	default:
		h.logger.Error("soul.coven-assign: failed", slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "coven-assign failed")}
	}
}

// respondCovenAssign пишет audit-payload + 200-ответ.
//
// Поля `label`/`labels` отражают применённую форму mode-а: для append/remove —
// `label`; для replace — `labels` (всегда массив, в т.ч. пустой при «снять
// все»). Audit-payload симметричен ответу.
func (h *SoulHandler) buildCovenAssignReply(req covenAssignFields, mode soul.CovenMode, scope soul.BulkScope, rep soul.Report, dryRun bool) SoulCovenAssignReply {
	scopeApplied := !scope.Unrestricted
	payload := middleware.AuditPayload{
		"mode":          string(mode),
		"selector":      normalizeCovenSelector(req.Selector),
		"matched":       rep.Matched,
		"changed":       rep.Changed,
		"status":        string(rep.Status),
		"scope_applied": scopeApplied,
		"dry_run":       dryRun,
		"source":        "api",
	}
	resp := soulCovenAssignResponse{
		Mode:    string(mode),
		Matched: rep.Matched,
		Changed: rep.Changed,
		Status:  string(rep.Status),
		DryRun:  dryRun,
	}
	if mode == soul.CovenReplace {
		// nil → []string{} для устойчивого JSON `[]` (replace с пустым набором —
		// валидная операция «снять все метки»).
		labels := req.Labels
		if labels == nil {
			labels = []string{}
		}
		payload["labels"] = labels
		resp.Labels = labels
		resp.HasSet = true
	} else {
		payload["label"] = req.Label
		resp.Label = req.Label
	}
	return SoulCovenAssignReply{Body: resp, AuditPayload: payload}
}

// normalizeCovenSelector — нормализованная форма селектора для audit-payload.
func normalizeCovenSelector(s covenAssignSelectorFields) map[string]any {
	out := map[string]any{"all": s.All}
	if len(s.SIDs) > 0 {
		out["sids"] = s.SIDs
	}
	if s.Coven != "" {
		out["coven"] = s.Coven
	}
	if s.Incarnation != "" {
		out["incarnation"] = s.Incarnation
	}
	if s.Status != "" {
		out["status"] = s.Status
	}
	return out
}

// covenLabelValidator возвращает активный CovenLabelValidator. В пилоте —
// format-only no-op (формат уже проверен ValidCoven); хук под будущий
// справочник окружений (Q1) без новых полей API.
func (h *SoulHandler) covenLabelValidator() soul.CovenLabelValidator {
	return soul.NoopCovenLabelValidator{}
}

// SoulCovenLabelSelector — middleware-helper для RBAC bulk coven-assign
// (`POST /v1/souls/coven`): извлекает назначаемую метку из JSON-body и
// возвращает `{"coven": label}` для permission-проверки (rbac.md § селектор
// `coven=`). Это гейт (b) — coven-scoped оператор проходит middleware только
// для метки в своём scope; bare/`*` — для любой.
//
// Body вычитывается (под уже навешенным MaxBytesReader-лимитом /v1/*) и
// восстанавливается для handler-а через io.NopCloser над буфером — handler
// декодирует тело повторно. Невалидный/пустой body → nil-селектор: тогда
// permission со `coven=`-селектором не сматчит (deny на middleware), а
// bare/`*` — пройдёт, и handler вернёт 400 на битом JSON. Это безопасно:
// под-привилегированный оператор без подходящего scope не проходит дальше.
//
// Mode=replace отдаёт `labels[]` вместо одной `label`. Enforcer.Matches не
// поддерживает multi-value selector → возвращаем первую метку набора как
// заявку «оператор хотя бы для одной из меток имеет право»; КАЖДАЯ метка
// набора повторно проверяется handler-side гейтом (b) перед БД — middleware
// здесь лишь грубый фильтр (deny под-привилегированного без подходящего
// scope), service-уровень закрывает остаток.
func SoulCovenLabelSelector(r *http.Request) map[string]string {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	// Восстанавливаем тело для handler-а в любом случае (даже на ошибке/пусто).
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) == 0 {
		return nil
	}
	var probe struct {
		Label  string   `json:"label"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil
	}
	if probe.Label != "" {
		return map[string]string{"coven": probe.Label}
	}
	if len(probe.Labels) > 0 {
		return map[string]string{"coven": probe.Labels[0]}
	}
	// Пустые label/labels: для replace с пустым labels = «снять все» — пускаем
	// дальше bare-permission. Coven-scoped без scope-match отвалится на
	// service-гейте (BulkReplaceCoven проверит, что целевые хосты ⊆ scope).
	return nil
}

// === POST /v1/souls/traits (traits-assign) — bulk operator-set trait-метки (ADR-060) ===

// SoulTraitsAssignInput — NATIVE request-форма POST /v1/souls/traits (handler-native).
// Traits — это map (key → scalar|list), отдельная ось рядом с плоским Coven (read/target
// пилот уже в HEAD: souls.traits jsonb, soulprint.self.traits). Mode-семантика:
//   - merge (дефолт): set/overwrite ключи из Traits, остальные сохранить;
//   - replace: заменить ВЕСЬ traits-map на Traits целиком (пустой = очистить);
//   - remove: удалить ключи из Keys (список имён).
//
// XOR Traits↔Keys по mode: merge/replace принимают `traits` (map), remove — `keys`
// (список имён). Selector — то же подмножество таргетинга soul.* (all/sids/coven/
// incarnation/status), что у coven-assign.
type SoulTraitsAssignInput struct {
	Mode     string
	Traits   map[string]any
	Keys     []string
	DryRun   bool
	Selector SoulCovenAssignSelectorInput
}

// soulTraitsAssignResponse — 200 body. status ∈ completed | partial. `keys` — список
// затронутых trait-КЛЮЧЕЙ (merge/replace → ключи переданного набора; remove → удаляемые).
// Значения traits в ответе НЕ эхуются (симметрия с audit-payload: фиксируем форму операции,
// не содержимое — trait-значения могут нести инфраструктурные данные хоста).
type soulTraitsAssignResponse struct {
	Mode    string   `json:"mode"`
	Keys    []string `json:"keys"`
	Matched int      `json:"matched"`
	Changed int      `json:"changed"`
	Status  string   `json:"status"`
	DryRun  bool     `json:"dry_run"`
}

// SoulTraitsAssignResponse — экспортированный алиас на внутренний wire-тип (через него
// huma-роут типизирует output; имя схемы выравнивает alias soulTraitsAssignReply в
// huma_soul_envelope.go).
type SoulTraitsAssignResponse = soulTraitsAssignResponse

// SoulTraitsAssignReply — результат [SoulHandler.AssignTraitsTyped] (handler-native):
// 200-тело + audit-payload.
type SoulTraitsAssignReply struct {
	Body         soulTraitsAssignResponse
	AuditPayload middleware.AuditPayload
}

// AssignTraitsTyped — доменная функция POST /v1/souls/traits (handler-native): bulk
// trait-assign со scope-intersection (гейт a — целевые хосты ⊆ coven-scope оператора).
// rawReq — native input; dryRunQuery — флаг из `?dry_run=true` (OR с body.dry_run).
//
// БЕЗОПАСНОСТЬ. Least-privilege держится тем же [soul.BulkScope] (coven-scope оператора),
// что и coven-assign: bulk не ослаблен. trait-КЛЮЧ НЕ является RBAC-измерением scope (в
// отличие от Coven-метки), поэтому гейта (b) на ключи нет — coven-scoped оператор не может
// мутировать traits хостов вне своего coven-scope (гейт a в WHERE-предикате), но любой
// валидный ключ внутри scope ему доступен.
//
// Ошибки — *problemError (422 невалидный mode / ключ / значение / nested / XOR-нарушение /
// пустой selector; 500 scoper nil / PG); успех — [SoulTraitsAssignReply] (200-тело +
// audit-payload, в т.ч. partial-семантика → 200).
func (h *SoulHandler) AssignTraitsTyped(ctx context.Context, claims *jwt.Claims, rawReq SoulTraitsAssignInput, dryRunQuery bool) (SoulTraitsAssignReply, error) {
	var zero SoulTraitsAssignReply
	// DEPRECATED (ADR-060 amend R1): operator-set trait-управление перенесено
	// per-soul → per-incarnation (incarnation.traits — источник истины, PUT
	// /v1/incarnations/{name}/traits). Per-soul write перетирается следующей
	// проекцией. Эндпоинт сохранён forward-compat; вызов сигналим в лог.
	h.logger.Warn("soul.traits-assign: DEPRECATED per-soul trait-write (ADR-060) — используйте PUT /v1/incarnations/{name}/traits",
		slog.String("by_aid", claims.Subject))
	if h.scoper == nil {
		h.logger.Error("soul.traits-assign: scoper not configured")
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "traits-assign unavailable")}
	}

	mode := soul.TraitMode(rawReq.Mode)
	if mode == "" {
		mode = soul.TraitMerge // дефолт.
	}
	if !soul.ValidTraitMode(mode) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'mode' must be one of: merge, replace, remove")}
	}

	// XOR traits↔keys по mode + format/значение-валидация. merge/replace оперируют
	// map-ом ключ→значение; remove — списком имён ключей.
	switch mode {
	case soul.TraitMerge, soul.TraitReplace:
		if len(rawReq.Keys) > 0 {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'keys' is allowed only for mode=remove; use 'traits' for merge/replace")}
		}
		if err := soul.ValidateTraitDelta(rawReq.Traits); err != nil {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		}
	case soul.TraitRemove:
		if len(rawReq.Traits) > 0 {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'traits' is allowed only for mode=merge/replace; use 'keys' for remove")}
		}
		if len(rawReq.Keys) == 0 {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'keys' is required and must be non-empty for mode=remove")}
		}
		if err := soul.ValidateTraitKeys(rawReq.Keys); err != nil {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		}
	}

	sel := soul.BulkSelector{
		All:         rawReq.Selector.All,
		SIDs:        rawReq.Selector.SIDs,
		Coven:       rawReq.Selector.Coven,
		Incarnation: rawReq.Selector.Incarnation,
	}
	if rawReq.Selector.Status != "" {
		st := soul.Status(rawReq.Selector.Status)
		if !soul.ValidStatus(st) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"selector 'status' must be one of pending/connected/disconnected/revoked/expired/destroyed")}
		}
		sel.Status = st
	}
	for _, s := range rawReq.Selector.SIDs {
		if !soul.ValidSID(s) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'sids' entry "+s+" must match "+soul.SIDPattern)}
		}
	}
	if rawReq.Selector.Coven != "" && !soul.ValidCoven(rawReq.Selector.Coven) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'coven' must match "+soul.CovenPattern)}
	}
	if rawReq.Selector.Incarnation != "" && !incarnation.ValidName(rawReq.Selector.Incarnation) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "selector 'incarnation' must match "+incarnation.NamePattern)}
	}

	pv := h.scoper.ResolvePurview(claims.Subject, "soul", "traits-assign")
	scope := soul.BulkScope{Covens: pv.Covens, Unrestricted: pv.Unrestricted}

	dryRun := rawReq.DryRun || dryRunQuery

	if dryRun {
		matched, err := soul.CountBulkMatched(ctx, h.pool, sel, scope)
		if err != nil {
			return zero, h.bulkErrorToProblem(err)
		}
		return h.buildTraitsAssignReply(rawReq, mode, scope, soul.Report{Matched: matched, Status: soul.BulkCompleted}, true), nil
	}

	var (
		rep soul.Report
		err error
	)
	if mode == soul.TraitReplace {
		rep, err = soul.BulkReplaceTraits(ctx, h.pool, sel, scope, rawReq.Traits)
	} else {
		rep, err = soul.BulkAssignTraits(ctx, h.pool, sel, scope, mode, rawReq.Traits, rawReq.Keys)
	}
	if err != nil {
		// partial: часть чанков закоммичена — 200 + status:partial (идемпотентно
		// до-повторяется оператором, откатывать небезопасно — паритет coven).
		if rep.Status == soul.BulkPartial {
			h.logger.Warn("soul.traits-assign: partial",
				slog.String("mode", rawReq.Mode),
				slog.Int("changed", rep.Changed),
				slog.Int("chunks", rep.ChunksCommitted),
				slog.Any("error", err),
			)
			return h.buildTraitsAssignReply(rawReq, mode, scope, rep, false), nil
		}
		return zero, h.bulkErrorToProblem(err)
	}

	return h.buildTraitsAssignReply(rawReq, mode, scope, rep, false), nil
}

// buildTraitsAssignReply собирает 200-ответ + audit-payload. `keys` — отсортированный
// список затронутых ключей (для merge/replace — ключи переданного набора; для remove —
// удаляемые). trait-значения НЕ кладутся ни в ответ, ни в audit (секрет-гигиена).
func (h *SoulHandler) buildTraitsAssignReply(req SoulTraitsAssignInput, mode soul.TraitMode, scope soul.BulkScope, rep soul.Report, dryRun bool) SoulTraitsAssignReply {
	keys := affectedTraitKeys(mode, req.Traits, req.Keys)
	payload := middleware.AuditPayload{
		"mode":          string(mode),
		"selector":      normalizeCovenSelector(covenAssignSelectorFields(req.Selector)),
		"keys":          keys,
		"matched":       rep.Matched,
		"changed":       rep.Changed,
		"status":        string(rep.Status),
		"scope_applied": !scope.Unrestricted,
		"dry_run":       dryRun,
		"source":        "api",
	}
	resp := soulTraitsAssignResponse{
		Mode:    string(mode),
		Keys:    keys,
		Matched: rep.Matched,
		Changed: rep.Changed,
		Status:  string(rep.Status),
		DryRun:  dryRun,
	}
	return SoulTraitsAssignReply{Body: resp, AuditPayload: payload}
}

// affectedTraitKeys — отсортированный набор затронутых trait-ключей: для merge/replace —
// ключи map-а Traits, для remove — список Keys. nil → []string{} (устойчивый JSON `[]`).
func affectedTraitKeys(mode soul.TraitMode, traits map[string]any, keys []string) []string {
	var out []string
	if mode == soul.TraitRemove {
		out = append([]string(nil), keys...)
	} else {
		out = make([]string, 0, len(traits))
		for k := range traits {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// SoulSIDSelector — middleware-helper для RBAC: извлекает SID из path-param
// для permission-проверки. Использует ключ селектора `host` (rbac.md §
// Грамматика селектора — `host` для per-Soul-таргетинга).
func SoulSIDSelector(r *http.Request) map[string]string {
	sid := chi.URLParam(r, "sid")
	if sid == "" {
		return nil
	}
	return map[string]string{"host": sid}
}

// SoulSshTargetInput — NATIVE request-форма PUT /v1/souls/{sid}/ssh-target (handler-native
// T5d). Заменяет SoulSSHTargetRequest: huma-input (пакет api) биндит тело по своим полям,
// затем зовёт UpdateSshTargetTyped с этой плоской моделью.
//
// Все поля required, кроме `ssh_provider` (P2 W-1, ADR-032 amendment 2026-05-27): операция
// «обновить SSH-реквизиты», а не «частично дополнить». `ssh_provider` (пусто → nil → routing
// идёт через coven_default → cluster_default; kebab-case, валидируется handler-ом до записи).
type SoulSshTargetInput struct {
	SSHPort     int
	SSHUser     string
	SoulPath    string
	SSHProvider string
}

// SoulSshTargetView — ПЛОСКАЯ доменная проекция 200-тела PUT /v1/souls/{sid}/ssh-target
// (handler-native T5d). Пакет api проецирует её в native-схему SoulSshTargetReply (nested
// ssh_target — class-A reuse native SoulSshTarget). Snapshot сохранённого состояния.
// SSHProvider пусто → ключ опущен native-типом (omitempty).
type SoulSshTargetView struct {
	SID         string
	SSHPort     int
	SSHUser     string
	SoulPath    string
	SSHProvider string
}

// SoulSshTargetReply — результат [SoulHandler.UpdateSshTargetTyped] (handler-native). Несёт
// доменную проекцию 200-тела (SoulSshTargetView: snapshot) + audit-payload.
type SoulSshTargetReply struct {
	Body         SoulSshTargetView
	AuditPayload middleware.AuditPayload
}

// UpdateSshTargetTyped — доменная функция PUT /v1/souls/{sid}/ssh-target (handler-native):
// обновление per-host SSH-реквизитов push-flow. req — native input. Ошибки — *problemError
// (422 невалидный sid/ssh_port/ssh_user/soul_path/ssh_provider; 404 нет soul; 500 PG); успех —
// [SoulSshTargetReply] (доменная проекция 200-тела + audit-payload).
func (h *SoulHandler) UpdateSshTargetTyped(ctx context.Context, sid string, req SoulSshTargetInput) (SoulSshTargetReply, error) {
	var zero SoulSshTargetReply
	if !soul.ValidSID(sid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}
	if req.SSHPort < 1 || req.SSHPort > 65535 {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'ssh_port' must be in [1..65535]")}
	}
	if req.SSHUser == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'ssh_user' is required")}
	}
	if req.SoulPath == "" || req.SoulPath[0] != '/' {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'soul_path' must be an absolute Unix path (start with '/')")}
	}
	// P2 W-1: optional `ssh_provider` — kebab-case имя плагина. Пусто → routing
	// уходит на coven_default/cluster_default уровни.
	provider := req.SSHProvider
	if provider != "" && !pushprovider.ValidName(provider) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'ssh_provider' must match "+pushprovider.NamePattern)}
	}

	target := &soul.SSHTarget{SSHPort: req.SSHPort, SSHUser: req.SSHUser, SoulPath: req.SoulPath}
	if provider != "" {
		sp := provider
		target.SSHProvider = &sp
	}
	if err := soul.UpdateSshTarget(ctx, h.pool, sid, target); err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "soul "+sid+" not found")}
		}
		h.logger.Error("soul.ssh-target.update: failed", slog.String("sid", sid), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "update ssh_target failed")}
	}

	auditPayload := middleware.AuditPayload{
		"sid":       sid,
		"ssh_port":  req.SSHPort,
		"ssh_user":  req.SSHUser,
		"soul_path": req.SoulPath,
	}
	if provider != "" {
		auditPayload["ssh_provider"] = provider
	}
	return SoulSshTargetReply{
		Body: SoulSshTargetView{
			SID:         sid,
			SSHPort:     req.SSHPort,
			SSHUser:     req.SSHUser,
			SoulPath:    req.SoulPath,
			SSHProvider: provider,
		},
		AuditPayload: auditPayload,
	}, nil
}

// parseTransport маппит JSON-строку transport в [soul.Transport]. Возвращает
// ok=false на пустую/неизвестную строку (→ 422 на handler-стороне).
func parseTransport(v string) (soul.Transport, bool) {
	switch soul.Transport(v) {
	case soul.TransportAgent:
		return soul.TransportAgent, true
	case soul.TransportSSH:
		return soul.TransportSSH, true
	default:
		return "", false
	}
}

// coalesceCoven нормализует nil-slice в пустой — для JSON `[]` вместо `null`
// (covens объявлен non-nullable в proto/OpenAPI).
func coalesceCoven(c []string) []string {
	if c == nil {
		return []string{}
	}
	return c
}

// coalesceTraits нормализует nil-map в пустой — для JSON `{}` вместо `null`
// (traits объявлен non-nullable в OpenAPI, симметрично coalesceCoven). bare-soul
// без operator-set меток отдаётся как `{}`, а не `null` — UI может рендерить
// пустой набор без nil-проверки (ADR-060 read-path).
func coalesceTraits(t map[string]any) map[string]any {
	if t == nil {
		return map[string]any{}
	}
	return t
}
