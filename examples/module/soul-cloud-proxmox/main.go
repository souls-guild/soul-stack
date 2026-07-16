// soul-cloud-proxmox is a real Soul Stack CloudDriver plugin for Proxmox VE
// (ADR-016 Phase 4 cloud parity; rollout by the soul-cloud-aws pattern).
//
// Builds into the static binary `soul-cloud-proxmox`. The Keeper-side
// `core.cloud.provisioned` module (ADR-017) starts it as a sub-process, performs
// the gRPC-stdio handshake (sdk/handshake), and calls CloudDriver RPCs.
//
// Credentials (A-flow, docs/keeper/cloud.md): Keeper resolves the secret from
// Vault and places plain values in CreateRequest.credentials /
// DestroyRequest.credentials; the driver does NOT call Vault. Proxmox supports
// two XOR forms: API-token (`<user>@<realm>!<token-id>=<value>`) or ticket-based
// (username+password+realm). Endpoint is required (https://<host>:8006). See
// pveapi.go.
//
// Shared framework (error taxonomy / wait-until-ready / retry-backoff) comes
// from sdk/clouddriver and is common to all rollout drivers. Provider-specific
// pieces here are only PVE REST API calls and the Proxmox error classifier
// (classify.go).
//
// # vm_id-paradigm: composite `<node>/<vmid>`
//
// Proxmox is NOT a cloud in the usual sense: a VM lives on a specific hypervisor
// cluster node; the same /qemu/<vmid> endpoint does not exist globally - it is
// per-node (/nodes/<node>/qemu/<vmid>). Therefore the driver serializes the proto
// VM identifier as a composite string `<node>/<vmid>`; this gives a self-contained
// ID for Destroy/Status without separate state storage on the keeper side
// (vmid->node map). Symmetrical with AWS instance-id (also a string, but without
// `/`). Parsing is splitVmID(); formatting is formatVmID(). The proto field
// VmInfo.vm_id (string) keeps its shape; operator constraint: no slashes in node
// names (Proxmox does not allow them either).
//
// # create model: clone from template, not launch image
//
// Unlike AWS (RunInstances from AMI) and YC (CreateInstance from image_id),
// Proxmox-create = `qm clone <template> <newid>`: the VM is created as a full or
// linked copy of an existing qemu template (VMID >= 100). The template already
// carries prepared cloud-init (or we extend config through SetVMConfig). Profile
// params (cores/memory/storage/bridge) are applied post-clone through /config;
// resources from the template are overwritten.
package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// profileSchemaJSON is profile_schema (JSON Schema draft 2020-12), embedded next
// to the binary. Same technique as in soul-cloud-aws.
//
//go:embed schema.json
var profileSchemaJSON []byte

// runTagKey is the idempotency tag (in Proxmox VM tags). Value = run/incarnation
// identifier (from profile.tags). Repeated Create with the same tag does not
// create duplicates - existing live VMs (running/stopped) are reused.
//
// The key name must be kebab-case for the Proxmox tag regex (`^[a-z0-9_-]+$`);
// colon is NOT allowed (unlike AWS "soulstack:run"), so use `soulstack-run`
// (without `:`). Mirrored name for YC.
const runTagKey = "soulstack-run"

// defaultBackoff is the [clouddriver.BackoffConfig] factory for wait/retry
// phases. It is a variable so L0 tests can replace it (fast MaxAttempts, short
// delays) without raising the 1s->2s->4s timer. Same technique as `newPveClient`
// (see pveapi.go).
var defaultBackoff = clouddriver.DefaultBackoff

// ProxmoxDriver implements CloudDriver for Proxmox VE.
type ProxmoxDriver struct {
	clouddriver.BaseDriver
}

// Schema publishes the embedded profile_schema.
func (d *ProxmoxDriver) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
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

// Validate carries no credentials (ValidateProfileRequest), so structural checks
// are here; auth happens in Create.
func (d *ProxmoxDriver) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	p := req.GetProfile().AsMap()
	var errs []string
	if stringField(p, "target_node") == "" {
		errs = append(errs, "profile.target_node is required")
	}
	if intField(p, "template_vmid") <= 0 {
		errs = append(errs, "profile.template_vmid is required (integer ≥ 100)")
	}
	return &pluginv1.ValidateProfileReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// vmProfile contains profile params parsed for the clone operation.
