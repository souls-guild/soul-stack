package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// withFastBackoff подменяет defaultBackoff на «нулевые» задержки + указанный
// MaxAttempts. Симметрично soul-cloud-aws.
func withFastBackoff(t *testing.T, maxAttempts int) {
	t.Helper()
	orig := defaultBackoff
	defaultBackoff = func() clouddriver.BackoffConfig {
		return clouddriver.BackoffConfig{
			Initial:     1 * time.Millisecond,
			Max:         1 * time.Millisecond,
			Factor:      1.0,
			MaxAttempts: maxAttempts,
		}
	}
	t.Cleanup(func() { defaultBackoff = orig })
}

// withDeterministicSuffix — стабильный хвост для makeVMName без runTag,
// иначе тесты получают рандомный suffix.
func withDeterministicSuffix(t *testing.T, suffix string) {
	t.Helper()
	orig := randomSuffix
	randomSuffix = func() string { return suffix }
	t.Cleanup(func() { randomSuffix = orig })
}

// --- fake-клиенты Azure ---

// fakeVMs / fakeNICs / fakePIPs — mock-реализации vmsAPI/nicsAPI/pipsAPI.
// Поведение настраивается per-метод; *Seq позволяет сменить результат между
// раундами поллера (как describeSeq в AWS-тестах).
type fakeVMs struct {
	createErr     error
	createCalled  int
	lastCreateVM  armcompute.VirtualMachine
	lastCreateRG  string
	lastCreateName string

	getSeq      []armcompute.VirtualMachinesClientGetResponse
	getIdx      int
	getFn       func(call int) (armcompute.VirtualMachinesClientGetResponse, error)
	getN        int

	deleteErr    error
	deleteCalls  []string // имена удалённых VM

	listResult []*armcompute.VirtualMachine
	listErr    error
}

func (f *fakeVMs) CreateAndWait(_ context.Context, rg, name string, params armcompute.VirtualMachine) (armcompute.VirtualMachine, error) {
	f.createCalled++
	f.lastCreateVM = params
	f.lastCreateRG = rg
	f.lastCreateName = name
	if f.createErr != nil {
		return armcompute.VirtualMachine{}, f.createErr
	}
	params.Name = to.Ptr(name)
	params.ID = to.Ptr("/subscriptions/sub/resourceGroups/" + rg + "/providers/Microsoft.Compute/virtualMachines/" + name)
	return params, nil
}

func (f *fakeVMs) DeleteAndWait(_ context.Context, _, name string) error {
	f.deleteCalls = append(f.deleteCalls, name)
	return f.deleteErr
}

func (f *fakeVMs) Get(_ context.Context, _, _ string, _ *armcompute.VirtualMachinesClientGetOptions) (armcompute.VirtualMachinesClientGetResponse, error) {
	call := f.getN
	f.getN++
	if f.getFn != nil {
		return f.getFn(call)
	}
	if len(f.getSeq) == 0 {
		return armcompute.VirtualMachinesClientGetResponse{}, nil
	}
	out := f.getSeq[f.getIdx]
	if f.getIdx < len(f.getSeq)-1 {
		f.getIdx++
	}
	return out, nil
}

func (f *fakeVMs) ListByRunTag(_ context.Context, _, _ string) ([]*armcompute.VirtualMachine, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResult, nil
}

type fakeNICs struct {
	createErr    error
	createCalls  []string
	deleteCalls  []string
	deleteErr    error
}

func (f *fakeNICs) CreateAndWait(_ context.Context, _, name string, _ armnetwork.Interface) (armnetwork.Interface, error) {
	f.createCalls = append(f.createCalls, name)
	if f.createErr != nil {
		return armnetwork.Interface{}, f.createErr
	}
	return armnetwork.Interface{
		ID:   to.Ptr("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/" + name),
		Name: to.Ptr(name),
	}, nil
}

func (f *fakeNICs) DeleteAndWait(_ context.Context, _, name string) error {
	f.deleteCalls = append(f.deleteCalls, name)
	return f.deleteErr
}

type fakePIPs struct {
	createErr    error
	createCalls  []string
	deleteCalls  []string
	deleteErr    error
	getResult    armnetwork.PublicIPAddress
	getErr       error
}

