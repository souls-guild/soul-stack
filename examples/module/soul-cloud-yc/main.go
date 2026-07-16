// soul-cloud-yc is the real Soul Stack CloudDriver plugin for Yandex Cloud
// (ADR-016 Phase 4 cloud parity; rollout follows the soul-cloud-aws pattern).
//
// It builds into the static `soul-cloud-yc` binary. The Keeper-side module
// `core.cloud.provisioned` (ADR-017) runs it as a subprocess, performs the
// gRPC-stdio handshake (sdk/handshake), and calls CloudDriver RPCs.
//
// Credentials (A-flow, docs/keeper/cloud.md): Keeper resolves the secret from
// Vault and puts it plain into CreateRequest.credentials /
// DestroyRequest.credentials; the driver does not call Vault. Yandex supports
// three XOR forms: iam_token / oauth_token / service_account_key (JSON blob).
// folder_id/zone arrive alongside them, symmetrical with awsCredentials.Region.
// Cloud-init userdata for bootstrapping the soul-agent is passed into
// metadata["user-data"] (YC convention).
//
// The shared framework (error taxonomy / wait-until-ready / retry-backoff)
// comes from sdk/clouddriver and is common to all rollout drivers. The only
// provider-specific parts here are compute/v1 API calls and the YC error
// classifier (classify.go).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"time"

	computev1 "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// profileSchemaJSON is profile_schema (JSON Schema draft 2020-12), embedded
// next to the binary. Same technique as in soul-cloud-aws: a separate file, not
// hardcoded in Go, so it is easier to keep in sync with
// manifest.spec.profile_schema.
//
//go:embed schema.json
var profileSchemaJSON []byte

// runLabelKey is the idempotency label: value = run/incarnation identifier
// from profile.labels. YC labels support dotted keys without special
// characters; this key name is kebab-case under YC rules
// (`[a-z][-_./\\@0-9a-z]*`). Repeated Create with the same label does not create
// duplicates: existing live VMs (PROVISIONING/STARTING/RUNNING) are reused.
const runLabelKey = "soulstack-run"

// userdataMetaKey is the standard YC instance metadata key read by cloud-init
// inside the guest OS as user-data (YC convention, symmetrical with EC2
// userdata and GCP startup-script).
const userdataMetaKey = "user-data"

// defaultBackoff is the [clouddriver.BackoffConfig] factory for wait/retry
// phases. It is a variable so L0 tests can replace it (fast MaxAttempts, short
// delays) without starting the 1s->2s->4s timer. Same technique as
// `newYcClient` (see ycapi.go).
var defaultBackoff = clouddriver.DefaultBackoff

// YcDriver implements CloudDriver for Yandex Cloud.
type YcDriver struct {
	clouddriver.BaseDriver
}

// Schema publishes the embedded profile_schema.
func (y *YcDriver) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
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

// Validate carries no credentials (ValidateProfileRequest), so structural
// checks happen here; auth happens during Create.
func (y *YcDriver) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	p := req.GetProfile().AsMap()
	var errs []string
	if stringField(p, "folder_id") == "" {
		errs = append(errs, "profile.folder_id is required")
	}
	if stringField(p, "zone") == "" {
		errs = append(errs, "profile.zone is required")
	}
	if stringField(p, "image_id") == "" {
		errs = append(errs, "profile.image_id is required")
	}
	if stringField(p, "subnet_id") == "" {
		errs = append(errs, "profile.subnet_id is required")
	}
	return &pluginv1.ValidateProfileReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// vmProfile contains profile parameters parsed for CreateInstance.
type vmProfile struct {
	folderID         string
	zone             string
	platformID       string
	cores            int64
	memoryBytes      int64
	coreFraction     int64
	imageID          string
	diskSizeBytes    int64
	diskType         string
	subnetID         string
	securityGroupIDs []string
	nat              bool
	serviceAccountID string
	labels           map[string]string
	runLabel         string
}

