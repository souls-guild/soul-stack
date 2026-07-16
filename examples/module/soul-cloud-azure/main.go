// soul-cloud-azure is a real Soul Stack CloudDriver plugin for Azure VM
// (ADR-016 Phase 4 cloud parity; rollout by the soul-cloud-aws reference
// pattern).
//
// Builds into the static binary `soul-cloud-azure`. The Keeper-side
// `core.cloud.provisioned` module (ADR-017) starts it as a sub-process, performs
// the gRPC-stdio handshake (sdk/handshake), and calls CloudDriver RPCs.
//
// Credentials (A-flow, docs/keeper/cloud.md): Keeper resolves the
// service-principal secret from Vault and places plain values into
// CreateRequest.credentials / DestroyRequest.credentials
// (tenant_id/client_id/client_secret + subscription_id/resource_group/location);
// the driver does NOT call Vault. Cloud-init userdata for bootstrapping the soul
// agent arrives in CreateRequest.userdata and is base64-encoded by the driver
// (Azure requirement for osProfile.customData).
//
// Shared framework (error taxonomy / wait-until-ready / retry-backoff) comes
// from sdk/clouddriver and is common to all rollout drivers. Provider-specific
// pieces here are only Azure ARM API calls and the Azure error classifier
// (classify.go).
//
// # Multi-resource Create transaction
//
// Unlike EC2, an Azure VM is a composite: PublicIP + NIC + VM are created by
// three separate ARM operations. The driver performs them sequentially (NIC
// requires an id reference to a ready PublicIP, VM requires a ready NIC), and on
// failure of step N it rolls back steps 1..N-1 in reverse order (best-effort).
//
// Composite identifier: primary `vm_id` = VM name (`<runTag>-vm-<idx>` or
// `soul-vm-<short-rand>` without runTag). NIC/PIP names are deterministic
// derivatives (`<vm_name>-nic` / `<vm_name>-pip`), allowing Keeper to reconstruct
// all three resources from one `vm_id` and Destroy them correctly in reverse
// order without storing a separate mapping.
package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// profileSchemaJSON is profile_schema (JSON Schema draft 2020-12), embedded next
// to the binary. Symmetrical with soul-cloud-aws.
//
//go:embed schema.json
var profileSchemaJSON []byte

// runTagKey is the idempotency tag: value = run/incarnation identifier. The tag
// name in Azure follows snake_case notation; colons in tag keys are allowed, but
// a hyphen is safer for CLI filters and compatible with all rollout providers
// (same tag in soul-cloud-aws).
const runTagKey = "soulstack-run"

// defaultBackoff is the [clouddriver.BackoffConfig] factory for wait/retry
// phases (see soul-cloud-aws). L0 tests replace it through withFastBackoff.
var defaultBackoff = clouddriver.DefaultBackoff

// randomSuffix is a short random suffix for the VM name when runTag is not set.
// It is a variable for deterministic L0 tests.
var randomSuffix = func() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// AzureDriver implements CloudDriver for Azure.
type AzureDriver struct {
	clouddriver.BaseDriver
}

// Schema publishes the embedded profile_schema (symmetrical with soul-cloud-aws).
func (a *AzureDriver) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	var raw map[string]any
	if err := json.Unmarshal(profileSchemaJSON, &raw); err != nil {
		return nil, fmt.Errorf("parse embedded schema.json: %w", err)
	}
	s, err := structpb.NewStruct(raw)
	if err != nil {
		return nil, fmt.Errorf("encode profile_schema: %w", err)
	}
	return &pluginv1.SchemaReply{ProfileSchema: s}, nil
}

