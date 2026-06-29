package cloud

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/profile"
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

// ProviderResolver резолвит реестровые ссылки шага `core.cloud.provisioned` в
// данные для CloudDriver-вызова:
//
//   - Resolve(provider-имя) → [ResolvedProvider]: driver-имя + plain-credentials
//     (param `provider`, симметрия с credentials A-flow).
//   - ResolveProfile(profile-имя) → VM-spec params из реестра `profiles`
//     (param `profile`, Вариант A: `profile` = ИМЯ строки реестра /v1/profiles,
//     не inline-object). Profile обязан быть пред-зарегистрирован.
//
// Оба метода держит один [CredentialsResolverPG] (Provider+Vault+Profile —
// один резолв-слой реестровых ссылок), внедряется в [Module] одним полем. Для
// unit-тестов модуля — fake (см. provisioned_test.go).
type ProviderResolver interface {
	Resolve(ctx context.Context, providerName string) (*ResolvedProvider, error)
	// ResolveProfile резолвит имя Profile-я в его VM-spec params. Имя не найдено
	// в реестре → ошибка (caller отдаёт SendFailed; маскировать не требуется —
	// Profile.Params секретов не несёт, но caller всё равно прогоняет через
	// maskErr единообразно).
	ResolveProfile(ctx context.Context, profileName string) (map[string]any, error)
}

// ProviderReader — узкое подмножество provider-CRUD (SelectByName), нужное
// резолверу. Сужение упрощает unit-тест без поднятия PG.
type ProviderReader interface {
	SelectByName(ctx context.Context, name string) (*provider.Provider, error)
}

// ProfileReader — узкое подмножество profile-CRUD (SelectByName), симметрично
// [ProviderReader]. Сужение упрощает unit-тест без поднятия PG.
type ProfileReader interface {
	SelectByName(ctx context.Context, name string) (*profile.Profile, error)
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
// `region`. Profile-реестр резолвится через [ResolveProfile] (тот же слой
// реестровых ссылок).
type CredentialsResolverPG struct {
	Providers ProviderReader
	Profiles  ProfileReader
	Vault     VaultReader
}

// NewCredentialsResolverPG — wire-helper. profiles обязателен: param `profile`
// шага `core.cloud.created` резолвится через ResolveProfile (Вариант A).
func NewCredentialsResolverPG(p ProviderReader, profiles ProfileReader, v VaultReader) *CredentialsResolverPG {
	return &CredentialsResolverPG{Providers: p, Profiles: profiles, Vault: v}
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

// profileReaderFunc адаптирует пакетную функцию profile.SelectByName к
// [ProfileReader], симметрично [providerReaderFunc].
type profileReaderFunc struct {
	db profile.ExecQueryRower
}

// NewProfileReaderPG оборачивает pgxpool.Pool (или Conn/Tx) в [ProfileReader]
// поверх свободной функции profile.SelectByName.
func NewProfileReaderPG(db profile.ExecQueryRower) ProfileReader {
	return profileReaderFunc{db: db}
}

func (r profileReaderFunc) SelectByName(ctx context.Context, name string) (*profile.Profile, error) {
	return profile.SelectByName(ctx, r.db, name)
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

// ResolveProfile читает Profile по имени и возвращает его VM-spec params
// (Вариант A: param `profile` шага `core.cloud.created` = ИМЯ строки реестра
// /v1/profiles). Имя не найдено → [profile.ErrProfileNotFound] (caller отдаёт
// SendFailed). Params могут быть nil (профиль без VM-spec — валидно).
func (r *CredentialsResolverPG) ResolveProfile(ctx context.Context, profileName string) (map[string]any, error) {
	p, err := r.Profiles.SelectByName(ctx, profileName)
	if err != nil {
		return nil, fmt.Errorf("resolve profile %q: %w", profileName, err)
	}
	return p.Params, nil
}