func (f *fakePIPs) CreateAndWait(_ context.Context, _, name string, _ armnetwork.PublicIPAddress) (armnetwork.PublicIPAddress, error) {
	f.createCalls = append(f.createCalls, name)
	if f.createErr != nil {
		return armnetwork.PublicIPAddress{}, f.createErr
	}
	return armnetwork.PublicIPAddress{
		ID:   to.Ptr("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/" + name),
		Name: to.Ptr(name),
	}, nil
}

func (f *fakePIPs) DeleteAndWait(_ context.Context, _, name string) error {
	f.deleteCalls = append(f.deleteCalls, name)
	return f.deleteErr
}

func (f *fakePIPs) Get(_ context.Context, _, _ string) (armnetwork.PublicIPAddress, error) {
	if f.getErr != nil {
		return armnetwork.PublicIPAddress{}, f.getErr
	}
	return f.getResult, nil
}

// withFakeClients подменяет фабрику клиентов на возврат заданной тройки.
func withFakeClients(t *testing.T, vms vmsAPI, nics nicsAPI, pips pipsAPI) {
	t.Helper()
	orig := newAzureClients
	newAzureClients = func(_ context.Context, _ azureCredentials) (azureClients, error) {
		return azureClients{vms: vms, nics: nics, pips: pips}, nil
	}
	t.Cleanup(func() { newAzureClients = orig })
}

// --- stream-mocks (симметрия AWS-теста) ---

type createStream struct {
	grpc.ServerStreamingServer[pluginv1.CreateEvent]
	ctx  context.Context
	sent []*pluginv1.CreateEvent
}

func (s *createStream) Send(e *pluginv1.CreateEvent) error { s.sent = append(s.sent, e); return nil }
func (s *createStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
func (s *createStream) last() *pluginv1.CreateEvent {
	if len(s.sent) == 0 {
		return nil
	}
	return s.sent[len(s.sent)-1]
}

type destroyStream struct {
	grpc.ServerStreamingServer[pluginv1.DestroyEvent]
	sent []*pluginv1.DestroyEvent
}

func (s *destroyStream) Send(e *pluginv1.DestroyEvent) error { s.sent = append(s.sent, e); return nil }
func (s *destroyStream) Context() context.Context            { return context.Background() }

type listStream struct {
	grpc.ServerStreamingServer[pluginv1.VmInfo]
	ctx  context.Context
	sent []*pluginv1.VmInfo
}

func (s *listStream) Send(v *pluginv1.VmInfo) error { s.sent = append(s.sent, v); return nil }
func (s *listStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// --- helpers ---

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	return s
}

func profileMap() map[string]any {
	return map[string]any{
		"location":  "westeurope",
		"vm_size":   "Standard_B2s",
		"subnet_id": "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/sn",
		"image": map[string]any{
			"publisher": "Canonical",
			"offer":     "0001-com-ubuntu-server-jammy",
			"sku":       "22_04-lts",
		},
	}
}

func credsMap() map[string]any {
	return map[string]any{
		"tenant_id":       "ten",
		"client_id":       "cli",
		"client_secret":   "sec",
		"subscription_id": "sub",
		"resource_group":  "rg",
		"location":        "westeurope",
	}
}

func runningVMResponse(name string) armcompute.VirtualMachinesClientGetResponse {
	return armcompute.VirtualMachinesClientGetResponse{
		VirtualMachine: armcompute.VirtualMachine{
			Name:     to.Ptr(name),
			Location: to.Ptr("westeurope"),
			Properties: &armcompute.VirtualMachineProperties{
				ProvisioningState: to.Ptr("Succeeded"),
				HardwareProfile:   &armcompute.HardwareProfile{VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes("Standard_B2s"))},
				InstanceView: &armcompute.VirtualMachineInstanceView{
					Statuses: []*armcompute.InstanceViewStatus{{Code: to.Ptr("PowerState/running")}},
				},
			},
			Tags: map[string]*string{runTagKey: to.Ptr("run-1")},
		},
	}
}

func pendingVMResponse(name string) armcompute.VirtualMachinesClientGetResponse {
	return armcompute.VirtualMachinesClientGetResponse{
		VirtualMachine: armcompute.VirtualMachine{
			Name: to.Ptr(name),
			Properties: &armcompute.VirtualMachineProperties{
				ProvisioningState: to.Ptr("Creating"),
			},
		},
	}
}

