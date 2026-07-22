// soul-cloud-aws is a real CloudDriver plugin for Soul Stack on AWS EC2
// (ADR-016 Phase 4 cloud parity; rollout pilot per docs/keeper/plugins.md).
//
// Builds into a static binary `soul-cloud-aws`. The Keeper-side module
// `core.cloud.provisioned` (ADR-017) runs it as a sub-process, performs
// the gRPC-stdio handshake (sdk/handshake), and calls CloudDriver RPC.
//
// Credentials (A-flow, docs/keeper/cloud.md): Keeper resolves the secret from Vault
// and puts plain values into CreateRequest.credentials / DestroyRequest.credentials;
// the driver does NOT call Vault. Cloud-init userdata for Soul agent bootstrap arrives in
// CreateRequest.userdata.
//
// Shared framework (error taxonomy / wait-until-ready / retry-backoff) comes from
// sdk/clouddriver and is common to all rollout drivers. Provider-specific code here is
// limited to EC2 API calls and the AWS error classifier (classify.go).
package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// profileSchemaJSON is profile_schema (JSON Schema draft 2020-12), embedded
// next to the binary. Rollout approach: schema is a separate file, not hardcoded in Go
// (easier to keep in sync with manifest.spec.profile_schema).
//
//go:embed schema.json
var profileSchemaJSON []byte

// runTagKey is the idempotency tag: value = run/incarnation identifier
// (from profile.tags). Repeated Create with the same tag does not create duplicates —
// existing running/pending VMs are reused.
const runTagKey = "soulstack:run"

// defaultBackoff is the [clouddriver.BackoffConfig] factory for wait/retry phases.
// Kept in a variable so L0 tests can replace it (small MaxAttempts,
// short delays) without waiting through 1s→2s→4s timers. Same technique as
// `newEC2Client` (see ec2api.go).
var defaultBackoff = clouddriver.DefaultBackoff

// AwsDriver is the CloudDriver implementation for AWS EC2.
type AwsDriver struct {
	clouddriver.BaseDriver
}

// Schema publishes the embedded profile_schema.
func (a *AwsDriver) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
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
func (a *AwsDriver) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	p := req.GetProfile().AsMap()
	var errs []string
	if stringField(p, "region") == "" {
		errs = append(errs, "profile.region is required")
	}
	if stringField(p, "ami") == "" {
		errs = append(errs, "profile.ami is required")
	}
	if stringField(p, "instance_type") == "" {
		errs = append(errs, "profile.instance_type is required")
	}
	return &pluginv1.ValidateProfileReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// vmProfile contains profile parameters parsed for RunInstances.
type vmProfile struct {
	region     string
	ami        string
	instType   string
	subnet     string
	secGroups  []string
	diskSizeGB int32
	diskType   string
	tags       map[string]string
	runTag     string
}

func parseProfile(p map[string]any) vmProfile {
	prof := vmProfile{
		region:   stringField(p, "region"),
		ami:      stringField(p, "ami"),
		instType: stringField(p, "instance_type"),
		tags:     map[string]string{},
	}
	if net, ok := p["network"].(map[string]any); ok {
		prof.subnet = stringField(net, "subnet")
		if sgs, ok := net["security_groups"].([]any); ok {
			for _, sg := range sgs {
				if s, ok := sg.(string); ok {
					prof.secGroups = append(prof.secGroups, s)
				}
			}
		}
	}
	if disk, ok := p["disk"].(map[string]any); ok {
		if sz, ok := disk["size_gb"].(float64); ok {
			prof.diskSizeGB = int32(sz)
		}
		prof.diskType = stringField(disk, "type")
	}
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

// Create: idempotency check by tag → RunInstances → wait-until-ready →
// final event with VmInfo (fqdn=SID). See the package doc comment.
func (a *AwsDriver) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	ctx := stream.Context()
	count := req.GetCount()
	if count <= 0 {
		count = 1
	}
	prof := parseProfile(req.GetProfile().AsMap())
	creds := credsFromMap(req.GetCredentials().AsMap())
	if creds.Region == "" {
		creds.Region = prof.region
	}

	cli, err := newEC2Client(ctx, creds)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.FailAuth, "ec2-client", err)
	}

	backoff := defaultBackoff()

	// Idempotency: if live VMs already exist for runTag, reuse them
	// and only create the missing count. Without runTag, no idempotency check is possible
	// (nothing to correlate the run with).
	var existing []ec2types.Instance
	if prof.runTag != "" {
		existing, err = a.findByRunTag(ctx, cli, backoff, prof.runTag)
		if err != nil {
			return sendCreateFailed(stream, clouddriver.Classify(classifyAWS, err), "describe-existing", err)
		}
		if int32(len(existing)) >= count {
			_ = stream.Send(&pluginv1.CreateEvent{
				Message: fmt.Sprintf("idempotent: %d VM already present for run %q", len(existing), prof.runTag),
			})
			return a.finalizeCreate(ctx, cli, stream, backoff, instanceIDs(existing))
		}
		count -= int32(len(existing))
	}

	if err := stream.Send(&pluginv1.CreateEvent{
		Message: fmt.Sprintf("ec2.RunInstances count=%d type=%s ami=%s", count, prof.instType, prof.ami),
	}); err != nil {
		return err
	}

	runOut, err := a.runInstances(ctx, cli, backoff, prof, req.GetUserdata(), count)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.Classify(classifyAWS, err), "RunInstances", err)
	}

	newIDs := make([]string, 0, len(runOut.Instances))
	for _, inst := range runOut.Instances {
		newIDs = append(newIDs, aws.ToString(inst.InstanceId))
	}
	allIDs := append(instanceIDs(existing), newIDs...)
	return a.finalizeCreate(ctx, cli, stream, backoff, allIDs)
}

