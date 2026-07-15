package handlers

// Incarnation secret reveal (NIM-74): from the State view an operator reveals the
// plaintext of a secret declared by the service's `revealable_secrets`, under the
// incarnation.view-secrets permission. The mechanism is generic (not redis-hardcoded):
// a service declares its revealable secrets (id/label/enumerate/vault_ref) in the manifest.
//
// Security invariants (BLOCKER):
//   - the secret value leaves the domain ONLY in the HTTP response body — never in
//     log/audit/OTel/error text (self-audit writes {name,secret_id,key,path},
//     WITHOUT the value — ADR-064 b);
//   - `key` is validated by pattern AND must be ∈ the enumerate array of the CURRENT
//     state BEFORE substitution into the path (anti-forgery); vault.ParseRef is the
//     second layer (traversal `..`/`.`);
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

// reRevealIdent — input form of secret_id / key (lowercase + `-`/`_`, redis
// AclUser.name class). Existence is checked separately (manifest / state);
// invalid form → 422 (garbage never reaches the Vault path).
var reRevealIdent = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// reRevealServiceSeg — safe Vault-path segment for inc.Service before
// substitution into {service} (no `/`,`#`,`..`; kebab). The service is valid per
// reServiceName at registration — this is fail-closed defense-in-depth against path injection.
var reRevealServiceSeg = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// RevealSecretView — domain projection of the 200 body of POST .../secrets/reveal. Value is
// the secret plaintext. The response body is the ONLY exit point of the value from the domain.
type RevealSecretView struct {
	Value string
}

// RevealableSecretItem — one declaration in the discovery response of GET .../secrets/revealable.
type RevealableSecretItem struct {
	SecretID  string
	Label     string
	StatePath string
	Keys      []string
}

// RevealableSecretsView — domain projection of the 200 body of GET .../secrets/revealable.
type RevealableSecretsView struct {
	Items []RevealableSecretItem
}

