package render

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// vaultRefPrefix — the marker for a Vault KV reference string
// (`vault:<mount>/<path>`). Matches the form parsed by vault.ParseRef.
const vaultRefPrefix = "vault:"

// resolveVaultRefs — the first pipeline phase (vault-resolve, [ADR-010]).
// Recursively walks a task's params and replaces every `vault:` ref string with
// the value read from Vault KV. No-op if there are no refs (PM-decision 2): the
// walk is cheap, and no extra Vault round-trip happens when there are no refs.
//
// Returns a new structure (the source isn't mutated): the orchestrator can render
// the same scenario again (retry), with fresh Vault values on every run.
//
// Errors:
//   - a `${ … }` marker inside a vault-ref → validation error: a vault-ref must be
//     a static string, interpolation in it is ambiguous (resolve ${} before or
//     after reading Vault?) and forbidden ([ADR-010], phase boundary).
//   - an unknown path / Vault transport error → propagated as-is
//     (vault.ErrVaultKVNotFound / wrapped).
func resolveVaultRefs(ctx context.Context, vc KVReader, params map[string]any) (map[string]any, error) {
	if len(params) == 0 {
		return params, nil
	}
	out, err := walkVaultValue(ctx, vc, params)
	if err != nil {
		return nil, err
	}
	m, _ := out.(map[string]any)
	return m, nil
}

// walkVaultValue recursively resolves vault-refs in an arbitrary YAML value
// (map / slice / scalar). Returns a new value of the same kind.
func walkVaultValue(ctx context.Context, vc KVReader, v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			rv, err := walkVaultValue(ctx, vc, val)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			rv, err := walkVaultValue(ctx, vc, val)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	case string:
		if !strings.HasPrefix(t, vaultRefPrefix) {
			return t, nil
		}
		return readVaultRef(ctx, vc, t)
	default:
		return v, nil
	}
}

// readVaultRef parses a `vault:` ref string and reads the corresponding secret.
//
// Ref form: `vault:<mount>/<path>` (vault.ParseRef) with an optional `#<field>`
// suffix — selecting one key from the KV secret. Without the suffix, the whole
// secret map is returned (downstream CEL extracts the needed field).
func readVaultRef(ctx context.Context, vc KVReader, ref string) (any, error) {
	if strings.Contains(ref, "${") {
		return nil, fmt.Errorf("render: vault-ref %q contains a ${...} marker -- a vault-ref must be a static string ([ADR-010], phase boundary)", ref)
	}
	if vc == nil {
		return nil, fmt.Errorf("render: vault-ref %q found, but the Vault client is not configured", ref)
	}

	body := ref
	var field string
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		body, field = ref[:i], ref[i+1:]
		if field == "" {
			return nil, fmt.Errorf("render: vault-ref %q: empty field name after '#'", ref)
		}
	}

	logical, err := vault.ParseRef(body)
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	data, err := vc.ReadKV(ctx, logical)
	if err != nil {
		// NIM-73: the path in FLAT form (logical, without the `vault:` prefix) is
		// actionable diagnostics that survives observability masking
		// (audit.vaultRefRe only catches `vault:<mount>/`). A not-found secret has no
		// value → no leak. Symmetric with shared/cel.callVault. `%w` preserves the
		// ErrVaultKVNotFound chain.
		if field != "" {
			return nil, fmt.Errorf("render: secret %s#%s failed to resolve: %w", logical, field, err)
		}
		return nil, fmt.Errorf("render: secret %s failed to resolve: %w", logical, err)
	}

	if field == "" {
		return data, nil
	}
	val, ok := data[field]
	if !ok {
		// Path+field name is actionable (which field to add), not secret value
		// (other fields' values don't go into the text). Flat form (NIM-73).
		return nil, fmt.Errorf("render: secret %s has no field %q", logical, field)
	}
	return val, nil
}
