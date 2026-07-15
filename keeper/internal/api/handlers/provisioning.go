// Provisioning-policy handler, Operator API (ADR-058 Part B) — runtime read and
// change of the `provisioning_allowed_methods` policy (keeper_settings): the list
// of allowed operator-CREATION methods ({user,ldap,oidc}). GET — read (no
// audit, permission provisioning.read); PUT — write+audit (event
// provisioning.policy_changed, permission provisioning.update).
//
// Domain layer over serviceregistry: GET reads the current policy snapshot via
// [ProvisioningPolicyReader] (Holder, a cluster-consistent atomic snapshot); PUT
// validates the list → CSV → [serviceregistry.Service.SetSetting] (upsert +
// cluster-wide Redis invalidate, the same service-invalidate channel — Holder.refresh
// re-reads the policy on all nodes). RBAC lives in middleware (router.go); here —
// error mapping to RFC 7807 + building the audit-payload.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

// ProvisioningPolicyReader — narrow read surface of the current policy from the snapshot
// (cluster-consistent, atomic). Implemented by *serviceregistry.Holder; declared as an
// interface so the handler can be tested without spinning up a Holder. ProvisioningPolicy
// returns a sorted list of allowed methods + a set flag (whether the policy is
// set; set=false → default "everything allowed", methods=nil).
type ProvisioningPolicyReader interface {
	ProvisioningPolicy() (methods []string, set bool)
}

// ProvisioningPolicyHandler — GET/PUT /v1/provisioning-policy. reader reads the
// policy snapshot (GET), svc writes it (PUT via SetSetting + invalidate). Both
// dependencies are required (the handler mounts only when both are non-nil, see
// router.go). Holds no state; safe for concurrent use.
type ProvisioningPolicyHandler struct {
	reader ProvisioningPolicyReader
	svc    *serviceregistry.Service
	logger *slog.Logger
}