const (
	defaultPlatformID = "standard-v3"
	defaultDiskSizeGB = 20
	defaultDiskType   = "network-ssd"
	gibibyte          = int64(1024 * 1024 * 1024)
	defaultCores      = int64(2)
	defaultMemoryGB   = int64(2)
)

func parseProfile(p map[string]any) vmProfile {
	prof := vmProfile{
		folderID:   stringField(p, "folder_id"),
		zone:       stringField(p, "zone"),
		platformID: stringField(p, "platform_id"),
		imageID:    stringField(p, "image_id"),
		subnetID:   stringField(p, "subnet_id"),
		labels:     map[string]string{},
	}
	if prof.platformID == "" {
		prof.platformID = defaultPlatformID
	}
	if res, ok := p["resources"].(map[string]any); ok {
		prof.cores = intField(res, "cores")
		if mem := intField(res, "memory_gb"); mem > 0 {
			prof.memoryBytes = mem * gibibyte
		}
		prof.coreFraction = intField(res, "core_fraction")
	}
	if prof.cores == 0 {
		prof.cores = defaultCores
	}
	if prof.memoryBytes == 0 {
		prof.memoryBytes = defaultMemoryGB * gibibyte
	}
	if disk, ok := p["disk"].(map[string]any); ok {
		if sz := intField(disk, "size_gb"); sz > 0 {
			prof.diskSizeBytes = sz * gibibyte
		}
		prof.diskType = stringField(disk, "type")
	}
	if prof.diskSizeBytes == 0 {
		prof.diskSizeBytes = int64(defaultDiskSizeGB) * gibibyte
	}
	if prof.diskType == "" {
		prof.diskType = defaultDiskType
	}
	if sgs, ok := p["security_group_ids"].([]any); ok {
		for _, sg := range sgs {
			if s, ok := sg.(string); ok {
				prof.securityGroupIDs = append(prof.securityGroupIDs, s)
			}
		}
	}
	if v, ok := p["nat"].(bool); ok {
		prof.nat = v
	}
	prof.serviceAccountID = stringField(p, "service_account_id")
	if labels, ok := p["labels"].(map[string]any); ok {
		for k, v := range labels {
			if s, ok := v.(string); ok {
				prof.labels[k] = s
			}
		}
	}
	prof.runLabel = prof.labels[runLabelKey]
	return prof
}

