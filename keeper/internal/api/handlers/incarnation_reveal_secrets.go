package handlers

// Incarnation secret reveal (NIM-74; rebuilt onto the derived path by [ADR-0083] §2):
// from the State view an operator reveals the plaintext of a secret the service
// declared as `type: secret` in its `state_schema`, under the incarnation.view-secrets
// permission.
//
// The service authors NO Vault path. The location comes from
// [config.SecretField.VaultPath] — the same derivation `core.state.*` writes
// through — so reveal and write cannot disagree about where a value lives, which is
// what the old hand-written `revealable_secrets[].vault_ref` could not guarantee.
//
// Security invariants (BLOCKER):
//   - the secret value leaves the domain ONLY in the HTTP response body — never in
//     log/audit/OTel/error text (self-audit writes {name,secret_id,key,path},
//     WITHOUT the value — ADR-064 b);
//   - `key` is validated as a Vault path segment AND must be ∈ the collection of the
//     CURRENT state BEFORE it becomes a path segment (anti-forgery); the positive
//     allowlist and the Vault floor are layers 2 and 3;
//   - the manifest version is ALWAYS inc.ServiceVersion (parity secretSchemaForIncarnation):
//     the client does not set the version (anti version-craft).

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// reRevealSecretID — input form of secret_id: the state field alone (a scalar
// secret) or `<state field>.<property>` (one secret of a collection element) —
// [config.SecretField.ID]. Each part is the Vault-segment class, so a form-valid id
// is one the derivation could produce; existence is checked separately against the
// manifest. Invalid form → 422, and garbage never reaches the Vault path.
var reRevealSecretID = regexp.MustCompile(`^[a-zA-Z0-9_-]+(\.[a-zA-Z0-9_-]+)?$`)

// reRevealServiceSeg — safe Vault-path segment for inc.Service before it is
// substituted into the derived path (no `/`,`#`,`..`; kebab). The service is valid per
// reServiceName at registration — this is fail-closed defense-in-depth against path injection.
var reRevealServiceSeg = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// RevealSecretView — domain projection of the 200 body of POST .../secrets/reveal. Value is
// the secret plaintext. The response body is the ONLY exit point of the value from the domain.
type RevealSecretView struct {
	Value string
}

// RevealableSecretItem — one declaration in the discovery response of GET .../secrets/revealable.
type RevealableSecretItem struct {
	SecretID string
	Label    string
	// StatePath is the top-level state_schema property holding the secret(s).
	StatePath string
	// Collection distinguishes "one secret per element, reveal needs a key" from
	// "one secret for the whole incarnation, reveal takes no key". Without it an
	// empty Keys list is ambiguous — a scalar secret and a collection with no
	// elements look identical, and the scalar one would never be offered.
	Collection bool
	Keys       []string
}

// RevealableSecretsView — domain projection of the 200 body of GET .../secrets/revealable.
type RevealableSecretsView struct {
	Items []RevealableSecretItem
}