// Validate performs driver-side structural checks. Full JSON Schema validation
// is done by Keeper against the published Schema; here we cover required fields
// as defense-in-depth (Keeper is not required to do it on every Create).
func (a *AzureDriver) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	p := req.GetProfile().AsMap()
	var errs []string
	if stringField(p, "location") == "" {
		errs = append(errs, "profile.location is required")
	}
	if stringField(p, "vm_size") == "" {
		errs = append(errs, "profile.vm_size is required")
	}
	if stringField(p, "subnet_id") == "" {
		errs = append(errs, "profile.subnet_id is required")
	}
	img, _ := p["image"].(map[string]any)
	if stringField(img, "publisher") == "" || stringField(img, "offer") == "" || stringField(img, "sku") == "" {
		errs = append(errs, "profile.image.{publisher,offer,sku} are required")
	}
	return &pluginv1.ValidateProfileReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// vmProfile contains profile params parsed for Create.
type vmProfile struct {
	location               string
	vmSize                 string
	imagePublisher         string
	imageOffer             string
	imageSKU               string
	imageVersion           string
	subnetID               string
	adminUsername          string
	sshPublicKey           string
	publicIPEnabled        bool
	publicIPSku            string
	publicIPDNSLabel       string
	diskSizeGB             int32
	diskType               string
	networkSecurityGroupID string
	tags                   map[string]string
	runTag                 string
}

func parseProfile(p map[string]any) vmProfile {
	prof := vmProfile{
		location:      stringField(p, "location"),
		vmSize:        stringField(p, "vm_size"),
		subnetID:      stringField(p, "subnet_id"),
		adminUsername: stringField(p, "admin_username"),
		sshPublicKey:  stringField(p, "ssh_public_key"),
		// public_ip defaults to true: an Azure VM without a public IP is useless
		// for soul bootstrap (Keeper cannot reach it); the operator disables it
		// explicitly.
		publicIPEnabled: true,
		publicIPSku:     "Standard",
		tags:            map[string]string{},
	}
	if prof.adminUsername == "" {
		prof.adminUsername = "soul"
	}
	if img, ok := p["image"].(map[string]any); ok {
		prof.imagePublisher = stringField(img, "publisher")
		prof.imageOffer = stringField(img, "offer")
		prof.imageSKU = stringField(img, "sku")
		prof.imageVersion = stringField(img, "version")
		if prof.imageVersion == "" {
			prof.imageVersion = "latest"
		}
	}
	if pip, ok := p["public_ip"].(map[string]any); ok {
		if e, ok := pip["enabled"].(bool); ok {
			prof.publicIPEnabled = e
		}
		if s := stringField(pip, "sku"); s != "" {
			prof.publicIPSku = s
		}
		prof.publicIPDNSLabel = stringField(pip, "dns_label")
	}
	if disk, ok := p["disk"].(map[string]any); ok {
		if sz, ok := disk["size_gb"].(float64); ok {
			prof.diskSizeGB = int32(sz)
		}
		prof.diskType = stringField(disk, "type")
	}
	prof.networkSecurityGroupID = stringField(p, "network_security_group_id")
	if tags, ok := p["tags"].(map[string]any); ok {
		for k, v := range tags {
			if s, ok := v.(string); ok {
				prof.tags[k] = s
			}
		}
	}
	prof.runTag = prof.tags[runTagKey]
	return prof
}

// resourceNames returns deterministic names for the VM/NIC/PIP resource trio by
// `vmName`. One primary `vm_id` = VM name is enough for Destroy: NIC/PIP names
// are reconstructed by the same rules without storing a mapping.
type resourceNames struct {
	vm  string
	nic string
	pip string
}

func makeResourceNames(vmName string) resourceNames {
	return resourceNames{vm: vmName, nic: vmName + "-nic", pip: vmName + "-pip"}
}

// makeVMName returns a deterministic VM name:
//   - with runTag: "<runTag>-vm-<idx>" (idx is 0-based order within the run);
//   - without runTag: "soul-vm-<3-byte-hex>" (through [randomSuffix], replaced by tests).
func makeVMName(runTag string, idx int) string {
	if runTag != "" {
		return fmt.Sprintf("%s-vm-%d", runTag, idx)
	}
	return "soul-vm-" + randomSuffix()
}

