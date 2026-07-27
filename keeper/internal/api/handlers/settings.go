// Settings handler, Operator API ([ADR-0073](docs/adr/0073-keeper-runtime-config-pg.md) (i))
// — the operator surface of the SettingsStore: the catalog of Keeper runtime
// settings with their EFFECTIVE values and where each one comes from, plus
// override (PUT) and revert (DELETE).
//
// GET — read (no audit, permission setting.read). PUT/DELETE — write+audit
// (setting.updated / setting.deleted, permissions setting.update /
// setting.delete).
//
// The write-gate runs the SAME field-registry parse + range check the read path
// uses, so a value that PUT accepts is a value the loader accepts: a rejection
// is a 422 with `keeper_settings` untouched (ADR-0073(i)). A successful write
// publishes the cluster invalidation through serviceregistry.Service and
// re-reads the overlay locally, because the publisher self-filters its own
// message by KID.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/settingsstore"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// SettingsConfigReader — the live config surface: the effective snapshot plus
// the FILE document (the overlay never enters it, so it is the honest answer to
// "does keeper.yml set this key?"). Implemented by
// *config.Store[config.KeeperConfig].
type SettingsConfigReader interface {
	Get() *config.KeeperConfig
	Document() *config.Document
	ValidateOverlay(entries []config.OverlayEntry) []diag.Diagnostic
}

// SettingsOverlayReader — the SettingsStore surface: which keys Postgres
// currently overrides, the entries as the merge sees them, and a synchronous
// re-read after a write. Implemented by *settingsstore.Store.
type SettingsOverlayReader interface {
	Values() map[string]any
	Overlay() []config.OverlayEntry
	Refresh(ctx context.Context) error
}

// Value sources, reported per key (ADR-0073(b), amended): built-in default <
// Postgres < this instance's file — a local decision on a host outranks the
// cluster-wide one.
const (
	SettingSourceDefault = "default"
	SettingSourceFile    = "file"
	SettingSourcePG      = "pg"
)

// SettingsHandler — GET /v1/settings, PUT|DELETE /v1/settings/{key}. Holds no
// state; safe for concurrent use.
type SettingsHandler struct {
	cfg     SettingsConfigReader
	overlay SettingsOverlayReader
	svc     *serviceregistry.Service
	logger  *slog.Logger
}

