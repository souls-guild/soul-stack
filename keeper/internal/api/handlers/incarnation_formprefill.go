package handlers

// Form-prefill handler of the Operator API (`POST /v1/incarnations/{name}/scenarios/
// {scenario}/form-prefill`) — day-2 pre-fill of the scenario's UI form with the CURRENT
// incarnation.state values (docs/input.md → "Pre-fill from state").
//
// Scenario-schema fields that declare `prefill_from_state: state.<path>` must open in the
// form not empty but with the current value of the corresponding state field (the operator
// edits the delta, does not re-enter everything). This endpoint is the sole resolver of such
// prefill hints: it reads the state of ONE incarnation {name} and returns
// `{values: {field: current-value}}`.
//
// Security invariants (blocker):
//   - path-whitelist: STRICTLY the paths declared via `prefill_from_state` in the
//     scenario schema are resolved. The client does NOT pass a path — the backend reads
//     the scenario schema, builds the set of declared paths and resolves only those.
//     Arbitrary state access through the endpoint is impossible;
//   - secret-exclusion: fields marked by the service secret schema
//     (secretSchemaForIncarnation) are EXCLUDED from the response entirely (pre-filling a
//     mask is useless), and the remaining values are additionally run through
//     maskWithSchema (defense-in-depth: a nested secret inside a prefill value is
//     suppressed by the same declarative layer, ADR-010 §7.4).
//
// RBAC — incarnation.get (read of a single incarnation: whoever sees the incarnation
// also gets the prefill of its form). No new permission is introduced, the scope selector
// is the same inScope as GetTyped/HistoryTyped (ADR-047). Read-only, no audit.

import (
	"context"
	"errors"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
)

// FormPrefillResult — NATIVE result of POST .../form-prefill (handler-native).
// Values — a map `field → current-value`: only scenario-schema fields that declare
// `prefill_from_state` AND whose path is covered by the current incarnation.state AND that
// are NOT secret. Fields with an uncovered path / secret are omitted. Non-nil (empty map
// when there are no prefill fields). The api package projects this into a native reply-DTO.
type FormPrefillResult struct {
	Values map[string]any
}

// FormPrefillTyped — domain function for POST /v1/incarnations/{name}/scenarios/
// {scenario}/form-prefill (READ, no audit). inScope — the RBAC scope predicate
// (ADR-047, action=get): out of scope → 404 (like GetTyped, we do not reveal someone
// else's incarnation).
//
// The schema version is ALWAYS inc.ServiceVersion (the deployed incarnation's version):
// the client does NOT set the version. This is security-critical — otherwise the path
// whitelist of declared prefill paths and the secret-set would resolve from DIFFERENT
// versions (whitelist from the client ref, secret-set always from ServiceVersion in
// secretSchemaForIncarnation), and crafting a ref to a version where a sensitive field is
// not yet marked secret would return its value. A single authoritative version closes the
// version-craft vector; maskWithSchema remains the 2nd defense-in-depth layer.
//
// Errors — *problemError (422 invalid path segment / 404 no incarnation or out of scope /
// 500 service-resolve failure). Best-effort over the schema: no loader / service not
// registered / scenario does not parse → empty values (the form opens without prefill, not
// 500 — prefill is optional).
func (h *IncarnationHandler) FormPrefillTyped(ctx context.Context, name, scenarioName string, inScope func(*incarnation.Incarnation) bool) (FormPrefillResult, error) {
	zero := FormPrefillResult{Values: map[string]any{}}

	if !incarnation.ValidName(name) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'name' must match "+incarnation.NamePattern)}
	}
	if !scenario.ValidScenarioName(scenarioName) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'scenario' must match "+scenario.ScenarioNamePattern)}
	}

	inc, err := incarnation.SelectByName(ctx, h.db, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "incarnation "+name+" not found")}
		}
		h.logger.Error("incarnation.form-prefill: select failed", slog.String("name", name), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "select incarnation failed")}
	}
	if inScope == nil || !inScope(inc) {
		// Out of scope — 404 (parity GetTyped: we do not reveal the existence of someone
		// else's incarnation via a 403/404 distinction).
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "incarnation "+name+" not found")}
	}

	// path-whitelist: the set of prefill paths declared in the scenario schema.
	// The client does not pass a path — we take it from the schema of the incarnation
	// version (inc.ServiceVersion), the same as secretSchemaForIncarnation. Best-effort:
	// schema unavailable → empty values.
	fields := h.prefillFieldsForScenario(ctx, inc, scenarioName)
	if len(fields) == 0 {
		return zero, nil
	}

	// secret-exclusion (blocker): secret fields are excluded from prefill entirely.
	// The service schema (state + create-input) is materialized once; nil →
	// degrade to the vault+regex defense-in-depth below (maskWithSchema).
	secretSchema := h.secretSchemaForIncarnation(ctx, inc)

	values := make(map[string]any, len(fields))
	for field, path := range fields {
		val, ok := resolveStatePath(inc.State, path)
		if !ok {
			continue // path not covered by the current state — field is omitted.
		}
		// exclude the secret field entirely (pre-filling a mask is meaningless). The paths
		// of state secrets in secretSchemaForIncarnation are stored RELATIVE to the state
		// root (collectStateSchemaSecrets: `admin_token`, `tls.key`) — without the
		// `state.` prefix; we compare against the tail form of the path.
		if secretSchema != nil && secretSchema.IsSecret(statePathTail(path)) {
			continue
		}
		values[field] = val
	}

	// defense-in-depth (ADR-010 §7.4): a nested secret inside a prefill value
	// (map/list with a secret leaf) is suppressed by the declarative+vault+regex layer, as
	// in the read-path GET incarnation. Top-level secret fields are already excluded above;
	// here we guard against nested secrets in a composite value. A masked leaf stays in the
	// form as a placeholder — better a mask than a leak.
	masked := maskWithSchema(values, secretSchema)
	return FormPrefillResult{Values: masked}, nil
}

