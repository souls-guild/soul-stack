// soul-cloud-gcp is a real CloudDriver plugin for Soul Stack on Google Cloud
// Compute Engine (ADR-016 Phase 4 cloud parity; rollout per docs/keeper/plugins.md
// after the AWS pilot).
//
// Builds into a static binary `soul-cloud-gcp`. The Keeper-side module
// `core.cloud.provisioned` (ADR-017) runs it as a sub-process, performs
// the gRPC-stdio handshake (sdk/handshake), and calls CloudDriver RPC.
//
// Credentials (A-flow, docs/keeper/cloud.md): Keeper resolves the service-account
// JSON key from Vault and puts plain values into CreateRequest.credentials /
// DestroyRequest.credentials; the driver does NOT call Vault. Cloud-init userdata for
// Soul agent bootstrap arrives in CreateRequest.userdata and is passed through as
// metadata item `user-data` (GCP convention for cloud-init).
//
// Shared framework (error taxonomy / wait-until-ready / retry-backoff) comes from
// sdk/clouddriver and is common to all rollout drivers. Provider-specific code here is
// limited to Compute Engine API calls and the GCP error classifier (classify.go).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// profileSchemaJSON is profile_schema (JSON Schema draft 2020-12), embedded
// next to the binary. Approach from the AWS pilot: schema is a separate file,
// not hardcoded in Go (easier to keep in sync with manifest.spec.profile_schema).
//
//go:embed schema.json
var profileSchemaJSON []byte

// runLabelKey is the idempotency label: value = run/incarnation identifier
// (from profile.labels). Repeated Create with the same label does not create
// duplicates — existing live VMs are reused. GCP labels are limited to
// `[a-z0-9_-]{0,63}` (slash is not allowed, unlike AWS tags), so the key is
// `soulstack_run` without colon/slash.
const runLabelKey = "soulstack_run"

// userDataMetadataKey is the standard metadata-item name that cloud-init
// reads on GCP VMs (https://cloudinit.readthedocs.io → DataSourceGCE).
const userDataMetadataKey = "user-data"

// defaultBackoff is the [clouddriver.BackoffConfig] factory for wait/retry phases.
// Kept in a variable so L0 tests can replace it (small MaxAttempts,
// short delays) without waiting through 1s→2s→4s timers. Same technique as
// `newGcpInstancesClient` (see gcpapi.go).
var defaultBackoff = clouddriver.DefaultBackoff

// GcpDriver is the CloudDriver implementation for Google Compute Engine.
type GcpDriver struct {
	clouddriver.BaseDriver
}

// Schema publishes the embedded profile_schema.
func (g *GcpDriver) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
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

// Validate does not carry credentials (ValidateProfileRequest), so only structural checks
// happen here; auth is checked during Create.
func (g *GcpDriver) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	p := req.GetProfile().AsMap()
	var errs []string
	if stringField(p, "project") == "" {
		errs = append(errs, "profile.project is required")
	}
	if stringField(p, "zone") == "" {
		errs = append(errs, "profile.zone is required")
	}
	if stringField(p, "machine_type") == "" {
		errs = append(errs, "profile.machine_type is required")
	}
	if stringField(p, "source_image") == "" {
		errs = append(errs, "profile.source_image is required")
	}
	return &pluginv1.ValidateProfileReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// vmProfile contains profile parameters parsed for Insert.
type vmProfile struct {
	project     string
	zone        string
	machineType string
	sourceImage string
	network     string
	subnet      string
	diskSizeGB  int64
	diskType    string
	labels      map[string]string
	runTag      string
}

func parseProfile(p map[string]any) vmProfile {
	prof := vmProfile{
		project:     stringField(p, "project"),
		zone:        stringField(p, "zone"),
		machineType: stringField(p, "machine_type"),
		sourceImage: stringField(p, "source_image"),
		labels:      map[string]string{},
	}
	if net, ok := p["network"].(map[string]any); ok {
		prof.network = stringField(net, "network")
		prof.subnet = stringField(net, "subnet")
	}
	if disk, ok := p["disk"].(map[string]any); ok {
		if sz, ok := disk["size_gb"].(float64); ok {
			prof.diskSizeGB = int64(sz)
		}
		prof.diskType = stringField(disk, "type")
	}
	if labels, ok := p["labels"].(map[string]any); ok {
		for k, v := range labels {
			if s, ok := v.(string); ok {
				prof.labels[k] = s
			}
		}
	}
	prof.runTag = prof.labels[runLabelKey]
	return prof
}