// RevealSecretTyped — domain function POST /v1/incarnations/{name}/secrets/reveal
// (SELF-AUDIT incarnation.secret_revealed). Resolves the plaintext of secret secretID for
// element key from Vault. Errors are *problemError (422 form / key arity, 404 out of scope |
// no secretID | key not in state | floor | no value in Vault / 500 failure).
//
// ALL branches after the RBAC gate are audited (parity auditInputVault): success
// result:"ok", denied result:"denied"+reason (the value is NOT stored). RBAC-403 is
// gate-level (the handler doesn't run) and is not audited here. A form-422 raised
// BEFORE the incarnation resolve is a malformed request, not a denied-reveal, and is
// not audited; the key-arity 422 sits after the scope gate and is.
func (h *IncarnationHandler) RevealSecretTyped(ctx context.Context, claims *jwt.Claims, name, secretID, key string) (RevealSecretView, error) {
	var zero RevealSecretView

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if !reRevealSecretID.MatchString(secretID) {
		return zero, incProblem(problem.TypeValidationFailed, "field 'secret_id' must match "+reRevealSecretID.String())
	}
	// Empty is legal — a scalar secret has no element to address. A non-empty key
	// must be a safe path segment: the SAME rule core.state.* applies when it
	// writes, so a value stored under a given key is revealable under it.
	if key != "" && !config.ValidVaultPathSegment(key) {
		return zero, incProblem(problem.TypeValidationFailed, "field 'key' must be a Vault path segment (letters, digits, `_` and `-`)")
	}

	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			// Resource absent — not audited (not a denied-reveal, nothing to attribute).
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.reveal-secret: select failed", slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "select incarnation failed")
	}
	inScope := h.GetInScopeFor(claims, "view-secrets")
	if inScope == nil || !inScope(inc) {
		// Out of scope — 404 (parity Get: don't reveal the existence of another's incarnation).
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "out_of_scope", "")
		return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
	}

	// Materialize the manifest at inc.ServiceVersion + look up the declaration.
	// Best-effort on snapshot unavailability → 404 (secret not revealable), not 500.
	field, ok := h.revealableSecretByID(ctx, inc, secretID)
	if !ok {
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "unknown_secret_id", "")
		return zero, revealNotFound(secretID, name)
	}

	if field.Collection() {
		// key must be ∈ the collection of the CURRENT state (anti-forgery: cannot
		// reveal a path not present in state right now).
		if !containsString(enumerateStateKeys(inc.State, field), key) {
			h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "key_not_in_state", "")
			return zero, incProblem(problem.TypeNotFound,
				"key "+key+" is not present in "+field.State+" of incarnation "+name)
		}
	} else if key != "" {
		// A scalar secret has one value and no element to address. Answering 404
		// here would read as "no such secret" for a request that names a real one.
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "key_not_expected", "")
		return zero, incProblem(problem.TypeValidationFailed,
			"secret "+secretID+" is a scalar field -- field 'key' must be empty")
	}

	if h.vault == nil {
		return zero, revealNotFound(secretID, name)
	}

	// inc.Service is a Vault-path segment; validate BEFORE it is substituted
	// (anti-injection: no `/`,`#`,`..`). The service is valid per reServiceName at
	// registration; here it's fail-closed defense-in-depth (data anomaly → 404, don't read).
	if !reRevealServiceSeg.MatchString(inc.Service) {
		h.logger.Warn("incarnation.reveal-secret: incarnation service unsafe for vault path",
			slog.String("name", name), slog.String("service", inc.Service))
		return zero, revealNotFound(secretID, name)
	}

	// FLOOR, first half (NIM-706): a service whose NAME is a reserved namespace. It is
	// refused at registration and refused again by the derivation below, so reaching
	// here means a row that predates the rule or a corrupted `service` column — deny
	// before deriving, so the audit records WHY rather than the derivation's generic
	// "something unsafe". Checked on the name because that is what the derivation
	// compares; the path-shaped half below stays for what a name cannot see.
	if config.IsReservedVaultNamespace(inc.Service) {
		h.logger.Warn("incarnation.reveal-secret: service name is a reserved vault namespace",
			slog.String("name", name), slog.String("service", inc.Service))
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "floor_denied", "")
		return zero, revealNotFound(secretID, name)
	}

	// The derivation ([ADR-0083] §1) checks every segment itself and fails closed;
	// a refusal here means state or the manifest carries something unsafe.
	logical, derr := field.VaultPath(h.vaultMount, inc.Service, inc.Name, key)
	if derr != nil {
		h.logger.Error("incarnation.reveal-secret: vault path derivation refused",
			slog.String("name", name), slog.String("secret_id", secretID), slog.Any("error", derr))
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "ref_invalid", "")
		return zero, revealNotFound(secretID, name)
	}

	// ★ Positive allowlist (NIM-74 C1, the MAIN guard): reveal reads ONLY under the
	// secret namespace of its own incarnation of its own service. The trailing `/` is
	// MANDATORY (otherwise prefix-confusion: `redis-prod` would match `redis-prod-other`).
	// A derived path satisfies it by construction — the check stays because it is
	// assembled here INDEPENDENTLY of the derivation, and so still bites if the
	// derivation ever changes what it emits.
	allowedPrefix := config.EffectiveVaultMount(h.vaultMount) + "/" + inc.Service + "/" + inc.Name + "/"
	if !strings.HasPrefix(logical, allowedPrefix) {
		h.logger.Warn("incarnation.reveal-secret: path outside service/incarnation namespace",
			slog.String("name", name), slog.String("secret_id", secretID), slog.String("path", logical))
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "out_of_service_scope", logical)
		return zero, revealNotFound(secretID, name)
	}

	// FLOOR, second half (NIM-74 C1, parity scenario/input_vault.go §3). The name check
	// above cannot see a MOUNT that spells a reserved word: `vault.kv_mount: keeper`
	// puts every service's secrets under `keeper/…` while each service name is
	// blameless. [config.PathUnderReservedNamespace] reads segment 0 as well as 1, so
	// this catches it. Unconditionally BEFORE ReadKV.
	if config.DeniedByVaultFloor(logical, nil) {
		h.logger.Warn("incarnation.reveal-secret: vault floor denied",
			slog.String("name", name), slog.String("secret_id", secretID), slog.String("path", logical))
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "floor_denied", logical)
		return zero, revealNotFound(secretID, name)
	}

	data, rerr := h.vault.ReadKV(ctx, logical)
	if rerr != nil {
		if errors.Is(rerr, vault.ErrVaultKVNotFound) {
			h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "vault_miss", logical)
			return zero, incProblem(problem.TypeNotFound, "secret value not found in Vault")
		}
		h.logger.Error("incarnation.reveal-secret: vault read failed",
			slog.String("name", name), slog.String("secret_id", secretID),
			slog.String("path", logical), slog.Any("error", rerr))
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "read_error", logical)
		return zero, incProblem(problem.TypeInternalError, "read secret failed")
	}
	value, ok := selectRevealField(data, field.VaultField())
	if !ok {
		// No field / nopass / non-string value — nothing to reveal.
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "field_missing", logical)
		return zero, incProblem(problem.TypeNotFound, "secret value not found in Vault")
	}

	// SELF-AUDIT of success (after ReadKV): the view fact WITHOUT the value (ADR-064 b).
	h.auditReveal(ctx, claims.Subject, name, secretID, key, "ok", "", logical)

	return RevealSecretView{Value: value}, nil
}

