package handlers

// Operator API handler-ы CRUD топологии Choir/Voice внутри инкарнации (ADR-044,
// S-T3). ОДИН [ChoirHandler] обслуживает ОБА ресурса (choirs + voices, sub-resource).
// Оборачивает domain-CRUD пакета [choir] (S-T2), маппит его sentinel-ошибки в RFC 7807
// и пишет audit на мутациях (handler-side self-audit, payload доступен только после
// успешной операции). created_by_aid / added_by_aid берутся из JWT-контекста
// (claims.Subject), НЕ из тела запроса.
//
// T5d-2c (handler-native): домен choir+voice отвязан от legacy-генерата. *Typed-функции
// принимают NATIVE request-типы (handlers.ChoirCreateInput / VoiceAddInput; huma-input
// в пакете api биндит и валидирует тело по этим полям) и возвращают доменные result-ы
// с ПЛОСКИМИ wire-полями (handlers.ChoirView / VoiceView) — НЕ legacy-генерата-Body. Native
// wire-DTO (схему OpenAPI) строит пакет api из этих полей (register-func
// huma_choir.go), oapi-генерёные типы в choir-домене не участвуют. (w,r)-оболочки
// сняты: HTTP обслуживает huma full-typed (MCP choir-домена нет).
//
// auditW допускает nil: без него mutating-trail не пишется (unit-тесты). Все
// зависимости immutable; safe for concurrent use.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// ChoirDB — узкая поверхность над pgxpool.Pool для CRUD-операций Choir/Voice
// (ADR-044, S-T3). Объединяет [choir.ExecQueryRower] (Create/Get/List/Delete +
// RemoveVoice) и [choir.TxBeginner] (AddVoice — atomic FOR UPDATE → membership-
// validate → INSERT → commit). Реальный *pgxpool.Pool удовлетворяет
// автоматически; unit-тесты передают fake. Симметрично [IncarnationDB].
type ChoirDB interface {
	choir.ExecQueryRower
	choir.TxBeginner
}

// ChoirHandler — handler-ы CRUD топологии Choir/Voice (ADR-044, S-T3). Делегирует
// domain-CRUD пакету [choir], маппит sentinel-ы в RFC 7807, пишет self-audit на
// мутациях.
type ChoirHandler struct {
	db     ChoirDB
	auditW audit.Writer
	logger *slog.Logger
}

// NewChoirHandler создаёт handler. auditW допускает nil (mutating-trail не
// пишется — допустимо в unit-тестах).
func NewChoirHandler(db ChoirDB, auditW audit.Writer, logger *slog.Logger) *ChoirHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ChoirHandler{db: db, auditW: auditW, logger: logger}
}

// ChoirSpecStub — непустой *ChoirHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaChoirSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки. db/auditW nil — handler
// никогда не исполняется в spec-режиме.
func ChoirSpecStub() *ChoirHandler { return &ChoirHandler{} }

// --- DTO ---------------------------------------------------------------

// ChoirView — ПЛОСКАЯ wire-форма Choir-записи (create-201 / list-item), handler-
// native. description/min_size/max_size/created_by_aid — `*` БЕЗ omitempty (nil →
// `null`); created_at — наносекундный time-wire `.UTC()` БЕЗ Truncate (паритет
// легаси: прежний wire choir-а нёс наносекунды).
type ChoirView struct {
	IncarnationName string
	ChoirName       string
	Description     *string
	MinSize         *int
	MaxSize         *int
	CreatedAt       time.Time
	CreatedByAID    *string
}

func toChoirView(c *choir.Choir) ChoirView {
	return ChoirView{
		IncarnationName: c.IncarnationName,
		ChoirName:       c.ChoirName,
		Description:     c.Description,
		MinSize:         c.MinSize,
		MaxSize:         c.MaxSize,
		CreatedAt:       c.CreatedAt.UTC(),
		CreatedByAID:    c.CreatedByAID,
	}
}

// VoiceView — ПЛОСКАЯ wire-форма Voice-членства (add-201 / list-item), handler-
// native. added_by_aid/position/role — `*` БЕЗ omitempty (nil → `null`); added_at —
// наносекундный time-wire `.UTC()` БЕЗ Truncate.
type VoiceView struct {
	IncarnationName string
	ChoirName       string
	SID             string
	Role            *string
	Position        *int
	AddedAt         time.Time
	AddedByAID      *string
}

