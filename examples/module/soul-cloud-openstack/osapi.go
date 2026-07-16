package main

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

// osAPI is the narrow subset of the nova/servers API used by the driver.
// Narrowing (instead of using *gophercloud.ServiceClient directly) gives L0 unit
// tests mockability without network access: tests provide a fake implementation
// (see driver_test.go). Signatures are intentionally domain-oriented (Create
// accepts already built servers.CreateOpts, ExtractServers is hidden under List)
// so the fake does not repeat all gophercloud pagination machinery.
type osAPI interface {
	CreateServer(ctx context.Context, opts servers.CreateOptsBuilder) (*servers.Server, error)
	GetServer(ctx context.Context, id string) (*servers.Server, error)
	DeleteServer(ctx context.Context, id string) error
	ListServers(ctx context.Context, opts servers.ListOptsBuilder) ([]servers.Server, error)
}

// osCredentials are provider credentials passed by Keeper in
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). The driver
// does NOT call Vault - Keeper has already resolved the secret.
//
// OpenStack authentication is Keystone v3 (identity service), password +
// domain + project form. Minimal field set for Keystone v3 password-scoped:
//   - auth_url                 - identity endpoint (.../v3);
//   - username + password      - user account;
//   - user_domain_name|_id     - user account domain;
//   - project_name|_id         - target project (tenant);
//   - project_domain_name|_id  - project domain (often matches user domain).
//
// region is provider-specific metadata and lives next to auth fields for
// symmetry with awsCredentials.Region. Empty region is valid (private clouds
// without regions). endpoint is an identity override for tests; in prod the
// Keystone catalog resolves compute-endpoint itself, so override is not needed.
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
	Endpoint          string // override identity-endpoint (tests); empty = AuthURL
}

// credKeys are credentials Struct field names (contract with Keeper-side
// CredentialsResolverPG: values from Vault KV + region from Provider registry).
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

// credsFromMap extracts [osCredentials] from a decoded credentials Struct.
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

// buildAuthOptions constructs gophercloud.AuthOptions from osCredentials with
// minimal validation (auth_url/username/password are required; domain and
// project need at least one value in each name|id pair). AllowReauth=true lets
// gophercloud transparently reissue an expired Keystone token inside one driver
// call (around the Create+Wait boundary).
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

// newOsClient constructs osAPI from the supplied credentials. Credentials come
// explicitly from Keeper - the driver must NOT pick up ambient environment
// (OS_* env vars).
//
// newOsClient is a variable so L0 tests can replace it with a fake factory
// without a real Keystone call (see driver_test.go). Same as
// newEC2Client/newYcClient in neighboring drivers.
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

// osRealClient is the real osAPI implementation over *gophercloud.ServiceClient
// nova-compute. It extracts (.Extract) from gophercloud result wrappers so driver
// code does not see gophercloud.Result. Pager for List is unrolled here too, so
// the fake returns flat []servers.Server, symmetrical with YC API.
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