func intField(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

// Create: fail closed without identity (no name and no run-label, otherwise a
// rerun creates orphan VMs, NIM-16) -> idempotency scan of live VMs by
// soulstack-run label -> create missing VMs with the first free indexes ->
// wait-until-ready -> final event with VmInfo (fqdn = YC internal DNS). name
// without label becomes run-label (label stamp). See the package doc comment.
func (y *YcDriver) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	ctx := stream.Context()
	count := req.GetCount()
	if count <= 0 {
		count = 1
	}
	prof := parseProfile(req.GetProfile().AsMap())
	creds := credsFromMap(req.GetCredentials().AsMap())
	// folder_id/zone in credentials take priority; if empty, take them from profile.
	if creds.FolderID == "" {
		creds.FolderID = prof.folderID
	}
	if creds.Zone == "" {
		creds.Zone = prof.zone
	}
	// Effective request values: profile can override credentials and vice versa;
	// both sides are valid, so choose the non-empty value.
	folderID := firstNonEmpty(prof.folderID, creds.FolderID)
	zone := firstNonEmpty(prof.zone, creds.Zone)

	// Fail closed: without name and run-label the run is indistinguishable from
	// previous runs, so idempotency scan is impossible and rerun would create
	// orphan VMs (NIM-16).
	nameBase := req.GetName()
	if nameBase == "" && prof.runLabel == "" {
		return sendCreateFailed(stream, clouddriver.FailInvalidParams, "identity",
			fmt.Errorf("no run identity: set step param `name` or profile.labels[%q]", runLabelKey))
	}
	// name without an explicit label becomes run-label: created VMs always carry
	// the soulstack-run label, and future runs match by it.
	if prof.runLabel == "" {
		prof.runLabel = nameBase
	}

	cli, err := newYcClient(ctx, creds)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.FailAuth, "yc-client", err)
	}

	backoff := defaultBackoff()

	// Idempotency (NIM-16): always scan live VMs in the batch by soulstack-run
	// label (after the guard runLabel is non-empty). If live VMs exist, reuse
	// them and create only the missing ones.
	existing, err := y.findByRunLabel(ctx, cli, backoff, folderID, prof.runLabel)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.Classify(classifyYC, err), "list-existing", err)
	}
	if int32(len(existing)) >= count {
		_ = stream.Send(&pluginv1.CreateEvent{
			Message: fmt.Sprintf("idempotent: %d VM already present for run %q", len(existing), prof.runLabel),
		})
		return y.finalizeCreate(ctx, cli, stream, backoff, instanceIDs(existing))
	}

	need := count - int32(len(existing))
	names := gapFillNames(existing, nameBase, prof.runLabel, need)
	if len(existing) > 0 {
		_ = stream.Send(&pluginv1.CreateEvent{
			Message: fmt.Sprintf("idempotent: reusing %d existing VM for run %q, creating %d more", len(existing), prof.runLabel, need),
		})
	}

	if err := stream.Send(&pluginv1.CreateEvent{
		Message: fmt.Sprintf("yc.InstanceService.Create count=%d zone=%s platform=%s image=%s", need, zone, prof.platformID, prof.imageID),
	}); err != nil {
		return err
	}

	newInstances, err := y.createInstances(ctx, cli, backoff, prof, folderID, zone, req.GetUserdata(), names)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.Classify(classifyYC, err), "CreateInstance", err)
	}

	allIDs := append(instanceIDs(existing), instanceIDs(newInstances)...)
	return y.finalizeCreate(ctx, cli, stream, backoff, allIDs)
}

// createInstances calls CreateInstance once per name. YC creates one VM per
// call (no batch Create like EC2.RunInstances). Names arrive ready from
// gap-fill: deterministic and collision-free with existing VMs (NIM-16).
func (y *YcDriver) createInstances(ctx context.Context, cli ycAPI, backoff clouddriver.BackoffConfig, prof vmProfile, folderID, zone, userdata string, names []string) ([]*computev1.Instance, error) {
	out := make([]*computev1.Instance, 0, len(names))
	for _, name := range names {
		in := buildCreateRequest(prof, folderID, zone, userdata, name)
		var inst *computev1.Instance
		err := clouddriver.Retry(ctx, backoff, classifyYC, func() error {
			var rerr error
			inst, rerr = cli.CreateInstance(ctx, in)
			return rerr
		})
		if err != nil {
			return out, err
		}
		out = append(out, inst)
	}
	return out, nil
}

// buildCreateRequest forms CreateInstanceRequest with a ready VM name
// (deterministic `<nameBase>-<seq>` / `soul-<runLabel>-<seq>` from gap-fill,
// NIM-16). runLabel is stamped into labels[soulstack-run] as run identity.
func buildCreateRequest(prof vmProfile, folderID, zone, userdata, name string) *computev1.CreateInstanceRequest {
	metadata := map[string]string{}
	if userdata != "" {
		metadata[userdataMetaKey] = userdata
	}
	labels := make(map[string]string, len(prof.labels))
	for k, v := range prof.labels {
		labels[k] = v
	}
	if prof.runLabel != "" {
		labels[runLabelKey] = prof.runLabel
	}

	natSpec := (*computev1.OneToOneNatSpec)(nil)
	if prof.nat {
		natSpec = &computev1.OneToOneNatSpec{IpVersion: computev1.IpVersion_IPV4}
	}

	resources := &computev1.ResourcesSpec{
		Cores:  prof.cores,
		Memory: prof.memoryBytes,
	}
	if prof.coreFraction > 0 {
		resources.CoreFraction = prof.coreFraction
	}

	req := &computev1.CreateInstanceRequest{
		FolderId:      folderID,
		Name:          name,
		ZoneId:        zone,
		PlatformId:    prof.platformID,
		Labels:        labels,
		Metadata:      metadata,
		ResourcesSpec: resources,
		BootDiskSpec: &computev1.AttachedDiskSpec{
			AutoDelete: true,
			Disk: &computev1.AttachedDiskSpec_DiskSpec_{
				DiskSpec: &computev1.AttachedDiskSpec_DiskSpec{
					TypeId: prof.diskType,
					Size:   prof.diskSizeBytes,
					Source: &computev1.AttachedDiskSpec_DiskSpec_ImageId{ImageId: prof.imageID},
				},
			},
		},
		NetworkInterfaceSpecs: []*computev1.NetworkInterfaceSpec{{
			SubnetId: prof.subnetID,
			PrimaryV4AddressSpec: &computev1.PrimaryAddressSpec{
				OneToOneNatSpec: natSpec,
			},
			SecurityGroupIds: prof.securityGroupIDs,
		}},
	}
	if prof.serviceAccountID != "" {
		req.ServiceAccountId = prof.serviceAccountID
	}
	return req
}

