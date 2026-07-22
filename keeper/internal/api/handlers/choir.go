package handlers

// Operator API handlers for CRUD of the Choir/Voice topology inside an incarnation
// (ADR-044, S-T3). ONE [ChoirHandler] serves BOTH resources (choirs + voices, a
// sub-resource). Wraps the domain CRUD of the [choir] package (S-T2), maps its sentinel
// errors to RFC 7807 and writes audit on mutations (handler-side self-audit, payload is
// available only after a successful operation). created_by_aid / added_by_aid come from
// the JWT context (claims.Subject), NOT from the request body.
//
// T5d-2c (handler-native): the choir+voice domain is detached from the legacy generator.
// *Typed functions take NATIVE request types (handlers.ChoirCreateInput / VoiceAddInput;
// the huma input in package api binds and validates the body against these fields) and
// return domain results with FLAT wire fields (handlers.ChoirView / VoiceView) — NOT a
// legacy-generator Body. The native wire-DTO (the OpenAPI schema) is built by package api
// from these fields (register func huma_choir.go); oapi-generated types take no part in the
// choir domain. The (w,r) wrappers are gone: HTTP is served by huma full-typed (no MCP for
// the choir domain).
//
// auditW allows nil: without it the mutating trail is not written (unit tests). All
// dependencies are immutable; safe for concurrent use.

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

// ChoirDB is a narrow surface over pgxpool.Pool for Choir/Voice CRUD operations
// (ADR-044, S-T3). Combines [choir.ExecQueryRower] (Create/Get/List/Delete +
// RemoveVoice) and [choir.TxBeginner] (AddVoice — atomic FOR UPDATE → membership
// validate → INSERT → commit). A real *pgxpool.Pool satisfies it automatically; unit
// tests pass a fake. Symmetric to [IncarnationDB].
type ChoirDB interface {
	choir.ExecQueryRower
	choir.TxBeginner
}

// ChoirHandler holds the CRUD handlers for the Choir/Voice topology (ADR-044, S-T3).
// Delegates domain CRUD to the [choir] package, maps sentinels to RFC 7807, writes
// self-audit on mutations.
type ChoirHandler struct {
	db     ChoirDB
	auditW audit.Writer
	logger *slog.Logger
}

// NewChoirHandler creates the handler. auditW allows nil (the mutating trail is not
// written — acceptable in unit tests).
func NewChoirHandler(db ChoirDB, auditW audit.Writer, logger *slog.Logger) *ChoirHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ChoirHandler{db: db, auditW: auditW, logger: logger}
}

// ChoirSpecStub is a non-nil *ChoirHandler stub for generating the huma-OpenAPI
// fragment (HumaChoirSpecYAML): on dump the domain handler is not called, but
// huma.Register requires non-nil for its no-op check. db/auditW nil — the handler
// never executes in spec mode.
func ChoirSpecStub() *ChoirHandler { return &ChoirHandler{} }

// --- DTO ---------------------------------------------------------------

// ChoirView is the FLAT wire form of a Choir record (create-201 / list-item),
// handler-native. description/min_size/max_size/created_by_aid — `*` with no omitempty
// (nil → `null`); created_at — nanosecond time-wire `.UTC()` without Truncate (legacy
// parity: the former choir wire carried nanoseconds).
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

// VoiceView is the FLAT wire form of a Voice membership (add-201 / list-item),
// handler-native. added_by_aid/position/role — `*` with no omitempty (nil → `null`);
// added_at — nanosecond time-wire `.UTC()` without Truncate.
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

// ChoirCreateInput is the NATIVE request form of POST /v1/incarnations/{name}/choirs
// (handler-native). created_by_aid is NOT taken from the body — it comes from the JWT
// context. Replaces ChoirCreateRequest.
type ChoirCreateInput struct {
	ChoirName   string
	Description *string
	MinSize     *int
	MaxSize     *int
}

// VoiceAddInput is the NATIVE request form of POST /v1/incarnations/{name}/choirs/{choir}/
// voices (handler-native). added_by_aid is NOT taken from the body. Replaces
// VoiceAddRequest.
type VoiceAddInput struct {
	SID      string
	Role     *string
	Position *int
}

// --- Create ------------------------------------------------------------

// CreateTyped is the domain function for POST /v1/incarnations/{name}/choirs (handler-native,
// self-audit): create a Choir. created_by_aid from claims (NOT from the body). The
// choir.created self-audit is written INSIDE the function (payload is available only after a
// successful INSERT). Errors are *problemError, success is [ChoirView].
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

// ChoirListPage is the domain result of GET /v1/incarnations/{name}/choirs (handler-native,
// full list with no server-side pagination). Package api projects it into the native
// envelope ChoirListReply.
type ChoirListPage struct {
	Items []ChoirView
}

// ListChoirsTyped is the domain function for GET /v1/incarnations/{name}/choirs (handler-native,
// READ, no audit). A nonexistent incarnation → 200 + items=[] (parity with domain ListChoirs).
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

// DeleteTyped is the domain function for DELETE /v1/incarnations/{name}/choirs/{choir}
// (handler-native, self-audit): delete a Choir (cascading its Voices). The choir.deleted
// self-audit is written INSIDE the function.
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

// AddVoiceTyped is the domain function for POST /v1/incarnations/{name}/choirs/{choir}/voices
// (handler-native, self-audit): add a Voice (membership of a SID in a Choir). added_by_aid
// from claims (NOT from the body). The choir.voice_added self-audit is written INSIDE the function.
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

// VoiceListPage is the domain result of GET /v1/incarnations/{name}/choirs/{choir}/voices
// (handler-native, full list). Package api projects it into the native envelope VoiceListReply.
type VoiceListPage struct {
	Items []VoiceView
}

// ListVoicesTyped is the domain function for GET /v1/incarnations/{name}/choirs/{choir}/voices
// (handler-native, READ, no audit). A nonexistent Choir → 200 + items=[] (parity with domain
// ListVoices).
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

// RemoveVoiceTyped is the domain function for DELETE /v1/incarnations/{name}/choirs/{choir}/
// voices/{sid} (handler-native, self-audit): remove a Voice. The choir.voice_removed
// self-audit is written INSIDE the function.
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

// writeAuditCtx writes an audit event best-effort (handler-side self-audit: payload is
// available only after a successful mutation). nil auditW → no-op (unit tests). An error is
// logged and does not affect the response (the UpdateHosts / CheckDrift pattern).
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

// derefInt maps *int → any (nil → nil) for the audit payload (omitempty semantics at the
// map-key level are done by the caller when needed; for choir.created we store min/max as
// nil when absent — the payload mask does not touch numbers).
func derefInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

// joinSIDs joins SIDs into a human-readable string for a problem detail.
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
