package main

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

// osAPI — узкое подмножество nova/servers API, которое использует драйвер.
// Сужение (а не *gophercloud.ServiceClient напрямую) даёт mockability для
// L0-unit-тестов без сети: тест подсовывает fake-реализацию (см.
// driver_test.go). Сигнатуры намеренно идут «по-доменному» (Create принимает
// уже собранные servers.CreateOpts, ExtractServers скрыт под List), чтобы
// fake не повторял всю gophercloud-машинерию пагинации.
type osAPI interface {
	CreateServer(ctx context.Context, opts servers.CreateOptsBuilder) (*servers.Server, error)
	GetServer(ctx context.Context, id string) (*servers.Server, error)
	DeleteServer(ctx context.Context, id string) error
	ListServers(ctx context.Context, opts servers.ListOptsBuilder) ([]servers.Server, error)
}

// osCredentials — credentials провайдера, переданные Keeper-ом в
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). Драйвер в
// Vault НЕ ходит — Keeper уже резолвил секрет.
//
// OpenStack-аутентификация — Keystone v3 (identity-сервис), форма пароль+
// domain+project. Минимальный набор полей по Keystone v3 password-scoped:
//   - auth_url                 — endpoint identity (.../v3);
//   - username + password      — учётка пользователя;
//   - user_domain_name|_id     — домен учётки;
//   - project_name|_id         — целевой project (tenant);
//   - project_domain_name|_id  — домен проекта (часто совпадает с user-доменом).
//
// region — provider-specific метаданные, лежит рядом с auth-полями для симметрии
// с awsCredentials.Region. Пустой region допустим (приватные облака без regions).
// endpoint — override identity для тестов; в проде Keystone-каталог сам резолвит
// compute-endpoint, override не нужен.
type osCredentials struct {
	AuthURL           string
	Username          string
	Password          string
	UserDomainName    string
	UserDomainID      string
	ProjectName       string
	ProjectID         string
	ProjectDomainName string
	ProjectDomainID   string
	Region            string
	Endpoint          string // override identity-endpoint (тесты); пусто = AuthURL
}

// credKeys — имена полей credentials-Struct (контракт с Keeper-side
// CredentialsResolverPG: значения из Vault KV + region из Provider-реестра).
const (
	credAuthURL           = "auth_url"
	credUsername          = "username"
	credPassword          = "password"
	credUserDomainName    = "user_domain_name"
	credUserDomainID      = "user_domain_id"
	credProjectName       = "project_name"
	credProjectID         = "project_id"
	credProjectDomainName = "project_domain_name"
	credProjectDomainID   = "project_domain_id"
	credRegion            = "region"
	credEndpoint          = "endpoint"
)

// credsFromMap извлекает [osCredentials] из decoded credentials-Struct.
func credsFromMap(m map[string]any) osCredentials {
	return osCredentials{
		AuthURL:           stringField(m, credAuthURL),
		Username:          stringField(m, credUsername),
		Password:          stringField(m, credPassword),
		UserDomainName:    stringField(m, credUserDomainName),
		UserDomainID:      stringField(m, credUserDomainID),
		ProjectName:       stringField(m, credProjectName),
		ProjectID:         stringField(m, credProjectID),
		ProjectDomainName: stringField(m, credProjectDomainName),
		ProjectDomainID:   stringField(m, credProjectDomainID),
		Region:            stringField(m, credRegion),
		Endpoint:          stringField(m, credEndpoint),
	}
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// buildAuthOptions конструирует gophercloud.AuthOptions из osCredentials с
// минимальной валидацией (auth_url/username/password — обязательны; domain и
// project — хотя бы по одному в каждой паре name|id). AllowReauth=true
// позволяет gophercloud прозрачно перевыпускать просроченный Keystone-токен
// внутри одного драйвер-вызова (на стыке Create+Wait).
func buildAuthOptions(c osCredentials) (gophercloud.AuthOptions, error) {
	if c.AuthURL == "" {
		return gophercloud.AuthOptions{}, fmt.Errorf("keystone auth_url is required")
	}
	if c.Username == "" || c.Password == "" {
		return gophercloud.AuthOptions{}, fmt.Errorf("keystone username/password required")
	}
	if c.UserDomainName == "" && c.UserDomainID == "" {
		return gophercloud.AuthOptions{}, fmt.Errorf("keystone user_domain_name or user_domain_id required")
	}
	if c.ProjectName == "" && c.ProjectID == "" {
		return gophercloud.AuthOptions{}, fmt.Errorf("keystone project_name or project_id required")
	}
	if c.ProjectDomainName == "" && c.ProjectDomainID == "" {
		return gophercloud.AuthOptions{}, fmt.Errorf("keystone project_domain_name or project_domain_id required")
	}
	authURL := c.AuthURL
	if c.Endpoint != "" {
		authURL = c.Endpoint
	}
	return gophercloud.AuthOptions{
		IdentityEndpoint: authURL,
		Username:         c.Username,
		Password:         c.Password,
		DomainName:       c.UserDomainName,
		DomainID:         c.UserDomainID,
		TenantName:       c.ProjectName,
		TenantID:         c.ProjectID,
		Scope: &gophercloud.AuthScope{
			ProjectName: c.ProjectName,
			ProjectID:   c.ProjectID,
			DomainName:  c.ProjectDomainName,
			DomainID:    c.ProjectDomainID,
		},
		AllowReauth: true,
	}, nil
}

// newOsClient конструирует osAPI из переданных credentials. credentials
// приходят от Keeper-а явно — драйвер НЕ должен подхватывать ambient-окружение
// (OS_* env-переменные).
//
// newOsClient вынесен в переменную, чтобы L0-тесты подменяли его fake-фабрикой
// без реального обращения к Keystone (см. driver_test.go). Аналогично
// newEC2Client/newYcClient в соседних драйверах.
var newOsClient = func(ctx context.Context, c osCredentials) (osAPI, error) {
	opts, err := buildAuthOptions(c)
	if err != nil {
		return nil, err
	}
	provider, err := openstack.AuthenticatedClient(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("keystone authenticate: %w", err)
	}
	compute, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{Region: c.Region})
	if err != nil {
		return nil, fmt.Errorf("nova endpoint: %w", err)
	}
	return &osRealClient{compute: compute}, nil
}

// osRealClient — реальная реализация osAPI поверх *gophercloud.ServiceClient
// nova-compute. Извлекает (.Extract) из result-обёрток gophercloud, чтобы
// драйверный код не видел gophercloud.Result. Pager для List разворачивается
// здесь же — fake тогда возвращает плоский []servers.Server, симметрично YC-API.
type osRealClient struct {
	compute *gophercloud.ServiceClient
}

func (c *osRealClient) CreateServer(ctx context.Context, opts servers.CreateOptsBuilder) (*servers.Server, error) {
	return servers.Create(ctx, c.compute, opts, nil).Extract()
}

func (c *osRealClient) GetServer(ctx context.Context, id string) (*servers.Server, error) {
	return servers.Get(ctx, c.compute, id).Extract()
}

func (c *osRealClient) DeleteServer(ctx context.Context, id string) error {
	return servers.Delete(ctx, c.compute, id).ExtractErr()
}

func (c *osRealClient) ListServers(ctx context.Context, opts servers.ListOptsBuilder) ([]servers.Server, error) {
	var all []servers.Server
	err := servers.List(c.compute, opts).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		pageServers, perr := servers.ExtractServers(page)
		if perr != nil {
			return false, perr
		}
		all = append(all, pageServers...)
		return true, nil
	})
	return all, err
}