// Create: idempotency check by label → Insert N VMs → wait-until-ready →
// final event with VmInfo (fqdn=GCP internal DNS, primary identifier).
// See the package doc comment.
func (g *GcpDriver) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	ctx := stream.Context()
	count := req.GetCount()
	if count <= 0 {
		count = 1
	}
	prof := parseProfile(req.GetProfile().AsMap())
	creds := credsFromMap(req.GetCredentials().AsMap())
	// project/zone may come from profile or credentials; credentials have
	// priority (same model as AWS region).
	if creds.Project == "" {
		creds.Project = prof.project
	}
	if creds.Zone == "" {
		creds.Zone = prof.zone
	}
	// Replace profile project/zone as well (Insert takes them from profile).
	if prof.project == "" {
		prof.project = creds.Project
	}
	if prof.zone == "" {
		prof.zone = creds.Zone
	}

	cli, err := newGcpInstancesClient(ctx, creds)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.FailAuth, "compute-client", err)
	}

	backoff := defaultBackoff()

	// Idempotency: if live VMs already exist for runTag, reuse them
	// and only create the missing count. Without runTag, no idempotency check is possible
	// (nothing to correlate the run with).
	var existing []*computepb.Instance
	if prof.runTag != "" {
		existing, err = g.findByRunLabel(ctx, cli, backoff, prof, prof.runTag)
		if err != nil {
			return sendCreateFailed(stream, clouddriver.Classify(classifyGCP, err), "list-existing", err)
		}
		if int32(len(existing)) >= count {
			_ = stream.Send(&pluginv1.CreateEvent{
				Message: fmt.Sprintf("idempotent: %d VM already present for run %q", len(existing), prof.runTag),
			})
			return g.finalizeCreate(ctx, cli, stream, backoff, prof, instanceNames(existing))
		}
		count -= int32(len(existing))
	}

	if err := stream.Send(&pluginv1.CreateEvent{
		Message: fmt.Sprintf("compute.Insert count=%d type=%s image=%s zone=%s", count, prof.machineType, prof.sourceImage, prof.zone),
	}); err != nil {
		return err
	}

	// New VM names: deterministic from runTag (for safe retry logic),
	// or timestamp-based if runTag is empty. Existing VMs continue their
	// index sequence (offset = len(existing)).
	offset := int32(len(existing))
	newNames := generateVMNames(prof.runTag, offset, count)
	for _, name := range newNames {
		if perr := g.insertOne(ctx, cli, backoff, prof, req.GetUserdata(), name); perr != nil {
			return sendCreateFailed(stream, clouddriver.Classify(classifyGCP, perr), "Insert", perr)
		}
	}

	allNames := append(instanceNames(existing), newNames...)
	return g.finalizeCreate(ctx, cli, stream, backoff, prof, allNames)
}