// vmName is the deterministic name of the i-th VM in the batch (NIM-16, pure
// function with no time component; otherwise idempotency scan would not find its
// VMs): nameBase (CreateRequest.name, self-onboard "Variant T" ADR-017(h)) ->
// `<nameBase>-<seq>` (Keeper predicted FQDN and baked a per-VM token under it,
// so the name MUST match); otherwise `soul-<runLabel>-<seq>`. Validity of
// nameBase against YC name constraint `[a-z][-a-z0-9]{1,62}` is the
// Keeper-side responsibility.
func vmName(nameBase, runLabel string, seq int32) string {
	if nameBase != "" {
		return fmt.Sprintf("%s-%d", nameBase, seq)
	}
	return fmt.Sprintf("soul-%s-%d", runLabel, seq)
}

// gapFillNames returns need names for missing VMs by taking the first free
// indexes seq=0,1,2,... (occupied = existing names), so on partial rerun new
// VMs do not collide by name with existing ones (NIM-16). Occupancy is computed
// from live existing VMs: a DELETING VM index may be reused, and YC will respond
// AlreadyExists loudly and without orphans.
func gapFillNames(existing []*computev1.Instance, nameBase, runLabel string, need int32) []string {
	occupied := make(map[string]bool, len(existing))
	for _, inst := range existing {
		occupied[inst.GetName()] = true
	}
	names := make([]string, 0, need)
	for seq := int32(0); int32(len(names)) < need; seq++ {
		n := vmName(nameBase, runLabel, seq)
		if occupied[n] {
			continue
		}
		names = append(names, n)
	}
	return names
}