type vmProfile struct {
	targetNode   string
	templateVMID int
	newVMIDStart int
	namePrefix   string
	fullClone    bool
	cores        int
	memory       int // MB
	storage      string
	bridge       string
	tags         map[string]string
	cicustom     string
	runTag       string
}

const (
	defaultNamePrefix = "soul"
	defaultCores      = 2
	defaultMemoryMB   = 2048
)

func parseProfile(p map[string]any) vmProfile {
	prof := vmProfile{
		targetNode:   stringField(p, "target_node"),
		templateVMID: int(intField(p, "template_vmid")),
		newVMIDStart: int(intField(p, "new_vmid_start")),
		namePrefix:   stringField(p, "name_prefix"),
		fullClone:    true, // default
		cores:        defaultCores,
		memory:       defaultMemoryMB,
		storage:      stringField(p, "storage"),
		bridge:       stringField(p, "bridge"),
		cicustom:     stringField(p, "cicustom"),
		tags:         map[string]string{},
	}
	if prof.namePrefix == "" {
		prof.namePrefix = defaultNamePrefix
	}
	if v, ok := p["full_clone"].(bool); ok {
		prof.fullClone = v
	}
	if c := intField(p, "cores"); c > 0 {
		prof.cores = int(c)
	}
	if m := intField(p, "memory"); m > 0 {
		prof.memory = int(m)
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

// Create: clone from template_vmid -> SetVMConfig (resources + cloud-init
// userdata) -> start -> wait-until-ready (running + guest-agent IP). See package
// doc comment.
func (d *ProxmoxDriver) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	ctx := stream.Context()
	count := req.GetCount()
	if count <= 0 {
		count = 1
	}
	prof := parseProfile(req.GetProfile().AsMap())
	creds := credsFromMap(req.GetCredentials().AsMap())

	cli, err := newPveClient(ctx, creds)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.FailAuth, "pve-client", err)
	}

	backoff := defaultBackoff()

	// Idempotency: if live VMs already exist by runTag, reuse them and add only
	// missing ones. Without runTag, do not perform an idempotency check.
	var existing []ClusterVM
	if prof.runTag != "" {
		existing, err = d.findByRunTag(ctx, cli, backoff, prof.runTag)
		if err != nil {
			return sendCreateFailed(stream, clouddriver.Classify(classifyProxmox, err), "list-existing", err)
		}
		if int32(len(existing)) >= count {
			_ = stream.Send(&pluginv1.CreateEvent{
				Message: fmt.Sprintf("idempotent: %d VM already present for run %q", len(existing), prof.runTag),
			})
			return d.finalizeCreate(ctx, cli, stream, backoff, clusterVmIDs(existing))
		}
		count -= int32(len(existing))
	}

	if err := stream.Send(&pluginv1.CreateEvent{
		Message: fmt.Sprintf("pve clone count=%d template_vmid=%d target=%s full=%v",
			count, prof.templateVMID, prof.targetNode, prof.fullClone),
	}); err != nil {
		return err
	}

	newVMs, err := d.cloneInstances(ctx, cli, backoff, prof, req.GetUserdata(), count)
	if err != nil {
		// newVMs may contain ALREADY created VMs (before the clone loop failed) -
		// anti-orphan: the final event returns them with failed=true.
		fail := &pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyProxmox, err), "clone", err),
			Failed:  true,
		}
		for _, v := range newVMs {
			fail.Vms = append(fail.Vms, &pluginv1.VmInfo{VmId: formatVmID(v.Node, v.VMID)})
		}
		return stream.Send(fail)
	}

	all := make([]string, 0, len(existing)+len(newVMs))
	for _, e := range existing {
		all = append(all, formatVmID(e.Node, e.VMID))
	}
	for _, n := range newVMs {
		all = append(all, formatVmID(n.Node, n.VMID))
	}
	return d.finalizeCreate(ctx, cli, stream, backoff, all)
}

// createdVM is a pair (node, vmid) for a freshly created VM. The driver collects
// it during the clone loop to return anti-orphan IDs on ctx-cancel.
type createdVM struct {
	Node string
	VMID int
}