func toVoiceView(v *choir.Voice) VoiceView {
	return VoiceView{
		IncarnationName: v.IncarnationName,
		ChoirName:       v.ChoirName,
		SID:             v.SID,
		Role:            v.Role,
		Position:        v.Position,
		AddedAt:         v.AddedAt.UTC(),
		AddedByAID:      v.AddedByAID,
	}
}

// ChoirCreateInput — NATIVE request-форма POST /v1/incarnations/{name}/choirs
// (handler-native). created_by_aid из тела НЕ принимается — берётся из JWT-контекста.
// Заменяет ChoirCreateRequest.
type ChoirCreateInput struct {
	ChoirName   string
	Description *string
	MinSize     *int
	MaxSize     *int
}

// VoiceAddInput — NATIVE request-форма POST /v1/incarnations/{name}/choirs/{choir}/
// voices (handler-native). added_by_aid из тела НЕ принимается. Заменяет
// VoiceAddRequest.
type VoiceAddInput struct {
	SID      string
	Role     *string
	Position *int
}

// --- Create ------------------------------------------------------------

// CreateTyped — доменная функция POST /v1/incarnations/{name}/choirs (handler-native,
// self-audit): создание Choir. created_by_aid из claims (НЕ из тела). self-audit
// choir.created пишется ВНУТРИ функции (payload доступен только после успешного
// INSERT-а). Ошибки — *problemError, успех — [ChoirView].
func (h *ChoirHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, name string, req ChoirCreateInput) (ChoirView, error) {
	var zero ChoirView
	if !incarnation.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+incarnation.NamePattern)}
	}
	if !choir.ValidChoirName(req.ChoirName) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'choir_name' must match ^[a-z][a-z0-9_-]*$")}
	}

	createdBy := claims.Subject
	c := &choir.Choir{
		IncarnationName: name,
		ChoirName:       req.ChoirName,
		Description:     req.Description,
		MinSize:         req.MinSize,
		MaxSize:         req.MaxSize,
		CreatedByAID:    &createdBy,
	}
	if err := choir.CreateChoir(ctx, h.db, c); err != nil {
		switch {
		case errors.Is(err, choir.ErrInvalidChoirName):
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"field 'choir_name' must match ^[a-z][a-z0-9_-]*$")}
		case errors.Is(err, choir.ErrInvalidSizeBounds):
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		case errors.Is(err, choir.ErrIncarnationNotFound):
			return zero, &problemError{problem.New(problem.TypeNotFound, "",
				"incarnation "+name+" not found")}
		case errors.Is(err, choir.ErrChoirExists):
			return zero, &problemError{problem.New(problem.TypeChoirExists, "",
				"choir "+req.ChoirName+" already exists in incarnation "+name)}
		default:
			h.logger.Error("choir.create: failed",
				slog.String("incarnation", name),
				slog.String("choir", req.ChoirName),
				slog.Any("error", err),
			)
			return zero, &problemError{problem.New(problem.TypeInternalError, "", "create choir failed")}
		}
	}

	h.writeAuditCtx(ctx, &audit.Event{
		EventType: audit.EventChoirCreated,
		Source:    audit.SourceAPI,
		ArchonAID: claims.Subject,
		Payload: map[string]any{
			"incarnation_name": name,
			"choir_name":       req.ChoirName,
			"min_size":         derefInt(req.MinSize),
			"max_size":         derefInt(req.MaxSize),
			"created_by_aid":   claims.Subject,
		},
	})

	return toChoirView(c), nil
}

// --- List --------------------------------------------------------------

// ChoirListPage — доменный результат GET /v1/incarnations/{name}/choirs (handler-
// native, full list без серверной пагинации). Пакет api проецирует в native envelope
// ChoirListReply.
type ChoirListPage struct {
	Items []ChoirView
}