// finalizeCreate waits for VM readiness (RUNNING + FQDN/IP) and sends the final
// event. Anti-orphan: on ctx-cancel/timeout, unfinished VMs are included in the
// final event with failed=true but populated vm_id, so Keeper can Destroy them.
func (y *YcDriver) finalizeCreate(ctx context.Context, cli ycAPI, stream grpc.ServerStreamingServer[pluginv1.CreateEvent], backoff clouddriver.BackoffConfig, vmIDs []string) error {
	probe := func(pctx context.Context, vmID string) clouddriver.ProbeResult {
		inst, perr := cli.GetInstance(pctx, vmID)
		if perr != nil {
			if clouddriver.Classify(classifyYC, perr).Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{Err: perr}
		}
		switch inst.GetStatus() {
		case computev1.Instance_RUNNING:
			if inst.GetFqdn() != "" || primaryIP(inst) != "" {
				return clouddriver.ProbeResult{Ready: true}
			}
			return clouddriver.ProbeResult{}
		case computev1.Instance_STOPPING, computev1.Instance_STOPPED,
			computev1.Instance_ERROR, computev1.Instance_CRASHED, computev1.Instance_DELETING:
			return clouddriver.ProbeResult{Err: fmt.Errorf("instance %s entered terminal state %q", vmID, inst.GetStatus())}
		default:
			return clouddriver.ProbeResult{}
		}
	}

	waitResults, waitErr := clouddriver.WaitUntilReady(ctx, backoff, vmIDs, probe,
		func(msg string) { _ = stream.Send(&pluginv1.CreateEvent{Message: msg}) })

	vms := make([]*pluginv1.VmInfo, 0, len(vmIDs))
	anyFailed := false
	for _, wr := range waitResults {
		vi := &pluginv1.VmInfo{VmId: wr.VMID}
		if wr.Ready {
			if inst, derr := cli.GetInstance(ctx, wr.VMID); derr == nil {
				vi.Fqdn = inst.GetFqdn()
				vi.PrimaryIp = primaryIP(inst)
				vi.Attributes = instanceAttributes(inst)
			}
		}
		if !wr.Ready {
			anyFailed = true
		}
		vms = append(vms, vi)
	}

	if waitErr != nil {
		// ctx-cancel / deadline: the final event carries vm_id for all created VMs
		// with failed=true (anti-orphan), so Keeper sees instance-id and can Destroy.
		return stream.Send(&pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyYC, waitErr), "wait-until-ready", waitErr),
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

// findByRunLabel lists live VMs (not STOPPED/ERROR/CRASHED/DELETING) with the
// given runLabel in the folder (NIM-16: always scan because identity is
// guaranteed by the guard in Create). YC ListInstances supports a simple filter
// DSL `labels.<key>="<value>"`; status is filtered afterward so we do not depend
// on DSL syntax variations across API versions. Unlike the wb driver, there is
// no name-match branch here (adopting pre-fix unlabeled VMs by name): anon names
// were random (time-seed), there is no deterministic match pattern, and there
// were no live runs.
func (y *YcDriver) findByRunLabel(ctx context.Context, cli ycAPI, backoff clouddriver.BackoffConfig, folderID, runLabel string) ([]*computev1.Instance, error) {
	in := &computev1.ListInstancesRequest{
		FolderId: folderID,
		Filter:   fmt.Sprintf(`labels.%s="%s"`, runLabelKey, runLabel),
		PageSize: 1000,
	}
	var out *computev1.ListInstancesResponse
	err := clouddriver.Retry(ctx, backoff, classifyYC, func() error {
		var rerr error
		out, rerr = cli.ListInstances(ctx, in)
		return rerr
	})
	if err != nil {
		return nil, err
	}
	live := make([]*computev1.Instance, 0, len(out.GetInstances()))
	for _, inst := range out.GetInstances() {
		if isLiveStatus(inst.GetStatus()) {
			live = append(live, inst)
		}
	}
	return live, nil
}

func isLiveStatus(s computev1.Instance_Status) bool {
	switch s {
	case computev1.Instance_PROVISIONING, computev1.Instance_STARTING,
		computev1.Instance_RUNNING, computev1.Instance_RESTARTING,
		computev1.Instance_UPDATING:
		return true
	}
	return false
}

// Destroy: per-VM DeleteInstance, stream per-VM events. YC DeleteInstance is not
// batched (unlike Salt-style API), so iterate over the list; for not_found,
// return success (destroy idempotency).
func (y *YcDriver) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newYcClient(ctx, creds)
	if err != nil {
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(clouddriver.FailAuth, "yc-client", err), Failed: true})
		return nil
	}

	backoff := defaultBackoff()
	for _, id := range req.GetVmIds() {
		vmID := id
		err := clouddriver.Retry(ctx, backoff, classifyYC, func() error {
			return cli.DeleteInstance(ctx, vmID)
		})
		if err == nil {
			_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmID, Message: "deleted"})
			continue
		}
		class := clouddriver.Classify(classifyYC, err)
		if class == clouddriver.FailNotFound {
			// not_found on Destroy is idempotent success (the VM is already gone).
			_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmID, Message: "already absent"})
			continue
		}
		_ = stream.Send(&pluginv1.DestroyEvent{
			VmId:    vmID,
			Message: clouddriver.FailMessage(class, "DeleteInstance", err),
			Failed:  true,
		})
	}
	return nil
}