// cloneInstances calls Clone count times. Proxmox has no batch clone, so we go
// sequentially. Each VM name is `<prefix>-<vmid>`. Proxmox dislikes parallel
// clones of the same template VMID (locks), so it is strictly sequential.
//
// Anti-orphan contract: return already successfully cloned VMs EVEN on an error
// in a later step - Keeper sees them in the final failed event.
func (d *ProxmoxDriver) cloneInstances(
	ctx context.Context, cli pveAPI, backoff clouddriver.BackoffConfig,
	prof vmProfile, userdata string, count int32,
) ([]createdVM, error) {
	out := make([]createdVM, 0, count)
	for i := int32(0); i < count; i++ {
		newID, err := d.allocVMID(ctx, cli, backoff, prof, int(i))
		if err != nil {
			return out, fmt.Errorf("alloc vmid: %w", err)
		}
		name := fmt.Sprintf("%s-%d", prof.namePrefix, newID)

		cloneErr := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
			_, e := cli.CloneVM(ctx, CloneParams{
				SourceNode:    prof.targetNode, // template lives on target_node (best-effort default)
				TemplateVMID:  prof.templateVMID,
				NewVMID:       newID,
				Name:          name,
				TargetNode:    prof.targetNode,
				TargetStorage: prof.storage,
				FullClone:     prof.fullClone,
			})
			return e
		})
		if cloneErr != nil {
			return out, cloneErr
		}
		out = append(out, createdVM{Node: prof.targetNode, VMID: newID})

		// Apply post-clone params: resources (cores/memory), tags, cloud-init
		// userdata. Use one POST /config to take one write lock.
		fields := buildConfigFields(prof, userdata)
		if err := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
			return cli.SetVMConfig(ctx, prof.targetNode, newID, fields)
		}); err != nil {
			return out, fmt.Errorf("set-config: %w", err)
		}

		// Start the VM.
		if _, err := func() (string, error) {
			var upid string
			rerr := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
				var e error
				upid, e = cli.StartVM(ctx, prof.targetNode, newID)
				return e
			})
			return upid, rerr
		}(); err != nil {
			return out, fmt.Errorf("start: %w", err)
		}
	}
	return out, nil
}

// allocVMID chooses VMID for a new VM: profile.new_vmid_start+seq if set,
// otherwise /cluster/nextid. Take NextID, and on collision (race with another
// operator) the Retry wrapper repeats the whole clone - Proxmox returns 500 "VM
// already exists".
func (d *ProxmoxDriver) allocVMID(ctx context.Context, cli pveAPI, backoff clouddriver.BackoffConfig, prof vmProfile, seq int) (int, error) {
	if prof.newVMIDStart > 0 {
		return prof.newVMIDStart + seq, nil
	}
	var id int
	err := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
		var e error
		id, e = cli.NextID(ctx)
		return e
	})
	return id, err
}

// buildConfigFields builds Proxmox form params for /config: resources, tags,
// network bridge (over net0 if set), cloud-init userdata.
//
// userdata strategy:
//   - If profile.cicustom is set, use cicustom (snippet path) as-is. This is a
//     path in /var/lib/vz/snippets/<...> that the operator must prepare in advance.
//   - Otherwise userdata from CreateRequest (cloud-init blob) is base64-encoded.
//     Passing through ciuser/cipassword/ssh-keys is impossible (that field is not
//     passed as a string); instead Proxmox accepts userdata through `cicustom=
//     user=<snippet>`. Alternatives are `description` or `serial0=socket`, but
//     cloud-init does not interpret them.
//
// For MVP: if cicustom is not set AND userdata exists, place userdata in
// `description` (VM documentation field) and mark the VM with the tag
// `soulstack-needs-snippet=1` so the operator sees that automatic userdata
// delivery in Proxmox requires snippets storage, which the driver cannot create.
//
// This is an intentional pilot limitation - extension through automatic snippet
// placement (via WebDAV / SSH to node) is deferred to a separate architectural
// decision (requires SSH access to the node or PVE storage API).
func buildConfigFields(prof vmProfile, userdata string) map[string]string {
	fields := map[string]string{
		"cores":  strconv.Itoa(prof.cores),
		"memory": strconv.Itoa(prof.memory),
	}
	if prof.bridge != "" {
		// virtio + bridge. virtio is Proxmox default for cloud-init templates.
		fields["net0"] = fmt.Sprintf("virtio,bridge=%s", prof.bridge)
	}
	if len(prof.tags) > 0 {
		// Proxmox tags: semicolon-separated, format `key=value` or just `key`.
		// For runTagKey use `<key>=<value>`, others are `key=value`.
		var parts []string
		for k, v := range prof.tags {
			if v == "" {
				parts = append(parts, k)
			} else {
				parts = append(parts, fmt.Sprintf("%s=%s", k, v))
			}
		}
		fields["tags"] = strings.Join(parts, ";")
	}
	switch {
	case prof.cicustom != "":
		fields["cicustom"] = prof.cicustom
	case userdata != "":
		// Put base64-encoded userdata into description so it is available to the
		// operator for debugging. See buildConfigFields doc comment about the
		// limitation: Proxmox requires a snippet file on the node for cloud-init
		// user-data; fully in-band userdata delivery without snippet is not
		// supported by the API.
		fields["description"] = "soul-stack userdata (base64): " +
			base64.StdEncoding.EncodeToString([]byte(userdata))
	}
	return fields
}