// --- Schema / Validate ---

func TestSchema_ParsesEmbedded(t *testing.T) {
	d := &AzureDriver{}
	rep, err := d.Schema(context.Background(), &pluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	m := rep.ProfileSchema.AsMap()
	req, _ := m["required"].([]any)
	if len(req) != 4 {
		t.Errorf("schema required=%v, want 4 fields", req)
	}
}

func TestValidate_MissingFields(t *testing.T) {
	d := &AzureDriver{}
	rep, err := d.Validate(context.Background(), &pluginv1.ValidateProfileRequest{
		Profile: mustStruct(t, map[string]any{"location": "westeurope"}),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.Ok {
		t.Error("expected Ok=false")
	}
	// vm_size + subnet_id + image
	if len(rep.Errors) != 3 {
		t.Errorf("errors=%v, want 3", rep.Errors)
	}
}

// --- Create happy 3-resource path ---

func TestCreate_HappyPath_ThreeResources(t *testing.T) {
	withDeterministicSuffix(t, "abc")
	vms := &fakeVMs{getSeq: []armcompute.VirtualMachinesClientGetResponse{runningVMResponse("soul-vm-abc")}}
	nics := &fakeNICs{}
	pips := &fakePIPs{
		getResult: armnetwork.PublicIPAddress{
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{
				IPAddress:   to.Ptr("4.4.4.4"),
				DNSSettings: &armnetwork.PublicIPAddressDNSSettings{Fqdn: to.Ptr("soul-vm-abc.westeurope.cloudapp.azure.com")},
			},
		},
	}
	withFakeClients(t, vms, nics, pips)

	d := &AzureDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, profileMap()),
		Credentials: mustStruct(t, credsMap()),
		Userdata:    "#cloud-config\n",
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if last == nil || last.Failed {
		t.Fatalf("final=%+v, want success", last)
	}
	if len(last.Vms) != 1 {
		t.Fatalf("vms=%d, want 1", len(last.Vms))
	}
	vm := last.Vms[0]
	if vm.VmId != "soul-vm-abc" {
		t.Errorf("vm_id=%q", vm.VmId)
	}
	if vm.Fqdn != "soul-vm-abc.westeurope.cloudapp.azure.com" {
		t.Errorf("fqdn=%q", vm.Fqdn)
	}
	if vm.PrimaryIp != "4.4.4.4" {
		t.Errorf("primary_ip=%q", vm.PrimaryIp)
	}
	// 3-resource путь пройден полностью.
	if len(pips.createCalls) != 1 || len(nics.createCalls) != 1 || vms.createCalled != 1 {
		t.Errorf("3-resource create calls: pip=%d nic=%d vm=%d",
			len(pips.createCalls), len(nics.createCalls), vms.createCalled)
	}
	// userdata в osProfile.customData как base64 (Azure-требование).
	cd := vms.lastCreateVM.Properties.OSProfile.CustomData
	if cd == nil {
		t.Fatal("customData not set")
	}
	decoded, derr := base64.StdEncoding.DecodeString(*cd)
	if derr != nil || string(decoded) != "#cloud-config\n" {
		t.Errorf("customData decoded=%q err=%v", decoded, derr)
	}
}

// --- Rollback при фейле NIC ---

func TestCreate_RollbackOnNICFail(t *testing.T) {
	withDeterministicSuffix(t, "rb1")
	withFastBackoff(t, 2)
	vms := &fakeVMs{}
	nics := &fakeNICs{createErr: &azcore.ResponseError{
		StatusCode: http.StatusBadRequest, ErrorCode: "InvalidParameter",
	}}
	pips := &fakePIPs{}
	withFakeClients(t, vms, nics, pips)

	d := &AzureDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1, Profile: mustStruct(t, profileMap()), Credentials: mustStruct(t, credsMap()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on NIC fail")
	}
	// PIP создан → rollback должен его удалить. VM не создавалась.
	if len(pips.deleteCalls) != 1 || pips.deleteCalls[0] != "soul-vm-rb1-pip" {
		t.Errorf("rollback expected delete PIP, got %v", pips.deleteCalls)
	}
	if vms.createCalled != 0 {
		t.Error("VM must not be created after NIC fail")
	}
}

// --- Rollback при фейле VM ---

func TestCreate_RollbackOnVMFail(t *testing.T) {
	withDeterministicSuffix(t, "rb2")
	withFastBackoff(t, 2)
	vms := &fakeVMs{createErr: &azcore.ResponseError{
		StatusCode: http.StatusBadRequest, ErrorCode: "InvalidParameter",
	}}
	nics := &fakeNICs{}
	pips := &fakePIPs{}
	withFakeClients(t, vms, nics, pips)

	d := &AzureDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1, Profile: mustStruct(t, profileMap()), Credentials: mustStruct(t, credsMap()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on VM fail")
	}
	// PIP + NIC созданы → rollback должен удалить обоих, обратный порядок (NIC, PIP).
	if len(nics.deleteCalls) != 1 || nics.deleteCalls[0] != "soul-vm-rb2-nic" {
		t.Errorf("rollback expected delete NIC, got %v", nics.deleteCalls)
	}
	if len(pips.deleteCalls) != 1 || pips.deleteCalls[0] != "soul-vm-rb2-pip" {
		t.Errorf("rollback expected delete PIP, got %v", pips.deleteCalls)
	}
}

// --- Wait-until-ready (pending → succeeded+running) ---

func TestCreate_WaitsForRunning(t *testing.T) {
	withDeterministicSuffix(t, "wt1")
	vms := &fakeVMs{
		// Первый Get (probe round) — Creating; второй и далее — Succeeded+running.
		getSeq: []armcompute.VirtualMachinesClientGetResponse{
			pendingVMResponse("soul-vm-wt1"),
			runningVMResponse("soul-vm-wt1"),
		},
	}
	nics := &fakeNICs{}
	pips := &fakePIPs{getResult: armnetwork.PublicIPAddress{
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{IPAddress: to.Ptr("1.2.3.4")},
	}}
	withFakeClients(t, vms, nics, pips)

	d := &AzureDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1, Profile: mustStruct(t, profileMap()), Credentials: mustStruct(t, credsMap()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.last().Failed {
		t.Fatalf("final=%+v, want success after wait", s.last())
	}
	if s.last().Vms[0].PrimaryIp != "1.2.3.4" {
		t.Errorf("primary_ip=%q", s.last().Vms[0].PrimaryIp)
	}
}

// --- Auth-error на старте Create (не доходит до создания ресурсов) ---

func TestCreate_AuthError_NoResources(t *testing.T) {
	withDeterministicSuffix(t, "au1")
	withFastBackoff(t, 1)
	vms := &fakeVMs{}
	nics := &fakeNICs{}
	pips := &fakePIPs{createErr: &azcore.ResponseError{
		StatusCode: http.StatusUnauthorized, ErrorCode: "AuthenticationFailed",
	}}
	withFakeClients(t, vms, nics, pips)

	d := &AzureDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1, Profile: mustStruct(t, profileMap()), Credentials: mustStruct(t, credsMap()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true")
	}
	if !strings.Contains(last.Message, "auth:") {
		t.Errorf("message=%q, want auth-class", last.Message)
	}
	// PIP create вызывался один раз и сразу провалил без retry (auth — не transient).
	if len(pips.createCalls) != 1 {
		t.Errorf("PIP create calls=%d, want 1 (no retry on auth)", len(pips.createCalls))
	}
	if len(nics.createCalls) != 0 || vms.createCalled != 0 {
		t.Error("NIC/VM must not be created after PIP auth fail")
	}
}

// --- Идемпотентность: tag-match → не создаёт повторно ---

func TestCreate_Idempotent_TagMatch(t *testing.T) {
	withDeterministicSuffix(t, "id1")
	prof := profileMap()
	prof["tags"] = map[string]any{runTagKey: "run-42"}

	existing := []*armcompute.VirtualMachine{{
		Name:     to.Ptr("run-42-vm-0"),
		Location: to.Ptr("westeurope"),
		Properties: &armcompute.VirtualMachineProperties{ProvisioningState: to.Ptr("Succeeded")},
		Tags:     map[string]*string{runTagKey: to.Ptr("run-42")},
	}}
	vms := &fakeVMs{
		listResult: existing,
		// finalizeCreate сделает Get(InstanceView) + fillVMInfo->Get(no expand) → seq из 2.
		getSeq: []armcompute.VirtualMachinesClientGetResponse{
			runningVMResponse("run-42-vm-0"),
			runningVMResponse("run-42-vm-0"),
		},
	}
	nics := &fakeNICs{}
	pips := &fakePIPs{getResult: armnetwork.PublicIPAddress{
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{IPAddress: to.Ptr("9.9.9.9")},
	}}
	withFakeClients(t, vms, nics, pips)

	d := &AzureDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1, Profile: mustStruct(t, prof), Credentials: mustStruct(t, credsMap()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if vms.createCalled != 0 {
		t.Errorf("VM create called %d; idempotent path must not create", vms.createCalled)
	}
	if len(nics.createCalls) != 0 || len(pips.createCalls) != 0 {
		t.Error("NIC/PIP create called on idempotent path")
	}
	if s.last().Failed {
		t.Fatalf("idempotent final failed: %+v", s.last())
	}
	if s.last().Vms[0].VmId != "run-42-vm-0" {
		t.Errorf("vm_id=%q, want run-42-vm-0", s.last().Vms[0].VmId)
	}
}

// --- Ctx-cancel anti-orphan: VM создалась, wait упёрся в cancel,
//     composite vm_id попадает в финальный event. ---

func TestCreate_CtxCancel_AntiOrphan(t *testing.T) {
	withDeterministicSuffix(t, "ao1")
	ctx, cancel := context.WithCancel(context.Background())
	vms := &fakeVMs{
		// все Get'ы возвращают pending — поллер крутится до отмены ctx.
		getSeq: []armcompute.VirtualMachinesClientGetResponse{pendingVMResponse("soul-vm-ao1")},
	}
	nics := &fakeNICs{}
	pips := &fakePIPs{}
	withFakeClients(t, vms, nics, pips)
	cancel() // отменим сразу — поллер уйдёт в sleepCtx и вернёт ctx.Err
	d := &AzureDriver{}
	s := &createStream{ctx: ctx}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1, Profile: mustStruct(t, profileMap()), Credentials: mustStruct(t, credsMap()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on ctx-cancel")
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "soul-vm-ao1" {
		t.Errorf("anti-orphan: final must carry composite vm_id, got %+v", last.Vms)
	}
}

// --- Destroy: 3-resource обратный порядок ---

func TestDestroy_ThreeResources_ReverseOrder(t *testing.T) {
	withFastBackoff(t, 1)
	vms := &fakeVMs{}
	nics := &fakeNICs{}
	pips := &fakePIPs{}
	withFakeClients(t, vms, nics, pips)

	d := &AzureDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds: []string{"run-1-vm-0"}, Credentials: mustStruct(t, credsMap()),
	}, s); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// VM сначала, потом NIC, потом PIP.
	if len(vms.deleteCalls) != 1 || vms.deleteCalls[0] != "run-1-vm-0" {
		t.Errorf("vm deletes=%v", vms.deleteCalls)
	}
	if len(nics.deleteCalls) != 1 || nics.deleteCalls[0] != "run-1-vm-0-nic" {
		t.Errorf("nic deletes=%v", nics.deleteCalls)
	}
	if len(pips.deleteCalls) != 1 || pips.deleteCalls[0] != "run-1-vm-0-pip" {
		t.Errorf("pip deletes=%v", pips.deleteCalls)
	}
	if len(s.sent) != 1 || s.sent[0].Failed {
		t.Errorf("destroy events=%+v", s.sent)
	}
}

// --- Destroy not-found на VM-шаге = идемпотентно, продолжаем NIC/PIP ---

func TestDestroy_NotFoundIsIdempotent(t *testing.T) {
	withFastBackoff(t, 1)
	notFound := &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "ResourceNotFound"}
	vms := &fakeVMs{deleteErr: notFound}
	nics := &fakeNICs{deleteErr: notFound}
	pips := &fakePIPs{deleteErr: notFound}
	withFakeClients(t, vms, nics, pips)

	d := &AzureDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds: []string{"vm-gone"}, Credentials: mustStruct(t, credsMap()),
	}, s); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(s.sent) != 1 || s.sent[0].Failed {
		t.Errorf("not-found destroy must be idempotent (success), got %+v", s.sent)
	}
	if s.sent[0].Message != "already absent" {
		t.Errorf("message=%q, want already absent", s.sent[0].Message)
	}
}