// NewSettingsHandler builds the handler. All three dependencies are required
// (the handler mounts only when they are wired, see router.go).
func NewSettingsHandler(cfg SettingsConfigReader, overlay SettingsOverlayReader, svc *serviceregistry.Service, logger *slog.Logger) *SettingsHandler {
	if cfg == nil {
		panic("handlers.NewSettingsHandler: config reader is nil")
	}
	if overlay == nil {
		panic("handlers.NewSettingsHandler: overlay reader is nil")
	}
	if svc == nil {
		panic("handlers.NewSettingsHandler: serviceregistry.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &SettingsHandler{cfg: cfg, overlay: overlay, svc: svc, logger: logger}
}

// SettingsSpecStub — a non-empty stub for the huma OpenAPI dump (the handler is
// never called in spec mode; parity with ProvisioningPolicySpecStub).
func SettingsSpecStub() *SettingsHandler {
	return &SettingsHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// SettingView — FLAT domain body of one catalog entry. Type/Bounds/Default
// describe the field so the UI renders the form from the backend catalog
// instead of hardcoding it (ADR-042); Value/Source describe the here-and-now.
//
// ClusterValue/OverriddenLocally are set only when this instance's keeper.yml
// shadows an existing cluster override (ADR-0073(b), amended — the file wins):
// the pair is what lets the UI say "the cluster asks for X, this host runs Y
// because its file says so" instead of silently showing the local value.
type SettingView struct {
	Key         string
	YAMLPath    string
	Type        string
	Bounds      string
	Default     any
	Value       any
	Source      string
	Description string

	ClusterValue      any
	OverriddenLocally bool
}

// ListTyped — GET /v1/settings (READ, no audit): the whole registry with the
// effective value of each key on THIS instance and where it came from.
func (h *SettingsHandler) ListTyped() []SettingView {
	cfg := h.cfg.Get()
	doc := h.cfg.Document()
	overridden := h.overlay.Values()

	fields := settingsstore.Fields()
	out := make([]SettingView, 0, len(fields))
	for _, f := range fields {
		out = append(out, settingView(f, cfg, doc, overridden))
	}
	return out
}

// SettingUpdateReply — result of PutTyped/DeleteTyped: the 200 body plus the
// audit fields.
type SettingUpdateReply struct {
	Body     SettingView
	Key      string
	Value    any
	Previous any
}

// AuditPayload builds the payload of setting.updated / setting.deleted: the key,
// the new value (absent on a delete) and the previous override, if there was
// one. Operational tunables only — the overlay is closed to security gates by
// admission rule (j.2), so nothing here is a secret.
func (r SettingUpdateReply) AuditPayload() middleware.AuditPayload {
	p := middleware.AuditPayload{"key": r.Key}
	if r.Value != nil {
		p["value"] = r.Value
	}
	if r.Previous != nil {
		p["previous"] = r.Previous
	}
	return p
}

// PutTyped — PUT /v1/settings/{key} (WRITE+AUDIT): validate through the field
// registry, then upsert the row and invalidate the cluster.
//
// Errors: 404 unknown key (admission is enumerated — an unregistered key does
// not exist), 422 unparsable/out-of-range value (the row is NOT written), 404
// caller-not-found (FK), 500.
func (h *SettingsHandler) PutTyped(ctx context.Context, claims *jwt.Claims, key, raw string) (SettingUpdateReply, error) {
	var zero SettingUpdateReply

	f, ok := settingsstore.Lookup(key)
	if !ok {
		return zero, &problemError{problem.New(problem.TypeNotFound, "",
			"unknown setting key "+key+" (see GET /v1/settings for the catalog)")}
	}
	v, err := f.Parse(raw)
	if err != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	}
	if err := h.validateCandidate(withOverride(h.overlay.Overlay(), f.YAMLPath, v)); err != nil {
		return zero, err
	}

	previous := h.overlay.Values()[key]

	callerAID := claims.Subject
	if _, err := h.svc.SetSetting(ctx, serviceregistry.SetSettingInput{
		Key:       f.Key,
		Value:     f.Format(v),
		CallerAID: &callerAID,
	}); err != nil {
		return zero, h.writeProblem(err, callerAID, "update setting failed")
	}

	h.refreshLocal(ctx, "update")
	return SettingUpdateReply{
		Body:     h.viewOf(f),
		Key:      f.Key,
		Value:    v,
		Previous: previous,
	}, nil
}

// DeleteTyped — DELETE /v1/settings/{key} (WRITE+AUDIT): drop the override, so
// the file value (or the built-in default) is back in effect. 404 when the key
// is unknown OR when there is no override to drop — both mean "nothing here to
// delete", and neither changes anything.
func (h *SettingsHandler) DeleteTyped(ctx context.Context, key string) (SettingUpdateReply, error) {
	var zero SettingUpdateReply

	f, ok := settingsstore.Lookup(key)
	if !ok {
		return zero, &problemError{problem.New(problem.TypeNotFound, "",
			"unknown setting key "+key+" (see GET /v1/settings for the catalog)")}
	}
	previous := h.overlay.Values()[key]
	// Dropping an override is a config change like any other: the layer below
	// may well break an invariant the override was holding up.
	if err := h.validateCandidate(withoutOverride(h.overlay.Overlay(), f.YAMLPath)); err != nil {
		return zero, err
	}

	if err := h.svc.DeleteSetting(ctx, f.Key); err != nil {
		if errors.Is(err, serviceregistry.ErrSettingNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "",
				"setting "+f.Key+" has no override to delete")}
		}
		return zero, h.writeProblem(err, "", "delete setting failed")
	}

	h.refreshLocal(ctx, "delete")
	return SettingUpdateReply{
		Body:     h.viewOf(f),
		Key:      f.Key,
		Previous: previous,
	}, nil
}

