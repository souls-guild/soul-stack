package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// prepareNotifyErr — validation/authorization of the notify block BEFORE opening the tx (ADR-052(g)
// amendment N2; FULL-TYPED ADR-054 §Pattern, batch-2f self-audit) without http.ResponseWriter/
// *http.Request. Builds ephemeral Tiding templates; voyage_id/name are stamped later in
// stampEphemeralTidings (after generating voyage_id). nil notify → (nil, nil). store=nil
// → fail-closed 500. Delegates to the shared core [prepareNotifyTidingsErr] (single source
// with the Cadence-permanent path); *problemError on failure. ctx — request context.
func (h *VoyageHandler) prepareNotifyErr(
	ctx context.Context, claims *jwt.Claims, req *voyageCreateRequest, kind voyage.Kind,
) ([]herald.Tiding, error) {
	if len(req.Notify) == 0 {
		return nil, nil
	}
	if h.store == nil {
		return nil, &problemError{problem.New(problem.TypeInternalError, "",
			"voyage orchestrator is not configured")}
	}
	tidings, perr := prepareNotifyTidingsErr(prepareNotifyDeps{
		store:    h.store,
		enforcer: h.enforcer,
		logName:  "voyage.notify",
		logger:   h.logger,
	}, ctx, claims, req.Notify, kind, notifyTidingShape{ephemeral: true})
	if perr != nil {
		return nil, &problemError{*perr}
	}
	return tidings, nil
}

// prepareNotifyDeps — dependencies of the shared [prepareNotifyTidingsErr], extracted from
// a concrete handler (Voyage/Cadence). store — the herald-CRUD pool (channel existence
// check), enforcer — RBAC (herald.read-guard), logName — the source log prefix
// ("voyage.notify"/"cadence.notify").
type prepareNotifyDeps struct {
	store    herald.ExecQueryRower
	enforcer middleware.PermissionChecker
	logName  string
	logger   *slog.Logger
}

// notifyTidingShape — the shape of the resulting Tiding, distinguishing ephemeral
// (Voyage: a one-shot rule, the voyage_id binding is set later) from permanent
// (Cadence: a permanent rule with the origin marker created_from_cadence_id and a
// cadence selector, both bound IMMEDIATELY by the schedule ULID).
type notifyTidingShape struct {
	// ephemeral=true → Voyage path (Ephemeral=true, voyage_id/name are stamped in
	// stampEphemeralTidings). false → Cadence path (a permanent rule).
	ephemeral bool
	// cadenceID — the schedule ULID (cadences.id), binding of a permanent rule: in
	// Ephemeral=false mode it is set BOTH in the Cadence selector (a subscription filter
	// "notify only about runs of this schedule") AND in CreatedFromCadenceID
	// (an origin marker for cascade deletion). Empty in ephemeral mode.
	cadenceID string
	// namePrefix — the deterministic name prefix of a permanent rule
	// (<cadence-name>-notify, the caller adds a unique suffix). Empty in
	// ephemeral mode (the name is eph-<ULID> in stampEphemeralTidings).
	namePrefix string
}

