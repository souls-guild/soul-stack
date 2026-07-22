// Package secretwrite materializes plaintext operator secrets to Vault at
// deterministic paths (ADR-064, NIM-11). Dual-mode secret intake: operator
// provides value (plaintext) instead of vault-ref, keeper writes it to Vault itself
// and stores only the internal ref in Postgres. Single write layer for Herald and
// Provider on top of the same vault.Client.WriteKV used by sigil/cert — a
// generalization of keeper-side write-path, not new infra code.
package secretwrite

import (
	"context"
	"fmt"
	"regexp"
)

// Secret domains — first segment of deterministic path secret/<domain>/…
const (
	DomainHerald   = "herald"
	DomainProvider = "provider"
)

// defaultMount is the default KV-mount (matches vault.defaultKVMount).
const defaultMount = "secret"

// segmentRe matches a safe path segment (domain/entity/field): letters/digits/`_`/`-`.
// Rejects `.`/`..`/slashes/empty — prevents scope bypass in Vault paths (ParseRef
// also rejects `..`, here fail-closed at write-path entry).
var segmentRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// VaultWriter is a narrow interface for writing to Vault KV (implemented by
// vault.Client.WriteKV). Narrowing to an interface enables fakes in unit/guard tests
// without a real Vault (symmetric to sigil.VaultWriter).
type VaultWriter interface {
	WriteKV(ctx context.Context, path string, data map[string]any) error
}

// Writer writes an operator's plaintext secret to Vault at path
// <mount>/<domain>/<entity>/<field> and returns the internal vault-ref for PG.
// mount comes from keeper.yml (vault.kv_mount, default "secret"); same client as
// sigil/cert. SECURITY: secret value must never leak into error/log text.
type Writer struct {
	vault VaultWriter
	mount string
}

// NewWriter constructs a writer. v is required (nil → error). mount=="" → "secret".
func NewWriter(v VaultWriter, mount string) (*Writer, error) {
	if v == nil {
		return nil, fmt.Errorf("secretwrite: nil VaultWriter")
	}
	if mount == "" {
		mount = defaultMount
	}
	return &Writer{vault: v, mount: mount}, nil
}

// WriteString writes a single string field secret {field: value} at a deterministic
// path and returns ref vault:<mount>/<domain>/<entity>/<field>#<field>. The explicit
// #field makes resolution (resolveVaultString) independent of the number of secret
// fields. value must never leak into errors.
func (w *Writer) WriteString(ctx context.Context, domain, entity, field, value string) (string, error) {
	path, err := w.path(domain, entity, field)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("secretwrite: empty %s/%s value", domain, field)
	}
	if err := w.vault.WriteKV(ctx, path, map[string]any{field: value}); err != nil {
		return "", fmt.Errorf("secretwrite: write %s/%s/%s: %w", domain, entity, field, err)
	}
	return "vault:" + path + "#" + field, nil
}

// WriteMap writes a multi-field secret (e.g. cloud credentials) at a deterministic
// path and returns ref vault:<mount>/<domain>/<entity>/<field> (no #field — the
// consumer reads the entire map, as in cloud.credentials.Resolve). Secret values
// must never leak into errors.
func (w *Writer) WriteMap(ctx context.Context, domain, entity, field string, data map[string]any) (string, error) {
	path, err := w.path(domain, entity, field)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("secretwrite: empty %s/%s data", domain, field)
	}
	if err := w.vault.WriteKV(ctx, path, data); err != nil {
		return "", fmt.Errorf("secretwrite: write %s/%s/%s: %w", domain, entity, field, err)
	}
	return "vault:" + path, nil
}

// path builds and validates the deterministic logical path
// <mount>/<domain>/<entity>/<field>. domain/entity/field must be safe segments
// (matching segmentRe).
func (w *Writer) path(domain, entity, field string) (string, error) {
	if !segmentRe.MatchString(domain) {
		return "", fmt.Errorf("secretwrite: invalid domain %q", domain)
	}
	if !segmentRe.MatchString(entity) {
		return "", fmt.Errorf("secretwrite: invalid entity %q", entity)
	}
	if !segmentRe.MatchString(field) {
		return "", fmt.Errorf("secretwrite: invalid field %q", field)
	}
	return w.mount + "/" + domain + "/" + entity + "/" + field, nil
}