// auditReveal writes the incarnation.secret_revealed event (success result:"ok" or
// denied result:"denied"+reason). The secret VALUE is NEVER placed in the payload
// (ADR-064 b); path is the logical Vault path (a location, not the secret) when present. An
// audit failure does NOT fail reveal (parity auditInputVault) — warn. h.auditW nil → no-op.
func (h *IncarnationHandler) auditReveal(ctx context.Context, aid, name, secretID, key, result, reason, path string) {
	if h.auditW == nil {
		return
	}
	payload := map[string]any{
		"name":      name,
		"secret_id": secretID,
		"key":       key,
		"result":    result,
	}
	if reason != "" {
		payload["reason"] = reason
	}
	if path != "" {
		payload["path"] = path
	}
	if err := h.auditW.Write(ctx, &audit.Event{
		EventType: audit.EventIncarnationSecretRevealed,
		Source:    apimiddleware.ScenarioInvocationSource(ctx),
		ArchonAID: aid,
		Payload:   payload,
	}); err != nil {
		h.logger.Warn("incarnation.reveal-secret: audit write failed",
			slog.String("name", name), slog.String("secret_id", secretID),
			slog.String("result", result), slog.Any("error", err))
	}
}

// RevealableSecretsTyped — domain function GET /v1/incarnations/{name}/secrets/
// revealable (READ, no audit). For each declared secret it collects the keys present
// in the current state (none for a scalar field). Out of scope → 404 (parity Get). An
// empty list is valid.
func (h *IncarnationHandler) RevealableSecretsTyped(ctx context.Context, claims *jwt.Claims, name string) (RevealableSecretsView, error) {
	zero := RevealableSecretsView{Items: []RevealableSecretItem{}}

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
		}
		h.logger.Error("incarnation.revealable-secrets: select failed", slog.String("name", name), slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "select incarnation failed")
	}
	inScope := h.GetInScopeFor(claims, "view-secrets")
	if inScope == nil || !inScope(inc) {
		return zero, incProblem(problem.TypeNotFound, "incarnation "+name+" not found")
	}

	items := make([]RevealableSecretItem, 0)
	for _, f := range h.revealableSecretsFor(ctx, inc) {
		keys := enumerateStateKeys(inc.State, f)
		if keys == nil {
			keys = []string{}
		}
		items = append(items, RevealableSecretItem{
			SecretID:   f.ID(),
			Label:      f.Label,
			StatePath:  f.State,
			Collection: f.Collection(),
			Keys:       keys,
		})
	}
	return RevealableSecretsView{Items: items}, nil
}