// finalizeCreate waits for VM readiness (running + guest-agent IP) and sends the
// final event. Anti-orphan: on ctx-cancel/timeout, unfinished VMs go into the
// final event with failed=true but populated vm_id - Keeper can Destroy them.
func (d *ProxmoxDriver) finalizeCreate(
	ctx context.Context, cli pveAPI,
	stream grpc.ServerStreamingServer[pluginv1.CreateEvent],
	backoff clouddriver.BackoffConfig, vmIDs []string,
) error {
	probe := func(pctx context.Context, vmID string) clouddriver.ProbeResult {
		node, id, err := splitVmID(vmID)
		if err != nil {
			return clouddriver.ProbeResult{Err: err}
		}
		st, perr := cli.GetVMStatus(pctx, node, id)
		if perr != nil {
			if clouddriver.Classify(classifyProxmox, perr).Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{Err: perr}
		}
		// Proxmox does not return terminal "error/crashed" statuses; VM is either
		// running or stopped. Long-running "stopped" during clone/migrate is normal
		// - it is still in pipeline; detect this by the `lock` field.
		if st.Lock != "" {
			// Under locked (clone/migrate/backup), keep waiting.
			return clouddriver.ProbeResult{}
		}
		if st.Status != "running" {
			// VM has not started after start-RPC yet - keep polling; no terminal.
			return clouddriver.ProbeResult{}
		}
		// Running. Check guest-agent -> IP.
		ip, ipErr := cli.GetGuestAgentInterfaces(pctx, node, id)
		if ipErr != nil {
			// Guest-agent may be unconfigured/not responding. This is potentially a
			// template configuration error: fail-closed by the rule "no guest-agent -
			// no IP - VM cannot be used". Detect "not configured" by text - Proxmox
			// returns 500/502 depending on state.
			class := clouddriver.Classify(classifyProxmox, ipErr)
			if class.Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{
				Err: fmt.Errorf("guest-agent not responding (template likely missing qemu-guest-agent): %w", ipErr),
			}
		}
		if ip == "" {
			// Guest-agent responded, but IP is not assigned yet (DHCP handshake in
			// flight).
			return clouddriver.ProbeResult{}
		}
		return clouddriver.ProbeResult{Ready: true}
	}

	waitResults, waitErr := clouddriver.WaitUntilReady(ctx, backoff, vmIDs, probe,
		func(msg string) { _ = stream.Send(&pluginv1.CreateEvent{Message: msg}) })

	vms := make([]*pluginv1.VmInfo, 0, len(vmIDs))
	anyFailed := false
	for _, wr := range waitResults {
		vi := &pluginv1.VmInfo{VmId: wr.VMID}
		if wr.Ready {
			node, id, err := splitVmID(wr.VMID)
			if err == nil {
				if st, derr := cli.GetVMStatus(ctx, node, id); derr == nil {
					if ip, ierr := cli.GetGuestAgentInterfaces(ctx, node, id); ierr == nil && ip != "" {
						vi.PrimaryIp = ip
						vi.Fqdn = st.Name // Proxmox convention: name = hostname (SID)
					}
					vi.Attributes = statusAttributes(st)
				}
			}
		}
		if !wr.Ready {
			anyFailed = true
		}
		vms = append(vms, vi)
	}

	if waitErr != nil {
		return stream.Send(&pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyProxmox, waitErr), "wait-until-ready", waitErr),
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

// findByRunTag lists live (running/stopped, qemu-type) VMs with the given runTag
// in the cluster. Proxmox /cluster/resources does not support server-side tag
// filtering - filter on the driver side.
func (d *ProxmoxDriver) findByRunTag(ctx context.Context, cli pveAPI, backoff clouddriver.BackoffConfig, runTag string) ([]ClusterVM, error) {
	var out []ClusterVM
	err := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
		all, e := cli.ListClusterVMs(ctx)
		if e != nil {
			return e
		}
		out = nil
		for _, vm := range all {
			if vm.Type != "qemu" {
				continue
			}
			if hasTag(vm.Tags, runTagKey, runTag) {
				out = append(out, vm)
			}
		}
		return nil
	})
	return out, err
}