// finalizeCreate waits for VM readiness (RUNNING + internal IP) and sends the final
// event. Anti-orphan: on ctx-cancel/timeout, unfinished VMs appear in the
// final event with failed=true and vm_id filled, so Keeper can
// Destroy.
func (g *GcpDriver) finalizeCreate(ctx context.Context, cli gcpInstancesAPI, stream grpc.ServerStreamingServer[pluginv1.CreateEvent], backoff clouddriver.BackoffConfig, prof vmProfile, vmNames []string) error {
	probe := func(pctx context.Context, name string) clouddriver.ProbeResult {
		inst, perr := cli.Get(pctx, &computepb.GetInstanceRequest{
			Project:  prof.project,
			Zone:     prof.zone,
			Instance: name,
		})
		if perr != nil {
			// Swallow transient probe errors; the poller will retry.
			if clouddriver.Classify(classifyGCP, perr).Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{Err: perr}
		}
		status := inst.GetStatus()
		switch status {
		case "RUNNING":
			if primaryIP(inst) != "" {
				return clouddriver.ProbeResult{Ready: true}
			}
			return clouddriver.ProbeResult{}
		case "TERMINATED", "STOPPING", "STOPPED", "SUSPENDED", "SUSPENDING":
			return clouddriver.ProbeResult{Err: fmt.Errorf("instance %s entered terminal state %q", name, status)}
		default:
			// PROVISIONING / STAGING / REPAIRING — keep waiting.
			return clouddriver.ProbeResult{}
		}
	}

	waitResults, waitErr := clouddriver.WaitUntilReady(ctx, backoff, vmNames, probe,
		func(msg string) { _ = stream.Send(&pluginv1.CreateEvent{Message: msg}) })

	vms := make([]*pluginv1.VmInfo, 0, len(vmNames))
	anyFailed := false
	for _, wr := range waitResults {
		vi := &pluginv1.VmInfo{VmId: wr.VMID}
		if wr.Ready {
			if inst, derr := cli.Get(ctx, &computepb.GetInstanceRequest{
				Project: prof.project, Zone: prof.zone, Instance: wr.VMID,
			}); derr == nil {
				vi.Fqdn = internalFQDN(wr.VMID, prof.zone, prof.project)
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
		// ctx-cancel / deadline: final event carries vm_id for all created VMs
		// with failed=true (anti-orphan), so Keeper sees instance-name and can Destroy.
		return stream.Send(&pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyGCP, waitErr), "wait-until-ready", waitErr),
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

// insertOne builds Instance resource and calls Insert + Operation.Wait for
// one VM. Insert is wrapped in retry/backoff (throttling-safe).
func (g *GcpDriver) insertOne(ctx context.Context, cli gcpInstancesAPI, backoff clouddriver.BackoffConfig, prof vmProfile, userdata, name string) error {
	inst := buildInstance(prof, userdata, name)
	in := &computepb.InsertInstanceRequest{
		Project:          prof.project,
		Zone:             prof.zone,
		InstanceResource: inst,
	}
	var op gcpOperation
	if err := clouddriver.Retry(ctx, backoff, classifyGCP, func() error {
		var rerr error
		op, rerr = cli.Insert(ctx, in)
		return rerr
	}); err != nil {
		return err
	}
	// Wait blocks until DONE; Operation error is returned as googleapi.Error.
	return op.Wait(ctx)
}

// findByRunLabel lists live (non-terminated) VMs with the given runTag.
// GCP filter syntax: `labels.<k>=<v> AND (status="RUNNING" OR status="PROVISIONING"…)`.
func (g *GcpDriver) findByRunLabel(ctx context.Context, cli gcpInstancesAPI, backoff clouddriver.BackoffConfig, prof vmProfile, runTag string) ([]*computepb.Instance, error) {
	filter := fmt.Sprintf(`labels.%s=%s`, runLabelKey, runTag)
	in := &computepb.ListInstancesRequest{
		Project: prof.project,
		Zone:    prof.zone,
		Filter:  proto.String(filter),
	}
	var out []*computepb.Instance
	err := clouddriver.Retry(ctx, backoff, classifyGCP, func() error {
		var rerr error
		out, rerr = cli.List(ctx, in)
		return rerr
	})
	if err != nil {
		return nil, err
	}
	// Filter terminal VMs client-side (GCP filter syntax for OR
	// is bulky; filtering after List is cheaper).
	alive := out[:0]
	for _, inst := range out {
		switch inst.GetStatus() {
		case "TERMINATED", "STOPPING", "STOPPED", "SUSPENDED", "SUSPENDING":
			// skip
		default:
			alive = append(alive, inst)
		}
	}
	return alive, nil
}

// Destroy: Delete for each VM, streaming per-VM events. Idempotency: 404
// (NotFound) for an individual VM means "already gone", so send a success event.
func (g *GcpDriver) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newGcpInstancesClient(ctx, creds)
	if err != nil {
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(clouddriver.FailAuth, "compute-client", err), Failed: true})
		return nil
	}

	backoff := defaultBackoff()
	for _, name := range req.GetVmIds() {
		err := clouddriver.Retry(ctx, backoff, classifyGCP, func() error {
			op, derr := cli.Delete(ctx, &computepb.DeleteInstanceRequest{
				Project:  creds.Project,
				Zone:     creds.Zone,
				Instance: name,
			})
			if derr != nil {
				return derr
			}
			return op.Wait(ctx)
		})
		if err != nil {
			class := clouddriver.Classify(classifyGCP, err)
			if class == clouddriver.FailNotFound {
				// not_found on Destroy is success by definition (idempotency).
				_ = stream.Send(&pluginv1.DestroyEvent{VmId: name, Message: "already absent"})
				continue
			}
			_ = stream.Send(&pluginv1.DestroyEvent{
				VmId:    name,
				Message: clouddriver.FailMessage(class, "Delete", err),
				Failed:  true,
			})
			continue
		}
		_ = stream.Send(&pluginv1.DestroyEvent{VmId: name, Message: "terminated"})
	}
	return nil
}

