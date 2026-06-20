package main

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"
)

// vmsAPI / nicsAPI / pipsAPI — узкие подмножества Azure ARM-клиентов, которые
// использует драйвер. Сужение даёт mockability L0-unit-тестов без сети.
//
// Отличие от AWS: armcompute/armnetwork-методы возвращают generic-Poller
// (`*runtime.Poller[T]`), что плохо мокается. Поэтому интерфейс exposes уже
// «развёрнутый» метод (Begin* + PollUntilDone в одном вызове). Реальная
// реализация делает их последовательно; mock возвращает плоский результат.
//
// Get/NewListAllPager оставлены как-есть — pager mockается per-test через
// runtime.NewPager или подмену метода целиком.
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

// azureClients — пакет mock-able API. Передаётся в драйвер целиком, чтобы
// L0-тесты подменяли по одному «слою».
type azureClients struct {
	vms  vmsAPI
	nics nicsAPI
	pips pipsAPI
}

// azureCredentials — credentials, переданные Keeper-ом в
// CreateRequest.credentials / DestroyRequest.credentials (A-flow). Драйвер в
// Vault НЕ ходит — Keeper уже резолвил секрет за него.
//
// subscription_id/resource_group/location идут в credentials рядом с
// service-principal-токеном (provider-specific, см. docs/keeper/cloud.md →
// Credentials-flow): по сути это «куда заходим», без них API-вызов невозможен.
type azureCredentials struct {
	TenantID       string
	ClientID       string
	ClientSecret   string
	SubscriptionID string
	ResourceGroup  string
	Location       string
}

// credKeys — имена полей credentials-Struct (контракт с Keeper-side
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

// newAzureClients конструирует тройку клиентов (VMs/NICs/PIPs) из переданных
// credentials. Service-principal явный — не дефолтная chain (безопасность:
// драйвер не должен подхватывать ambient-идентичность Keeper-хоста).
//
// newAzureClients вынесен в переменную, чтобы L0-тесты подменяли его fake-
// фабрикой без сети.
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

// --- real-implementations: оборачивают Begin*+PollUntilDone в один вызов ---

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

// ListByRunTag — VM по runTag в пределах resource-group. Pager раскручивается
// здесь (Azure-pager не умеет в server-side filter по тегам, фильтруем in-Go).
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
