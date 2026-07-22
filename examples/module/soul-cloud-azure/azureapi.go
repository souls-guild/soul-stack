package main

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"
)

// vmsAPI / nicsAPI / pipsAPI are narrow subsets of Azure ARM clients used by
// the driver. Narrowing gives L0 unit tests mockability without network access.
//
// Difference from AWS: armcompute/armnetwork methods return generic Pollers
// (`*runtime.Poller[T]`), which are awkward to mock. Therefore the interface
// exposes an already "unrolled" method (Begin* + PollUntilDone in one call). The
// real implementation performs those sequentially; the mock returns a flat
// result.
//
// Get/NewListAllPager are left as-is - the pager is mocked per test through
// runtime.NewPager or by replacing the method entirely.
type vmsAPI interface {
	CreateAndWait(ctx context.Context, rg, name string, params armcompute.VirtualMachine) (armcompute.VirtualMachine, error)
	DeleteAndWait(ctx context.Context, rg, name string) error
	Get(ctx context.Context, rg, name string, opts *armcompute.VirtualMachinesClientGetOptions) (armcompute.VirtualMachinesClientGetResponse, error)
	ListByRunTag(ctx context.Context, rg, runTag string) ([]*armcompute.VirtualMachine, error)
}

type nicsAPI interface {
	CreateAndWait(ctx context.Context, rg, name string, params armnetwork.Interface) (armnetwork.Interface, error)
	DeleteAndWait(ctx context.Context, rg, name string) error
}

type pipsAPI interface {
	CreateAndWait(ctx context.Context, rg, name string, params armnetwork.PublicIPAddress) (armnetwork.PublicIPAddress, error)
	DeleteAndWait(ctx context.Context, rg, name string) error
	Get(ctx context.Context, rg, name string) (armnetwork.PublicIPAddress, error)
}

// azureClients is a bundle of mockable APIs. It is passed into the driver as a
// whole so L0 tests can replace one "layer" at a time.
type azureClients struct {
	vms  vmsAPI
	nics nicsAPI
	pips pipsAPI
}

// azureCredentials are credentials passed by Keeper in
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). The driver
// does NOT call Vault - Keeper has already resolved the secret for it.
//
// subscription_id/resource_group/location travel in credentials alongside the
// service-principal token (provider-specific, see docs/keeper/cloud.md ->
// Credentials-flow): effectively this is "where we enter", and API calls are
// impossible without them.
type azureCredentials struct {
	TenantID       string
	ClientID       string
	ClientSecret   string
	SubscriptionID string
	ResourceGroup  string
	Location       string
}

// credKeys are credentials Struct field names (contract with Keeper-side
// CredentialsResolverPG).
const (
	credTenantID       = "tenant_id"
	credClientID       = "client_id"
	credClientSecret   = "client_secret"
	credSubscriptionID = "subscription_id"
	credResourceGroup  = "resource_group"
	credLocation       = "location"
)