// validateCandidate dry-runs the whole prospective overlay through the config
// pipeline before anything is written (ADR-0073(i)). The per-field range check
// cannot see cross-field invariants — `poll_floor <= poll_ceiling` exists only on
// the merged config — and a row that passes PUT but fails the merge would be
// rejected by every reader as a whole (all-or-nothing, ADR-0073(h)): the cluster
// would keep its last-good overlay while a restarting instance came up on the
// file base. Fail-closed: 422, nothing written.
func (h *SettingsHandler) validateCandidate(entries []config.OverlayEntry) error {
	diags := h.cfg.ValidateOverlay(entries)
	if !diag.HasErrors(diags) {
		return nil
	}
	return &problemError{problem.New(problem.TypeValidationFailed, "",
		"the resulting configuration is invalid: "+firstError(diags))}
}

// withOverride returns entries with path set to value (replacing any existing
// entry for it). The input is never mutated — it is the live snapshot.
func withOverride(entries []config.OverlayEntry, path string, value any) []config.OverlayEntry {
	out := make([]config.OverlayEntry, 0, len(entries)+1)
	for _, e := range entries {
		if e.Path != path {
			out = append(out, e)
		}
	}
	return append(out, config.OverlayEntry{Path: path, Value: value})
}

// withoutOverride returns entries with path removed.
func withoutOverride(entries []config.OverlayEntry, path string) []config.OverlayEntry {
	out := make([]config.OverlayEntry, 0, len(entries))
	for _, e := range entries {
		if e.Path != path {
			out = append(out, e)
		}
	}
	return out
}

func firstError(diags []diag.Diagnostic) string {
	for _, d := range diags {
		if d.Level == diag.LevelError {
			return d.Message
		}
	}
	return "unknown validation error"
}

// refreshLocal re-reads the overlay on THIS node right after a write: the
// publisher self-filters its own invalidation by KID, so without this the
// writer itself would lag behind the cluster by up to one TTL period.
func (h *SettingsHandler) refreshLocal(ctx context.Context, op string) {
	if err := h.overlay.Refresh(ctx); err != nil {
		h.logger.Warn("settings: local overlay refresh after write failed, the cluster poll will catch up",
			slog.String("op", op), slog.Any("error", err))
	}
}

func (h *SettingsHandler) viewOf(f settingsstore.Field) SettingView {
	return settingView(f, h.cfg.Get(), h.cfg.Document(), h.overlay.Values())
}

func (h *SettingsHandler) writeProblem(err error, callerAID, msg string) error {
	switch {
	case errors.Is(err, serviceregistry.ErrOperatorNotFound):
		return &problemError{problem.New(problem.TypeNotFound, "",
			"caller AID "+callerAID+" not found in operators registry")}
	case errors.Is(err, serviceregistry.ErrInvalidSettingKey):
		// Unreachable — registry keys satisfy the CHECK by construction
		// (guard test), defensive.
		return &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error("settings: write failed", slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", msg)}
	}
}

// settingView renders one catalog entry. `source` is answered from evidence
// rather than guessed, in the precedence order of ADR-0073(b) as amended: an
// explicit path in THIS instance's file wins, else the cluster override, else
// the built-in default.
//
// When the file wins over an existing cluster value, ClusterValue carries what
// Postgres holds — otherwise the operator would see their PUT accepted and this
// instance quietly ignoring it, with nothing in the catalog to explain why.
func settingView(f settingsstore.Field, cfg *config.KeeperConfig, doc *config.Document, overridden map[string]any) SettingView {
	inFile := doc != nil && doc.HasPath(f.YAMLPath)
	clusterValue, inPG := overridden[f.Key]

	source := SettingSourceDefault
	switch {
	case inFile:
		source = SettingSourceFile
	case inPG:
		source = SettingSourcePG
	}

	view := SettingView{
		Key:         f.Key,
		YAMLPath:    f.YAMLPath,
		Type:        string(f.Kind),
		Bounds:      f.Bounds(),
		Default:     f.Default,
		Value:       f.Read(cfg),
		Source:      source,
		Description: f.Description,
	}
	if inFile && inPG {
		view.ClusterValue = clusterValue
		view.OverriddenLocally = true
	}
	return view
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}