// finalizeCreate waits for VM readiness (running + IP/DNS) and sends the final event.
// Anti-orphan: on ctx-cancel/timeout, unfinished VMs still appear in the final
// event with failed=true and vm_id filled, so Keeper can Destroy them.
func (a *AwsDriver) finalizeCreate(ctx context.Context, cli ec2API, stream grpc.ServerStreamingServer[pluginv1.CreateEvent], backoff clouddriver.BackoffConfig, vmIDs []string) error {
	probe := func(pctx context.Context, vmID string) clouddriver.ProbeResult {
		inst, perr := a.describeOne(pctx, cli, vmID)
		if perr != nil {
			// Swallow transient probe errors; the poller will retry.
			if clouddriver.Classify(classifyAWS, perr).Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{Err: perr}
		}
		switch inst.State.Name {
		case ec2types.InstanceStateNameRunning:
			if aws.ToString(inst.PrivateDnsName) != "" || aws.ToString(inst.PublicIpAddress) != "" || aws.ToString(inst.PrivateIpAddress) != "" {
				return clouddriver.ProbeResult{Ready: true}
			}
			return clouddriver.ProbeResult{}
		case ec2types.InstanceStateNameTerminated, ec2types.InstanceStateNameStopping, ec2types.InstanceStateNameStopped:
			return clouddriver.ProbeResult{Err: fmt.Errorf("instance %s entered terminal state %q", vmID, inst.State.Name)}
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
			if inst, derr := a.describeOne(ctx, cli, wr.VMID); derr == nil {
				vi.Fqdn = aws.ToString(inst.PrivateDnsName)
				vi.PrimaryIp = firstNonEmpty(aws.ToString(inst.PrivateIpAddress), aws.ToString(inst.PublicIpAddress))
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
		// with failed=true (anti-orphan), so Keeper sees instance-id and can Destroy.
		return stream.Send(&pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyAWS, waitErr), "wait-until-ready", waitErr),
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

// runInstances wraps ec2.RunInstances in retry/backoff (throttling-safe).
func (a *AwsDriver) runInstances(ctx context.Context, cli ec2API, backoff clouddriver.BackoffConfig, prof vmProfile, userdata string, count int32) (*ec2.RunInstancesOutput, error) {
	in := &ec2.RunInstancesInput{
		ImageId:      aws.String(prof.ami),
		InstanceType: ec2types.InstanceType(prof.instType),
		MinCount:     aws.Int32(count),
		MaxCount:     aws.Int32(count),
	}
	if prof.subnet != "" {
		in.SubnetId = aws.String(prof.subnet)
	}
	if len(prof.secGroups) > 0 {
		in.SecurityGroupIds = prof.secGroups
	}
	if userdata != "" {
		// EC2 requires base64; aws-sdk-go-v2 RunInstances does NOT encode UserData
		// (serializes it as a plain string), so we encode it ourselves.
		in.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(userdata)))
	}
	if len(prof.tags) > 0 {
		in.TagSpecifications = []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeInstance,
			Tags:         awsTags(prof.tags),
		}}
	}

	var out *ec2.RunInstancesOutput
	err := clouddriver.Retry(ctx, backoff, classifyAWS, func() error {
		var rerr error
		out, rerr = cli.RunInstances(ctx, in)
		return rerr
	})
	return out, err
}

// findByRunTag lists live (non-terminated) VMs with the given runTag.
func (a *AwsDriver) findByRunTag(ctx context.Context, cli ec2API, backoff clouddriver.BackoffConfig, runTag string) ([]ec2types.Instance, error) {
	in := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + runTagKey), Values: []string{runTag}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}},
		},
	}
	var out *ec2.DescribeInstancesOutput
	err := clouddriver.Retry(ctx, backoff, classifyAWS, func() error {
		var rerr error
		out, rerr = cli.DescribeInstances(ctx, in)
		return rerr
	})
	if err != nil {
		return nil, err
	}
	return flattenInstances(out), nil
}