// Create: multi-resource transaction PIP -> NIC -> VM with rollback on failure.
// See the package doc comment.
func (a *AzureDriver) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	ctx := stream.Context()
	count := req.GetCount()
	if count <= 0 {
		count = 1
	}
	prof := parseProfile(req.GetProfile().AsMap())
	creds := credsFromMap(req.GetCredentials().AsMap())
	if creds.Location == "" {
		creds.Location = prof.location
	}

	cli, err := newAzureClients(ctx, creds)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.FailAuth, "azure-client", err)
	}

	backoff := defaultBackoff()

	// Idempotency: reuse already created VMs with the same runTag.
	var existing []*armcompute.VirtualMachine
	if prof.runTag != "" {
		existing, err = a.findByRunTag(ctx, cli, backoff, creds.ResourceGroup, prof.runTag)
		if err != nil {
			return sendCreateFailed(stream, clouddriver.Classify(classifyAzure, err), "list-existing", err)
		}
		if int32(len(existing)) >= count {
			_ = stream.Send(&pluginv1.CreateEvent{
				Message: fmt.Sprintf("idempotent: %d VM already present for run %q", len(existing), prof.runTag),
			})
			return a.finalizeCreate(ctx, cli, stream, backoff, creds, existingVMIDs(existing))
		}
		count -= int32(len(existing))
	}

	// Create `count` new VMs, each as a multi-resource transaction.
	newIDs := make([]string, 0, count)
	startIdx := len(existing)
	for i := int32(0); i < count; i++ {
		name := makeVMName(prof.runTag, startIdx+int(i))
		if err := stream.Send(&pluginv1.CreateEvent{
			Message: fmt.Sprintf("azure.Create vm=%s size=%s image=%s/%s/%s",
				name, prof.vmSize, prof.imagePublisher, prof.imageOffer, prof.imageSKU),
		}); err != nil {
			return err
		}
		if err := a.createOneVM(ctx, cli, backoff, creds, prof, name, req.GetUserdata(), stream); err != nil {
			// createOneVM already performed rollback and sent partial events;
			// anti-orphan: the name of an already created (then rolled back) VM
			// does NOT enter the final VM list (rollback succeeded -> no resources).
			//
			// Exception: rollback partially failed -> the name goes into the final
			// event as failed (see createOneVM).
			final := append(existingVMIDs(existing), newIDs...)
			return sendFinalRollbackFail(stream, clouddriver.Classify(classifyAzure, err), "create-vm", err, final)
		}
		newIDs = append(newIDs, name)
	}

	all := append(existingVMIDs(existing), newIDs...)
	return a.finalizeCreate(ctx, cli, stream, backoff, creds, all)
}

