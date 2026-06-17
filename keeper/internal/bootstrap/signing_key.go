package bootstrap

import (
	"context"
	"fmt"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// LoadSigningKey читает JWT signing-key из Vault KV по `vault:<mount>/<path>`-
// ссылке. Вынесено из Init, чтобы переиспользовать в `keeper run` (HTTP
// JWT verifier) без дублирования parseVaultRef / extractSigningKey.
//
// signingKeyRef — `auth.jwt.signing_key_ref` из keeper.yml. Возвращает
// raw signing-key bytes (декодированный base64 если KV содержит base64,
// raw — иначе; см. extractSigningKey).
func LoadSigningKey(ctx context.Context, vc *keepervault.Client, signingKeyRef string) ([]byte, error) {
	if vc == nil {
		return nil, fmt.Errorf("bootstrap: vault client is nil")
	}
	if signingKeyRef == "" {
		return nil, fmt.Errorf("bootstrap: signing_key_ref is empty")
	}
	path, err := keepervault.ParseRef(signingKeyRef)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: signing_key_ref: %w", err)
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: read vault %q: %w", path, err)
	}
	return extractSigningKey(kv)
}
