package cloud

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/provider"
	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// ResolvedProvider — результат резолва Provider-реестра в данные, нужные для
// вызова CloudDriver-плагина (credentials A-flow, docs/keeper/cloud.md):
//
//   - Driver — имя CloudDriver-плагина (Provider.Type), под которым плагин
//     зарегистрирован в discovery-кеше (PluginHost lookup по нему).
//   - Credentials — plain-секрет из Vault (по Provider.CredentialsRef) +
//     `region` из Provider-реестра, склеенные в один map. Уходит в
//     CreateRequest.credentials / DestroyRequest.credentials как Struct.
//     Драйвер в Vault НЕ ходит — Keeper резолвит секрет за него (Вариант A).
//
// region кладётся внутрь Credentials, а не отдельным полем: он provider-specific
// (у Proxmox/OpenStack своего `region` нет), driver сам решает, как его читать.
type ResolvedProvider struct {
	Driver      string
	Credentials map[string]any
}

// regionKey — ключ, под которым `region` из Provider-реестра кладётся в
// credentials-map (рядом с plain-секретом из Vault). Driver читает его как
// обычное credentials-поле.
const regionKey = "region"

// ProviderResolver резолвит имя Provider-а (param `provider` шага
// `core.cloud.provisioned`) в [ResolvedProvider]: driver-имя + plain-credentials.
// Внедряется в [Module]; прод-реализация — [CredentialsResolverPG]. Для unit-
// тестов модуля — fake (см. provisioned_test.go).
type ProviderResolver interface {
	Resolve(ctx context.Context, providerName string) (*ResolvedProvider, error)
}

// ProviderReader — узкое подмножество provider-CRUD (SelectByName), нужное
// резолверу. Сужение упрощает unit-тест без поднятия PG.
type ProviderReader interface {
	SelectByName(ctx context.Context, name string) (*provider.Provider, error)
}

// VaultReader — узкое подмножество keeper/internal/vault.Client (ReadKV),
// симметрично coremod/vault.VaultReader. Дублируется, чтобы резолвер не тащил
// весь vault-pipeline транзитивно.
type VaultReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// CredentialsResolverPG — прод-реализация [ProviderResolver]: читает Provider
// из Postgres, резолвит `credentials_ref` (vault:<mount>/<path>) через Vault KV,
// возвращает driver-имя (Provider.Type) + plain-credentials с добавленным
// `region`.
type CredentialsResolverPG struct {
	Providers ProviderReader
	Vault     VaultReader
}

// NewCredentialsResolverPG — wire-helper.
func NewCredentialsResolverPG(p ProviderReader, v VaultReader) *CredentialsResolverPG {
	return &CredentialsResolverPG{Providers: p, Vault: v}
}

// providerReaderFunc адаптирует пакетную функцию provider.SelectByName
// (свободную, не метод) к [ProviderReader]. db фиксируется при wire-up.
type providerReaderFunc struct {
	db provider.ExecQueryRower
}

// NewProviderReaderPG оборачивает pgxpool.Pool (или Conn/Tx) в [ProviderReader]
// поверх свободной функции provider.SelectByName.
func NewProviderReaderPG(db provider.ExecQueryRower) ProviderReader {
	return providerReaderFunc{db: db}
}

func (r providerReaderFunc) SelectByName(ctx context.Context, name string) (*provider.Provider, error) {
	return provider.SelectByName(ctx, r.db, name)
}

// Resolve читает Provider по имени, резолвит credentials_ref через Vault и
// собирает credentials-map. region добавляется под ключом [regionKey].
//
// Безопасность: возвращаемый Credentials содержит plain-секрет — caller обязан
// прогонять его через audit.MaskSecrets на ЛЮБОМ выходе (см. provisioned.go).
func (r *CredentialsResolverPG) Resolve(ctx context.Context, providerName string) (*ResolvedProvider, error) {
	p, err := r.Providers.SelectByName(ctx, providerName)
	if err != nil {
		return nil, fmt.Errorf("resolve provider %q: %w", providerName, err)
	}

	logical, err := vault.ParseRef(p.CredentialsRef)
	if err != nil {
		return nil, fmt.Errorf("provider %q credentials_ref: %w", providerName, err)
	}

	secret, err := r.Vault.ReadKV(ctx, logical)
	if err != nil {
		return nil, fmt.Errorf("provider %q vault read: %w", providerName, err)
	}

	creds := make(map[string]any, len(secret)+1)
	for k, v := range secret {
		creds[k] = v
	}
	// region из Provider-реестра имеет приоритет над одноимённым полем секрета:
	// реестровое значение — авторитетный источник, секрет хранит только auth-данные.
	creds[regionKey] = p.Region

	return &ResolvedProvider{Driver: p.Type, Credentials: creds}, nil
}