// createOneVM is a multi-resource transaction for one VM with rollback on
// failure.
//
// Order: PIP -> NIC -> VM. Rollback goes in reverse order (VM -> NIC -> PIP) and
// skips resources that were not created yet. Each step is wrapped in Retry
// (transient API failures do not fail the whole create; this is critical for
// Azure, whose throttling frequency is higher than AWS).
func (a *AzureDriver) createOneVM(
	ctx context.Context, cli azureClients, backoff clouddriver.BackoffConfig,
	creds azureCredentials, prof vmProfile, vmName, userdata string,
	stream grpc.ServerStreamingServer[pluginv1.CreateEvent],
) error {
	names := makeResourceNames(vmName)

	// List of created resources for rollback (last-in-first-out).
	var created []resourceRef
	rollback := func(opErr error) {
		_ = stream.Send(&pluginv1.CreateEvent{
			Message: fmt.Sprintf("rollback after %v: deleting %d resource(s)", opErr, len(created)),
		})
		// Reverse order: VM -> NIC -> PIP. ctx may already be canceled - rollback
		// uses Background for best-effort cleanup (anti-orphan), otherwise orphan
		// resources remain in the subscription.
		rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for i := len(created) - 1; i >= 0; i-- {
			r := created[i]
			if delErr := r.delete(rbCtx, cli, creds.ResourceGroup); delErr != nil {
				// Retry a transient rollback error once or twice (fast backoff);
				// persistent errors are written to the event and we continue.
				_ = stream.Send(&pluginv1.CreateEvent{
					Message: fmt.Sprintf("rollback warn: delete %s %q: %v", r.kind, r.name, delErr),
				})
			}
		}
	}

	tagsForResource := mergeTags(prof.tags)

	// --- 1. PublicIP (optional) ---
	var pipID string
	if prof.publicIPEnabled {
		pipParams := buildPublicIP(prof, tagsForResource)
		if err := clouddriver.Retry(ctx, backoff, classifyAzure, func() error {
			pip, perr := cli.pips.CreateAndWait(ctx, creds.ResourceGroup, names.pip, pipParams)
			if perr != nil {
				return perr
			}
			if pip.ID != nil {
				pipID = *pip.ID
			}
			return nil
		}); err != nil {
			// Rollback is empty - nothing to roll back.
			return fmt.Errorf("publicip create %s: %w", names.pip, err)
		}
		created = append(created, resourceRef{kind: "publicip", name: names.pip})
	}

	// --- 2. NIC ---
	var nicID string
	nicParams := buildNIC(prof, pipID, tagsForResource)
	if err := clouddriver.Retry(ctx, backoff, classifyAzure, func() error {
		nic, nerr := cli.nics.CreateAndWait(ctx, creds.ResourceGroup, names.nic, nicParams)
		if nerr != nil {
			return nerr
		}
		if nic.ID != nil {
			nicID = *nic.ID
		}
		return nil
	}); err != nil {
		rollback(err)
		return fmt.Errorf("nic create %s: %w", names.nic, err)
	}
	created = append(created, resourceRef{kind: "nic", name: names.nic})

	// --- 3. VM ---
	vmParams := buildVM(prof, nicID, userdata, tagsForResource)
	if err := clouddriver.Retry(ctx, backoff, classifyAzure, func() error {
		_, verr := cli.vms.CreateAndWait(ctx, creds.ResourceGroup, names.vm, vmParams)
		return verr
	}); err != nil {
		rollback(err)
		return fmt.Errorf("vm create %s: %w", names.vm, err)
	}
	// Do NOT add the VM to `created`: a successfully created VM is not subject to
	// rollback (it is the final target). If wait-until-ready fails later, that is
	// anti-orphan, not rollback (see finalizeCreate).
	return nil
}

// finalizeCreate waits for VM readiness (ProvisioningState=Succeeded +
// powerState=running) and sends the final event.
func (a *AzureDriver) finalizeCreate(
	ctx context.Context, cli azureClients,
	stream grpc.ServerStreamingServer[pluginv1.CreateEvent],
	backoff clouddriver.BackoffConfig, creds azureCredentials, vmIDs []string,
) error {
	probe := func(pctx context.Context, vmID string) clouddriver.ProbeResult {
		ready, perr := a.probeVMReady(pctx, cli, creds.ResourceGroup, vmID)
		if perr != nil {
			if clouddriver.Classify(classifyAzure, perr).Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{Err: perr}
		}
		return clouddriver.ProbeResult{Ready: ready}
	}

	waitResults, waitErr := clouddriver.WaitUntilReady(ctx, backoff, vmIDs, probe,
		func(msg string) { _ = stream.Send(&pluginv1.CreateEvent{Message: msg}) })

	vms := make([]*pluginv1.VmInfo, 0, len(vmIDs))
	anyFailed := false
	for _, wr := range waitResults {
		vi := &pluginv1.VmInfo{VmId: wr.VMID}
		if wr.Ready {
			a.fillVMInfo(ctx, cli, creds, wr.VMID, vi)
		}
		if !wr.Ready {
			anyFailed = true
		}
		vms = append(vms, vi)
	}

	if waitErr != nil {
		return stream.Send(&pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyAzure, waitErr), "wait-until-ready", waitErr),
			Vms:     vms,
			Failed:  true,
		})
	}
	return stream.Send(&pluginv1.CreateEvent{
		Message: "completed",
		Vms:     vms,
		Failed:  anyFailed,
	})
}