// Status polls one VM (Get). credentials arrive through the separate
// StatusRequest.credentials field (A-flow, symmetric with Create/Destroy).
func (g *GcpDriver) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newGcpInstancesClient(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("compute-client: %w", err)
	}
	inst, err := cli.Get(ctx, &computepb.GetInstanceRequest{
		Project: creds.Project, Zone: creds.Zone, Instance: req.GetVmId(),
	})
	if err != nil {
		return nil, fmt.Errorf("Get %s: %w", req.GetVmId(), err)
	}
	return &pluginv1.StatusReply{
		State:      inst.GetStatus(),
		Attributes: instanceAttributes(inst),
	}, nil
}

// List streams VM inventory by filter. credentials arrive through the separate
// ListRequest.credentials field (A-flow, symmetric with Create/Destroy/Status). Filter
// field `soulstack_run` is promoted to GCP filter `labels.soulstack_run=<v>`.
func (g *GcpDriver) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newGcpInstancesClient(ctx, creds)
	if err != nil {
		return fmt.Errorf("compute-client: %w", err)
	}
	filter := req.GetFilter().AsMap()
	in := &computepb.ListInstancesRequest{
		Project: creds.Project,
		Zone:    creds.Zone,
	}
	if runTag := stringField(filter, runLabelKey); runTag != "" {
		in.Filter = proto.String(fmt.Sprintf(`labels.%s=%s`, runLabelKey, runTag))
	}
	insts, err := cli.List(ctx, in)
	if err != nil {
		return fmt.Errorf("List: %w", err)
	}
	for _, inst := range insts {
		name := inst.GetName()
		if serr := stream.Send(&pluginv1.VmInfo{
			VmId:       name,
			Fqdn:       internalFQDN(name, creds.Zone, creds.Project),
			PrimaryIp:  primaryIP(inst),
			Attributes: instanceAttributes(inst),
		}); serr != nil {
			return serr
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&GcpDriver{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-gcp:", err)
		os.Exit(1)
	}
}

// --- helpers ---

func sendCreateFailed(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error) error {
	return stream.Send(&pluginv1.CreateEvent{Message: clouddriver.FailMessage(class, op, err), Failed: true})
}

// buildInstance builds Instance resource from profile + userdata. machineType
// and sourceImage may arrive in short or full form; for machineType
// GCP API requires full path `zones/<zone>/machineTypes/<type>`, so normalize
// here.
func buildInstance(prof vmProfile, userdata, name string) *computepb.Instance {
	mt := prof.machineType
	if !strings.Contains(mt, "/") {
		mt = fmt.Sprintf("zones/%s/machineTypes/%s", prof.zone, mt)
	}
	disk := &computepb.AttachedDisk{
		Boot:       proto.Bool(true),
		AutoDelete: proto.Bool(true),
		InitializeParams: &computepb.AttachedDiskInitializeParams{
			SourceImage: proto.String(prof.sourceImage),
		},
	}
	if prof.diskSizeGB > 0 {
		disk.InitializeParams.DiskSizeGb = proto.Int64(prof.diskSizeGB)
	}
	if prof.diskType != "" {
		disk.InitializeParams.DiskType = proto.String(fmt.Sprintf("zones/%s/diskTypes/%s", prof.zone, prof.diskType))
	}
	network := prof.network
	if network == "" {
		network = "global/networks/default"
	} else if !strings.Contains(network, "/") {
		network = "global/networks/" + network
	}
	ni := &computepb.NetworkInterface{Network: proto.String(network)}
	if prof.subnet != "" {
		sn := prof.subnet
		if !strings.Contains(sn, "/") {
			// Subnet is regional, but zone-region is strictly deterministic: regional
			// part = zone without suffix `-<letter>`.
			region := zoneRegion(prof.zone)
			sn = fmt.Sprintf("regions/%s/subnetworks/%s", region, sn)
		}
		ni.Subnetwork = proto.String(sn)
	}
	inst := &computepb.Instance{
		Name:              proto.String(name),
		MachineType:       proto.String(mt),
		Disks:             []*computepb.AttachedDisk{disk},
		NetworkInterfaces: []*computepb.NetworkInterface{ni},
	}
	if len(prof.labels) > 0 {
		inst.Labels = make(map[string]string, len(prof.labels))
		for k, v := range prof.labels {
			inst.Labels[k] = v
		}
	}
	// cloud-init userdata: GCP convention is metadata-item with key `user-data`
	// (cloud-init DataSourceGCE). NOT base64 (unlike EC2): GCP passes
	// metadata as a plain string.
	if userdata != "" {
		inst.Metadata = &computepb.Metadata{
			Items: []*computepb.Items{{
				Key:   proto.String(userDataMetadataKey),
				Value: proto.String(userdata),
			}},
		}
	}
	return inst
}

// generateVMNames creates deterministic names for VMs in one run. On retry
// after Keeper crash, names match previous ones and findByRunLabel
// finds existing VMs. If runTag is empty, use timestamp (one-shot
// run without idempotency guarantee).
//
// GCP name requirements: lowercase letters/digits/hyphens, starts with a letter,
// ends with a letter/digit, max 63 characters.
func generateVMNames(runTag string, offset, count int32) []string {
	out := make([]string, 0, count)
	prefix := "soul-" + sanitizeName(runTag)
	if runTag == "" {
		prefix = fmt.Sprintf("soul-%d", time.Now().UnixNano())
	}
	for i := int32(0); i < count; i++ {
		out = append(out, fmt.Sprintf("%s-%d", prefix, offset+i))
	}
	return out
}

// sanitizeName converts arbitrary string to GCP name format: lowercase,
// hyphen-separated, max 50 characters (reserve for suffix index).
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	out := b.String()
	out = strings.Trim(out, "-")
	if len(out) > 50 {
		out = out[:50]
	}
	if out == "" {
		out = "run"
	}
	return out
}