// hasTag checks whether a key=value pair exists in the semicolon-separated tag
// list. Proxmox tag list format is `t1;k=v;t2` without escaping; keys are unique.
func hasTag(tags, key, value string) bool {
	for _, t := range strings.Split(tags, ";") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if kv := strings.SplitN(t, "=", 2); len(kv) == 2 {
			if kv[0] == key && kv[1] == value {
				return true
			}
		}
	}
	return false
}

// Destroy: stop -> delete, per-VM events. Proxmox API DELETE on a running VM
// returns 500; stop (force) is required first. Idempotent on 404 / "does not
// exist".
func (d *ProxmoxDriver) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newPveClient(ctx, creds)
	if err != nil {
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(clouddriver.FailAuth, "pve-client", err), Failed: true})
		return nil
	}

	backoff := defaultBackoff()
	for _, vmIDStr := range req.GetVmIds() {
		node, id, perr := splitVmID(vmIDStr)
		if perr != nil {
			_ = stream.Send(&pluginv1.DestroyEvent{
				VmId:    vmIDStr,
				Message: clouddriver.FailMessage(clouddriver.FailInvalidParams, "parse-vm_id", perr),
				Failed:  true,
			})
			continue
		}

		// Stop. not_found at this step is ok; VM may have already been deleted.
		stopErr := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
			_, e := cli.StopVM(ctx, node, id)
			return e
		})
		if stopErr != nil {
			class := clouddriver.Classify(classifyProxmox, stopErr)
			if class == clouddriver.FailNotFound {
				_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmIDStr, Message: "already absent"})
				continue
			}
			// Proxmox may return 500 "VM is not running" - that is success for us.
			// The classifier maps it to transient, but Retry has already exhausted
			// attempts; recognize by body text.
			if isNotRunning(stopErr) {
				// continue to delete
			} else {
				_ = stream.Send(&pluginv1.DestroyEvent{
					VmId:    vmIDStr,
					Message: clouddriver.FailMessage(class, "stop", stopErr),
					Failed:  true,
				})
				continue
			}
		}

		// Delete. not_found -> idempotent success.
		delErr := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
			_, e := cli.DeleteVM(ctx, node, id)
			return e
		})
		if delErr != nil {
			class := clouddriver.Classify(classifyProxmox, delErr)
			if class == clouddriver.FailNotFound {
				_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmIDStr, Message: "already absent"})
				continue
			}
			_ = stream.Send(&pluginv1.DestroyEvent{
				VmId:    vmIDStr,
				Message: clouddriver.FailMessage(class, "delete", delErr),
				Failed:  true,
			})
			continue
		}
		_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmIDStr, Message: "deleted"})
	}
	return nil
}

// isNotRunning checks for "VM is not running" in the stop error body. Proxmox
// returns 500 with text instead of a separate status code. This heuristic is not
// risky: a false positive at most causes an extra delete call.
func isNotRunning(err error) bool {
	var hErr *pveHTTPError
	if !errors.As(err, &hErr) {
		return false
	}
	body := strings.ToLower(hErr.Body)
	return strings.Contains(body, "not running") || strings.Contains(body, "is stopped")
}