// probeVMReady is true when VM ProvisioningState=Succeeded AND InstanceView
// contains PowerState/running. Uses one Get with Expand=InstanceView to avoid
// hitting the API twice.
func (a *AzureDriver) probeVMReady(ctx context.Context, cli azureClients, rg, vmName string) (bool, error) {
	resp, err := cli.vms.Get(ctx, rg, vmName, &armcompute.VirtualMachinesClientGetOptions{
		Expand: to.Ptr(armcompute.InstanceViewTypesInstanceView),
	})
	if err != nil {
		return false, err
	}
	if resp.Properties == nil {
		return false, nil
	}
	if resp.Properties.ProvisioningState == nil || *resp.Properties.ProvisioningState != "Succeeded" {
		// Failed-ProvisioningState is terminal, not transient: return it as error,
		// and the poller will close this VM.
		if resp.Properties.ProvisioningState != nil && *resp.Properties.ProvisioningState == "Failed" {
			return false, fmt.Errorf("vm %s provisioning failed", vmName)
		}
		return false, nil
	}
	iv := resp.Properties.InstanceView
	if iv == nil {
		return false, nil
	}
	for _, st := range iv.Statuses {
		if st == nil || st.Code == nil {
			continue
		}
		// PowerState status format is "PowerState/<state>".
		if *st.Code == "PowerState/running" {
			return true, nil
		}
		if *st.Code == "PowerState/deallocated" || *st.Code == "PowerState/stopped" {
			return false, fmt.Errorf("vm %s entered terminal power state %q", vmName, *st.Code)
		}
	}
	return false, nil
}

// fillVMInfo fills VmInfo (fqdn/primary_ip/attributes) from a ready VM.
func (a *AzureDriver) fillVMInfo(ctx context.Context, cli azureClients, creds azureCredentials, vmName string, vi *pluginv1.VmInfo) {
	names := makeResourceNames(vmName)
	// VM is needed only for attributes (vm_size/state) - without InstanceView.
	vmResp, err := cli.vms.Get(ctx, creds.ResourceGroup, vmName, nil)
	if err == nil {
		vi.Attributes = vmAttributes(vmResp.VirtualMachine, creds.Location)
	}
	// Primary IP / FQDN come from PublicIP, if present.
	if pip, err := cli.pips.Get(ctx, creds.ResourceGroup, names.pip); err == nil {
		if pip.Properties != nil {
			if pip.Properties.IPAddress != nil {
				vi.PrimaryIp = *pip.Properties.IPAddress
			}
			if pip.Properties.DNSSettings != nil && pip.Properties.DNSSettings.Fqdn != nil {
				vi.Fqdn = *pip.Properties.DNSSettings.Fqdn
			}
		}
	}
	// Without PublicIP / DNS label: fallback FQDN = VM name (internal name, unique
	// within the resource group). This is the `SID` for bootstrap.
	if vi.Fqdn == "" {
		vi.Fqdn = vmName
	}
}

// findByRunTag lists VMs with the given runTag in the resource group.
func (a *AzureDriver) findByRunTag(ctx context.Context, cli azureClients, backoff clouddriver.BackoffConfig, rg, runTag string) ([]*armcompute.VirtualMachine, error) {
	var out []*armcompute.VirtualMachine
	err := clouddriver.Retry(ctx, backoff, classifyAzure, func() error {
		v, rerr := cli.vms.ListByRunTag(ctx, rg, runTag)
		if rerr != nil {
			return rerr
		}
		out = v
		return nil
	})
	return out, err
}