// ListChoirsTyped — доменная функция GET /v1/incarnations/{name}/choirs (handler-
// native, READ, БЕЗ audit). Несуществующая incarnation → 200 + items=[] (parity domain
// ListChoirs).
func (h *ChoirHandler) ListChoirsTyped(ctx context.Context, name string) (ChoirListPage, error) {
	var zero ChoirListPage
	if !incarnation.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+incarnation.NamePattern)}
	}
	choirs, err := choir.ListChoirs(ctx, h.db, name)
	if err != nil {
		h.logger.Error("choir.list: failed", slog.String("incarnation", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list choirs failed")}
	}
	items := make([]ChoirView, 0, len(choirs))
	for _, c := range choirs {
		items = append(items, toChoirView(c))
	}
	return ChoirListPage{Items: items}, nil
}

// --- Delete ------------------------------------------------------------

// DeleteTyped — доменная функция DELETE /v1/incarnations/{name}/choirs/{choir}
// (handler-native, self-audit): удаление Choir (каскадом его Voice-ы). self-audit
// choir.deleted пишется ВНУТРИ функции.
func (h *ChoirHandler) DeleteTyped(ctx context.Context, claims *jwt.Claims, name, choirName string) error {
	if !incarnation.ValidName(name) {
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+incarnation.NamePattern)}
	}
	if !choir.ValidChoirName(choirName) {
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'choir' must match ^[a-z][a-z0-9_-]*$")}
	}
	if err := choir.DeleteChoir(ctx, h.db, name, choirName); err != nil {
		if errors.Is(err, choir.ErrChoirNotFound) {
			return &problemError{problem.New(problem.TypeNotFound, "",
				"choir "+choirName+" not found in incarnation "+name)}
		}
		h.logger.Error("choir.delete: failed",
			slog.String("incarnation", name), slog.String("choir", choirName), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "delete choir failed")}
	}
	h.writeAuditCtx(ctx, &audit.Event{
		EventType: audit.EventChoirDeleted,
		Source:    audit.SourceAPI,
		ArchonAID: claims.Subject,
		Payload: map[string]any{
			"incarnation_name": name,
			"choir_name":       choirName,
		},
	})
	return nil
}

// --- AddVoice ----------------------------------------------------------

// AddVoiceTyped — доменная функция POST /v1/incarnations/{name}/choirs/{choir}/voices
// (handler-native, self-audit): добавление Voice (членство SID в Choir-е). added_by_aid
// из claims (НЕ из тела). self-audit choir.voice_added пишется ВНУТРИ функции.
func (h *ChoirHandler) AddVoiceTyped(ctx context.Context, claims *jwt.Claims, name, choirName string, req VoiceAddInput) (VoiceView, error) {
	var zero VoiceView
	if !incarnation.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+incarnation.NamePattern)}
	}
	if !choir.ValidChoirName(choirName) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'choir' must match ^[a-z][a-z0-9_-]*$")}
	}
	if !soul.ValidSID(req.SID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'sid' must match "+soul.SIDPattern)}
	}
	if req.Role != nil && !validHostRole(*req.Role) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'role' must be lowercase kebab-case (1..63 chars)")}
	}
	if req.Position != nil && *req.Position < 0 {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'position' must be >= 0")}
	}

	addedBy := claims.Subject
	v := &choir.Voice{
		IncarnationName: name,
		ChoirName:       choirName,
		SID:             req.SID,
		Role:            req.Role,
		Position:        req.Position,
		AddedByAID:      &addedBy,
	}
	if err := choir.AddVoice(ctx, h.db, v); err != nil {
		var notMembers *choir.ErrNotMembers
		switch {
		case errors.Is(err, choir.ErrChoirNotFound):
			return zero, &problemError{problem.New(problem.TypeNotFound, "",
				"choir "+choirName+" not found in incarnation "+name)}
		case errors.As(err, &notMembers):
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"SID(s) not members of incarnation "+name+": "+joinSIDs(notMembers.Missing))}
		case errors.Is(err, choir.ErrVoiceExists):
			return zero, &problemError{problem.New(problem.TypeVoiceExists, "",
				"voice "+req.SID+" already exists in choir "+choirName)}
		default:
			h.logger.Error("choir.add-voice: failed",
				slog.String("incarnation", name), slog.String("choir", choirName),
				slog.String("sid", req.SID), slog.Any("error", err))
			return zero, &problemError{problem.New(problem.TypeInternalError, "", "add voice failed")}
		}
	}

	payload := map[string]any{
		"incarnation_name": name,
		"choir_name":       choirName,
		"sid":              req.SID,
		"added_by_aid":     claims.Subject,
	}
	if v.Role != nil {
		payload["role"] = *v.Role
	}
	if v.Position != nil {
		payload["position"] = *v.Position
	}
	h.writeAuditCtx(ctx, &audit.Event{
		EventType: audit.EventChoirVoiceAdded,
		Source:    audit.SourceAPI,
		ArchonAID: claims.Subject,
		Payload:   payload,
	})

	return toVoiceView(v), nil
}

