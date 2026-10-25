// LDAP group sync handler, Operator API (ADR-058 amendment NIM-1053) — the
// settings of the background sync (`ldap_sync` in keeper_settings) and its
// status. GET — read (no audit, permission setting.read); PUT — write+audit
// (event ldap_sync.settings_changed, permission setting.update).
//
// The settings live in Postgres and nowhere else: the leader re-reads them on
// every tick, so no invalidation is needed and every instance answers the same.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/ldapsync"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

// LDAPSyncStore is the read/write surface of the sync state ([ldapsync.PGStore]).
type LDAPSyncStore interface {
	LoadSettings(ctx context.Context) (ldapsync.Settings, bool, error)
	SaveSettings(ctx context.Context, s ldapsync.Settings, callerAID string) error
	Status(ctx context.Context) (ldapsync.Status, error)
}

// LDAPSyncHandler — GET/PUT /v1/ldap-sync.
type LDAPSyncHandler struct {
	store   LDAPSyncStore
	enabled bool
	logger  *slog.Logger
}

// NewLDAPSyncHandler builds the handler. enabled says whether this instance
// runs the sync at all (auth.ldap configured and Redis present for the
// leader lease); the settings stay editable either way.
func NewLDAPSyncHandler(store LDAPSyncStore, enabled bool, logger *slog.Logger) *LDAPSyncHandler {
	if store == nil {
		panic("handlers.NewLDAPSyncHandler: store is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &LDAPSyncHandler{store: store, enabled: enabled, logger: logger}
}

// LDAPSyncSpecStub — a non-nil handler for the OpenAPI dump (never called).
func LDAPSyncSpecStub() *LDAPSyncHandler {
	return &LDAPSyncHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// LDAPSyncView — the domain body of GET and PUT.
type LDAPSyncView struct {
	Interval    string
	SettingsSet bool
	// SettingsError — the stored value could not be read; Interval is then the
	// default the sync runs on.
	SettingsError string
	Enabled       bool
	LastRunAt     *time.Time
	LastSuccessAt *time.Time
	Operators     map[ldapsync.Outcome]int
	Pending       int
}

// GetTyped — GET /v1/ldap-sync. A stored value this version cannot parse is
// reported as the default — what the sync then runs on — rather than as a
// 500 that would hide the status too.
func (h *LDAPSyncHandler) GetTyped(ctx context.Context) (LDAPSyncView, error) {
	settings, set, err := h.store.LoadSettings(ctx)
	var settingsErr string
	switch {
	case errors.Is(err, ldapsync.ErrInvalidSettings):
		h.logger.Error("ldap_sync: stored settings unreadable, reporting the default the sync runs on", slog.Any("error", err))
		settings = ldapsync.DefaultSettings()
		settingsErr = err.Error()
	case err != nil:
		h.logger.Error("ldap_sync: load settings failed", slog.Any("error", err))
		return LDAPSyncView{}, &problemError{problem.New(problem.TypeInternalError, "", "read ldap sync settings failed")}
	}
	v, err := h.view(ctx, settings, set)
	v.SettingsError = settingsErr
	return v, err
}

func (h *LDAPSyncHandler) view(ctx context.Context, settings ldapsync.Settings, set bool) (LDAPSyncView, error) {
	st, err := h.store.Status(ctx)
	if err != nil {
		h.logger.Error("ldap_sync: read status failed", slog.Any("error", err))
		return LDAPSyncView{}, &problemError{problem.New(problem.TypeInternalError, "", "read ldap sync status failed")}
	}
	return LDAPSyncView{
		Interval:      ldapsync.FormatInterval(settings.Interval),
		SettingsSet:   set,
		Enabled:       h.enabled,
		LastRunAt:     st.LastRunAt,
		LastSuccessAt: st.LastSuccessAt,
		Operators:     st.Operators,
		Pending:       st.Pending,
	}, nil
}

// LDAPSyncUpdateInput — the PUT body, handler-native.
type LDAPSyncUpdateInput struct {
	Interval string
}

// LDAPSyncUpdateReply — the PUT result: the 200 body plus the audit fields.
type LDAPSyncUpdateReply struct {
	Body        LDAPSyncView
	Settings    ldapsync.Settings
	Previous    ldapsync.Settings
	PreviousSet bool
}

// AuditPayload — payload of `ldap_sync.settings_changed`.
func (r LDAPSyncUpdateReply) AuditPayload() middleware.AuditPayload {
	p := middleware.AuditPayload{"settings": settingsPayload(r.Settings)}
	if r.PreviousSet {
		p["previous"] = settingsPayload(r.Previous)
	}
	return p
}

func settingsPayload(s ldapsync.Settings) map[string]any {
	return map[string]any{"interval": ldapsync.FormatInterval(s.Interval)}
}

// PutTyped — PUT /v1/ldap-sync: replace semantics over the settings object.
// 422 for an interval outside [1m, 24h] or unparsable; nothing is written.
func (h *LDAPSyncHandler) PutTyped(ctx context.Context, claims *jwt.Claims, in LDAPSyncUpdateInput) (LDAPSyncUpdateReply, error) {
	var zero LDAPSyncUpdateReply
	interval, err := ldapsync.ParseInterval(in.Interval)
	if err != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	}
	next := ldapsync.Settings{Interval: interval}

	// A stored value this version cannot parse is replaced, not refused: PUT is
	// the way out of it. The audit then has no previous to show.
	prev, prevSet, err := h.store.LoadSettings(ctx)
	if err != nil {
		h.logger.Warn("ldap_sync: previous settings unreadable, overwriting", slog.Any("error", err))
		prevSet = false
	}

	// The status goes into the body; read it before writing, so a failure here
	// cannot leave a written but unaudited change behind a 500.
	body, err := h.view(ctx, next, true)
	if err != nil {
		return zero, err
	}

	callerAID := claims.Subject
	if err := h.store.SaveSettings(ctx, next, callerAID); err != nil {
		if errors.Is(err, serviceregistry.ErrOperatorNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "",
				"caller AID "+callerAID+" not found in operators registry")}
		}
		h.logger.Error("ldap_sync: save settings failed", slog.String("by_aid", callerAID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "update ldap sync settings failed")}
	}
	return LDAPSyncUpdateReply{Body: body, Settings: next, Previous: prev, PreviousSet: prevSet}, nil
}