// Destroy: VM -> NIC -> PIP (reverse Create order). Per-VM events.
func (a *AzureDriver) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newAzureClients(ctx, creds)
	if err != nil {
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(clouddriver.FailAuth, "azure-client", err), Failed: true})
		return nil
	}

	backoff := defaultBackoff()
	for _, vmName := range req.GetVmIds() {
		names := makeResourceNames(vmName)
		allMissing, stepErr := a.destroyOne(ctx, cli, backoff, creds.ResourceGroup, names)
		if stepErr != nil {
			class := clouddriver.Classify(classifyAzure, stepErr)
			_ = stream.Send(&pluginv1.DestroyEvent{
				VmId:    vmName,
				Message: clouddriver.FailMessage(class, "destroy", stepErr),
				Failed:  true,
			})
			continue
		}
		// All three steps returned not-found => resources are already absent =>
		// idempotent. Otherwise at least one was actually deleted => "terminated".
		msg := "terminated"
		if allMissing {
			msg = "already absent"
		}
		_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmName, Message: msg})
	}
	return nil
}

// destroyOne performs VM -> NIC -> PIP, each through Retry. not-found at any step
// continues (idempotency); returns allMissing=true only if ALL three steps were
// not-found (for correct "already absent" semantics).
func (a *AzureDriver) destroyOne(ctx context.Context, cli azureClients, backoff clouddriver.BackoffConfig, rg string, names resourceNames) (bool, error) {
	steps := []struct {
		kind   string
		delete func() error
	}{
		{"vm", func() error { return cli.vms.DeleteAndWait(ctx, rg, names.vm) }},
		{"nic", func() error { return cli.nics.DeleteAndWait(ctx, rg, names.nic) }},
		{"pip", func() error { return cli.pips.DeleteAndWait(ctx, rg, names.pip) }},
	}
	allMissing := true
	for _, s := range steps {
		err := clouddriver.Retry(ctx, backoff, classifyAzure, s.delete)
		if err == nil {
			allMissing = false
			continue
		}
		if clouddriver.Classify(classifyAzure, err) == clouddriver.FailNotFound {
			continue
		}
		return false, fmt.Errorf("%s %s: %w", s.kind, names.vm, err)
	}
	return allMissing, nil
}

// Status polls one VM with Expand=InstanceView (state + powerState).
func (a *AzureDriver) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newAzureClients(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("azure-client: %w", err)
	}
	resp, err := cli.vms.Get(ctx, creds.ResourceGroup, req.GetVmId(),
		&armcompute.VirtualMachinesClientGetOptions{Expand: to.Ptr(armcompute.InstanceViewTypesInstanceView)})
	if err != nil {
		return nil, fmt.Errorf("vms.Get %s: %w", req.GetVmId(), err)
	}
	state := derivePowerState(resp.VirtualMachine)
	return &pluginv1.StatusReply{
		State:      state,
		Attributes: vmAttributes(resp.VirtualMachine, creds.Location),
	}, nil
}