// prepareNotifyTidingsErr — the shared validator/authorizer/builder of the notify block
// (ADR-052(g)/(m); FULL-TYPED ADR-054 §Pattern), common to the Voyage-ephemeral and
// Cadence-permanent paths. So the shape/validation/RBAC of the two call sites do not diverge, all
// notify-block logic lives here; the ephemeral⟺permanent difference is in [notifyTidingShape]
// (the caller chooses). Without http.ResponseWriter/*http.Request — returns
// (*problem.Details) instead of problem.Write (the calling layer decides how to deliver:
// huma envelope / (w,r) wrapper); instance in Details is empty (the caller sets the path).
//
// Check order (fail-closed, security-critical, identical for both paths):
//   - syntax (herald-name / on-enum / annotations-object / projection-paths)
//     → 422 BEFORE any DB access;
//   - channel existence (a nonexistent herald) → 422 (not an FK-500 on insert in the tx);
//   - RBAC herald.read on EVERY channel (cannot subscribe a notification to a channel
//     without access, ADR-052(g)) → 403.
//
// kind determines the On→event_types mapping (scenario_run.* / command_run.*).
func prepareNotifyTidingsErr(
	deps prepareNotifyDeps, ctx context.Context, claims *jwt.Claims,
	notify []voyageNotifyRequest, kind voyage.Kind, shape notifyTidingShape,
) ([]herald.Tiding, *problem.Details) {
	templates := make([]herald.Tiding, 0, len(notify))
	for i := range notify {
		n := &notify[i]
		idx := "notify[" + strconv.Itoa(i) + "]"

		if !herald.ValidName(n.Herald) {
			return nil, problemDetailsPtr(problem.TypeValidationFailed,
				idx+".herald: имя "+n.Herald+" must match "+herald.NamePattern)
		}
		eventTypes, etErr := notifyEventTypes(kind, n.On)
		if etErr != "" {
			return nil, problemDetailsPtr(problem.TypeValidationFailed, idx+".on: "+etErr)
		}
		if err := herald.ValidateAnnotationsJSON(n.Annotations); err != nil {
			return nil, problemDetailsPtr(problem.TypeValidationFailed,
				idx+".annotations: "+publicErr(err))
		}
		annotations := decodeAnnotations(n.Annotations)
		if err := herald.ValidateProjection(n.Projection); err != nil {
			return nil, problemDetailsPtr(problem.TypeValidationFailed,
				idx+".projection: "+publicErr(err))
		}

		// Channel existence: a nonexistent herald → 422 (not an FK-500 on insert in
		// the tx). The same store pool as the parent CRUD (herald.ExecQueryRower ⊂
		// voyage/cadence.ExecQueryRower).
		if _, err := herald.SelectHeraldByName(ctx, deps.store, n.Herald); err != nil {
			if errors.Is(err, herald.ErrHeraldNotFound) {
				return nil, problemDetailsPtr(problem.TypeValidationFailed,
					idx+": herald "+n.Herald+" does not exist")
			}
			deps.logger.Error(deps.logName+": herald existence check failed", slog.Any("error", err))
			return nil, problemDetailsPtr(problem.TypeInternalError, "notify herald check failed")
		}

		// RBAC herald.read on the channel (ADR-052(g)): cannot subscribe a notification to
		// a channel without access. bare-check (herald channels are not context-scoped in MVP).
		if perr := checkHeraldReadPermissionErr(deps.enforcer, claims.Subject); perr != nil {
			return nil, perr
		}

		t := herald.Tiding{
			Herald:       n.Herald,
			EventTypes:   eventTypes,
			OnlyFailures: n.OnlyFailures,
			OnlyChanges:  n.OnlyChanges,
			Annotations:  annotations,
			Projection:   n.Projection,
			Enabled:      true,
			CreatedByAID: aidPtr(claims.Subject),
		}
		if shape.ephemeral {
			// Voyage path: a one-shot rule. voyage_id/name are stamped later
			// (stampEphemeralTidings), here only the Ephemeral flag.
			t.Ephemeral = true
		} else {
			// Cadence path: a permanent rule, bound IMMEDIATELY by the schedule ULID
			// (rename-safe). The Cadence selector filters the subscription to runs of THIS
			// schedule; CreatedFromCadenceID — the origin marker for cascade deletion (ADR-052
			// §m / ADR-046 §9). The name is deterministically unique (caller — addNotifyName).
			cadenceID := shape.cadenceID
			t.Cadence = &cadenceID
			t.CreatedFromCadenceID = &cadenceID
			t.Name = permanentNotifyName(shape.namePrefix, i)
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// problemDetailsPtr — a helper: a heap [problem.Details] for returning from *Err
// functions (instance empty — the caller sets it). Reduces &-literal noise.
func problemDetailsPtr(typ, detail string) *problem.Details {
	d := problem.New(typ, "", detail)
	return &d
}

// checkHeraldReadPermissionErr — bare-check RBAC herald.read (ADR-052(g)), shared
// by the Voyage/Cadence notify paths (FULL-TYPED ADR-054 §Pattern). nil → allowed;
// on deny — *problem.Details (revoked → TypeOperatorRevokedToken, no-perm → 403).
func checkHeraldReadPermissionErr(enforcer middleware.PermissionChecker, aid string) *problem.Details {
	if err := enforcer.Check(aid, "herald", "read", nil); err != nil {
		if errors.Is(err, rbac.ErrOperatorRevoked) {
			return problemDetailsPtr(problem.TypeOperatorRevokedToken, "archon "+aid+" has been revoked")
		}
		return problemDetailsPtr(problem.TypeForbidden, "operator lacks required permission herald.read")
	}
	return nil
}

// permanentNotifyName builds a deterministically unique name for a permanent
// Cadence notify rule: the first element — `<prefix>-notify`, subsequent ones —
// `<prefix>-notify-<i+1>` (i — the index in the notify array). Tiding.Name — PK,
// a collision is not allowed; the index in a stable array is unique by construction.
// prefix is already truncated by the caller (cappedNotifyPrefix) to the allowed length
// (NamePattern ^[a-z0-9-]{1,63}$).
func permanentNotifyName(prefix string, i int) string {
	name := prefix + "-notify"
	if i > 0 {
		name += "-" + strconv.Itoa(i+1)
	}
	return name
}

// stampEphemeralTidings sets the generated voyage_id and a deterministically unique
// name into the templates (from prepareNotify) — after voyage_id is
// known (buildVoyageRow). The name — `eph-<lowercase-ULID>` (a fresh ULID per
// rule): unique, matches NamePattern (^[a-z0-9-]{1,63}$, a Crockford ULID
// lowercased ⊂ [a-z0-9]). A fresh ULID per rule prevents name collisions
// when several notify elements target one herald.
func stampEphemeralTidings(templates []herald.Tiding, voyageID string) {
	for i := range templates {
		vid := voyageID
		templates[i].VoyageID = &vid
		templates[i].Name = "eph-" + strings.ToLower(audit.NewULID())
	}
}

// notifyEventTypes maps notify.on (run terminals) to audit event types by
// kind (ADR-052(g)). Empty on ⇒ all three terminals. An unknown value → err.
//
//	completed → scenario_run.completed       / command_run.completed
//	failed    → scenario_run.failed          / command_run.failed
//	partial   → scenario_run.partial_failed  / command_run.partial_failed
func notifyEventTypes(kind voyage.Kind, on []string) (eventTypes []string, errMsg string) {
	terminals := on
	if len(terminals) == 0 {
		terminals = []string{notifyOnCompleted, notifyOnFailed, notifyOnPartial}
	}
	prefix := "scenario_run."
	if kind == voyage.KindCommand {
		prefix = "command_run."
	}
	seen := make(map[string]struct{}, len(terminals))
	out := make([]string, 0, len(terminals))
	for _, t := range terminals {
		var action string
		switch t {
		case notifyOnCompleted:
			action = "completed"
		case notifyOnFailed:
			action = "failed"
		case notifyOnPartial:
			action = "partial_failed"
		default:
			return nil, "значение " + t + " must be one of {completed, failed, partial}"
		}
		et := prefix + action
		if _, dup := seen[et]; dup {
			continue
		}
		seen[et] = struct{}{}
		out = append(out, et)
	}
	return out, ""
}

// decodeAnnotations unpacks raw JSON annotations into a map (the object shape is already
// guaranteed by ValidateAnnotationsJSON). Empty/null → nil (= no static
// fields).
func decodeAnnotations(raw json.RawMessage) map[string]any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var m map[string]any
	// Safe: ValidateAnnotationsJSON already confirmed a valid JSON object.
	_ = json.Unmarshal(raw, &m)
	return m
}

// publicErr — public-safe text of a herald validator error (it already forms
// a message without internal SQL/stack; we strip only the pkg prefix `herald: `).
func publicErr(err error) string {
	return strings.TrimPrefix(err.Error(), "herald: ")
}