// revealableSecretByID materializes the incarnation manifest and looks up the
// declaration by [config.SecretField.ID].
func (h *IncarnationHandler) revealableSecretByID(ctx context.Context, inc *incarnation.Incarnation, secretID string) (config.SecretField, bool) {
	for _, f := range h.revealableSecretsFor(ctx, inc) {
		if f.ID() == secretID {
			return f, true
		}
	}
	return config.SecretField{}, false
}

// revealableSecretsFor materializes the service snapshot at the incarnation's version
// (inc.ServiceVersion — the same authoritative version as secretSchemaForIncarnation) and
// returns the secrets declared in its `state_schema`. Best-effort: loader/services nil,
// service not registered, load error → nil (nothing to reveal).
//
// The refusals of [config.CollectSecretFields] are dropped rather than surfaced: the
// same issues are errors at load time (validateSecretFields), so a snapshot that
// reaches here carries none. Failing discovery on them would turn a torn invariant
// into a 500 on a read-only endpoint.
func (h *IncarnationHandler) revealableSecretsFor(ctx context.Context, inc *incarnation.Incarnation) []config.SecretField {
	if h.loader == nil || h.services == nil || inc == nil {
		return nil
	}
	ref, ok := h.services.Resolve(inc.Service)
	if !ok {
		return nil
	}
	if inc.ServiceVersion != "" {
		ref.Ref = inc.ServiceVersion
	}
	art, err := h.loader.Load(ctx, ref)
	if err != nil || art == nil || art.Manifest == nil {
		return nil
	}
	fields, _ := config.CollectSecretFields(art.Manifest.StateSchema)
	return fields
}

// enumerateStateKeys collects the keys of a collection secret from the current state:
// the value of the sibling property named by the declaration's `key:` on every element
// of `state.<field>`. A scalar field has no keys (nil). A missing path / non-array →
// nil (fail-closed, no panic).
//
// Keys are filtered by [config.ValidVaultPathSegment] — the same rule
// core.state.* applies on write, so discovery never advertises a key reveal
// would reject — and deduped (a duplicate in state doesn't produce duplicates in
// discovery or in the check set).
func enumerateStateKeys(state map[string]any, f config.SecretField) []string {
	if !f.Collection() {
		return nil
	}
	arr, ok := state[f.State].([]any)
	if !ok {
		return nil
	}
	seen := make(map[string]struct{}, len(arr))
	out := make([]string, 0, len(arr))
	for _, el := range arr {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		nm, ok := m[f.Key].(string)
		if !ok || !config.ValidVaultPathSegment(nm) {
			continue
		}
		if _, dup := seen[nm]; dup {
			continue
		}
		seen[nm] = struct{}{}
		out = append(out, nm)
	}
	return out
}

// selectRevealField picks the single string field of the secret named by the
// declaration ([config.SecretField.VaultField]). Absent / non-string → ("", false): we
// reveal a scalar value, not a serialized structure.
func selectRevealField(data map[string]any, field string) (string, bool) {
	if field == "" {
		return "", false
	}
	v, ok := data[field]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return s, true
}

// revealNotFound — a unified 404 "secret not revealable" (no secretID /
// snapshot unavailable / broken derivation / vault not configured): one text so the
// reasons aren't distinguishable from outside.
func revealNotFound(secretID, name string) error {
	return incProblem(problem.TypeNotFound, "secret "+secretID+" is not revealable for incarnation "+name)
}

// containsString — linear search (key sets are small: a handful to tens of ACL users).
func containsString(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}
