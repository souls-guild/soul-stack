package scenario

// Scoped resolution of `vault:` refs in operator input (docs/input.md →
// "vault_scope").
//
// This is a channel SEPARATE from the authoring one (`vault:`/`${vault()}` in
// task params). The authoring channel is trusted (service author), resolved
// in the render phase (keeper/internal/render). The input channel is
// operator-supplied, so resolution is scoped by the field's `vault_scope` +
// a hard deny-list (fork C), and every resolution (ok/denied) is audited
// (security signal).
//
// Per-ref check: has vault_scope? no → reject; path matches scope? no →
// reject; path in deny-list? yes → reject; else ReadKV. Scope/deny checks are
// pure (shared/config); this file adds the KV read + audit.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// InputVaultReader is a narrow subset of keeper/internal/vault.Client: KV
// reads only. Same shared client that resolves authoring refs and reads the
// signing key.
type InputVaultReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// inputVaultAuditCtx is the run's fixed context for the resolution audit
// event: who (aid), where (incarnation/scenario). Field and path are added
// inline.
type inputVaultAuditCtx struct {
	aid         string
	incarnation string
	scenario    string
}

// Errors for input-vault-ref resolution. Messages never carry the resolved
// secret; the path is not secret (it's logged), but the operator-facing
// error only carries the reason.
var (
	errInputVaultNoScope = errors.New("vault:-ref value in input is forbidden: field has no vault_scope (default-deny)")
	errInputVaultOutOf   = errors.New("vault:-ref value in input is outside the allowed vault_scope")
	errInputVaultDenied  = errors.New("vault:-ref value in input points to a forbidden path (deny-list)")
)

// newInputVaultResolver builds a config.InputVaultResolver for one run.
// resolve is called from config.ResolveInputValuesVault on every string
// value with a `vault:` prefix. vc may be nil — then the factory returns nil
// (input-vault-refs unsupported; default-deny already ran earlier in
// ResolveInputValues, which doesn't resolve refs). extraDeny is a
// config-level extension of the system floor.
func (r *Runner) newInputVaultResolver(ctx context.Context, ac inputVaultAuditCtx, extraDeny []string) config.InputVaultResolver {
	return buildInputVaultResolver(ctx, r.deps.Vault, r.deps.Audit, r.logger, ac, extraDeny)
}

// buildInputVaultResolver is the package-level form of
// [Runner.newInputVaultResolver]: builds a config.InputVaultResolver from
// explicit vault-reader / audit-writer / logger. Factored out so the Acolyte
// path ([RenderForHost]) reproduces the same scoped input-vault-ref
// resolution without standing up a Runner. Behavior is identical to the old
// method (which now delegates here). vc == nil → nil (refs aren't resolved).
func buildInputVaultResolver(ctx context.Context, vc InputVaultReader, w audit.Writer, log *slog.Logger, ac inputVaultAuditCtx, extraDeny []string) config.InputVaultResolver {
	if vc == nil {
		return nil
	}
	return func(name string, s *config.InputSchema, raw string) (any, error) {
		// 1. field without vault_scope, but with a vault: value → default-deny.
		if s.VaultScope == "" {
			auditInputVault(ctx, w, ac, name, "", "denied", "no_scope", log)
			return nil, fmt.Errorf("input %q: %w", name, errInputVaultNoScope)
		}

		// Parse ref into a logical path (strip vault: prefix and #field suffix).
		logical, field, perr := parseInputVaultRef(raw)
		if perr != nil {
			auditInputVault(ctx, w, ac, name, "", "denied", "parse_error", log)
			return nil, fmt.Errorf("input %q: %w", name, perr)
		}

		// 2. scope match.
		if !config.MatchesVaultScope(s.VaultScope, logical) {
			auditInputVault(ctx, w, ac, name, logical, "denied", "out_of_scope", log)
			return nil, fmt.Errorf("input %q: %w", name, errInputVaultOutOf)
		}

		// 3. hard deny-list (AFTER scope, unconditionally: a safety net
		//    against an author error in vault_scope).
		if config.DeniedByVaultFloor(logical, extraDeny) {
			auditInputVault(ctx, w, ac, name, logical, "denied", "deny_list", log)
			return nil, fmt.Errorf("input %q: %w", name, errInputVaultDenied)
		}

		// 4. ReadKV.
		data, err := vc.ReadKV(ctx, logical)
		if err != nil {
			auditInputVault(ctx, w, ac, name, logical, "denied", "read_error", log)
			// The Vault error is propagated without a secret value (ReadKV
			// doesn't carry one anyway — only path/sentinel).
			return nil, fmt.Errorf("input %q: reading vault-ref: %w", name, err)
		}

		val, err := selectVaultField(data, field)
		if err != nil {
			auditInputVault(ctx, w, ac, name, logical, "denied", "field_missing", log)
			return nil, fmt.Errorf("input %q: %w", name, err)
		}

		auditInputVault(ctx, w, ac, name, logical, "ok", "", log)
		return val, nil
	}
}

// parseInputVaultRef parses `vault:<mount>/<path>[#<field>]` into a logical
// path and an optional field. Same form as authoring refs
// (render.readVaultRef), but without importing the render package — the
// input channel is self-contained. The `${…}` marker is forbidden (static
// string, like in the authoring channel).
func parseInputVaultRef(ref string) (logical, field string, err error) {
	body := ref
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		body, field = ref[:i], ref[i+1:]
		if field == "" {
			return "", "", errors.New("vault-ref: empty field name after '#'")
		}
	}
	logical, perr := vault.ParseRef(body)
	if perr != nil {
		return "", "", perr
	}
	return logical, field, nil
}

// selectVaultField extracts a field from a KV secret. Without field, returns
// the whole map (downstream validation expects a string for a secret field,
// but the "whole map" case is left to the consumer — in practice secret
// fields use #field).
func selectVaultField(data map[string]any, field string) (any, error) {
	if field == "" {
		return data, nil
	}
	v, ok := data[field]
	if !ok {
		return nil, fmt.Errorf("field %q is missing from the secret", field)
	}
	return v, nil
}

// auditInputVault writes the audit event for an input-vault-ref resolution.
// path is NOT secret (it's logged), the secret value is never included.
// denied is audited too (security signal). The initiator's aid goes in the
// payload (keeper-internal write path, archon_aid column = NULL per the
// ADR-022 source taxonomy). An audit failure doesn't fail the run, only logs
// — resolution can't be blocked by an audit-write failure, but the security
// trail can't be silently dropped either. w == nil → no trail written
// (unit/L0).
func auditInputVault(ctx context.Context, w audit.Writer, ac inputVaultAuditCtx, field, path, result, reason string, log *slog.Logger) {
	if w == nil {
		return
	}
	payload := map[string]any{
		"field":       field,
		"incarnation": ac.incarnation,
		"scenario":    ac.scenario,
		"result":      result,
	}
	if ac.aid != "" {
		payload["aid"] = ac.aid
	}
	if path != "" {
		payload["path"] = path
	}
	if reason != "" {
		payload["reason"] = reason
	}
	ev := &audit.Event{
		EventType: audit.EventInputVaultResolved,
		Source:    audit.SourceKeeperInternal,
		Payload:   payload,
	}
	if err := w.Write(ctx, ev); err != nil && log != nil {
		log.Warn("scenario: writing audit input.vault_resolved failed",
			slog.String("field", field), slog.String("result", result), slog.Any("error", err))
	}
}