// List streams VM inventory (optional runTag filter).
func (a *AzureDriver) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newAzureClients(ctx, creds)
	if err != nil {
		return fmt.Errorf("azure-client: %w", err)
	}
	filter := req.GetFilter().AsMap()
	runTag := stringField(filter, runTagKey)
	if runTag == "" {
		// Without a filter, return an empty list (protects against accidentally
		// dumping the whole subscription). Symmetrical with the AWS variant where
		// filter is required in scenarios.
		return nil
	}
	vms, err := cli.vms.ListByRunTag(ctx, creds.ResourceGroup, runTag)
	if err != nil {
		return fmt.Errorf("vms.List: %w", err)
	}
	for _, vm := range vms {
		if vm == nil || vm.Name == nil {
			continue
		}
		vi := &pluginv1.VmInfo{VmId: *vm.Name}
		a.fillVMInfo(ctx, cli, creds, *vm.Name, vi)
		if serr := stream.Send(vi); serr != nil {
			return serr
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&AzureDriver{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-azure:", err)
		os.Exit(1)
	}
}

// --- ARM parameters (builders) ---

func buildPublicIP(prof vmProfile, tags map[string]*string) armnetwork.PublicIPAddress {
	pip := armnetwork.PublicIPAddress{
		Location: &prof.location,
		SKU:      &armnetwork.PublicIPAddressSKU{Name: skuName(prof.publicIPSku)},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
		Tags: tags,
	}
	if prof.publicIPDNSLabel != "" {
		pip.Properties.DNSSettings = &armnetwork.PublicIPAddressDNSSettings{
			DomainNameLabel: &prof.publicIPDNSLabel,
		}
	}
	return pip
}

func skuName(s string) *armnetwork.PublicIPAddressSKUName {
	switch s {
	case "Basic":
		return to.Ptr(armnetwork.PublicIPAddressSKUNameBasic)
	default:
		return to.Ptr(armnetwork.PublicIPAddressSKUNameStandard)
	}
}

func buildNIC(prof vmProfile, pipID string, tags map[string]*string) armnetwork.Interface {
	ipCfg := &armnetwork.InterfaceIPConfiguration{
		Name: to.Ptr("ipconfig1"),
		Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
			PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
			Subnet:                    &armnetwork.Subnet{ID: &prof.subnetID},
		},
	}
	if pipID != "" {
		ipCfg.Properties.PublicIPAddress = &armnetwork.PublicIPAddress{ID: &pipID}
	}
	nic := armnetwork.Interface{
		Location: &prof.location,
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{ipCfg},
		},
		Tags: tags,
	}
	if prof.networkSecurityGroupID != "" {
		nic.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: &prof.networkSecurityGroupID}
	}
	return nic
}

func buildVM(prof vmProfile, nicID, userdata string, tags map[string]*string) armcompute.VirtualMachine {
	vm := armcompute.VirtualMachine{
		Location: &prof.location,
		Tags:     tags,
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(prof.vmSize)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{
					Publisher: &prof.imagePublisher,
					Offer:     &prof.imageOffer,
					SKU:       &prof.imageSKU,
					Version:   &prof.imageVersion,
				},
				OSDisk: buildOSDisk(prof),
			},
			OSProfile: &armcompute.OSProfile{
				ComputerName:  to.Ptr(prof.adminUsername + "-vm"), // hostname inside VM
				AdminUsername: &prof.adminUsername,
			},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
					ID: &nicID,
					Properties: &armcompute.NetworkInterfaceReferenceProperties{
						Primary: to.Ptr(true),
					},
				}},
			},
		},
	}
	// SSH public key -> Linux profile. Without it Azure requires a password,
	// which does not fit us (cloud-init runs SSH-only).
	if prof.sshPublicKey != "" {
		vm.Properties.OSProfile.LinuxConfiguration = &armcompute.LinuxConfiguration{
			DisablePasswordAuthentication: to.Ptr(true),
			SSH: &armcompute.SSHConfiguration{
				PublicKeys: []*armcompute.SSHPublicKey{{
					Path:    to.Ptr("/home/" + prof.adminUsername + "/.ssh/authorized_keys"),
					KeyData: &prof.sshPublicKey,
				}},
			},
		}
	}
	// cloud-init userdata -> customData (base64, Azure requirement).
	if userdata != "" {
		vm.Properties.OSProfile.CustomData = to.Ptr(base64.StdEncoding.EncodeToString([]byte(userdata)))
	}
	return vm
}