// --- Status: возвращает power-state из InstanceView ---

func TestStatus_PowerStateFromInstanceView(t *testing.T) {
	vms := &fakeVMs{getSeq: []armcompute.VirtualMachinesClientGetResponse{runningVMResponse("v1")}}
	withFakeClients(t, vms, &fakeNICs{}, &fakePIPs{})

	d := &AzureDriver{}
	rep, err := d.Status(context.Background(), &pluginv1.StatusRequest{
		VmId: "v1", Credentials: mustStruct(t, credsMap()),
	})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if rep.State != "running" {
		t.Errorf("state=%q, want running", rep.State)
	}
	if rep.Attributes == nil {
		t.Error("attributes must be populated")
	}
}

// --- List: фильтр по runTag ---

func TestList_FilterByRunTag(t *testing.T) {
	vms := &fakeVMs{listResult: []*armcompute.VirtualMachine{
		{Name: to.Ptr("vm-a"), Location: to.Ptr("westeurope"), Properties: &armcompute.VirtualMachineProperties{ProvisioningState: to.Ptr("Succeeded")}},
		{Name: to.Ptr("vm-b"), Location: to.Ptr("westeurope"), Properties: &armcompute.VirtualMachineProperties{ProvisioningState: to.Ptr("Succeeded")}},
	}}
	pips := &fakePIPs{getResult: armnetwork.PublicIPAddress{
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{IPAddress: to.Ptr("1.1.1.1")},
	}}
	withFakeClients(t, vms, &fakeNICs{}, pips)

	d := &AzureDriver{}
	s := &listStream{}
	if err := d.List(&pluginv1.ListRequest{
		Filter:      mustStruct(t, map[string]any{runTagKey: "run-list"}),
		Credentials: mustStruct(t, credsMap()),
	}, s); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(s.sent) != 2 {
		t.Errorf("list events=%d, want 2", len(s.sent))
	}
}

