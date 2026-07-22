package bootstrap

import (
	"context"
	"fmt"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// LoadSigningKey reads the JWT signing-key from Vault KV via a
// `vault:<mount>/<path>` reference. Extracted out of Init so it can be
// reused in `keeper run` (HTTP JWT verifier) without duplicating
// parseVaultRef / extractSigningKey.
//
// signingKeyRef is `auth.jwt.signing_key_ref` from keeper.yml. Returns
// raw signing-key bytes (base64-decoded if the KV value is base64,
// raw otherwise; see extractSigningKey).
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