func credsFromMap(m map[string]any) azureCredentials {
	return azureCredentials{
		TenantID:       stringField(m, credTenantID),
		ClientID:       stringField(m, credClientID),
		ClientSecret:   stringField(m, credClientSecret),
		SubscriptionID: stringField(m, credSubscriptionID),
		ResourceGroup:  stringField(m, credResourceGroup),
		Location:       stringField(m, credLocation),
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

// newAzureClients constructs the client trio (VMs/NICs/PIPs) from the supplied
// credentials. Service-principal is explicit - not the default chain (security:
// the driver must not pick up ambient identity from the Keeper host).
//
// newAzureClients is a variable so L0 tests can replace it with a network-free
// fake factory.
var newAzureClients = func(_ context.Context, c azureCredentials) (azureClients, error) {
	cred, err := azidentity.NewClientSecretCredential(c.TenantID, c.ClientID, c.ClientSecret, nil)
	if err != nil {
		return azureClients{}, err
	}
	vmFactory, err := armcompute.NewClientFactory(c.SubscriptionID, cred, nil)
	if err != nil {
		return azureClients{}, err
	}
	netFactory, err := armnetwork.NewClientFactory(c.SubscriptionID, cred, nil)
	if err != nil {
		return azureClients{}, err
	}
	return azureClients{
		vms:  &realVMs{cli: vmFactory.NewVirtualMachinesClient()},
		nics: &realNICs{cli: netFactory.NewInterfacesClient()},
		pips: &realPIPs{cli: netFactory.NewPublicIPAddressesClient()},
	}, nil
}

// --- real implementations: wrap Begin*+PollUntilDone in one call ---

type realVMs struct {
	cli *armcompute.VirtualMachinesClient
}

func (r *realVMs) CreateAndWait(ctx context.Context, rg, name string, params armcompute.VirtualMachine) (armcompute.VirtualMachine, error) {
	poller, err := r.cli.BeginCreateOrUpdate(ctx, rg, name, params, nil)
	if err != nil {
		return armcompute.VirtualMachine{}, err
	}
	resp, err := poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{})
	if err != nil {
		return armcompute.VirtualMachine{}, err
	}
	return resp.VirtualMachine, nil
}

func (r *realVMs) DeleteAndWait(ctx context.Context, rg, name string) error {
	poller, err := r.cli.BeginDelete(ctx, rg, name, nil)
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{})
	return err
}

func (r *realVMs) Get(ctx context.Context, rg, name string, opts *armcompute.VirtualMachinesClientGetOptions) (armcompute.VirtualMachinesClientGetResponse, error) {
	return r.cli.Get(ctx, rg, name, opts)
}

// ListByRunTag lists VMs by runTag within the resource group. The pager is
// unrolled here (Azure pager has no server-side tag filter, so filtering is done
// in Go).
func (r *realVMs) ListByRunTag(ctx context.Context, rg, runTag string) ([]*armcompute.VirtualMachine, error) {
	pager := r.cli.NewListPager(rg, nil)
	var out []*armcompute.VirtualMachine
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, vm := range page.Value {
			if vm == nil || vm.Tags == nil {
				continue
			}
			if v, ok := vm.Tags[runTagKey]; ok && v != nil && *v == runTag {
				out = append(out, vm)
			}
		}
	}
	return out, nil
}

type realNICs struct{ cli *armnetwork.InterfacesClient }

func (r *realNICs) CreateAndWait(ctx context.Context, rg, name string, params armnetwork.Interface) (armnetwork.Interface, error) {
	poller, err := r.cli.BeginCreateOrUpdate(ctx, rg, name, params, nil)
	if err != nil {
		return armnetwork.Interface{}, err
	}
	resp, err := poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{})
	if err != nil {
		return armnetwork.Interface{}, err
	}
	return resp.Interface, nil
}

func (r *realNICs) DeleteAndWait(ctx context.Context, rg, name string) error {
	poller, err := r.cli.BeginDelete(ctx, rg, name, nil)
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{})
	return err
}

type realPIPs struct {
	cli *armnetwork.PublicIPAddressesClient
}

func (r *realPIPs) CreateAndWait(ctx context.Context, rg, name string, params armnetwork.PublicIPAddress) (armnetwork.PublicIPAddress, error) {
	poller, err := r.cli.BeginCreateOrUpdate(ctx, rg, name, params, nil)
	if err != nil {
		return armnetwork.PublicIPAddress{}, err
	}
	resp, err := poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{})
	if err != nil {
		return armnetwork.PublicIPAddress{}, err
	}
	return resp.PublicIPAddress, nil
}

func (r *realPIPs) DeleteAndWait(ctx context.Context, rg, name string) error {
	poller, err := r.cli.BeginDelete(ctx, rg, name, nil)
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{})
	return err
}

func (r *realPIPs) Get(ctx context.Context, rg, name string) (armnetwork.PublicIPAddress, error) {
	resp, err := r.cli.Get(ctx, rg, name, nil)
	if err != nil {
		return armnetwork.PublicIPAddress{}, err
	}
	return resp.PublicIPAddress, nil
}