// --- List без runTag → пустой ответ (защита от full-subscription dump) ---

func TestList_WithoutFilter_Empty(t *testing.T) {
	vms := &fakeVMs{listResult: []*armcompute.VirtualMachine{{Name: to.Ptr("vm-x")}}}
	withFakeClients(t, vms, &fakeNICs{}, &fakePIPs{})

	d := &AzureDriver{}
	s := &listStream{}
	if err := d.List(&pluginv1.ListRequest{
		Credentials: mustStruct(t, credsMap()),
	}, s); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(s.sent) != 0 {
		t.Errorf("without runTag filter must return empty, got %d", len(s.sent))
	}
}

// --- classify (Azure error codes + HTTP status) ---

func TestClassifyAzure_Codes(t *testing.T) {
	cases := []struct {
		name string
		err  *azcore.ResponseError
		want clouddriver.FailClass
	}{
		{"401-auth", &azcore.ResponseError{StatusCode: http.StatusUnauthorized, ErrorCode: "AuthenticationFailed"}, clouddriver.FailAuth},
		{"403-auth", &azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "AuthorizationFailed"}, clouddriver.FailAuth},
		{"404-not-found", &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "ResourceNotFound"}, clouddriver.FailNotFound},
		{"400-invalid-params", &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "InvalidParameter"}, clouddriver.FailInvalidParams},
		{"429-throttle", &azcore.ResponseError{StatusCode: http.StatusTooManyRequests, ErrorCode: "TooManyRequests"}, clouddriver.FailTransient},
		{"500-transient", &azcore.ResponseError{StatusCode: http.StatusInternalServerError, ErrorCode: ""}, clouddriver.FailTransient},
		{"quota", &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "QuotaExceeded"}, clouddriver.FailQuota},
		{"publicip-quota", &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "PublicIPCountLimitReached"}, clouddriver.FailQuota},
		{"409-conflict-as-invalid", &azcore.ResponseError{StatusCode: http.StatusConflict, ErrorCode: "PropertyChangeNotAllowed"}, clouddriver.FailInvalidParams},
		{"400-with-NotFound-code", &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "ResourceGroupNotFound"}, clouddriver.FailNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAzure(tc.err)
			if got != tc.want {
				t.Errorf("classifyAzure(%+v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	// Не-API ошибка → transient.
	if got := classifyAzure(errors.New("dial tcp: timeout")); got != clouddriver.FailTransient {
		t.Errorf("non-API err class=%v, want transient", got)
	}
}
