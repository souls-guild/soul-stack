package trial

import (
	"context"
	"fmt"
	"strings"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// fixtureVault — sealed KVReader over fixtures.vault. Implements
// render.KVReader, replacing real *vault.Client with static
// secret map. Keys are normalized to relative form (without mount prefix
// "secret/"), to match both logical and relative access.
type fixtureVault struct {
	secrets map[string]map[string]any
}

// newFixtureVault builds reader from fixtures.vault. nil/empty map →
// reader returning ErrVaultKVNotFound on any ref (like Vault without secret).
func newFixtureVault(secrets map[string]map[string]any) *fixtureVault {
	norm := make(map[string]map[string]any, len(secrets))
	for k, v := range secrets {
		norm[normalizeVaultKey(k)] = v
	}
	return &fixtureVault{secrets: norm}
}

// ReadKV returns mock secret by path. path accepted in logical
// ("secret/keeper/...") and relative ("keeper/...") forms — both are normalized
// to relative key in map, symmetric to keepervault.Client.ReadKV.
func (f *fixtureVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	data, ok := f.secrets[normalizeVaultKey(path)]
	if !ok {
		return nil, fmt.Errorf("%w: %s (not in fixtures.vault)", keepervault.ErrVaultKVNotFound, path)
	}
	return data, nil
}

// normalizeVaultKey removes leading slash and default KV-mount prefix
// "secret/" (only it — custom mount paths not supported in L0 fixture),
// bringing logical and relative forms to one view.
func normalizeVaultKey(k string) string {
	k = strings.TrimPrefix(k, "/")
	return strings.TrimPrefix(k, "secret/")
}