// describeOne reads one VM (without retry; it is called from the poller,
// which retries across rounds itself).
func (a *AwsDriver) describeOne(ctx context.Context, cli ec2API, vmID string) (ec2types.Instance, error) {
	out, err := cli.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{vmID}})
	if err != nil {
		return ec2types.Instance{}, err
	}
	insts := flattenInstances(out)
	if len(insts) == 0 {
		return ec2types.Instance{}, fmt.Errorf("instance %s not found", vmID)
	}
	return insts[0], nil
}

// Destroy: TerminateInstances, streaming per-VM events.
func (a *AwsDriver) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newEC2Client(ctx, creds)
	if err != nil {
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(clouddriver.FailAuth, "ec2-client", err), Failed: true})
		return nil
	}

	backoff := defaultBackoff()
	out, err := func() (*ec2.TerminateInstancesOutput, error) {
		var o *ec2.TerminateInstancesOutput
		rerr := clouddriver.Retry(ctx, backoff, classifyAWS, func() error {
			var e error
			o, e = cli.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: req.GetVmIds()})
			return e
		})
		return o, rerr
	}()
	if err != nil {
		class := clouddriver.Classify(classifyAWS, err)
		// not_found on Destroy is success by definition (VM is already gone): send per-VM
		// terminated, not failed (destroy idempotency).
		if class == clouddriver.FailNotFound {
			for _, id := range req.GetVmIds() {
				_ = stream.Send(&pluginv1.DestroyEvent{VmId: id, Message: "already absent"})
			}
			return nil
		}
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(class, "TerminateInstances", err), Failed: true})
		return nil
	}

	for _, st := range out.TerminatingInstances {
		_ = stream.Send(&pluginv1.DestroyEvent{
			VmId:    aws.ToString(st.InstanceId),
			Message: fmt.Sprintf("terminating (%s)", st.CurrentState.Name),
		})
	}
	return nil
}

// Status polls one VM (DescribeInstances). credentials arrive through the separate
// StatusRequest.credentials field (A-flow, symmetric with Create/Destroy).
func (a *AwsDriver) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newEC2Client(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("ec2-client: %w", err)
	}
	inst, err := a.describeOne(ctx, cli, req.GetVmId())
	if err != nil {
		return nil, fmt.Errorf("DescribeInstances %s: %w", req.GetVmId(), err)
	}
	return &pluginv1.StatusReply{
		State:      string(inst.State.Name),
		Attributes: instanceAttributes(inst),
	}, nil
}

// List streams VM inventory by filter. credentials arrive through the separate
// ListRequest.credentials field (A-flow, symmetric with Create/Destroy/Status). Old
// "creds inside filter-Struct" workaround has been removed.
func (a *AwsDriver) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newEC2Client(ctx, creds)
	if err != nil {
		return fmt.Errorf("ec2-client: %w", err)
	}
	filter := req.GetFilter().AsMap()
	in := &ec2.DescribeInstancesInput{}
	if runTag := stringField(filter, runTagKey); runTag != "" {
		in.Filters = []ec2types.Filter{{Name: aws.String("tag:" + runTagKey), Values: []string{runTag}}}
	}
	out, err := cli.DescribeInstances(ctx, in)
	if err != nil {
		return fmt.Errorf("DescribeInstances: %w", err)
	}
	for _, inst := range flattenInstances(out) {
		if serr := stream.Send(&pluginv1.VmInfo{
			VmId:       aws.ToString(inst.InstanceId),
			Fqdn:       aws.ToString(inst.PrivateDnsName),
			PrimaryIp:  firstNonEmpty(aws.ToString(inst.PrivateIpAddress), aws.ToString(inst.PublicIpAddress)),
			Attributes: instanceAttributes(inst),
		}); serr != nil {
			return serr
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&AwsDriver{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-aws:", err)
		os.Exit(1)
	}
}

// --- helpers ---

func sendCreateFailed(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error) error {
	return stream.Send(&pluginv1.CreateEvent{Message: clouddriver.FailMessage(class, op, err), Failed: true})
}

func awsTags(tags map[string]string) []ec2types.Tag {
	out := make([]ec2types.Tag, 0, len(tags))
	for k, v := range tags {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

func flattenInstances(out *ec2.DescribeInstancesOutput) []ec2types.Instance {
	if out == nil {
		return nil
	}
	var insts []ec2types.Instance
	for _, r := range out.Reservations {
		insts = append(insts, r.Instances...)
	}
	return insts
}

func instanceIDs(insts []ec2types.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, i := range insts {
		out = append(out, aws.ToString(i.InstanceId))
	}
	return out
}

func instanceAttributes(inst ec2types.Instance) *structpb.Struct {
	m := map[string]any{
		"instance_type": string(inst.InstanceType),
		"az":            azOf(inst),
	}
	if inst.LaunchTime != nil {
		m["launch_time"] = inst.LaunchTime.UTC().Format(time.RFC3339)
	}
	if pub := aws.ToString(inst.PublicIpAddress); pub != "" {
		m["public_ip"] = pub
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

func azOf(inst ec2types.Instance) string {
	if inst.Placement != nil {
		return aws.ToString(inst.Placement.AvailabilityZone)
	}
	return ""
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