func buildOSDisk(prof vmProfile) *armcompute.OSDisk {
	od := &armcompute.OSDisk{
		CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
	}
	if prof.diskSizeGB > 0 {
		od.DiskSizeGB = &prof.diskSizeGB
	}
	if prof.diskType != "" {
		od.ManagedDisk = &armcompute.ManagedDiskParameters{
			StorageAccountType: to.Ptr(armcompute.StorageAccountTypes(prof.diskType)),
		}
	}
	return od
}

// derivePowerState extracts power-state from InstanceView, falling back to
// ProvisioningState. Returns the string for StatusReply.State.
func derivePowerState(vm armcompute.VirtualMachine) string {
	if vm.Properties != nil && vm.Properties.InstanceView != nil {
		for _, st := range vm.Properties.InstanceView.Statuses {
			if st == nil || st.Code == nil {
				continue
			}
			if len(*st.Code) > len("PowerState/") && (*st.Code)[:len("PowerState/")] == "PowerState/" {
				return (*st.Code)[len("PowerState/"):]
			}
		}
	}
	if vm.Properties != nil && vm.Properties.ProvisioningState != nil {
		return *vm.Properties.ProvisioningState
	}
	return ""
}

// vmAttributes is a snapshot of VM fields for StatusReply.Attributes /
// VmInfo.Attributes.
func vmAttributes(vm armcompute.VirtualMachine, locationFallback string) *structpb.Struct {
	m := map[string]any{}
	if vm.Properties != nil && vm.Properties.HardwareProfile != nil && vm.Properties.HardwareProfile.VMSize != nil {
		m["vm_size"] = string(*vm.Properties.HardwareProfile.VMSize)
	}
	loc := locationFallback
	if vm.Location != nil && *vm.Location != "" {
		loc = *vm.Location
	}
	if loc != "" {
		m["location"] = loc
	}
	if vm.Properties != nil && vm.Properties.ProvisioningState != nil {
		m["provisioning_state"] = *vm.Properties.ProvisioningState
	}
	if vm.Properties != nil && vm.Properties.TimeCreated != nil {
		m["created_at"] = vm.Properties.TimeCreated.UTC().Format(time.RFC3339)
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

// --- helpers ---

func sendCreateFailed(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error) error {
	return stream.Send(&pluginv1.CreateEvent{Message: clouddriver.FailMessage(class, op, err), Failed: true})
}

// sendFinalRollbackFail sends the final event with already-created vm_id values
// (anti-orphan) and failed=true. After rollback, resources for the specific
// failed VM are gone, but other `successful` VMs (if already created) must be
// included in the final event so Keeper can Destroy them if needed.
func sendFinalRollbackFail(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error, vmIDs []string) error {
	vms := make([]*pluginv1.VmInfo, 0, len(vmIDs))
	for _, id := range vmIDs {
		vms = append(vms, &pluginv1.VmInfo{VmId: id})
	}
	return stream.Send(&pluginv1.CreateEvent{
		Message: clouddriver.FailMessage(class, op, err),
		Vms:     vms,
		Failed:  true,
	})
}

func mergeTags(in map[string]string) map[string]*string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*string, len(in))
	for k, v := range in {
		v := v
		out[k] = &v
	}
	return out
}

func existingVMIDs(vms []*armcompute.VirtualMachine) []string {
	out := make([]string, 0, len(vms))
	for _, v := range vms {
		if v != nil && v.Name != nil {
			out = append(out, *v.Name)
		}
	}
	return out
}

// resourceRef is an entry for an already-created resource in the rollback chain.
type resourceRef struct {
	kind string // "publicip" | "nic" | "vm"
	name string
}

func (r resourceRef) delete(ctx context.Context, cli azureClients, rg string) error {
	switch r.kind {
	case "publicip":
		return cli.pips.DeleteAndWait(ctx, rg, r.name)
	case "nic":
		return cli.nics.DeleteAndWait(ctx, rg, r.name)
	case "vm":
		return cli.vms.DeleteAndWait(ctx, rg, r.name)
	}
	return errors.New("unknown resource kind: " + r.kind)
}