// RevealSecretTyped — domain function POST /v1/incarnations/{name}/secrets/reveal
// (SELF-AUDIT incarnation.secret_revealed). Resolves the plaintext of secret secretID for
// element key from Vault. Errors are *problemError (422 form / 404 out of scope | no
// secretID | key not in state | floor | no value in Vault / 500 failure).
//
// ALL branches after the RBAC gate are audited (parity auditInputVault): success
// result:"ok", denied result:"denied"+reason (the value is NOT stored). RBAC-403 is
// gate-level (the handler doesn't run) and is not audited here. Form-422 (broken
// name/secret_id/key) is a malformed request BEFORE incarnation resolve, not a denied-reveal.
func (h *IncarnationHandler) RevealSecretTyped(ctx context.Context, claims *jwt.Claims, name, secretID, key string) (RevealSecretView, error) {
	var zero RevealSecretView

	if !incarnation.ValidName(name) {
		return zero, incProblem(problem.TypeValidationFailed, "path 'name' must match "+incarnation.NamePattern)
	}
	if !reRevealIdent.MatchString(secretID) {
		return zero, incProblem(problem.TypeValidationFailed, "field 'secret_id' must match "+reRevealIdent.String())
	}
	if !reRevealIdent.MatchString(key) {
		return zero, incProblem(problem.TypeValidationFailed, "field 'key' must match "+reRevealIdent.String())
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

	// Materialize the manifest at inc.ServiceVersion + look up secretID. Best-effort
	// on snapshot unavailability → 404 (secret not revealable), not 500.
	rs, ok := h.revealableSecretByID(ctx, inc, secretID)
	if !ok {
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "unknown_secret_id", "")
		return zero, revealNotFound(secretID, name)
	}

	// key must be ∈ the enumerate array of the CURRENT state (anti-forgery: cannot
	// reveal a path not present in state right now).
	if !containsString(enumerateStateKeys(inc.State, rs.Enumerate), key) {
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "key_not_in_state", "")
		return zero, incProblem(problem.TypeNotFound,
			"key "+key+" is not present in "+statePathTail(rs.Enumerate)+" of incarnation "+name)
	}

	if h.vault == nil {
		return zero, revealNotFound(secretID, name)
	}

	// inc.Service is a Vault-path segment; validate BEFORE substitution (anti-injection:
	// no `/`,`#`,`..`). The service is valid per reServiceName at registration; here it's
	// fail-closed defense-in-depth (data anomaly → 404, don't read).
	if !reRevealServiceSeg.MatchString(inc.Service) {
		h.logger.Warn("incarnation.reveal-secret: incarnation service unsafe for vault path",
			slog.String("name", name), slog.String("service", inc.Service))
		return zero, revealNotFound(secretID, name)
	}

	// Literal substitution of validated values (inc.Service — reRevealServiceSeg,
	// inc.Name — NamePattern, key — reRevealIdent AND ∈ state) + vault.ParseRef as a 2nd layer.
	rendered := strings.ReplaceAll(rs.VaultRef, "{service}", inc.Service)
	rendered = strings.ReplaceAll(rendered, "{incarnation}", inc.Name)
	rendered = strings.ReplaceAll(rendered, "{key}", key)

	body, field := rendered, ""
	if i := strings.LastIndexByte(rendered, '#'); i >= 0 {
		body, field = rendered[:i], rendered[i+1:]
	}
	logical, perr := vault.ParseRef("vault:" + body)
	if perr != nil {
		// Traversal / broken path form — secret not revealable (don't leak path details).
		h.logger.Error("incarnation.reveal-secret: vault ref invalid",
			slog.String("name", name), slog.String("secret_id", secretID), slog.Any("error", perr))
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "ref_invalid", "")
		return zero, revealNotFound(secretID, name)
	}

	// ★ Positive allowlist (NIM-74 C1, the MAIN guard): reveal reads ONLY under the
	// secret namespace of its own incarnation of its own service. The trailing `/` is
	// MANDATORY (otherwise prefix-confusion: `redis-prod` would match `redis-prod-other`).
	// AFTER ParseRef, BEFORE ReadKV.
	allowedPrefix := "secret/" + inc.Service + "/" + inc.Name + "/"
	if !strings.HasPrefix(logical, allowedPrefix) {
		h.logger.Warn("incarnation.reveal-secret: path outside service/incarnation namespace",
			slog.String("name", name), slog.String("secret_id", secretID), slog.String("path", logical))
		h.auditReveal(ctx, claims.Subject, name, secretID, key, "denied", "out_of_service_scope", logical)
		return zero, revealNotFound(secretID, name)
	}

	// FLOOR backstop (NIM-74 C1, parity scenario/input_vault.go §3): a safeguard for the
	// edge-case of a service with a reserved name (secret/keeper/, secret/internal/).
	// Unconditionally BEFORE ReadKV.
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
	value, ok := selectRevealField(data, field)
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
// revealable (READ, no audit). For each revealable_secret it collects keys from the
// enumerate array of the current state. Out of scope → 404 (parity Get). An empty list is valid.
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
	for _, rs := range h.revealableSecretsFor(ctx, inc) {
		keys := enumerateStateKeys(inc.State, rs.Enumerate)
		if keys == nil {
			keys = []string{}
		}
		items = append(items, RevealableSecretItem{
			SecretID:  rs.ID,
			Label:     rs.Label,
			StatePath: statePathTail(rs.Enumerate),
			Keys:      keys,
		})
	}
	return RevealableSecretsView{Items: items}, nil
}

// revealableSecretByID materializes the incarnation manifest and looks up the declaration by id.
func (h *IncarnationHandler) revealableSecretByID(ctx context.Context, inc *incarnation.Incarnation, secretID string) (config.RevealableSecret, bool) {
	for _, rs := range h.revealableSecretsFor(ctx, inc) {
		if rs.ID == secretID {
			return rs, true
		}
	}
	return config.RevealableSecret{}, false
}

// revealableSecretsFor materializes the service snapshot at the incarnation's version
// (inc.ServiceVersion — the same authoritative version as secretSchemaForIncarnation) and
// returns the manifest's `revealable_secrets`. Best-effort: loader/services nil,
// service not registered, load error → nil (nothing to reveal).
func (h *IncarnationHandler) revealableSecretsFor(ctx context.Context, inc *incarnation.Incarnation) []config.RevealableSecret {
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
	return art.Manifest.RevealableSecrets
}

// enumerateStateKeys resolves the enumerate array from state (`state.<array>`) and collects
// element names (`element.name` — the redis AclUser.name convention). A missing path /
// non-array → nil (fail-closed, no panic). Keys are filtered by the same reRevealIdent
// that validates reveal (discovery does not advertise a key that reveal would reject 422),
// and deduped (a duplicate name in state doesn't produce duplicates in discovery/the check set).
func enumerateStateKeys(state map[string]any, enumerate string) []string {
	v, ok := resolveStatePath(state, enumerate)
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
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
		nm, ok := m["name"].(string)
		if !ok || nm == "" || !reRevealIdent.MatchString(nm) {
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

// selectRevealField picks a single string field of the secret (the `#field` form is
// mandatory). Empty field / absent / non-string → ("", false): we reveal only a
// scalar value, not a serialized structure.
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
// snapshot unavailable / broken ref / vault not configured): one text so the
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