// NewProvisioningPolicyHandler builds the handler. reader/svc are required (panic on
// nil — the only misconfiguration point; caller must pass non-nil).
func NewProvisioningPolicyHandler(reader ProvisioningPolicyReader, svc *serviceregistry.Service, logger *slog.Logger) *ProvisioningPolicyHandler {
	if reader == nil {
		panic("handlers.NewProvisioningPolicyHandler: reader is nil")
	}
	if svc == nil {
		panic("handlers.NewProvisioningPolicyHandler: serviceregistry.Service is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ProvisioningPolicyHandler{reader: reader, svc: svc, logger: logger}
}

// ProvisioningPolicySpecStub — a non-empty stub for generating the huma OpenAPI
// fragment (the handler is not called during dump, only non-nil is needed for the register
// functions' no-op check; reader/svc nil — the handler does not execute in spec mode, parity
// ServiceSpecStub).
func ProvisioningPolicySpecStub() *ProvisioningPolicyHandler {
	return &ProvisioningPolicyHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ProvisioningPolicyView — FLAT domain body of the GET/PUT response (handler-native).
// AllowedMethods — sorted list of allowed methods (when PolicySet=false
// — nil, package api projects it to `[]`); PolicySet=false → policy not set (default
// "everything allowed").
type ProvisioningPolicyView struct {
	AllowedMethods []string
	PolicySet      bool
}

// GetTyped — domain function GET /v1/provisioning-policy (READ, no audit): reads the
// current policy from the reader's snapshot. No errors (snapshot read).
func (h *ProvisioningPolicyHandler) GetTyped() ProvisioningPolicyView {
	methods, set := h.reader.ProvisioningPolicy()
	return ProvisioningPolicyView{AllowedMethods: methods, PolicySet: set}
}

// ProvisioningPolicyUpdateInput — NATIVE request form for PUT /v1/provisioning-policy.
type ProvisioningPolicyUpdateInput struct {
	AllowedMethods []string
}

// ProvisioningPolicyUpdateReply — result of PutTyped: 200 body (new policy) +
// audit fields (new list + the previous one, if any).
type ProvisioningPolicyUpdateReply struct {
	Body           ProvisioningPolicyView
	AllowedMethods []string
	Previous       []string
	PreviousSet    bool
}

// AuditPayload builds the audit-payload for the PUT route (provisioning.policy_changed):
// the new allowed_methods list + previous (the prior list, if the policy was
// set). No secrets (method names are public).
func (r ProvisioningPolicyUpdateReply) AuditPayload() middleware.AuditPayload {
	p := middleware.AuditPayload{"allowed_methods": r.AllowedMethods}
	if r.PreviousSet {
		p["previous"] = r.Previous
	}
	return p
}

// PutTyped — domain function PUT /v1/provisioning-policy (WRITE+AUDIT): validates
// the list (non-empty, each ∈ {user,ldap,oidc}), joins it into CSV, writes via
// serviceregistry.Service.SetSetting (upsert + cluster-wide invalidate). claims provides
// the callerAID for updated_by_aid. Errors are *problemError (422 anti-lockout/invalid
// method, 404 caller-not-found, 500), success is [ProvisioningPolicyUpdateReply].
//
// Anti-lockout: empty list → 422 (cannot forbid ALL methods). Validation and
// normalization are delegated to serviceregistry.ParseProvisioningMethods (the single
// source for the method domain and semantics), so PUT and PoolSource.Load don't diverge.
func (h *ProvisioningPolicyHandler) PutTyped(ctx context.Context, claims *jwt.Claims, req ProvisioningPolicyUpdateInput) (ProvisioningPolicyUpdateReply, error) {
	var zero ProvisioningPolicyUpdateReply

	// Normalization + validation via the single parser (lowercase/trim/dedup + domain
	// {user,ldap,oidc} + anti-lockout "non-empty"). CSV from the input list is the same
	// format that keeper_settings stores and PoolSource.Load reads.
	csv := joinMethodsCSV(req.AllowedMethods)
	methods, err := serviceregistry.ParseProvisioningMethods(csv)
	switch {
	case err == nil:
	case errors.Is(err, serviceregistry.ErrEmptyProvisioningMethods):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"allowed_methods must be non-empty (anti-lockout): cannot disable operator provisioning by all methods")}
	case errors.Is(err, serviceregistry.ErrInvalidProvisioningMethod):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	default:
		h.logger.Error("provisioning.policy: parse methods failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "update provisioning policy failed")}
	}

	// Previous value for the audit-payload (best-effort; the reader's snapshot is the
	// cluster-consistent current status).
	prev, prevSet := h.reader.ProvisioningPolicy()

	// Canonical normalized form for writing: sorted set → CSV.
	normalized := sortedMethods(methods)
	callerAID := claims.Subject
	if _, err := h.svc.SetSetting(ctx, serviceregistry.SetSettingInput{
		Key:       serviceregistry.SettingProvisioningAllowedMethods,
		Value:     joinMethodsCSV(normalized),
		CallerAID: &callerAID,
	}); err != nil {
		switch {
		case errors.Is(err, serviceregistry.ErrOperatorNotFound):
			return zero, &problemError{problem.New(problem.TypeNotFound, "",
				"caller AID "+callerAID+" not found in operators registry")}
		case errors.Is(err, serviceregistry.ErrInvalidSettingKey):
			// Unreachable (the key is a well-known const), defensive.
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		default:
			h.logger.Error("provisioning.policy: set setting failed",
				slog.String("by_aid", callerAID), slog.Any("error", err))
			return zero, &problemError{problem.New(problem.TypeInternalError, "", "update provisioning policy failed")}
		}
	}

	return ProvisioningPolicyUpdateReply{
		Body:           ProvisioningPolicyView{AllowedMethods: normalized, PolicySet: true},
		AllowedMethods: normalized,
		Previous:       prev,
		PreviousSet:    prevSet,
	}, nil
}

// sortedMethods turns the set of allowed methods into a sorted list.
func sortedMethods(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// joinMethodsCSV joins the list of methods into the keeper_settings CSV form (with ',').
func joinMethodsCSV(methods []string) string {
	return strings.Join(methods, ",")
}