// zoneRegion returns GCP region for a zone (europe-west1-b → europe-west1).
func zoneRegion(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// internalFQDN is the GCP internal DNS name of VM. Used as SID.
func internalFQDN(name, zone, project string) string {
	return fmt.Sprintf("%s.%s.c.%s.internal", name, zone, project)
}

// primaryIP is the first internal IP of VM (private IP of first NIC); if empty,
// use the first external IP (access-config NatIP).
func primaryIP(inst *computepb.Instance) string {
	for _, ni := range inst.NetworkInterfaces {
		if ip := ni.GetNetworkIP(); ip != "" {
			return ip
		}
	}
	for _, ni := range inst.NetworkInterfaces {
		for _, ac := range ni.AccessConfigs {
			if ip := ac.GetNatIP(); ip != "" {
				return ip
			}
		}
	}
	return ""
}

func instanceNames(insts []*computepb.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, i := range insts {
		out = append(out, i.GetName())
	}
	return out
}

func instanceAttributes(inst *computepb.Instance) *structpb.Struct {
	m := map[string]any{
		"machine_type": shortRef(inst.GetMachineType()),
		"zone":         shortRef(inst.GetZone()),
	}
	if ct := inst.GetCreationTimestamp(); ct != "" {
		m["creation_timestamp"] = ct
	}
	for _, ni := range inst.NetworkInterfaces {
		for _, ac := range ni.AccessConfigs {
			if ip := ac.GetNatIP(); ip != "" {
				m["public_ip"] = ip
				break
			}
		}
		if _, ok := m["public_ip"]; ok {
			break
		}
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

// shortRef trims GCP resource URL to its last segment (full URL form
// `https://www.googleapis.com/compute/v1/projects/p/zones/europe-west1-b` →
// `europe-west1-b`).
func shortRef(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}