// Status polls one VM (GetVMStatus). credentials come in a separate
// StatusRequest.credentials field (A-flow, symmetrical with Create/Destroy).
func (d *ProxmoxDriver) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newPveClient(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("pve-client: %w", err)
	}
	node, id, perr := splitVmID(req.GetVmId())
	if perr != nil {
		return nil, fmt.Errorf("parse vm_id %q: %w", req.GetVmId(), perr)
	}
	st, err := cli.GetVMStatus(ctx, node, id)
	if err != nil {
		return nil, fmt.Errorf("GetVMStatus %s: %w", req.GetVmId(), err)
	}
	return &pluginv1.StatusReply{
		State:      st.Status,
		Attributes: statusAttributes(st),
	}, nil
}

// List streams VM inventory in the cluster (optionally filtered by runTag).
// credentials come in a separate ListRequest.credentials field (A-flow,
// symmetrical with Create/Destroy/Status). Per-VM IP is NOT requested through
// guest-agent (expensive for large inventory); primary_ip is filled only by
// Create.
func (d *ProxmoxDriver) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newPveClient(ctx, creds)
	if err != nil {
		return fmt.Errorf("pve-client: %w", err)
	}
	filter := req.GetFilter().AsMap()
	wantTag := stringField(filter, runTagKey)

	all, err := cli.ListClusterVMs(ctx)
	if err != nil {
		return fmt.Errorf("ListClusterVMs: %w", err)
	}
	for _, vm := range all {
		if vm.Type != "qemu" {
			continue
		}
		if wantTag != "" && !hasTag(vm.Tags, runTagKey, wantTag) {
			continue
		}
		attrs, _ := structpb.NewStruct(map[string]any{
			"node":   vm.Node,
			"status": vm.Status,
			"tags":   vm.Tags,
		})
		if serr := stream.Send(&pluginv1.VmInfo{
			VmId:       formatVmID(vm.Node, vm.VMID),
			Fqdn:       vm.Name, // Proxmox convention: name = hostname (SID)
			Attributes: attrs,
		}); serr != nil {
			return serr
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&ProxmoxDriver{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-proxmox:", err)
		os.Exit(1)
	}
}

// --- helpers ---

func sendCreateFailed(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error) error {
	return stream.Send(&pluginv1.CreateEvent{Message: clouddriver.FailMessage(class, op, err), Failed: true})
}

// formatVmID builds composite vm_id `<node>/<vmid>` (see package doc comment on
// vm_id paradigm).
func formatVmID(node string, vmid int) string {
	return fmt.Sprintf("%s/%d", node, vmid)
}

// splitVmID parses composite vm_id `<node>/<vmid>`. Returning an error means the
// vm_id was formed by another component (not our Create) - let Keeper see
// explicit invalid_params.
func splitVmID(s string) (node string, vmid int, err error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", 0, fmt.Errorf("vm_id must be `<node>/<vmid>`, got %q", s)
	}
	id, perr := strconv.Atoi(parts[1])
	if perr != nil {
		return "", 0, fmt.Errorf("vm_id vmid part %q: %w", parts[1], perr)
	}
	return parts[0], id, nil
}

func clusterVmIDs(vms []ClusterVM) []string {
	out := make([]string, 0, len(vms))
	for _, v := range vms {
		out = append(out, formatVmID(v.Node, v.VMID))
	}
	return out
}

// statusAttributes is a subset of VMStatus fields useful to the operator in
// proto Struct.
func statusAttributes(st VMStatus) *structpb.Struct {
	m := map[string]any{
		"node":   st.Node,
		"vmid":   float64(st.VMID),
		"status": st.Status,
		"name":   st.Name,
	}
	if st.QmpStatus != "" {
		m["qmp_status"] = st.QmpStatus
	}
	if st.Lock != "" {
		m["lock"] = st.Lock
	}
	if st.Maxmem > 0 {
		m["max_memory"] = float64(st.Maxmem)
	}
	if st.Maxdisk > 0 {
		m["max_disk"] = float64(st.Maxdisk)
	}
	if st.Cpus > 0 {
		m["cpus"] = st.Cpus
	}
	if st.Tags != "" {
		m["tags"] = st.Tags
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}
