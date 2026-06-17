package trial

import (
	"context"
	"fmt"
	"strings"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// fixtureVault — герметичный KVReader поверх fixtures.vault. Реализует
// render.KVReader, заменяя реальный *vault.Client на статическую карту
// секретов. Ключи нормализуются к relative-форме (без mount-префикса
// "secret/"), чтобы совпадать и с logical-, и с relative-обращением.
type fixtureVault struct {
	secrets map[string]map[string]any
}

// newFixtureVault строит reader из fixtures.vault. nil/пустая карта →
// reader, отвечающий ErrVaultKVNotFound на любой ref (как Vault без секрета).
func newFixtureVault(secrets map[string]map[string]any) *fixtureVault {
	norm := make(map[string]map[string]any, len(secrets))
	for k, v := range secrets {
		norm[normalizeVaultKey(k)] = v
	}
	return &fixtureVault{secrets: norm}
}

// ReadKV возвращает мок-секрет по path. path принимается в logical
// ("secret/keeper/...") и relative ("keeper/...") формах — обе нормализуются
// к relative-ключу карты, симметрично keepervault.Client.ReadKV.
func (f *fixtureVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	data, ok := f.secrets[normalizeVaultKey(path)]
	if !ok {
		return nil, fmt.Errorf("%w: %s (нет в fixtures.vault)", keepervault.ErrVaultKVNotFound, path)
	}
	return data, nil
}

// normalizeVaultKey снимает leading slash и префикс дефолтного KV-mount
// "secret/" (только его — кастомные mount-пути в L0-фикстуре не поддержаны),
// приводя logical и relative формы к одному виду.
func normalizeVaultKey(k string) string {
	k = strings.TrimPrefix(k, "/")
	return strings.TrimPrefix(k, "secret/")
}