// prefillFieldsForScenario builds the set of declared prefill fields of the scenario
// schema: `field → state.<path>` (path-whitelist). It materializes the service snapshot at
// the incarnation version (inc.ServiceVersion — the SAME authoritative version as
// secretSchemaForIncarnation, the client does NOT set it), reads `scenario/<name>/
// main.yml`, parses the input schema and collects fields with a non-empty
// `prefill_from_state`. Best-effort (parity collectCreateInputSecrets): any failure → nil
// (form without prefill, not an error). Predicate access to arbitrary state paths is
// impossible — only what the schema author declared.
func (h *IncarnationHandler) prefillFieldsForScenario(ctx context.Context, inc *incarnation.Incarnation, scenarioName string) map[string]string {
	if h.loader == nil || h.services == nil || inc == nil {
		return nil
	}
	serviceRef, ok := h.services.Resolve(inc.Service)
	if !ok {
		return nil
	}
	// The version is the one the incarnation was created/migrated at (the same schema that
	// ListScenarios handed the form AND that secretSchemaForIncarnation uses).
	// There is no client override: a single version for whitelist+secret-set is the anti
	// version-craft invariant (see the FormPrefillTyped doc comment).
	if inc.ServiceVersion != "" {
		serviceRef.Ref = inc.ServiceVersion
	}
	art, err := h.loader.Load(ctx, serviceRef)
	if err != nil || art == nil {
		return nil
	}
	data, err := h.loader.ReadFile(art, "scenario/"+scenarioName+"/main.yml")
	if err != nil || len(data) == 0 {
		return nil
	}
	scn, _, _, perr := artifact.LoadScenarioManifestResolved(art, "scenario/"+scenarioName+"/main.yml", data)
	if perr != nil || scn == nil {
		return nil
	}
	var out map[string]string
	for fieldName, s := range scn.Input {
		if s == nil || s.PrefillFromState == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[fieldName] = s.PrefillFromState
	}
	return out
}

// resolveStatePath navigates incarnation.state by the dot-path `state.<seg>[.<seg>…]`
// (the form is validated by the rePrefillFromStatePath schema). Returns (value, found).
// An intermediate non-map segment / missing key → (nil, false) — fail-closed (the field is
// omitted from prefill, parity statepredicate no-such-key). The root token `state` is
// dropped (the path starts inside incarnation.state itself).
func resolveStatePath(state map[string]any, path string) (any, bool) {
	segs := statePathSegments(path)
	if len(segs) == 0 {
		return nil, false
	}
	cur := state
	for i, seg := range segs {
		v, ok := cur[seg]
		if !ok {
			return nil, false
		}
		if i == len(segs)-1 {
			return v, true
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return nil, false
}

// statePathSegments splits `state.<seg>[.<seg>…]` into a list of segments WITHOUT the
// root `state` (the path addresses fields inside incarnation.state itself).
func statePathSegments(path string) []string {
	tail := statePathTail(path)
	if tail == "" {
		return nil
	}
	return splitDot(tail)
}

// statePathTail drops the root `state.` prefix, returning the address inside state
// (`state.redis_users` → `redis_users`). path is validated by the schema as `state.<...>`,
// so the prefix is always present; defensive: without the prefix → empty string
// (resolveStatePath returns not-found, the field is omitted).
func statePathTail(path string) string {
	const root = "state."
	if len(path) <= len(root) || path[:len(root)] != root {
		return ""
	}
	return path[len(root):]
}

// splitDot — cheap split on `.` without allocating a regexp (segments are already
// validated as snake_case by the schema).
func splitDot(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