// Status polls one VM (GetInstance). credentials arrive in the separate
// StatusRequest.credentials field (A-flow, symmetrical with Create/Destroy).
func (y *YcDriver) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newYcClient(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("yc-client: %w", err)
	}
	inst, err := cli.GetInstance(ctx, req.GetVmId())
	if err != nil {
		return nil, fmt.Errorf("GetInstance %s: %w", req.GetVmId(), err)
	}
	return &pluginv1.StatusReply{
		State:      inst.GetStatus().String(),
		Attributes: instanceAttributes(inst),
	}, nil
}

// List streams VM inventory in a folder (optionally filtered by runLabel).
// credentials arrive in the separate ListRequest.credentials field (A-flow,
// symmetrical with Create/Destroy/Status).
func (y *YcDriver) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newYcClient(ctx, creds)
	if err != nil {
		return fmt.Errorf("yc-client: %w", err)
	}
	filter := req.GetFilter().AsMap()
	folderID := firstNonEmpty(stringField(filter, "folder_id"), creds.FolderID)
	if folderID == "" {
		return fmt.Errorf("folder_id is required (in filter or credentials)")
	}
	in := &computev1.ListInstancesRequest{FolderId: folderID, PageSize: 1000}
	if runLabel := stringField(filter, runLabelKey); runLabel != "" {
		in.Filter = fmt.Sprintf(`labels.%s="%s"`, runLabelKey, runLabel)
	}
	out, err := cli.ListInstances(ctx, in)
	if err != nil {
		return fmt.Errorf("ListInstances: %w", err)
	}
	for _, inst := range out.GetInstances() {
		if serr := stream.Send(&pluginv1.VmInfo{
			VmId:       inst.GetId(),
			Fqdn:       inst.GetFqdn(),
			PrimaryIp:  primaryIP(inst),
			Attributes: instanceAttributes(inst),
		}); serr != nil {
			return serr
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&YcDriver{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-yc:", err)
		os.Exit(1)
	}
}

// --- helpers ---

func sendCreateFailed(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error) error {
	return stream.Send(&pluginv1.CreateEvent{Message: clouddriver.FailMessage(class, op, err), Failed: true})
}

func instanceIDs(insts []*computev1.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, i := range insts {
		out = append(out, i.GetId())
	}
	return out
}

// primaryIP is the private IP of the first network interface; if NAT (public
// address) exists, it is used as fallback so the Soul-host is reachable from
// outside the VPC for that profile.
func primaryIP(inst *computev1.Instance) string {
	for _, ni := range inst.GetNetworkInterfaces() {
		if pa := ni.GetPrimaryV4Address(); pa != nil {
			if pa.GetAddress() != "" {
				return pa.GetAddress()
			}
			if nat := pa.GetOneToOneNat(); nat != nil && nat.GetAddress() != "" {
				return nat.GetAddress()
			}
		}
	}
	return ""
}

func instanceAttributes(inst *computev1.Instance) *structpb.Struct {
	m := map[string]any{
		"platform_id": inst.GetPlatformId(),
		"zone":        inst.GetZoneId(),
	}
	if ca := inst.GetCreatedAt(); ca != nil {
		m["created_at"] = ca.AsTime().UTC().Format(time.RFC3339)
	}
	if res := inst.GetResources(); res != nil {
		m["cores"] = res.GetCores()
		m["memory_bytes"] = res.GetMemory()
		if res.GetCoreFraction() > 0 {
			m["core_fraction"] = res.GetCoreFraction()
		}
	}
	// Public IP as a separate attribute for transparency (it may already have
	// landed in primary_ip as fallback).
	for _, ni := range inst.GetNetworkInterfaces() {
		if pa := ni.GetPrimaryV4Address(); pa != nil {
			if nat := pa.GetOneToOneNat(); nat != nil && nat.GetAddress() != "" {
				m["public_ip"] = nat.GetAddress()
				break
			}
		}
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