// --- ListVoices --------------------------------------------------------

// VoiceListPage — доменный результат GET /v1/incarnations/{name}/choirs/{choir}/voices
// (handler-native, full list). Пакет api проецирует в native envelope VoiceListReply.
type VoiceListPage struct {
	Items []VoiceView
}

// ListVoicesTyped — доменная функция GET /v1/incarnations/{name}/choirs/{choir}/voices
// (handler-native, READ, БЕЗ audit). Несуществующий Choir → 200 + items=[] (parity
// domain ListVoices).
func (h *ChoirHandler) ListVoicesTyped(ctx context.Context, name, choirName string) (VoiceListPage, error) {
	var zero VoiceListPage
	if !incarnation.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+incarnation.NamePattern)}
	}
	if !choir.ValidChoirName(choirName) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'choir' must match ^[a-z][a-z0-9_-]*$")}
	}
	voices, err := choir.ListVoices(ctx, h.db, name, choirName)
	if err != nil {
		h.logger.Error("choir.list-voices: failed",
			slog.String("incarnation", name), slog.String("choir", choirName), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list voices failed")}
	}
	items := make([]VoiceView, 0, len(voices))
	for _, v := range voices {
		items = append(items, toVoiceView(v))
	}
	return VoiceListPage{Items: items}, nil
}

// --- RemoveVoice -------------------------------------------------------

// RemoveVoiceTyped — доменная функция DELETE /v1/incarnations/{name}/choirs/{choir}/
// voices/{sid} (handler-native, self-audit): удаление Voice. self-audit
// choir.voice_removed пишется ВНУТРИ функции.
func (h *ChoirHandler) RemoveVoiceTyped(ctx context.Context, claims *jwt.Claims, name, choirName, sid string) error {
	if !incarnation.ValidName(name) {
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'name' must match "+incarnation.NamePattern)}
	}
	if !choir.ValidChoirName(choirName) {
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'choir' must match ^[a-z][a-z0-9_-]*$")}
	}
	if !soul.ValidSID(sid) {
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'sid' must match "+soul.SIDPattern)}
	}
	if err := choir.RemoveVoice(ctx, h.db, name, choirName, sid); err != nil {
		if errors.Is(err, choir.ErrVoiceNotFound) {
			return &problemError{problem.New(problem.TypeNotFound, "",
				"voice "+sid+" not found in choir "+choirName)}
		}
		h.logger.Error("choir.remove-voice: failed",
			slog.String("incarnation", name), slog.String("choir", choirName),
			slog.String("sid", sid), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "remove voice failed")}
	}
	h.writeAuditCtx(ctx, &audit.Event{
		EventType: audit.EventChoirVoiceRemoved,
		Source:    audit.SourceAPI,
		ArchonAID: claims.Subject,
		Payload: map[string]any{
			"incarnation_name": name,
			"choir_name":       choirName,
			"sid":              sid,
		},
	})
	return nil
}

// --- helpers -----------------------------------------------------------

// writeAuditCtx пишет audit-event best-effort (handler-side self-audit: payload
// доступен только после успешной мутации). nil-auditW → no-op (unit-тесты). Ошибка
// логируется, на response не влияет (паттерн UpdateHosts / CheckDrift).
func (h *ChoirHandler) writeAuditCtx(ctx context.Context, ev *audit.Event) {
	if h.auditW == nil {
		return
	}
	if err := h.auditW.Write(ctx, ev); err != nil {
		h.logger.Error("choir: audit write failed",
			slog.String("event_type", string(ev.EventType)),
			slog.Any("error", err),
		)
	}
}

// derefInt — *int → any (nil → nil) для audit-payload (omitempty-семантика
// на уровне map-ключа делается caller-ом при необходимости; для choir.created
// min/max кладём как nil при отсутствии — payload-mask не трогает числа).
func derefInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

// joinSIDs склеивает SID-ы в человекочитаемую строку для problem-detail.
func joinSIDs(sids []string) string {
	out := ""
	for i, s := range sids {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
